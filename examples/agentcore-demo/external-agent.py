#!/usr/bin/env python3
"""Unmanaged external agent — the "stranger path" client.

Simulates a caller that never touched AWS: another cloud, a partner, a script,
a compromised workload. No AgentCore component is in this path, so F5 BNK is
the only checkpoint between it and the MCP tool.

Run from inside the VPC — the BNK VIP is private and not internet-exposed.

  python3 external-agent.py --prompt "forecast NVDA"
  python3 external-agent.py --tool get_account_balance --account ACC-1001

Exit codes:  0 allowed · 1 refused by policy (401/403/429) · 2 transport error
"""

import argparse
import json
import os
import re
import sys
import urllib.error
import urllib.request

DEFAULT_URL = os.environ.get("BNK_MCP_URL", "http://10.0.10.150/v1/mcp/forecast")
DEFAULT_HOST = os.environ.get("BNK_INGRESS_HOST", "bnk-ingress.bnk-demo.internal")
# Demo credential, matching MCP_EXTERNAL_TOKEN in mcp-tool/kustomization.yaml.
DEFAULT_TOKEN = os.environ.get("BNK_MCP_TOKEN", "demo-external-token-4b9e2d")

# Uppercase 1-5 char run, the usual ticker shape.
TICKER_RE = re.compile(r"\b([A-Z]{1,5})\b")


def extract_symbol(prompt: str, fallback: str = "AAPL") -> str:
    """Pull a ticker out of the prompt, ignoring common English words."""
    stopwords = {"I", "A", "THE", "FOR", "GET", "ME", "AND", "OF", "IS", "TO"}
    for candidate in TICKER_RE.findall(prompt.upper()):
        if candidate not in stopwords:
            return candidate
    return fallback


def build_payload(args: argparse.Namespace) -> dict:
    if args.tool == "get_account_balance":
        arguments = {"account_id": args.account}
    else:
        arguments = {"symbol": extract_symbol(args.prompt), "days": args.days}
    return {
        "jsonrpc": "2.0",
        "id": 1,
        "method": "tools/call",
        "params": {"name": args.tool, "arguments": arguments},
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--prompt", default="forecast AAPL", help="free-text prompt; a ticker is extracted from it")
    parser.add_argument("--tool", default="forecast",
                        choices=["forecast", "get_account_balance"],
                        help="MCP tool to invoke (get_account_balance is privileged and should be refused)")
    parser.add_argument("--account", default="ACC-1001", help="account id for get_account_balance")
    parser.add_argument("--days", type=int, default=30, help="forecast horizon")
    parser.add_argument("--url", default=DEFAULT_URL, help="BNK VIP MCP endpoint")
    parser.add_argument("--host", default=DEFAULT_HOST, help="Host header the HTTPRoute matches on")
    parser.add_argument("--token", default=DEFAULT_TOKEN, help="bearer token to present")
    parser.add_argument("--timeout", type=float, default=15.0, help="request timeout, seconds")
    args = parser.parse_args()

    payload = build_payload(args)
    request = urllib.request.Request(
        args.url,
        data=json.dumps(payload).encode(),
        headers={
            "Host": args.host,
            "Content-Type": "application/json",
            "Accept": "application/json",
            "Authorization": f"Bearer {args.token}",
        },
        method="POST",
    )

    print(f"external-agent -> {args.tool} via {args.url}")
    try:
        with urllib.request.urlopen(request, timeout=args.timeout) as response:
            print(f"  {response.status} allowed")
            print(f"  {response.read().decode()}")
            return 0
    except urllib.error.HTTPError as exc:
        body = exc.read().decode(errors="replace")
        label = {
            401: "refused — authentication",
            403: "refused — authorization (BNK policy)",
            429: "refused — rate limit (BNK policy)",
        }.get(exc.code, "error")
        print(f"  {exc.code} {label}")
        print(f"  {body}")
        return 1 if exc.code in (401, 403, 429) else 2
    except urllib.error.URLError as exc:
        # No response at all: firewall drop, wrong VIP, or no route from here.
        print(f"  no response: {exc.reason}")
        return 2


if __name__ == "__main__":
    sys.exit(main())
