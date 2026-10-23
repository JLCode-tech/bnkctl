# Google Cloud Vertex AI Agent + F5 BNK Runner

This agent runs on Google Cloud Vertex AI (or containerized in GKE) using Gemini models (e.g. `gemini-2.5-pro` / `gemini-1.5-flash`).

## How it works

1. **Reasoning**: The agent receives a natural language query ("Forecast stock performance").
2. **Tool Selection**: Gemini stops and requests the `forecast` tool call.
3. **Governed Transport**: The tool invocation traverses the F5 BIG-IP Next for Kubernetes (BNK) Ingress VIP on GKE.
4. **Policy Enforcement**: F5 BNK enforces rate limits (20 req/s), client bearer tokens, and records audit telemetry into Loki / BNK Forge.
5. **Narration**: Gemini uses the verified facts to narrate the response to the user.

## Running Locally / In Test

```bash
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
python3 vertex_agent.py --symbol GOOGL --days 30
```
