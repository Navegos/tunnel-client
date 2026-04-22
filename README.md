# Tunnel Client

The tunnel client is an enterprise-hosted agent that connects a private MCP
(Model Context Protocol) server to OpenAI-hosted products over a secure,
outbound-only HTTPS channel. It lets customers keep MCP servers inside their own
network while OpenAI products use an OpenAI-hosted MCP tunnel URL.

## Documentation

- **Start here**: [`docs/onboarding.md`](docs/onboarding.md)
- **Architecture diagrams**: [`docs/architecture.md`](docs/architecture.md)
- **Enterprise customer handoff**:
  [`docs/enterprise-customer-onboarding.md`](docs/enterprise-customer-onboarding.md)
- **Configuration reference**: [`docs/configuration.md`](docs/configuration.md)
- **Deployment guides**: [`docs/deployment/overview.md`](docs/deployment/overview.md)
- **Troubleshooting**: [`docs/troubleshooting.md`](docs/troubleshooting.md)
- **Development & testing**: [`docs/development.md`](docs/development.md)
- **Roadmap / design notes**: [`docs/roadmap.md`](docs/roadmap.md)

## For Codex / Claude / Copilot

Binary-first flow:

```bash
tunnel-client help quickstart
tunnel-client profiles samples list
tunnel-client init --sample sample_mcp_stdio_local --profile local-stdio --tunnel-id tunnel_0123456789abcdef0123456789abcdef --mcp-command "python /path/to/server.py"
tunnel-client doctor --profile local-stdio --explain
tunnel-client run --profile local-stdio
```

Plugin flow for Codex:

```bash
tunnel-client plugin codex install
tunnel-client help plugin
tunnel-client plugin codex uninstall
```

Choose the raw binary when you want the smallest possible setup surface. Choose
the plugin when you want Codex-native alias management, local process
supervision, and reusable tunnel state.

Starter prompts for Codex:

- `Figure out what tunnel-client is for from the binary help, then get me to /ui with the shortest local path.`
- `I only have the source checkout. Figure out how to build tunnel-client, then get me to /ui with the shortest local path.`
- `Use tunnel-client to create or reuse a profile, run doctor --explain, and then start the daemon.`
- `Install the Codex plugin from the tunnel-client binary, connect the provided tunnel id, and tell me whether the runtime is launched, healthy, or ready.`

## What it does

- The client **long-polls** the OpenAI tunnel control plane over HTTPS:
  - `GET /v1/tunnel/{tunnel_id}/poll`
  - `POST /v1/tunnel/{tunnel_id}/response`
- On startup, it fetches tunnel metadata for operator visibility:
  - `GET /v1/tunnels/{tunnel_id}`
- It forwards received JSON-RPC requests to your configured MCP server over
  HTTP(S), stdio, or in-memory transport.
- It routes commands by channel: `main` always targets the configured MCP
  server, while `harpoon` is enabled only when Harpoon has registered targets.
- On startup, it fetches OAuth Protected Resource Metadata from the MCP server
  for diagnostics.
- For OAuth auth-server handling, `authorization_servers[0]` from PRMD is the
  only source of truth and metadata fetch target.
- Metadata is accepted even when `issuer` differs from
  `authorization_servers[0]` (external IdP issuer URLs are supported), with
  mismatch diagnostics preserved in logs/state.
- It exposes an **admin/health server** (`/healthz`, `/readyz`, `/metrics`) and
  a lightweight **admin UI** (`/ui`) for operational status.
- The admin UI Overview reports channel availability and reasons when channels
  are disabled.
- The admin UI Logs tab can switch the live runtime log level between `debug`,
  `info`, and `warn` without restarting the process.
- The admin UI log export returns a redacted support bundle with recent logs
  plus a point-in-time Prometheus snapshot from `/metrics` and a redacted
  runtime YAML snapshot containing argv, relevant environment, actual YAML
  config, and effective config.
- It embeds the **Harpoon MCP server** to provide a labeled, allowlisted
  outbound HTTP client for internal tooling.

## Admin UI build notes

The admin UI assets under `pkg/adminui/assets` are generated from the TypeScript/Svelte
source in `adminui/`. To rebuild them locally:

```bash
./scripts/build_admin_ui.sh ./adminui ./pkg/adminui/assets
# or
make admin-ui
```

## CLI

- `tunnel-client` shows help and available subcommands.
- `tunnel-client help <topic>` shows embedded task-oriented help for
  `quickstart`, `samples`, `doctor`, `oauth`, and `plugin`.
- `tunnel-client dev mcp-stub` runs an embedded demo MCP + OAuth metadata server
  for one-binary end-to-end validation.
- `tunnel-client init` writes a validated first-use profile.
- `tunnel-client doctor` validates config and explains what is missing before
  startup.
- `tunnel-client profiles samples list|show` exposes built-in sample profiles.
- `tunnel-client plugin codex install|uninstall|export` installs, removes, or exports the embedded
  Tunnel MCP plugin bundle.
- `tunnel-client run` starts the client poller.
- `tunnel-client admin tunnels get <id>` is the read-only metadata lookup used
  on the runtime-user path; broader `admin tunnels` CRUD still requires an
  admin key.

## License
This project is licensed under the [Apache License 2.0](LICENSE).
