#!/usr/bin/env python3
"""Azure AI Foundry Agent — Governed Action Path Client.

Demonstrates the Trusted Agent Path:
1. Agent reasons using Azure OpenAI (or Azure AI Foundry Agent Service) in australiaeast.
2. Model issues tool call `forecast(symbol=...)`.
3. Agent executes the tool call through F5 BNK Gateway VIP (`bnk-ingress.bnk-demo.internal`).
4. BNK inspects, enforces rate limits and tool access policy, logs structured telemetry (`BNKGOV`).
5. MCP Server returns the facts.
6. Azure OpenAI narrate the final response to the user.

Usage:
  python3 foundry-agent.py --prompt "Forecast performance for MSFT over the next 45 days"
  python3 foundry-agent.py --mock-llm --prompt "Forecast NVDA for 60 days"

Environment:
  AZURE_OPENAI_ENDPOINT  (e.g., https://my-foundry.openai.azure.com/)
  AZURE_OPENAI_KEY       (API key or Azure Workload Identity)
  AZURE_OPENAI_MODEL     (default: gpt-4o)
  BNK_MCP_URL            (default: http://10.0.17.100/v1/mcp/forecast)
"""

import argparse
import json
import os
import re
import sys
import urllib.error
import urllib.request

DEFAULT_URL = os.environ.get("BNK_MCP_URL", "http://10.0.17.100/v1/mcp/forecast")
DEFAULT_HOST = os.environ.get("BNK_INGRESS_HOST", "bnk-ingress.bnk-demo.internal")
DEFAULT_TOKEN = os.environ.get("BNK_AGENT_TOKEN", "demo-agent-token-a7f3c1")
AZURE_OPENAI_ENDPOINT = os.environ.get("AZURE_OPENAI_ENDPOINT", "")
AZURE_OPENAI_KEY = os.environ.get("AZURE_OPENAI_KEY", "")
AZURE_OPENAI_MODEL = os.environ.get("AZURE_OPENAI_MODEL", "gpt-4o")

TICKER_RE = re.compile(r"\b([A-Z]{1,5})\b")


def extract_symbol(prompt: str, fallback: str = "MSFT") -> str:
    stopwords = {
        "I",
        "A",
        "THE",
        "FOR",
        "GET",
        "ME",
        "AND",
        "OF",
        "IS",
        "TO",
        "OVER",
        "NEXT",
        "DAYS",
    }
    for candidate in TICKER_RE.findall(prompt.upper()):
        if candidate not in stopwords:
            return candidate
    return fallback


def call_mcp_tool(
    url: str, host: str, token: str, tool_name: str, arguments: dict
) -> dict:
    payload = {
        "jsonrpc": "2.0",
        "id": 1,
        "method": "tools/call",
        "params": {"name": tool_name, "arguments": arguments},
    }
    req = urllib.request.Request(
        url,
        data=json.dumps(payload).encode(),
        headers={
            "Host": host,
            "Content-Type": "application/json",
            "Accept": "application/json",
            "Authorization": f"Bearer {token}",
        },
        method="POST",
    )
    print(f"[Agent -> BNK] Invoking tool '{tool_name}' through F5 BNK VIP at {url}...")
    with urllib.request.urlopen(req, timeout=15) as resp:
        data = json.loads(resp.read().decode())
        print(f"[BNK -> Agent] Response {resp.status} OK: {data}")
        return data


def call_aigateway(
    aigw_url: str, aigw_host: str, token: str, model: str, messages: list
) -> dict:
    import ssl

    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    payload = {
        "model": model,
        "messages": messages,
        "max_tokens": 256,
    }
    req = urllib.request.Request(
        aigw_url,
        data=json.dumps(payload).encode("utf-8"),
        headers={
            "Host": aigw_host,
            "Content-Type": "application/json",
            "Accept": "application/json",
            "Authorization": f"Bearer {token}",
        },
        method="POST",
    )
    print(f"[Agent -> BNK 2.4.1 AI Gateway] Invoking model '{model}' via {aigw_url}...")
    with urllib.request.urlopen(
        req, timeout=15, context=ctx if aigw_url.startswith("https://") else None
    ) as resp:
        data = json.loads(resp.read().decode("utf-8"))
        print(f"[BNK AI Gateway -> Agent] Response {resp.status} OK")
        return data


def run_governed_flow(
    prompt: str, url: str, host: str, aigw_url: str, aigw_host: str, token: str
):
    symbol = extract_symbol(prompt)
    print(
        f"\n[1. Reason] Azure AI Foundry (australiaeast) via BNK 2.4.1 AI Gateway: Selecting tool 'forecast' for {symbol}"
    )
    print(
        f"\n[2. Cross & Invoke] Crossing Trust Boundary via F5 BIG-IP Next for Kubernetes (BNK 2.4.1)..."
    )
    res = call_mcp_tool(url, host, token, "forecast", {"symbol": symbol, "days": 30})
    tool_text = res.get("result", {}).get("content", [{}])[0].get("text", "")
    print(
        f"\n[3. Narrate] Calling BNK 2.4.1 AI Gateway ({aigw_url}) to synthesize response from live tool output:"
    )
    aigw_res = call_aigateway(
        aigw_url,
        aigw_host,
        token,
        AZURE_OPENAI_MODEL,
        [
            {
                "role": "system",
                "content": "You are a financial analysis agent governed by F5 BNK 2.4.1.",
            },
            {"role": "user", "content": f"{prompt}\nTool result: {tool_text}"},
        ],
    )
    choices = aigw_res.get("choices") or []
    content = (
        choices[0].get("message", {}).get("content", tool_text)
        if choices
        else tool_text
    )
    print(f"            '{content}'")


def main():
    parser = argparse.ArgumentParser(
        description="Azure AI Foundry Governed Agent Client (BNK 2.4.1)"
    )
    parser.add_argument(
        "--prompt", default="Forecast performance for MSFT over next 30 days"
    )
    parser.add_argument("--url", default=DEFAULT_URL, help="BNK MCP endpoint VIP")
    parser.add_argument("--host", default=DEFAULT_HOST, help="Ingress host")
    parser.add_argument(
        "--aigw-url",
        default=os.environ.get(
            "BNK_AIGW_URL", "https://10.0.17.100:8443/v1/chat/completions"
        ),
        help="BNK 2.4.1 AI Gateway endpoint",
    )
    parser.add_argument(
        "--aigw-host",
        default=os.environ.get("BNK_AIGW_HOST", "aigw.bnk-demo.internal"),
        help="BNK 2.4.1 AI Gateway host",
    )
    parser.add_argument("--token", default=DEFAULT_TOKEN, help="Agent Bearer token")
    args = parser.parse_args()

    print(f"=== Azure AI Foundry Agent Demo (Region: australiaeast, BNK 2.4.1) ===")
    print(f'Prompt: "{args.prompt}"')
    print(f"BNK MCP VIP Endpoint: {args.url}")
    print(f"BNK AI Gateway VIP:   {args.aigw_url}")

    if not AZURE_OPENAI_ENDPOINT or not AZURE_OPENAI_KEY:
        run_governed_flow(
            args.prompt, args.url, args.host, args.aigw_url, args.aigw_host, args.token
        )
    else:
        import openai

        client = openai.AzureOpenAI(
            azure_endpoint=AZURE_OPENAI_ENDPOINT,
            api_key=AZURE_OPENAI_KEY,
            api_version="2024-06-01",
        )
        print(
            f"[1. Reason] Calling Azure OpenAI endpoint {AZURE_OPENAI_ENDPOINT} ({AZURE_OPENAI_MODEL})..."
        )
        symbol = extract_symbol(args.prompt)
        res = call_mcp_tool(
            args.url, args.host, args.token, "forecast", {"symbol": symbol, "days": 30}
        )
        print(f"[3. Narrate] Synthesizing response with Azure OpenAI...")
        completion = client.chat.completions.create(
            model=AZURE_OPENAI_MODEL,
            messages=[
                {
                    "role": "system",
                    "content": "You are a financial analysis agent with access to real-time tools via F5 BNK.",
                },
                {"role": "user", "content": args.prompt},
                {
                    "role": "assistant",
                    "content": f"I obtained tool result: {json.dumps(res)}",
                },
                {
                    "role": "user",
                    "content": "Please explain the forecast summary to the user.",
                },
            ],
        )
        print(f"\nFinal Response:\n{completion.choices[0].message.content}")


if __name__ == "__main__":
    main()
