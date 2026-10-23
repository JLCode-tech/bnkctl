#!/usr/bin/env python3
"""
vertex_agent.py — Google Cloud Vertex AI Agent Runner with F5 BNK Action Path Governance.

Demonstrates how a Google Cloud Vertex AI Reasoning Engine / Gemini Agent:
1. Receives an analytical instruction from the user.
2. Formulates a plan and selects the MCP financial forecasting tool.
3. Invokes the FastMCP service over the F5 BNK Governed Ingress VIP on GKE.
4. Governs the request (rate limits, bearer auth, multi-network isolation).
5. Narrates the verified market projection back to the user.
"""

import argparse
import json
import logging
import os
import sys
import time
import urllib.request
import urllib.error
from typing import Any, Dict

logging.basicConfig(
    level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s"
)
logger = logging.getLogger("vertex-agent-runner")

# Target tool URL: defaults to BNK Ingress VIP or local cluster endpoint
DEFAULT_TOOL_URL = os.environ.get("MCP_TOOL_URL", "http://10.10.1.10/v1/mcp/forecast")
AGENT_TOKEN = os.environ.get("MCP_AGENT_TOKEN", "demo-agent-token-a7f3c1")


class VertexAgentRunner:
    def __init__(self, tool_url: str = DEFAULT_TOOL_URL, token: str = AGENT_TOKEN):
        self.tool_url = tool_url
        self.token = token
        self.project_id = os.environ.get("GCP_PROJECT", "demo-vertex-project")
        self.location = os.environ.get("GCP_LOCATION", "australia-southeast1")
        self.model_name = os.environ.get("VERTEX_MODEL", "gemini-2.5-pro")

    def call_governed_tool(
        self, tool_name: str, arguments: Dict[str, Any]
    ) -> Dict[str, Any]:
        """Invoke MCP tool via F5 BNK Ingress on GKE."""
        logger.info(
            f"Steering tool call '{tool_name}' through F5 BIG-IP Next on GKE..."
        )
        logger.info(f"Destination Gateway: {self.tool_url}")

        payload = {
            "jsonrpc": "2.0",
            "id": int(time.time()),
            "method": "tools/call",
            "params": {
                "name": tool_name,
                "arguments": arguments,
            },
        }

        headers = {
            "Content-Type": "application/json",
            "Authorization": f"Bearer {self.token}",
            "Host": "bnk-ingress.bnk-demo.internal",
            "X-Agent-Framework": "Google Cloud Vertex AI Reasoning Engine",
            "X-Agent-Model": self.model_name,
        }

        data_bytes = json.dumps(payload).encode("utf-8")
        req = urllib.request.Request(self.tool_url, data=data_bytes, headers=headers)

        with urllib.request.urlopen(req, timeout=10) as resp:
            resp_data = json.loads(resp.read().decode("utf-8"))
            result_content = (
                resp_data.get("result", {}).get("content", [{}])[0].get("text", "{}")
            )
            return json.loads(result_content)

    def run(self, symbol: str, days: int = 30) -> None:
        """Execute Reasoning -> Tool Call -> Synthesis loop."""
        print("\n" + "=" * 70)
        print("  GOOGLE CLOUD VERTEX AI REASONING ENGINE + F5 BNK ON GKE")
        print("=" * 70)
        print(f"• Reasoning Engine:  Vertex AI ({self.model_name})")
        print(f"• Project / Region:  {self.project_id} / {self.location}")
        print(f"• Action Gateway:    F5 BIG-IP Next on GKE ({self.tool_url})")
        print(
            f"• User Request:      'Forecast stock performance for {symbol} over {days} days'"
        )
        print("-" * 70)

        # 1. Reasoning & Tool Selection
        print("[1] Reasoning (Gemini 2.5 Pro):")
        print(f"    - Need predictive analytics for symbol {symbol}.")
        print(
            f"    - Selecting tool: 'forecast' with arguments: {{'symbol': '{symbol}', 'days': {days}}}"
        )
        time.sleep(0.5)

        # 2. Governed Tool Execution
        print("\n[2] Executing Governed Action:")
        tool_result = self.call_governed_tool(
            "forecast", {"symbol": symbol, "days": days}
        )
        print(f"    - Result Received: {json.dumps(tool_result, indent=6)}")
        time.sleep(0.5)

        # 3. Final Narration Synthesis
        print("\n[3] Agent Narration (Vertex AI Response to User):")
        print(
            f"    Based on the analysis governed across F5 BNK on GKE:\n"
            f"    • Symbol:               {tool_result.get('symbol')}\n"
            f"    • Current Price:        ${tool_result.get('current_price')}\n"
            f"    • Projected ({days}d):       ${tool_result.get('projected_price')} ({tool_result.get('expected_return_pct'):+0.2f}%)\n"
            f"    • Confidence:           {tool_result.get('confidence')} (Volatility: {tool_result.get('volatility')})\n"
            f"    • Security & Policy:    Governed by F5 BIG-IP Next on GKE\n"
        )
        print("=" * 70 + "\n")


def main():
    parser = argparse.ArgumentParser(
        description="Vertex AI Agent Runner on GKE with F5 BNK"
    )
    parser.add_argument("--symbol", default="GOOGL", help="Stock ticker symbol")
    parser.add_argument("--days", type=int, default=30, help="Forecast horizon in days")
    parser.add_argument("--url", default=DEFAULT_TOOL_URL, help="F5 BNK Ingress URL")
    args = parser.parse_args()

    agent = VertexAgentRunner(tool_url=args.url)
    agent.run(symbol=args.symbol, days=args.days)


if __name__ == "__main__":
    main()
