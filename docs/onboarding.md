# Onboarding Guide

This guide helps you get from “zero” to a working `tunnel-client` process connected to your MCP server.

## 1) Prerequisites

- A reachable MCP server endpoint (Streamable HTTP MCP).
- A tunnel control-plane API key.
- A provisioned `tunnel_id` for your environment.

## 2) Build

From the repository root:

```bash
cd api/tunnel-client
go build -o bin/tunnel-client ./cmd/client
```

## 3) Configure

At minimum, you must set:

- `CONTROL_PLANE_API_KEY`: control-plane authentication.
- `CONTROL_PLANE_TUNNEL_ID`: the tunnel identifier for this deployment.
- `MCP_SERVER_URL`: your MCP server endpoint.

Example:

```bash
export CONTROL_PLANE_API_KEY="sk-..."        # preferred
export CONTROL_PLANE_TUNNEL_ID="tunnel_<abc>"
export MCP_SERVER_URL="https://mcp.internal.example.com/mcp"
```

For the full surface (flags, defaults, advanced knobs), see [`configuration.md`](configuration.md).

### OAuth-protected MCP (supported)

- `Authorization` headers are forwarded through tunnel-service to your MCP server.
- Custom MCP request headers configured on the app are forwarded through tunnel-service, except
  internal auth and IP-forwarding transport headers.
- OAuth discovery GETs are forwarded to the tunnel-client; discovery payloads and `WWW-Authenticate resource_metadata` are rewritten to tunnel-service URLs for the same `tunnel_id`.
- `authorization_servers[0]` from PRMD is the only source of truth and metadata fetch target for auth-server metadata enrichment and Harpoon OAuth target registration.
- Auth-server metadata is accepted even when metadata `issuer` differs from `authorization_servers[0]` (external IdP issuer topologies are supported), and mismatch diagnostics are retained.
- The authorization server itself is not tunneled—if it is only reachable on-prem/behind a firewall and not accessible from the internet or the tunnel-client host, the OAuth flow can fail.

## 4) Run

```bash
./bin/tunnel-client run --log.level=info --log.format=struct-text
```

The process will:

- Start polling the control plane for work.
- Forward JSON-RPC requests to your MCP server.
- Expose health endpoints on `HEALTH_LISTEN_ADDR` (default `:8080`).

## 5) Verify

In another shell:

```bash
curl -fsS "http://127.0.0.1:8080/healthz"
curl -fsS "http://127.0.0.1:8080/readyz"
curl -fsS "http://127.0.0.1:8080/metrics" | head
```

## 6) Next reads

- **Deployments**: [`deployment/overview.md`](deployment/overview.md)
- **Architecture**: [`architecture.md`](architecture.md)
- **Troubleshooting**: [`troubleshooting.md`](troubleshooting.md)
