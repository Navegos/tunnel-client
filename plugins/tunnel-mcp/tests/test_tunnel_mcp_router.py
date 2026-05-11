from __future__ import annotations

import json
import os
import pathlib
import shutil
import stat
import subprocess
import sys
import tempfile
import unittest

PLUGIN_ROOT = pathlib.Path(__file__).resolve().parents[1]
ENTRYPOINT = PLUGIN_ROOT / "scripts" / "tunnel_mcp"
sys.path.insert(0, str(PLUGIN_ROOT / "scripts"))

from tunnel_mcp_installer.binary import discover_tunnel_client_bin  # noqa: E402


def _write_fake_tunnel_client(path: pathlib.Path) -> None:
    path.write_text(
        "#!/bin/sh\n"
        ': > "$ROUTER_TEST_OUTPUT"\n'
        'for arg in "$@"; do\n'
        '  printf \'%s\\n\' "$arg" >> "$ROUTER_TEST_OUTPUT"\n'
        "done\n",
        encoding="utf-8",
    )
    path.chmod(path.stat().st_mode | stat.S_IXUSR)


def _write_fake_tunnel_client_with_diagnose_help(path: pathlib.Path) -> None:
    path.write_text(
        "#!/bin/sh\n"
        'if [ "$1" = "codex" ] && [ "$2" = "diagnose" ] && [ "$3" = "--help" ]; then\n'
        '  printf "%s\\n" "usage: tunnel-client codex diagnose [--plugin-root path]"\n'
        "  exit 0\n"
        "fi\n"
        ': > "$ROUTER_TEST_OUTPUT"\n'
        'for arg in "$@"; do\n'
        '  printf \'%s\\n\' "$arg" >> "$ROUTER_TEST_OUTPUT"\n'
        "done\n",
        encoding="utf-8",
    )
    path.chmod(path.stat().st_mode | stat.S_IXUSR)


def _write_fake_legacy_tunnel_client(path: pathlib.Path) -> None:
    path.write_text(
        "#!/bin/sh\n"
        'if [ "$1" = "--version" ]; then\n'
        '  printf "%s\\n" "tunnel-client 0.1.0-legacy"\n'
        "  exit 0\n"
        "fi\n"
        'if [ "$1" = "codex" ] && [ "$2" = "diagnose" ] && [ "$3" = "--help" ]; then\n'
        '  printf "%s\\n" "usage: tunnel-client codex diagnose [alias]"\n'
        "  exit 0\n"
        "fi\n"
        'if [ "$1" = "codex" ] && [ "$2" = "diagnose" ]; then\n'
        '  for arg in "$@"; do\n'
        '    if [ "$arg" = "--plugin-root" ]; then\n'
        '      printf "%s\\n" "unknown flag: --plugin-root" >&2\n'
        "      exit 2\n"
        "    fi\n"
        "  done\n"
        '  printf "%s\\n" \'{"diagnose":"legacy"}\'\n'
        "  exit 0\n"
        "fi\n"
        'if [ "$1" = "codex" ] && [ "$2" = "status" ]; then\n'
        '  printf "%s\\n" \'{"app_server_supported":true,"assistant_state":"ready"}\'\n'
        "  exit 0\n"
        "fi\n"
        'if [ "$1" = "admin-profiles" ] && [ "$2" = "list" ]; then\n'
        '  printf "%s\\n" \'{"active_profile":"default","path":"/tmp/admin-profiles.json","profiles":[]}\'\n'
        "  exit 0\n"
        "fi\n"
        'printf "%s\\n" "unexpected args: $*" >&2\n'
        "exit 1\n",
        encoding="utf-8",
    )
    path.chmod(path.stat().st_mode | stat.S_IXUSR)


def _write_sleeping_tunnel_client(path: pathlib.Path) -> None:
    path.write_text(
        "#!/bin/sh\nsleep 30\n",
        encoding="utf-8",
    )
    path.chmod(path.stat().st_mode | stat.S_IXUSR)


def _make_isolated_plugin_root(tmp_path: pathlib.Path) -> pathlib.Path:
    plugin_root = tmp_path / "plugin"
    scripts_dir = plugin_root / "scripts"
    scripts_dir.mkdir(parents=True)
    shutil.copy2(ENTRYPOINT, scripts_dir / "tunnel_mcp")
    (scripts_dir / "tunnel_mcp").chmod((scripts_dir / "tunnel_mcp").stat().st_mode | stat.S_IXUSR)
    return plugin_root


def _run_router(
    entrypoint: pathlib.Path, args: list[str], env: dict[str, str]
) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [str(entrypoint), *args],
        check=False,
        capture_output=True,
        text=True,
        env=env,
    )


class TunnelMCPRouterTest(unittest.TestCase):
    def assert_output_args(self, output_path: pathlib.Path, expected: list[str]) -> None:
        self.assertEqual(
            output_path.read_text(encoding="utf-8").splitlines(),
            expected,
        )

    def test_router_forwards_runtimes_command_and_adds_json(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = pathlib.Path(tmp)
            plugin_root = _make_isolated_plugin_root(tmp_path)
            output_path = tmp_path / "args.json"
            fake_bin = tmp_path / "tunnel-client"
            _write_fake_tunnel_client(fake_bin)

            result = _run_router(
                plugin_root / "scripts" / "tunnel_mcp",
                [
                    "--tunnel-client-bin",
                    str(fake_bin),
                    "connect",
                    "--alias",
                    "docs-mcp",
                    "--mcp-command",
                    "python server.py",
                ],
                {**os.environ, "ROUTER_TEST_OUTPUT": str(output_path)},
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assert_output_args(
                output_path,
                [
                    "runtimes",
                    "connect",
                    "--alias",
                    "docs-mcp",
                    "--mcp-command",
                    "python server.py",
                    "--json",
                ],
            )

    def test_router_forwards_admin_profiles_command_and_adds_json(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = pathlib.Path(tmp)
            plugin_root = _make_isolated_plugin_root(tmp_path)
            output_path = tmp_path / "args.json"
            fake_bin = tmp_path / "tunnel-client"
            _write_fake_tunnel_client(fake_bin)

            result = _run_router(
                plugin_root / "scripts" / "tunnel_mcp",
                [
                    "--tunnel-client-bin",
                    str(fake_bin),
                    "admin-profiles",
                    "list",
                ],
                {**os.environ, "ROUTER_TEST_OUTPUT": str(output_path)},
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assert_output_args(output_path, ["admin-profiles", "list", "--json"])

    def test_router_forwards_rm_alias_and_adds_json(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = pathlib.Path(tmp)
            plugin_root = _make_isolated_plugin_root(tmp_path)
            output_path = tmp_path / "args.json"
            fake_bin = tmp_path / "tunnel-client"
            _write_fake_tunnel_client(fake_bin)

            result = _run_router(
                plugin_root / "scripts" / "tunnel_mcp",
                [
                    "--tunnel-client-bin",
                    str(fake_bin),
                    "rm",
                    "docs-mcp",
                ],
                {**os.environ, "ROUTER_TEST_OUTPUT": str(output_path)},
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assert_output_args(output_path, ["runtimes", "rm", "docs-mcp", "--json"])

    def test_router_help_lists_supported_routes(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = pathlib.Path(tmp)
            plugin_root = _make_isolated_plugin_root(tmp_path)
            result = _run_router(
                plugin_root / "scripts" / "tunnel_mcp",
                ["--help"],
                dict(os.environ),
            )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("admin-profiles", result.stdout)
        self.assertIn("rm", result.stdout)
        self.assertIn("diagnose", result.stdout)
        self.assertIn("cleanup", result.stdout)

    def test_router_uses_tunnel_client_bin_env_var(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = pathlib.Path(tmp)
            plugin_root = _make_isolated_plugin_root(tmp_path)
            output_path = tmp_path / "args.json"
            fake_bin = tmp_path / "tunnel-client"
            _write_fake_tunnel_client(fake_bin)

            result = _run_router(
                plugin_root / "scripts" / "tunnel_mcp",
                [
                    "status",
                    "docs-mcp",
                ],
                {
                    **os.environ,
                    "ROUTER_TEST_OUTPUT": str(output_path),
                    "TUNNEL_CLIENT_BIN": str(fake_bin),
                },
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assert_output_args(output_path, ["runtimes", "status", "docs-mcp", "--json"])

    def test_plugin_only_missing_binary_guidance_includes_public_setup_steps(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = pathlib.Path(tmp)
            plugin_root = _make_isolated_plugin_root(tmp_path)
            result = _run_router(
                plugin_root / "scripts" / "tunnel_mcp",
                ["status", "docs-mcp"],
                {"PATH": ""},
            )

            self.assertEqual(result.returncode, 2)
            self.assertIn("tunnel-client was not found", result.stderr)
            self.assertIn("Discovery methods tried:", result.stderr)
            self.assertIn("--tunnel-client-bin: not provided", result.stderr)
            self.assertIn("TUNNEL_CLIENT_BIN: not set", result.stderr)
            self.assertIn("installed .tunnel-client-bin hint: not present", result.stderr)
            self.assertIn("adjacent build outputs:", result.stderr)
            self.assertIn("PATH: no tunnel-client executable found", result.stderr)
            self.assertIn("https://github.com/openai/tunnel-client", result.stderr)
            self.assertIn("https://github.com/openai/tunnel-client/releases/latest", result.stderr)
            self.assertIn("git clone https://github.com/openai/tunnel-client.git", result.stderr)
            self.assertIn("go build -o bin/tunnel-client ./cmd/client", result.stderr)
            self.assertIn("go build -o bin/tunnel-client.exe ./cmd/client", result.stderr)
            self.assertIn("tunnel-client.exe", result.stderr)
            self.assertIn("does not auto-download, auto-clone, or auto-run", result.stderr)

    def test_missing_hint_does_not_execute_ambient_path_shim(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = pathlib.Path(tmp)
            plugin_root = _make_isolated_plugin_root(tmp_path)
            bin_dir = tmp_path / "pathdir"
            bin_dir.mkdir()
            shim = bin_dir / "tunnel-client"
            _write_sleeping_tunnel_client(shim)

            result = _run_router(
                plugin_root / "scripts" / "tunnel_mcp",
                ["list"],
                {"PATH": str(bin_dir)},
            )

            self.assertEqual(result.returncode, 2)
            self.assertIn("PATH: found", result.stderr)
            self.assertIn("ignored because .tunnel-client-bin is missing", result.stderr)

    def test_missing_hint_ignores_ancestor_bin_without_repo_marker(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = pathlib.Path(tmp)
            plugin_root = tmp_path / "root" / "nested" / "plugin"
            scripts_dir = plugin_root / "scripts"
            scripts_dir.mkdir(parents=True)
            shutil.copy2(ENTRYPOINT, scripts_dir / "tunnel_mcp")
            (scripts_dir / "tunnel_mcp").chmod(
                (scripts_dir / "tunnel_mcp").stat().st_mode | stat.S_IXUSR
            )

            ancestor_bin_dir = tmp_path / "root" / "bin"
            ancestor_bin_dir.mkdir(parents=True)
            ancestor_bin = ancestor_bin_dir / "tunnel-client"
            _write_sleeping_tunnel_client(ancestor_bin)

            result = _run_router(
                plugin_root / "scripts" / "tunnel_mcp",
                ["list"],
                {"PATH": ""},
            )

            self.assertEqual(result.returncode, 2)
            self.assertIn("adjacent build outputs:", result.stderr)
            self.assertIn("PATH: no tunnel-client executable found", result.stderr)

    def test_missing_hint_accepts_repo_root_adjacent_binary(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = pathlib.Path(tmp)
            repo_root = tmp_path / "repo"
            plugin_root = repo_root / "plugins" / "tunnel-mcp"
            scripts_dir = plugin_root / "scripts"
            scripts_dir.mkdir(parents=True)
            shutil.copy2(ENTRYPOINT, scripts_dir / "tunnel_mcp")
            (scripts_dir / "tunnel_mcp").chmod(
                (scripts_dir / "tunnel_mcp").stat().st_mode | stat.S_IXUSR
            )
            (repo_root / "api" / "tunnel-client" / "cmd" / "client").mkdir(parents=True)

            output_path = tmp_path / "args.json"
            fake_bin = repo_root / "bin" / "tunnel-client"
            fake_bin.parent.mkdir(parents=True)
            _write_fake_tunnel_client(fake_bin)

            result = _run_router(
                plugin_root / "scripts" / "tunnel_mcp",
                ["status", "docs-mcp"],
                {**os.environ, "ROUTER_TEST_OUTPUT": str(output_path), "PATH": ""},
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assert_output_args(output_path, ["runtimes", "status", "docs-mcp", "--json"])

    def test_missing_hint_finds_nested_bazel_client_binary(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = pathlib.Path(tmp)
            repo_root = tmp_path / "repo"
            plugin_root = repo_root / "plugins" / "tunnel-mcp"
            scripts_dir = plugin_root / "scripts"
            scripts_dir.mkdir(parents=True)
            shutil.copy2(ENTRYPOINT, scripts_dir / "tunnel_mcp")
            (scripts_dir / "tunnel_mcp").chmod(
                (scripts_dir / "tunnel_mcp").stat().st_mode | stat.S_IXUSR
            )
            (repo_root / "api" / "tunnel-client" / "cmd" / "client").mkdir(parents=True)

            output_path = tmp_path / "args.json"
            fake_bin = (
                repo_root
                / "bazel-bin"
                / "api"
                / "tunnel-client"
                / "cmd"
                / "client"
                / "client_"
                / "client"
            )
            fake_bin.parent.mkdir(parents=True)
            _write_fake_tunnel_client(fake_bin)

            result = _run_router(
                plugin_root / "scripts" / "tunnel_mcp",
                ["status", "docs-mcp"],
                {**os.environ, "ROUTER_TEST_OUTPUT": str(output_path), "PATH": ""},
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assert_output_args(output_path, ["runtimes", "status", "docs-mcp", "--json"])

    def test_python_installer_finds_nested_bazel_client_binary(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = pathlib.Path(tmp)
            repo_root = tmp_path / "repo"
            plugin_root = repo_root / "plugins" / "tunnel-mcp"
            plugin_root.mkdir(parents=True)
            (repo_root / "api" / "tunnel-client" / "cmd" / "client").mkdir(parents=True)
            fake_bin = (
                repo_root
                / "bazel-bin"
                / "api"
                / "tunnel-client"
                / "cmd"
                / "client"
                / "client_"
                / "client"
            )
            fake_bin.parent.mkdir(parents=True)
            _write_fake_tunnel_client(fake_bin)

            discovered = discover_tunnel_client_bin(plugin_root, None)

            self.assertEqual(discovered, fake_bin.resolve())

    def test_router_forwards_diagnose_to_codex_command_with_plugin_root(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = pathlib.Path(tmp)
            plugin_root = _make_isolated_plugin_root(tmp_path)
            output_path = tmp_path / "args.json"
            fake_bin = tmp_path / "tunnel-client"
            _write_fake_tunnel_client_with_diagnose_help(fake_bin)

            result = _run_router(
                plugin_root / "scripts" / "tunnel_mcp",
                [
                    "--tunnel-client-bin",
                    str(fake_bin),
                    "diagnose",
                    "docs-mcp",
                ],
                {**os.environ, "ROUTER_TEST_OUTPUT": str(output_path)},
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assert_output_args(
                output_path,
                [
                    "codex",
                    "diagnose",
                    "--plugin-root",
                    str(plugin_root),
                    "docs-mcp",
                    "--json",
                ],
            )

    def test_router_diagnose_falls_back_when_plugin_root_flag_is_unsupported(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = pathlib.Path(tmp)
            plugin_root = _make_isolated_plugin_root(tmp_path)
            fake_bin = tmp_path / "tunnel-client"
            _write_fake_legacy_tunnel_client(fake_bin)

            result = _run_router(
                plugin_root / "scripts" / "tunnel_mcp",
                [
                    "--tunnel-client-bin",
                    str(fake_bin),
                    "diagnose",
                    "docs-mcp",
                ],
                dict(os.environ),
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            payload = json.loads(result.stdout)
            self.assertEqual(
                payload["app_router_compatibility"]["unsupported_flag"], "--plugin-root"
            )
            self.assertEqual(
                payload["app_router_compatibility"]["fallback_used"],
                "codex_diagnose_without_plugin_root",
            )
            self.assertEqual(payload["fallback"]["json"], {"diagnose": "legacy"})

    def test_router_self_check_reports_compatibility_and_secret_reference_presence(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = pathlib.Path(tmp)
            plugin_root = _make_isolated_plugin_root(tmp_path)
            manifest_dir = plugin_root / ".codex-plugin"
            manifest_dir.mkdir()
            (manifest_dir / "plugin.json").write_text('{"version":"9.9.9"}', encoding="utf-8")
            fake_bin = tmp_path / "tunnel-client"
            _write_fake_legacy_tunnel_client(fake_bin)

            result = _run_router(
                plugin_root / "scripts" / "tunnel_mcp",
                [
                    "--tunnel-client-bin",
                    str(fake_bin),
                    "self-check",
                ],
                {**os.environ, "CONTROL_PLANE_API_KEY": "redacted-test-value"},
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            payload = json.loads(result.stdout)
            self.assertEqual(payload["selected_binary_path"], str(fake_bin))
            self.assertEqual(payload["plugin_version"], "9.9.9")
            self.assertIn("tunnel-client 0.1.0-legacy", payload["binary_version"])
            self.assertEqual(payload["active_admin_profile"]["name"], "default")
            self.assertEqual(
                payload["app_router_compatibility"]["unsupported_flag"], "--plugin-root"
            )
            self.assertTrue(payload["runtime_key_reference_present"])
            self.assertNotIn("redacted-test-value", result.stdout)

    def test_adjacent_windows_style_exe_name_is_accepted(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = pathlib.Path(tmp)
            plugin_root = _make_isolated_plugin_root(tmp_path)
            output_path = tmp_path / "args.json"
            bin_dir = plugin_root / "bin"
            bin_dir.mkdir(parents=True)
            fake_bin = bin_dir / "tunnel-client.exe"
            _write_fake_tunnel_client(fake_bin)

            result = _run_router(
                plugin_root / "scripts" / "tunnel_mcp",
                ["status", "docs-mcp"],
                {**os.environ, "ROUTER_TEST_OUTPUT": str(output_path), "PATH": ""},
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assert_output_args(output_path, ["runtimes", "status", "docs-mcp", "--json"])


if __name__ == "__main__":
    unittest.main()
