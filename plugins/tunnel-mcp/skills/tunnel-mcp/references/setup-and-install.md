# Setup and install

Use the binary-owned install path when a `tunnel-client` binary is available:

- `tunnel-client codex plugin install`
- `tunnel-client codex plugin uninstall`
- `tunnel-client codex status`

Use the exported bundle only when the binary is not already installed or when
you need to inspect the plugin contents first:

- `tunnel-client codex plugin export --dir /tmp/tunnel-mcp`
- `cd /tmp/tunnel-mcp && python3 scripts/install_plugin.py --tunnel-client-bin /path/to/tunnel-client`

After install, prefer the installed plugin router and persisted
`.tunnel-client-bin` hint over an ambient `tunnel-client` found on `PATH`.

The plugin is a thin router. It does not own protocol logic. After install,
use the native CLI for runtime operations:

- `tunnel-client runtimes create ...`
- `tunnel-client runtimes connect ...`
- `tunnel-client runtimes list`
- `tunnel-client admin-profiles list`

If Codex already had the plugin loaded, restart the existing Codex session
after install or uninstall so the loaded plugin inventory matches the on-disk
bundle.

If `tunnel-client` itself is missing, consult `binary.md` for public-safe
release and source-build paths before trying to use the plugin.
