# Tunnel Client

The tunnel client is an enterprise-hosted agent that connects your internal MCP (Model Context Protocol) server to OpenAI-hosted products over a secure, outbound-only HTTPS channel.

## Documentation

- **Start here**: [`docs/onboarding.md`](docs/onboarding.md)
- **Architecture**: [`docs/architecture.md`](docs/architecture.md)
- **Configuration reference**: [`docs/configuration.md`](docs/configuration.md)
- **Deployment guides**: [`docs/deployment/overview.md`](docs/deployment/overview.md)
- **Troubleshooting**: [`docs/troubleshooting.md`](docs/troubleshooting.md)
- **Development & testing**: [`docs/development.md`](docs/development.md)
- **Roadmap / design notes**: [`docs/roadmap.md`](docs/roadmap.md)

## What it does (high level)

- The client **long-polls** the OpenAI tunnel control plane over HTTPS:
  - `GET /v1/tunnel/{tunnel_id}/poll`
  - `POST /v1/tunnel/{tunnel_id}/response`
- It forwards the received JSON-RPC requests to your configured MCP server over HTTP(S).
- It exposes an **admin/health server** (`/healthz`, `/readyz`, `/metrics`) for probes and Prometheus scraping.

## CLI

- `tunnel-client` defaults to the `run` command.
- `tunnel-client run` is equivalent and provided for future subcommands.

## License
This project is licensed under the [Apache License 2.0](LICENSE).
