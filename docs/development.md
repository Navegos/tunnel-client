# Development & Testing

This document is for contributors working on `tunnel-client`.

## Build

```bash
./scripts/build_admin_ui.sh ./adminui ./pkg/adminui/assets
# or
make admin-ui
go build -o bin/tunnel-client ./cmd/client
```

Use `./bin/tunnel-client` for local source-checkout runs unless `bin/` is on
your `PATH`.

Before creating a release tag, stamp the source version so downloaded release
archives build with the tag semantic version:

```bash
make release-source-version VERSION=1.2.3
make release-tag VERSION=1.2.3 WORD=ember-orchid
```

## Unit tests

```bash
go test ./...
```

## E2E tests (in-repo harness)

The `e2e/` tests use in-repo test doubles under `testsupport/`:

- `testsupport/mocktunnelservice`: simulates the control plane poll/response endpoints.
- `testsupport/mockmcpserver`: a Streamable HTTP MCP server double.

Run:

```bash
go test ./e2e -count=1
```

## MCP tunnel proxy test patterns

There are two supported wrapper patterns for tests that start an MCP server and
need tunnel-client in the path:

- Remote control plane: start your MCP server, then start `tunnel-client run`
  with `CONTROL_PLANE_API_KEY`, `--control-plane.tunnel-id`, and
  `--mcp-server-url` or `--mcp-command`. Use this when a test should exercise a
  hosted control plane.
- Local control plane: start your MCP server, then start
  `tunnel-client dev proxy --mcp-server-url <url> --print-json`. This runs a
  local in-memory control plane plus tunnel-client in one process and prints an
  `mcp_url` that tests can POST JSON-RPC requests to.

`dev proxy` runs the local control plane and tunnel-client in one process. It
prefers a Unix-domain socket for tunnel-client control-plane traffic when the OS
supports it and falls back to TCP otherwise. It starts no health/admin listener
by default; pass `--health-listen-addr 127.0.0.1:0` or
`--health-url-file <path>` only when a test needs `/healthz`, `/readyz`,
`/metrics`, or `/ui`. The `--backend auto|go|rust` flag defaults to `auto`.
Public builds use the Go backend unless an optional Rust backend adapter is
linked into the binary; explicit `--backend rust` fails clearly when unavailable.

Stable touch points:

- Go tests can import `go.openai.org/api/tunnel-client/pkg/localproxy` and call
  `localproxy.Start`.
- Python tests can copy or import
  `wrappers/mcp-tunnel-client-proxy/python/mcp_tunnel_client_proxy.py`.
- TypeScript tests can copy or import
  `wrappers/mcp-tunnel-client-proxy/typescript/mcp_tunnel_client_proxy.ts`.
- Copyable example subprojects live under `examples/`.

Run the wrapper/example suite with:

```bash
bazel test //api/tunnel-client:mcp_tunnel_client_proxy_tests
```

## Repo structure (high level)

- `cmd/client`: CLI entrypoint
- `pkg/*`: implementation packages
- `e2e/`: end-to-end tests using in-repo mocks
- `testsupport/`: test helpers and doubles
