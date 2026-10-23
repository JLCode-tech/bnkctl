import logging
import os

from mcp.client.streamable_http import streamablehttp_client
from strands.tools.mcp.mcp_client import MCPClient

logger = logging.getLogger(__name__)

# The MCP tool sits behind the F5 BNK VIP, resolved via the demo's private
# Route 53 zone. BNK enforces the rate limit and the privileged-tool decision
# in the data path before this ever reaches the pod.
MCP_URL = os.environ.get(
    "FINANCE_MCP_URL", "http://bnk-ingress.bnk-demo.internal/v1/mcp/forecast"
)

# Demo credential, matching MCP_AGENT_TOKEN in mcp-tool/kustomization.yaml. The
# agent token is the privileged one — it may reach every tool. A real deployment
# would source this from AgentCore Identity or Secrets Manager, not a default.
MCP_TOKEN = os.environ.get("FINANCE_MCP_TOKEN", "demo-agent-token-a7f3c1")


def get_finance_tool_mcp_client() -> MCPClient | None:
    """Returns an MCP Client for the finance-tool remote MCP server."""
    headers = {"Authorization": f"Bearer {MCP_TOKEN}"}
    logger.info("finance-tool MCP client -> %s", MCP_URL)
    return MCPClient(lambda: streamablehttp_client(MCP_URL, headers=headers))


def get_all_remote_mcp_clients() -> list[MCPClient]:
    """Returns all configured remote MCP clients."""
    clients = [get_finance_tool_mcp_client()]
    return [c for c in clients if c is not None]
