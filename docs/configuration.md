# Configuration Reference

`tunnel-client` can be configured via CLI flags or environment variables.

- **Precedence**: flags > environment variables > defaults.
- **Requirement**: you must provide a control-plane API key, a tunnel ID, and an MCP server URL.

## Commands

- `run` (default): start the tunnel client poll loop.
- `tunnel-client` and `tunnel-client run` are equivalent.

## Control plane

- **Base URL**
  - Flag: `--control-plane.base-url`
  - Env: `CONTROL_PLANE_BASE_URL`
  - Default: `https://api.openai.com`
  - **Important**: this value is treated as the **host root**, not a pre-prefixed path.
    - Correct: `https://api.openai.com`
    - Incorrect: `https://api.openai.com/v1/tunnel` (would create `/v1/tunnel/v1/tunnel/...`)
- **Tunnel ID**
  - Flag: `--control-plane.tunnel-id`
  - Env: `CONTROL_PLANE_TUNNEL_ID`
  - Required: yes
  - Allowed characters: `A–Z a–z 0–9 _ -`
- **API key**
  - Flag: `--control-plane.api-key=env:VARNAME` or `--control-plane.api-key=file:/path/to/secret`
  - Env (preferred): `CONTROL_PLANE_API_KEY`
  - Env (fallback): `OPENAI_API_KEY` (used only if `CONTROL_PLANE_API_KEY` is unset)
  - Required: yes
- **Poll timeout**
  - Flag: `--control-plane.poll-timeout`
  - Env: `CONTROL_PLANE_POLL_TIMEOUT`
  - Default: `30s`
- **Max in-flight buffer**
  - Flag: `--control-plane.max-inflight`
  - Env: `CONTROL_PLANE_MAX_INFLIGHT_REQUESTS`
  - Default: `20` (max `10000`)
- **Extra headers (optional)**
  - Flag (repeatable): `--control-plane.extra-headers "Key: Value"`
  - Env: `CONTROL_PLANE_EXTRA_HEADERS="Key: Value, Key2: Value2"`

## MCP server

- **Server URL**
  - Flag: `--mcp.server-url`
  - Env: `MCP_SERVER_URL`
  - Required: yes
- **Connection max TTL**
  - Flag: `--mcp.connection-max-ttl`
  - Env: `MCP_CONNECTION_MAX_TTL`
  - Default: `10m`
- **Max concurrent requests**
  - Flag: `--mcp.max-concurrent-requests`
  - Env: `MCP_MAX_CONCURRENT_REQUESTS`
  - Default: `10`

**OAuth-protected MCP notes:**
- Forwards inbound `Authorization` headers and discovery GETs through the tunnel-client; discovery payload `resource` and `WWW-Authenticate resource_metadata` are rewritten to tunnel-service URLs for the same `tunnel_id`.
- The authorization server is not tunneled. If it is only reachable on-prem/behind a firewall and not accessible from the internet or the tunnel-client host, the OAuth flow can fail.

## Logging

- **Level**
  - Flag: `--log.level` (`debug`, `info`, `warn`)
  - Env: `LOG_LEVEL`
  - Default: `info`
- **Format**
  - Flag: `--log.format` (`struct-text`, `json`)
  - Env: `LOG_FORMAT`
  - Default: unset (uses Go’s default logger behavior)
- **File (optional)**
  - Flag: `--log.file`
  - Env: `LOG_FILE`
  - Default: stdout (when unset)
- **Raw HTTP logging (dangerous)**
  - Flag: `--log.http-raw-unsafe`
  - Env: `LOG_HTTP_RAW_UNSAFE`
  - Default: `false`
  - Warning: may log sensitive headers/bodies; enable only for controlled debugging.

## Health/admin server

- **Listen address**
  - Flag: `--health.listen-addr`
  - Env: `HEALTH_LISTEN_ADDR`
  - Default: `:8080`
- **URL file (optional)**
  - Flag: `--health.url-file`
  - Env: `HEALTH_URL_FILE`
  - Use when binding to a random port (e.g., `:0`) and you need to publish the resolved base URL.

## Process utilities

- **PID file (optional)**
  - Flag: `--pid.file`
  - Env: `PID_FILE`

## Example configurations

### Minimal env-var run

```bash
export CONTROL_PLANE_API_KEY="sk-..."
export CONTROL_PLANE_TUNNEL_ID="tunnel_<abc>"
export MCP_SERVER_URL="https://mcp.internal.example.com/mcp"

./bin/tunnel-client --log.level=info --log.format=struct-text
```

### API key via file

```bash
./bin/tunnel-client \
  --control-plane.tunnel-id=tunnel_<abc> \
  --control-plane.api-key=file:/run/secrets/control-plane-api-key \
  --mcp.server-url=https://mcp.internal.example.com/mcp \
  --log.level=info \
  --log.format=json
```
