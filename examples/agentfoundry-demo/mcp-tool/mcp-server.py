#!/usr/bin/env python3
"""
Minimal MCP financial tool for the F5 BNK + Azure AgentFoundry demo.

MCP over streamable-http (stateless) — the transport used by both the Azure AI
Foundry agent runtime's MCP client and external-agent.py.

Endpoints, as a client sees them through BNK:
  POST /v1/mcp/forecast                              MCP JSON-RPC, auth required
  GET  /v1/mcp/forecast/.well-known/agent-card.json  A2A card, unauthenticated

Two tools on purpose:
  forecast(symbol, days)        benign  — any authenticated caller
  get_account_balance(account)  SENSITIVE — privileged callers only

BNK enforces rate limiting and sensitive tool restrictions at the cluster network boundary.
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

AGENT_TOKEN = os.environ.get("MCP_AGENT_TOKEN", "demo-agent-token-a7f3c1")
EXTERNAL_TOKEN = os.environ.get("MCP_EXTERNAL_TOKEN", "demo-external-token-4b9e2d")

PUBLIC_URL = os.environ.get(
    "MCP_PUBLIC_URL", "http://bnk-ingress.bnk-demo.internal/v1/mcp/forecast"
)
LISTEN_PORT = int(os.environ.get("MCP_PORT", "8080"))

PUBLIC_PATHS = frozenset({"/.well-known/agent-card.json"})


class BearerAuthMiddleware(BaseHTTPMiddleware):
    """Require a known bearer token, and record which caller it identifies."""

    async def dispatch(self, request: Request, call_next):
        if request.url.path in PUBLIC_PATHS:
            return await call_next(request)

        scheme, _, presented = request.headers.get("authorization", "").partition(" ")
        if scheme.lower() != "bearer" or not presented:
            return self._unauthorized("missing or malformed Authorization header")

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
        "description": "Financial forecasting tool for Azure AgentFoundry demo",
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
        "provider": {"name": "F5 BNK Azure Demo"},
    }


async def agent_card(request: Request) -> Response:
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
        log.error("MCP_AGENT_TOKEN is unset — refusing to start.")
        sys.exit(1)

    import uvicorn

    log.info("finance-tool on :%d, advertising %s", LISTEN_PORT, PUBLIC_URL)
    uvicorn.run(build_app(), host="0.0.0.0", port=LISTEN_PORT)
