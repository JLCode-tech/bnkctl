#!/usr/bin/env python3
"""
mcp-server.py — Governed FastMCP Server for Google Cloud Vertex AI & GKE.
Pure standard-library implementation (no pip install required).

Exposes financial tools over Streamable-HTTP / JSON-RPC:
- forecast: calculate market projection for a symbol
- get_account_balance: sensitive financial inquiry requiring privileged token
"""

import json
import logging
import os
import random
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Dict

logging.basicConfig(
    level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s"
)
logger = logging.getLogger("vertex-mcp-tool")

LISTEN_PORT = int(os.environ.get("MCP_PORT", "8080"))
AGENT_TOKEN = os.environ.get("MCP_AGENT_TOKEN", "demo-agent-token-a7f3c1")
EXTERNAL_TOKEN = os.environ.get("MCP_EXTERNAL_TOKEN", "demo-external-token-4b9e2d")
PUBLIC_URL = os.environ.get(
    "PUBLIC_URL", "http://bnk-ingress.bnk-demo.internal/v1/mcp/forecast"
)


def calculate_forecast(symbol: str, days: int = 30) -> Dict[str, Any]:
    """Calculate price and trend forecast for a given stock symbol."""
    base_price = {
        "GOOGL": 190.50,
        "AAPL": 225.00,
        "MSFT": 440.00,
        "NVDA": 128.00,
    }.get(symbol.upper(), 100.0)

    seed = abs(hash(f"{symbol.upper()}:{days}"))
    drift = ((seed % 70) - 20) / 1000.0
    projected = round(base_price * ((1 + drift) ** (days / 30)), 2)
    volatility = round(0.12 + ((seed % 160) / 1000.0), 3)

    return {
        "symbol": symbol.upper(),
        "horizon_days": days,
        "current_price": base_price,
        "projected_price": projected,
        "expected_return_pct": round(((projected - base_price) / base_price) * 100, 2),
        "volatility": volatility,
        "confidence": "HIGH" if volatility < 0.20 else "MEDIUM",
        "governed_by": "F5 BIG-IP Next on GKE (Host-Device Dataplane)",
    }


def get_balance(account_id: str) -> Dict[str, Any]:
    """Retrieve account balance for a given account ID."""
    return {
        "account_id": account_id,
        "currency": "USD",
        "cash_balance": 450000.00,
        "margin_available": 150000.00,
        "portfolio_valuation": 2480920.50,
        "risk_tier": "TIER_1_ENTERPRISE",
        "governed_by": "F5 BIG-IP Next on GKE (Host-Device Dataplane)",
    }


class MCPHandler(BaseHTTPRequestHandler):
    def send_json(self, status_code: int, data: Any):
        body = json.dumps(data).encode("utf-8")
        self.send_response(status_code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def check_auth(self) -> bool:
        auth_header = self.headers.get("Authorization", "")
        if not auth_header.startswith("Bearer "):
            self.send_json(
                401,
                {
                    "error": "Unauthorized. Bearer token required across F5 BNK action gateway."
                },
            )
            return False

        token = auth_header[7:].strip()
        if token not in (AGENT_TOKEN, EXTERNAL_TOKEN):
            self.send_json(403, {"error": "Forbidden. Invalid token supplied."})
            return False

        return True

    def do_GET(self):
        path = self.path.split("?")[0]
        if path == "/healthz":
            self.send_json(
                200, {"status": "healthy", "service": "gke-vertex-finance-tool"}
            )
            return

        if path == "/.well-known/agent-card.json":
            self.send_json(
                200,
                {
                    "name": "gke-vertex-finance-tool",
                    "description": "Financial forecasting and portfolio service governed by F5 BNK on GKE",
                    "version": "1.0.0",
                    "url": PUBLIC_URL,
                    "capabilities": ["tools"],
                    "governance": {
                        "gateway": "F5 BIG-IP Next for Kubernetes",
                        "cloud": "Google Cloud GKE",
                        "agent_framework": "Google Cloud Vertex AI Reasoning Engine",
                    },
                },
            )
            return

        if path in ("/", "/v1/mcp/forecast"):
            if not self.check_auth():
                return
            res = calculate_forecast("GOOGL", 30)
            self.send_json(200, res)
            return

        self.send_json(404, {"error": f"Path '{path}' not found"})

    def do_POST(self):
        path = self.path.split("?")[0]
        if path in ("/", "/v1/mcp/forecast"):
            if not self.check_auth():
                return

            clen = int(self.headers.get("Content-Length", 0))
            body_bytes = self.rfile.read(clen) if clen > 0 else b"{}"
            try:
                body = json.loads(body_bytes.decode("utf-8"))
            except Exception:
                body = {}

            method = body.get("method")
            params = body.get("params", {})
            req_id = body.get("id", 1)

            # Tool listing
            if method == "tools/list":
                self.send_json(
                    200,
                    {
                        "jsonrpc": "2.0",
                        "id": req_id,
                        "result": {
                            "tools": [
                                {
                                    "name": "forecast",
                                    "description": "Forecast symbol price and trend over horizon days",
                                    "inputSchema": {
                                        "type": "object",
                                        "properties": {
                                            "symbol": {"type": "string"},
                                            "days": {"type": "integer", "default": 30},
                                        },
                                        "required": ["symbol"],
                                    },
                                },
                                {
                                    "name": "get_account_balance",
                                    "description": "Sensitive tool returning confidential balance",
                                    "inputSchema": {
                                        "type": "object",
                                        "properties": {
                                            "account_id": {"type": "string"},
                                        },
                                        "required": ["account_id"],
                                    },
                                },
                            ]
                        },
                    },
                )
                return

            # Tool invocation
            if method == "tools/call":
                tool_name = params.get("name")
                args = params.get("arguments", {})

                if tool_name == "forecast":
                    res = calculate_forecast(
                        args.get("symbol", "GOOGL"), int(args.get("days", 30))
                    )
                    self.send_json(
                        200,
                        {
                            "jsonrpc": "2.0",
                            "id": req_id,
                            "result": {
                                "content": [{"type": "text", "text": json.dumps(res)}]
                            },
                        },
                    )
                    return

                if tool_name == "get_account_balance":
                    res = get_balance(args.get("account_id", "ACC-7890"))
                    self.send_json(
                        200,
                        {
                            "jsonrpc": "2.0",
                            "id": req_id,
                            "result": {
                                "content": [{"type": "text", "text": json.dumps(res)}]
                            },
                        },
                    )
                    return

                self.send_json(
                    404,
                    {
                        "jsonrpc": "2.0",
                        "id": req_id,
                        "error": {
                            "code": -32601,
                            "message": f"Tool '{tool_name}' not found",
                        },
                    },
                )
                return

            if "symbol" in body:
                res = calculate_forecast(
                    body.get("symbol", "GOOGL"), int(body.get("days", 30))
                )
                self.send_json(200, res)
                return

            self.send_json(
                200,
                {
                    "status": "ready",
                    "service": "gke-vertex-finance-mcp",
                    "hint": "Use JSON-RPC method tools/call or tools/list",
                },
            )
            return

        self.send_json(404, {"error": f"Path '{path}' not found"})

    def log_message(self, format, *args):
        logger.info(
            "%s - - [%s] %s"
            % (self.client_address[0], self.log_date_time_string(), format % args)
        )


def run_server():
    server = ThreadingHTTPServer(("0.0.0.0", LISTEN_PORT), MCPHandler)
    logger.info(f"Starting Governed MCP Server on port {LISTEN_PORT}...")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    run_server()
