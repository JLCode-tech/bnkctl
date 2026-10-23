#!/usr/bin/env python3
"""
Minimal MCP financial tool for the F5 BNK + AgentCore demo.

MCP over streamable-http (stateless) — the transport used by both the AgentCore
runtime's MCP client and external-agent.py.

Endpoints, as a client sees them through BNK:
  POST /v1/mcp/forecast                              MCP JSON-RPC, auth required
  GET  /v1/mcp/forecast/.well-known/agent-card.json  A2A card, unauthenticated

Two tools on purpose:
  forecast(symbol, days)        benign  — any authenticated caller
  get_account_balance(account)  SENSITIVE — privileged callers only

The second tool exists so authorization has something to be about. BNK decides
who may reach it; this service is the last line rather than the only one.

DEMO SCOPE: both tools return made-up data. The point is to be a realistic MCP
endpoint worth governing, not a real finance service.
"""

import hmac
import json
import logging
import os
import random
import sys

from fastmcp import FastMCP
from starlette.applications import Starlette
from starlette.middleware.base import BaseHTTPMiddleware
from starlette.requests import Request
from starlette.responses import JSONResponse, Response
from starlette.routing import Mount, Route

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("finance-tool")

# Accepted bearer tokens, supplied by the Secret in kustomization.yaml.
# Deliberately fails closed: a governance demo that runs open when
# misconfigured teaches exactly the wrong lesson.
AGENT_TOKEN = os.environ.get("MCP_AGENT_TOKEN", "")
EXTERNAL_TOKEN = os.environ.get("MCP_EXTERNAL_TOKEN", "")

PUBLIC_URL = os.environ.get(
    "MCP_PUBLIC_URL", "http://bnk-ingress.bnk-demo.internal/v1/mcp/forecast"
)
LISTEN_PORT = int(os.environ.get("MCP_PORT", "8080"))

# Agent-card discovery is public by design: Forge and A2A clients read it before
# they hold a credential, and it exposes no data.
PUBLIC_PATHS = frozenset({"/.well-known/agent-card.json"})


class BearerAuthMiddleware(BaseHTTPMiddleware):
    """Require a known bearer token, and record which caller it identifies.

    App-level defence in depth — NOT the demo's primary control. F5 BNK enforces
    the rate limit and the sensitive-tool decision in the data path, before a
    request reaches this pod. A static token map is a stand-in so the
    Authorization header means something; BNK can validate real JWTs via
    F5BigAccessJwtConfig, and AgentCore Policy does identity-aware per-tool
    authorization properly when a Gateway is in the path.
    """

    async def dispatch(self, request: Request, call_next):
        if request.url.path in PUBLIC_PATHS:
            return await call_next(request)

        scheme, _, presented = request.headers.get("authorization", "").partition(" ")
        if scheme.lower() != "bearer" or not presented:
            return self._unauthorized("missing or malformed Authorization header")

        # Constant-time compares: token checks should not leak length or prefix.
        if hmac.compare_digest(presented, AGENT_TOKEN):
            caller = "agent"
        elif EXTERNAL_TOKEN and hmac.compare_digest(presented, EXTERNAL_TOKEN):
            caller = "external"
        else:
            return self._unauthorized("unrecognised bearer token")

        log.debug("authenticated caller=%s path=%s", caller, request.url.path)
        return await call_next(request)

    @staticmethod
    def _unauthorized(reason: str) -> JSONResponse:
        return JSONResponse(
            {
                "jsonrpc": "2.0",
                "id": None,
                "error": {"code": -32001, "message": f"unauthorized: {reason}"},
            },
            status_code=401,
            headers={"WWW-Authenticate": "Bearer"},
        )


def _make_agent_card() -> dict:
    return {
        "name": "Finance Tool",
        "description": "Financial forecasting tool",
        "url": PUBLIC_URL,
        "version": "1.0.0",
        "protocolVersion": "a2a-v1.0",
        "capabilities": {"streaming": False, "pushNotifications": False},
        "skills": [
            {
                "id": "financial-forecast",
                "name": "Financial Forecast",
                "description": "Provides a simple financial forecast for a symbol.",
                "tags": ["finance", "forecast"],
                "examples": ["forecast AAPL for 30 days"],
            },
            {
                "id": "account-balance",
                "name": "Account Balance",
                "description": "Reads a customer account balance. Access is restricted at the network edge.",
                "tags": ["finance", "account", "sensitive"],
                "examples": ["balance for account ACC-1001"],
            },
        ],
        "defaultInputModes": ["text"],
        "defaultOutputModes": ["text"],
        "provider": {"name": "F5 BNK Demo"},
    }


async def agent_card(request: Request) -> Response:
    # Forge probes via the k8s service proxy API, which reads text/plain, runs
    # json.loads() -> str(), then json.loads() again. Returning a JSON-encoded
    # JSON string makes Forge's final decode yield the dict.
    return Response(
        content=json.dumps(json.dumps(_make_agent_card())), media_type="text/plain"
    )


mcp = FastMCP("finance-tool")


@mcp.tool()
def forecast(symbol: str, days: int = 30) -> str:
    """Provides a simple financial forecast for a symbol."""
    growth = (abs(hash(f"{symbol}:{days}")) % 16) + 5
    return f"Forecast for {symbol} over {days} days: up (expected growth: {growth}%)."


@mcp.tool()
def get_account_balance(account_id: str) -> str:
    """Returns the current balance for a customer account, given its account id."""
    # Deterministic from the account id so demo output is stable.
    cents = (abs(hash(account_id)) % 9_000_000) + 1_000_000
    return f"Account {account_id} balance: ${cents / 100:,.2f} (as of today)."


def build_app() -> Starlette:
    mcp_app = mcp.http_app(
        transport="streamable-http",
        path="/",
        json_response=True,
        stateless_http=True,
    )
    app = Starlette(
        debug=False,
        lifespan=mcp_app.router.lifespan_context,
        routes=[
            Route("/.well-known/agent-card.json", agent_card),
            Mount("/", app=mcp_app),
        ],
    )
    app.add_middleware(BearerAuthMiddleware)
    return app


if __name__ == "__main__":
    if not AGENT_TOKEN:
        log.error(
            "MCP_AGENT_TOKEN is unset — refusing to start. Apply the whole "
            "kustomize base (mcp-tool/), which generates the Secret."
        )
        sys.exit(1)

    import uvicorn

    log.info("finance-tool on :%d, advertising %s", LISTEN_PORT, PUBLIC_URL)
    uvicorn.run(build_app(), host="0.0.0.0", port=LISTEN_PORT)
