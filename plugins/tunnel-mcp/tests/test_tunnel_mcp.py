from __future__ import annotations

import contextlib
import http.server
import io
import json
import os
import pathlib
import shlex
import shutil
import subprocess
import sys
import tempfile
import threading
import unittest

PLUGIN_ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPTS_DIR = PLUGIN_ROOT / "scripts"
INSTALLER = SCRIPTS_DIR / "install_plugin.py"
sys.path.insert(0, str(SCRIPTS_DIR))

from tunnel_mcp_cli import commands, runtime, state  # noqa: E402


class FakeRunner:
    def __init__(self, *, tmux_installed: bool = True, health_base_url: str | None = None) -> None:
        self.calls: list[list[str]] = []
        self.envs: list[dict[str, str]] = []
        self.admin_envs: list[dict[str, str]] = []
        self.remote: dict[str, dict[str, object]] = {}
        self.sessions: set[str] = set()
        self.next_id = 1
        self.tmux_installed = tmux_installed
        self.admin_error = ""
        self.admin_returncode = 1
        self.health_base_url = (
            os.environ.get("TUNNEL_MCP_TEST_HEALTH_BASE_URL", "")
            if health_base_url is None
            else health_base_url
        )

    def __call__(self, args: list[str], **kwargs: object) -> subprocess.CompletedProcess[str]:
        self.calls.append(list(args))
        env = kwargs.get("env")
        if isinstance(env, dict):
            env_copy = dict(env)
        else:
            env_copy = {}
        self.envs.append(env_copy)
        if args == ["tmux", "-V"]:
            if not self.tmux_installed:
                raise FileNotFoundError("tmux")
            return _completed(args, 0, stdout="tmux 3.5\n")
        if args[:2] == ["tmux", "has-session"]:
            if not self.tmux_installed:
                raise FileNotFoundError("tmux")
            target = args[-1].removeprefix("=")
            return _completed(args, 0 if target in self.sessions else 1)
        if args[:3] == ["tmux", "new-session", "-d"]:
            if not self.tmux_installed:
                raise FileNotFoundError("tmux")
            session = _tmux_session_name_arg(args)
            self.sessions.add(session)
            if self.health_base_url:
                _write_health_url_for_alias(_alias_from_session_name(session), self.health_base_url)
            return _completed(args, 0)
        if args[:3] == ["tmux", "kill-session", "-t"]:
            if not self.tmux_installed:
                raise FileNotFoundError("tmux")
            target = args[-1].removeprefix("=")
            if target not in self.sessions:
                return _completed(args, 1, stderr="can't find session")
            self.sessions.remove(target)
            return _completed(args, 0)
        if len(args) >= 7 and args[1] == "admin":
            self.admin_envs.append(env_copy)
            return self._admin(args)
        return _completed(args, 99, stderr="unexpected command")

    def _admin(self, args: list[str]) -> subprocess.CompletedProcess[str]:
        if self.admin_error:
            return _completed(args, self.admin_returncode, stderr=self.admin_error)
        idx = args.index("tunnels")
        subcommand = args[idx + 1]
        if subcommand == "get":
            tunnel_id = args[idx + 2]
            tunnel = self.remote.get(tunnel_id)
            if tunnel is None:
                return _completed(
                    args, 1, stderr="request GET /v1/tunnels/%s failed: 404 not found" % tunnel_id
                )
            return _completed(args, 0, stdout=json.dumps(tunnel))
        if subcommand == "list":
            return _completed(args, 0, stdout=json.dumps({"tunnels": list(self.remote.values())}))
        if subcommand == "create":
            tunnel_id = f"tunnel_{self.next_id:032x}"
            self.next_id += 1
            tunnel = {
                "id": tunnel_id,
                "name": _value_after(args, "--name"),
                "description": _value_after(args, "--description"),
                "organization_ids": _values_after(args, "--organization-id"),
                "workspace_ids": _values_after(args, "--workspace-id"),
                "tenant_ids": [],
            }
            self.remote[tunnel_id] = tunnel
            return _completed(args, 0, stdout=json.dumps(tunnel))
        return _completed(args, 99, stderr="unexpected admin command")


class FakePopenFactory:
    def __init__(
        self, *, poll_results: list[object] | None = None, health_base_url: str | None = None
    ) -> None:
        self.calls: list[list[str]] = []
        self.envs: list[dict[str, str]] = []
        self.poll_results = list(poll_results or [None])
        self.health_base_url = (
            os.environ.get("TUNNEL_MCP_TEST_HEALTH_BASE_URL", "")
            if health_base_url is None
            else health_base_url
        )

    def __call__(self, args: list[str], **kwargs: object) -> object:
        self.calls.append(list(args))
        env = kwargs.get("env")
        if isinstance(env, dict):
            self.envs.append(dict(env))
        else:
            self.envs.append({})
        poll_results = list(self.poll_results)
        if self.health_base_url:
            profile_name = _value_after(args, "--profile")
            _write_health_url_for_alias(profile_name, self.health_base_url)

        class FakeProcess:
            pid = 43210

            def __init__(self) -> None:
                self._poll_results = list(poll_results)
                self._last = None

            def poll(self) -> object:
                if self._poll_results:
                    self._last = self._poll_results.pop(0)
                    return self._last
                return self._last

        return FakeProcess()


class TunnelMCPTest(unittest.TestCase):
    def setUp(self) -> None:
        self._env = os.environ.copy()
        self.temp = tempfile.TemporaryDirectory()
        os.environ["CODEX_HOME"] = self.temp.name
        os.environ["XDG_CONFIG_HOME"] = str(pathlib.Path(self.temp.name) / "xdg")
        os.environ["OPENAI_ADMIN_KEY"] = "admin-key"
        os.environ["CONTROL_PLANE_API_KEY"] = "runtime-key"
        self._health_server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), _HealthHandler)
        self._health_thread = threading.Thread(
            target=self._health_server.serve_forever,
            daemon=True,
        )
        self._health_thread.start()
        self.health_base_url = f"http://127.0.0.1:{self._health_server.server_address[1]}"
        os.environ["TUNNEL_MCP_TEST_HEALTH_BASE_URL"] = self.health_base_url

    def tearDown(self) -> None:
        self._health_server.shutdown()
        self._health_server.server_close()
        self._health_thread.join(timeout=2)
        os.environ.clear()
        os.environ.update(self._env)
        self.temp.cleanup()

    def test_plugin_entrypoint_help_executes(self) -> None:
        result = subprocess.run(
            [sys.executable, str(SCRIPTS_DIR / "tunnel_mcp"), "--help"],
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("create", result.stdout)
        self.assertIn("connect", result.stdout)

    def test_plugin_entrypoint_runs_from_standalone_copy(self) -> None:
        standalone_root = pathlib.Path(self.temp.name) / "standalone-plugin"
        shutil.copytree(
            PLUGIN_ROOT,
            standalone_root,
            ignore=shutil.ignore_patterns("__pycache__", "*.pyc"),
        )

        result = subprocess.run(
            [sys.executable, str(standalone_root / "scripts" / "tunnel_mcp"), "--help"],
            check=False,
            capture_output=True,
            text=True,
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("create", result.stdout)
        self.assertIn("connect", result.stdout)

    def test_local_installer_installs_plugin_without_external_repo_script(self) -> None:
        codex_home = pathlib.Path(self.temp.name) / "codex-home"

        result = subprocess.run(
            [sys.executable, str(INSTALLER), "--codex-home", str(codex_home)],
            check=False,
            capture_output=True,
            text=True,
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Installed tunnel-mcp@debug", result.stdout)
        manifest_path = (
            codex_home
            / "plugins"
            / "cache"
            / "debug"
            / "tunnel-mcp"
            / "local"
            / ".codex-plugin"
            / "plugin.json"
        )
        self.assertTrue(manifest_path.is_file())
        config_text = (codex_home / "config.toml").read_text(encoding="utf-8")
        self.assertIn('[plugins."tunnel-mcp@debug"]', config_text)

    def test_local_installer_runs_from_standalone_copy_without_source_arg(self) -> None:
        standalone_root = pathlib.Path(self.temp.name) / "standalone-plugin"
        shutil.copytree(
            PLUGIN_ROOT,
            standalone_root,
            ignore=shutil.ignore_patterns("__pycache__", "*.pyc"),
        )
        codex_home = pathlib.Path(self.temp.name) / "codex-home"

        result = subprocess.run(
            [
                sys.executable,
                str(standalone_root / "scripts" / "install_plugin.py"),
                "--codex-home",
                str(codex_home),
            ],
            check=False,
            capture_output=True,
            text=True,
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        installed_entrypoint = (
            codex_home
            / "plugins"
            / "cache"
            / "debug"
            / "tunnel-mcp"
            / "local"
            / "scripts"
            / "tunnel_mcp"
        )
        self.assertTrue(installed_entrypoint.is_file())

    def test_local_installer_rejects_unsafe_plugin_manifest_name(self) -> None:
        standalone_root = pathlib.Path(self.temp.name) / "standalone-plugin"
        shutil.copytree(
            PLUGIN_ROOT,
            standalone_root,
            ignore=shutil.ignore_patterns("__pycache__", "*.pyc"),
        )
        manifest_path = standalone_root / ".codex-plugin" / "plugin.json"
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        manifest["name"] = "../escape"
        manifest_path.write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
        codex_home = pathlib.Path(self.temp.name) / "codex-home"

        result = subprocess.run(
            [
                sys.executable,
                str(standalone_root / "scripts" / "install_plugin.py"),
                "--codex-home",
                str(codex_home),
            ],
            check=False,
            capture_output=True,
            text=True,
        )

        self.assertEqual(result.returncode, 1)
        self.assertIn("plugin manifest name", result.stderr)
        self.assertFalse((codex_home / "plugins" / "cache" / "debug" / "escape").exists())

    def test_local_installer_rejects_unsafe_marketplace_name(self) -> None:
        codex_home = pathlib.Path(self.temp.name) / "codex-home"

        result = subprocess.run(
            [
                sys.executable,
                str(INSTALLER),
                "--codex-home",
                str(codex_home),
                "--marketplace",
                "../escape",
            ],
            check=False,
            capture_output=True,
            text=True,
        )

        self.assertEqual(result.returncode, 1)
        self.assertIn("--marketplace", result.stderr)
        self.assertFalse((codex_home / "plugins" / "cache" / "escape").exists())

    def test_local_installer_persists_tunnel_client_binary_hint(self) -> None:
        codex_home = pathlib.Path(self.temp.name) / "codex-home"
        fake_tunnel_client = _write_fake_tunnel_client(
            pathlib.Path(self.temp.name) / "bin" / "tunnel-client"
        )

        result = subprocess.run(
            [
                sys.executable,
                str(INSTALLER),
                "--codex-home",
                str(codex_home),
                "--tunnel-client-bin",
                str(fake_tunnel_client),
            ],
            check=False,
            capture_output=True,
            text=True,
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        hint_path = (
            codex_home
            / "plugins"
            / "cache"
            / "debug"
            / "tunnel-mcp"
            / "local"
            / ".tunnel-client-bin"
        )
        self.assertTrue(hint_path.is_file())
        self.assertEqual(
            hint_path.read_text(encoding="utf-8").strip(),
            str(fake_tunnel_client.resolve()),
        )
        self.assertIn(f"Tunnel client: {fake_tunnel_client.resolve()}", result.stdout)

    def test_installed_plugin_uses_persisted_binary_hint_from_empty_cwd(self) -> None:
        codex_home = pathlib.Path(self.temp.name) / "codex-home"
        fake_tunnel_client = _write_fake_tunnel_client(
            pathlib.Path(self.temp.name) / "bin" / "tunnel-client"
        )
        install = subprocess.run(
            [
                sys.executable,
                str(INSTALLER),
                "--codex-home",
                str(codex_home),
                "--tunnel-client-bin",
                str(fake_tunnel_client),
            ],
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(install.returncode, 0, install.stderr)

        installed_entrypoint = (
            codex_home
            / "plugins"
            / "cache"
            / "debug"
            / "tunnel-mcp"
            / "local"
            / "scripts"
            / "tunnel_mcp"
        )
        empty_cwd = pathlib.Path(self.temp.name) / "empty-cwd"
        empty_cwd.mkdir()
        env = os.environ.copy()
        env["CODEX_HOME"] = str(codex_home)
        env["XDG_CONFIG_HOME"] = str(pathlib.Path(self.temp.name) / "xdg")
        env["PATH"] = "/usr/bin:/bin"

        result = subprocess.run(
            [
                sys.executable,
                str(installed_entrypoint),
                "list",
                "--organization-id",
                "org_123",
            ],
            check=False,
            capture_output=True,
            text=True,
            cwd=empty_cwd,
            env=env,
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        payload = json.loads(result.stdout)
        self.assertEqual(payload["remote_tunnels"], [])
        self.assertEqual(payload["admin_profile"], "default")

    def test_installed_plugin_reports_friendly_missing_binary_error(self) -> None:
        standalone_root = pathlib.Path(self.temp.name) / "standalone-plugin"
        shutil.copytree(
            PLUGIN_ROOT,
            standalone_root,
            ignore=shutil.ignore_patterns("__pycache__", "*.pyc"),
        )
        codex_home = pathlib.Path(self.temp.name) / "codex-home"
        install = subprocess.run(
            [
                sys.executable,
                str(standalone_root / "scripts" / "install_plugin.py"),
                "--codex-home",
                str(codex_home),
            ],
            check=False,
            capture_output=True,
            text=True,
            cwd=pathlib.Path(self.temp.name),
        )
        self.assertEqual(install.returncode, 0, install.stderr)

        installed_entrypoint = (
            codex_home
            / "plugins"
            / "cache"
            / "debug"
            / "tunnel-mcp"
            / "local"
            / "scripts"
            / "tunnel_mcp"
        )
        empty_cwd = pathlib.Path(self.temp.name) / "empty-cwd"
        empty_cwd.mkdir()
        env = os.environ.copy()
        env["CODEX_HOME"] = str(codex_home)
        env["XDG_CONFIG_HOME"] = str(pathlib.Path(self.temp.name) / "xdg")
        env.pop("TUNNEL_CLIENT_BIN", None)
        env["PATH"] = "/usr/bin:/bin"

        result = subprocess.run(
            [
                sys.executable,
                str(installed_entrypoint),
                "list",
                "--organization-id",
                "org_123",
            ],
            check=False,
            capture_output=True,
            text=True,
            cwd=empty_cwd,
            env=env,
        )

        self.assertEqual(result.returncode, 1)
        self.assertIn("tunnel-client binary not found", result.stderr)
        self.assertIn("--tunnel-client-bin", result.stderr)
        self.assertIn("go build -o bin/tunnel-client ./cmd/client", result.stderr)
        self.assertNotIn("Traceback", result.stderr)

    def test_connect_writes_native_profile_and_starts_tmux_with_profile_flag(self) -> None:
        fake = FakeRunner()
        code, stdout, stderr = _run_main(
            [
                "connect",
                "--alias",
                "Awesome MCP",
                "--organization-id",
                "org_123",
                "--mcp-server-url",
                "http://127.0.0.1:3001/mcp",
            ],
            fake,
        )

        self.assertEqual(code, 0, stderr)
        payload = json.loads(stdout)
        profile_path = pathlib.Path(payload["profile_path"])
        self.assertEqual(payload["profile_name"], "awesome-mcp")
        self.assertEqual(
            profile_path,
            pathlib.Path(self.temp.name) / "xdg" / "tunnel-client" / "awesome-mcp.yaml",
        )
        config = json.loads(profile_path.read_text(encoding="utf-8"))
        self.assertEqual(config["control_plane"]["api_key"], "env:CONTROL_PLANE_API_KEY")
        self.assertEqual(config["control_plane"]["tunnel_id"], payload["tunnel"]["id"])
        self.assertEqual(config["health"]["listen_addr"], "127.0.0.1:0")
        self.assertEqual(config["mcp"]["server_urls"][0]["channel"], "main")
        self.assertEqual(config["mcp"]["server_urls"][0]["url"], "http://127.0.0.1:3001/mcp")
        tmux_calls = [call for call in fake.calls if call[:2] == ["tmux", "new-session"]]
        self.assertEqual(len(tmux_calls), 1)
        self.assertEqual(
            _tmux_session_name_arg(tmux_calls[0]),
            _expected_tmux_session_name("awesome-mcp"),
        )
        self.assertIn("CONTROL_PLANE_API_KEY=runtime-key", _tmux_env_entries(tmux_calls[0]))
        tmux_command = shlex.split(_tmux_command_arg(tmux_calls[0]))
        self.assertIn(pathlib.Path(tmux_command[0]).name, {"tunnel-client", "client"})
        self.assertEqual(_value_after(tmux_command, "--profile"), "awesome-mcp")
        self.assertEqual(
            _value_after(tmux_command, "--profile-dir"),
            str(pathlib.Path(self.temp.name) / "xdg" / "tunnel-client"),
        )
        self.assertEqual(payload["ui_url"], self.health_base_url + "/ui")
        self.assertEqual(payload["runtime_state"], "ready")
        self.assertTrue(payload["healthy"])
        self.assertTrue(payload["ready"])

    def test_tmux_session_name_is_scoped_by_state_root(self) -> None:
        first = runtime.tmux_session_name("docs-mcp", pathlib.Path(self.temp.name) / "one")
        second = runtime.tmux_session_name("docs-mcp", pathlib.Path(self.temp.name) / "two")

        self.assertNotEqual(first, second)
        self.assertTrue(first.startswith("tunnel-mcp__docs-mcp__"))
        self.assertTrue(second.startswith("tunnel-mcp__docs-mcp__"))

    def test_connect_existing_tunnel_id_uses_runtime_key_without_admin_crud(self) -> None:
        fake = FakeRunner()
        os.environ["TUNNEL_RUNTIME_KEY"] = "custom-runtime-key"
        fake.remote["tunnel_0123456789abcdef0123456789abcdef"] = {
            "id": "tunnel_0123456789abcdef0123456789abcdef",
            "name": "Prod MCP",
            "description": "Existing prod tunnel",
            "organization_ids": ["org_123"],
            "workspace_ids": ["ws_123"],
            "tenant_ids": [],
        }
        code, stdout, stderr = _run_main(
            [
                "connect",
                "--alias",
                "Prod MCP",
                "--tunnel-id",
                "tunnel_0123456789abcdef0123456789abcdef",
                "--runtime-api-key",
                "env:TUNNEL_RUNTIME_KEY",
                "--mcp-command",
                "python server.py",
            ],
            fake,
        )

        self.assertEqual(code, 0, stderr)
        payload = json.loads(stdout)
        self.assertEqual(payload["tunnel"]["id"], "tunnel_0123456789abcdef0123456789abcdef")
        self.assertEqual(payload["tunnel"]["name"], "Prod MCP")
        self.assertEqual(payload["tunnel"]["description"], "Existing prod tunnel")
        config = json.loads(pathlib.Path(payload["profile_path"]).read_text(encoding="utf-8"))
        self.assertEqual(config["control_plane"]["tunnel_id"], payload["tunnel"]["id"])
        self.assertEqual(config["control_plane"]["api_key"], "env:TUNNEL_RUNTIME_KEY")
        admin_calls = _admin_calls(fake)
        self.assertEqual(len(admin_calls), 1)
        tunnel_get_index = admin_calls[0].index("tunnels")
        self.assertEqual(
            admin_calls[0][tunnel_get_index : tunnel_get_index + 3],
            ["tunnels", "get", "tunnel_0123456789abcdef0123456789abcdef"],
        )
        self.assertNotIn("OPENAI_ADMIN_KEY", fake.admin_envs[0])
        self.assertEqual(fake.admin_envs[0]["CONTROL_PLANE_API_KEY"], "custom-runtime-key")
        tmux_calls = [call for call in fake.calls if call[:2] == ["tmux", "new-session"]]
        self.assertIn("TUNNEL_RUNTIME_KEY=custom-runtime-key", _tmux_env_entries(tmux_calls[0]))

    def test_connect_allows_explicit_profile_name(self) -> None:
        fake = FakeRunner()
        code, stdout, stderr = _run_main(
            [
                "connect",
                "--alias",
                "Sample MCP",
                "--profile",
                "sample_mcp_with_dcr",
                "--tunnel-id",
                "tunnel_0123456789abcdef0123456789abcdef",
                "--mcp-server-url",
                "https://mcp.example/mcp",
            ],
            fake,
        )

        self.assertEqual(code, 0, stderr)
        payload = json.loads(stdout)
        self.assertEqual(payload["profile_name"], "sample_mcp_with_dcr")
        self.assertTrue(payload["profile_path"].endswith("sample_mcp_with_dcr.yaml"))
        tmux_calls = [call for call in fake.calls if call[:2] == ["tmux", "new-session"]]
        tmux_command = shlex.split(_tmux_command_arg(tmux_calls[0]))
        self.assertIn(pathlib.Path(tmux_command[0]).name, {"tunnel-client", "client"})
        self.assertEqual(_value_after(tmux_command, "--profile"), "sample_mcp_with_dcr")
        self.assertEqual(
            _value_after(tmux_command, "--profile-dir"),
            str(pathlib.Path(self.temp.name) / "xdg" / "tunnel-client"),
        )

    def test_connect_rejects_literal_runtime_api_key(self) -> None:
        fake = FakeRunner()
        code, _stdout, stderr = _run_main(
            [
                "connect",
                "--alias",
                "bad-runtime",
                "--tunnel-id",
                "tunnel_0123456789abcdef0123456789abcdef",
                "--runtime-api-key",
                "sk-1234567890abcdef",
                "--mcp-command",
                "python server.py",
            ],
            fake,
        )

        self.assertEqual(code, 1)
        self.assertIn("runtime api_key must be", stderr)
        self.assertEqual(_admin_calls(fake), [])

    def test_connect_starts_background_process_when_tmux_missing(self) -> None:
        fake = FakeRunner(tmux_installed=False)
        fake_popen = FakePopenFactory()

        code, stdout, stderr = _run_main(
            [
                "connect",
                "--alias",
                "No Tmux MCP",
                "--organization-id",
                "org_123",
                "--mcp-command",
                "python server.py",
            ],
            fake,
            popen_factory=fake_popen,
        )

        self.assertEqual(code, 0, stderr)
        payload = json.loads(stdout)
        self.assertEqual(payload["mode"], "process")
        self.assertEqual(payload["pid"], 43210)
        self.assertTrue(payload["log_path"].endswith("no-tmux-mcp.log"))
        self.assertIn(pathlib.Path(fake_popen.calls[0][0]).name, {"tunnel-client", "client"})
        self.assertEqual(fake_popen.calls[0][1], "run")
        self.assertEqual(_value_after(fake_popen.calls[0], "--profile"), "no-tmux-mcp")
        self.assertEqual(
            _value_after(fake_popen.calls[0], "--profile-dir"),
            str(pathlib.Path(self.temp.name) / "xdg" / "tunnel-client"),
        )
        self.assertEqual(fake_popen.envs[0]["CONTROL_PLANE_API_KEY"], "runtime-key")
        process = state.load_processes()["no-tmux-mcp"]
        self.assertEqual(process.mode, "process")
        self.assertEqual(process.pid, 43210)
        self.assertEqual(process.session_name, "")

    def test_connect_prefers_adjacent_tunnel_client_binary_before_path(self) -> None:
        fake = FakeRunner(tmux_installed=False)
        fake_popen = FakePopenFactory()
        local_bin = pathlib.Path(self.temp.name) / "tunnel-client"
        local_bin.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
        local_bin.chmod(0o755)
        previous_cwd = pathlib.Path.cwd()

        try:
            os.chdir(self.temp.name)
            code, stdout, stderr = _run_main(
                [
                    "connect",
                    "--alias",
                    "adjacent-bin",
                    "--tunnel-id",
                    "tunnel_0123456789abcdef0123456789abcdef",
                    "--mcp-command",
                    "python server.py",
                ],
                fake,
                popen_factory=fake_popen,
            )
        finally:
            os.chdir(previous_cwd)

        self.assertEqual(code, 0, stderr)
        json.loads(stdout)
        self.assertEqual(fake_popen.calls[0][0], str(local_bin.resolve()))

    def test_connect_returns_json_diagnostics_when_process_exits_immediately(self) -> None:
        fake = FakeRunner(tmux_installed=False)
        fake_popen = FakePopenFactory(poll_results=[17])

        code, stdout, stderr = _run_main(
            [
                "connect",
                "--alias",
                "dies-fast",
                "--tunnel-id",
                "tunnel_0123456789abcdef0123456789abcdef",
                "--mcp-command",
                "python server.py",
            ],
            fake,
            popen_factory=fake_popen,
        )

        self.assertEqual(code, 2, stderr)
        payload = json.loads(stdout)
        self.assertFalse(payload["started"])
        self.assertFalse(payload["running"])
        self.assertEqual(payload["exit_code"], 17)
        self.assertIn("recorded process pid is not running", payload["local"]["issues"])
        self.assertTrue(payload["local"]["log"]["exists"])

    def test_connect_reuses_local_alias_when_remote_admin_auth_is_missing(self) -> None:
        root = state.ensure_state_dirs()
        state.save_aliases(
            {
                "docs-mcp": state.AliasRecord(
                    alias="docs-mcp",
                    tunnel_id="tunnel_11111111111111111111111111111111",
                    name="Docs MCP",
                    organization_ids=("org_123",),
                )
            },
            root,
        )
        fake = FakeRunner(tmux_installed=False)
        fake.admin_error = "admin key reference env:OPENAI_ADMIN_KEY is not available"
        fake_popen = FakePopenFactory()

        code, stdout, stderr = _run_main(
            [
                "connect",
                "--alias",
                "docs-mcp",
                "--mcp-command",
                "python server.py",
            ],
            fake,
            popen_factory=fake_popen,
        )

        self.assertEqual(code, 0, stderr)
        payload = json.loads(stdout)
        self.assertEqual(payload["tunnel"]["id"], "tunnel_11111111111111111111111111111111")
        self.assertIn("admin key reference", payload["remote_error"])
        self.assertEqual(len(_admin_calls(fake)), 1)
        process = state.load_processes(root)["docs-mcp"]
        self.assertEqual(process.target_value, "python server.py")

    def test_connect_persists_explicit_profile_dir(self) -> None:
        fake = FakeRunner()
        profile_dir = pathlib.Path(self.temp.name) / "profiles"

        code, stdout, stderr = _run_main(
            [
                "connect",
                "--alias",
                "custom-dir",
                "--profile-dir",
                str(profile_dir),
                "--tunnel-id",
                "tunnel_0123456789abcdef0123456789abcdef",
                "--mcp-server-url",
                "https://mcp.example/mcp",
            ],
            fake,
        )

        self.assertEqual(code, 0, stderr)
        payload = json.loads(stdout)
        self.assertEqual(payload["profile_dir"], str(profile_dir))
        self.assertTrue(payload["profile_path"].startswith(str(profile_dir)))
        process = state.load_processes()["custom-dir"]
        self.assertEqual(process.profile_dir, str(profile_dir))
        self.assertIn("--profile-dir", process.command)
        self.assertIn(str(profile_dir), process.command)

    def test_connect_requires_health_for_tmux_success(self) -> None:
        fake = FakeRunner(health_base_url="")

        code, stdout, stderr = _run_main(
            [
                "connect",
                "--alias",
                "tmux-no-health",
                "--tunnel-id",
                "tunnel_0123456789abcdef0123456789abcdef",
                "--mcp-server-url",
                "https://mcp.example/mcp",
            ],
            fake,
        )

        self.assertEqual(code, 2, stderr)
        payload = json.loads(stdout)
        self.assertFalse(payload["started"])
        self.assertFalse(payload["healthy"])
        self.assertTrue(payload["running"])
        self.assertIn("health URL file has not been populated", payload["local"]["issues"])
        self.assertEqual(payload["local"]["runtime_state"], "starting")
        self.assertTrue(
            payload["next_steps"][0].startswith(
                "tunnel-client doctor --profile tmux-no-health --profile-dir "
            )
        )

    def test_connect_rejects_inline_secret_material(self) -> None:
        fake = FakeRunner()
        code, _stdout, stderr = _run_main(
            [
                "connect",
                "--alias",
                "bad",
                "--organization-id",
                "org_123",
                "--mcp-command",
                "python server.py --api-key=sk-1234567890abcdef",
            ],
            fake,
        )

        self.assertEqual(code, 1)
        self.assertIn("inline secret material", stderr)
        self.assertFalse((pathlib.Path(self.temp.name) / "tunnel-mcp" / "aliases.yaml").exists())

    def test_connect_rejects_space_separated_secret_flag_material(self) -> None:
        fake = FakeRunner()
        code, _stdout, stderr = _run_main(
            [
                "connect",
                "--alias",
                "bad",
                "--organization-id",
                "org_123",
                "--mcp-command",
                "python server.py --api-key secret123456",
            ],
            fake,
        )

        self.assertEqual(code, 1)
        self.assertIn("inline secret material", stderr)
        self.assertFalse((pathlib.Path(self.temp.name) / "tunnel-mcp" / "aliases.yaml").exists())

    def test_connect_allows_space_separated_secret_reference(self) -> None:
        fake = FakeRunner()
        code, stdout, stderr = _run_main(
            [
                "connect",
                "--alias",
                "env-ref",
                "--organization-id",
                "org_123",
                "--mcp-command",
                "python server.py --api-key env:SERVER_API_KEY",
            ],
            fake,
        )

        self.assertEqual(code, 0, stderr)
        payload = json.loads(stdout)
        config = json.loads(pathlib.Path(payload["profile_path"]).read_text(encoding="utf-8"))
        self.assertEqual(
            config["mcp"]["commands"][0]["command"],
            "python server.py --api-key env:SERVER_API_KEY",
        )

    def test_create_persists_alias(self) -> None:
        fake = FakeRunner()
        code, stdout, stderr = _run_main(
            ["create", "--alias", "Docs MCP", "--organization-id", "org_123"],
            fake,
        )

        self.assertEqual(code, 0, stderr)
        payload = json.loads(stdout)
        aliases = state.load_aliases()
        self.assertIn("docs-mcp", aliases)
        self.assertEqual(aliases["docs-mcp"].tunnel_id, payload["tunnel"]["id"])
        self.assertEqual(aliases["docs-mcp"].organization_ids, ("org_123",))

    def test_create_persists_admin_profile_and_links_alias(self) -> None:
        fake = FakeRunner()
        os.environ["SANDBOX_ADMIN_KEY"] = "sandbox-admin-key"
        code, stdout, stderr = _run_main(
            [
                "create",
                "--alias",
                "Docs MCP",
                "--organization-id",
                "org_123",
                "--admin-profile",
                "Sandbox Admin",
                "--admin-key",
                "env:SANDBOX_ADMIN_KEY",
                "--control-plane-base-url",
                "https://sandbox.example.com",
            ],
            fake,
        )

        self.assertEqual(code, 0, stderr)
        payload = json.loads(stdout)
        self.assertEqual(payload["admin_profile"], "sandbox-admin")
        profiles = state.load_admin_profiles()
        self.assertEqual(profiles["sandbox-admin"].admin_key, "env:SANDBOX_ADMIN_KEY")
        self.assertEqual(
            profiles["sandbox-admin"].control_plane_base_url,
            "https://sandbox.example.com",
        )
        aliases = state.load_aliases()
        self.assertEqual(aliases["docs-mcp"].admin_profile, "sandbox-admin")
        admin_call = _admin_calls(fake)[0]
        self.assertNotIn("--admin-key", admin_call)
        self.assertEqual(fake.admin_envs[0]["OPENAI_ADMIN_KEY"], "sandbox-admin-key")
        self.assertEqual(
            _value_after(admin_call, "--control-plane.base-url"),
            "https://sandbox.example.com",
        )

    def test_create_rejects_literal_admin_key(self) -> None:
        fake = FakeRunner()
        code, _stdout, stderr = _run_main(
            [
                "create",
                "--alias",
                "bad-admin",
                "--organization-id",
                "org_123",
                "--admin-key",
                "sk-1234567890abcdef",
            ],
            fake,
        )

        self.assertEqual(code, 1)
        self.assertIn("admin profile default admin_key must be", stderr)
        self.assertEqual(_admin_calls(fake), [])

    def test_create_recovers_from_stale_alias_when_remote_get_fails(self) -> None:
        root = state.ensure_state_dirs()
        state.save_aliases(
            {
                "docs-mcp": state.AliasRecord(
                    alias="docs-mcp",
                    tunnel_id="tunnel_deadbeefdeadbeefdeadbeefdeadbeef",
                    name="docs-mcp",
                    organization_ids=("org_123",),
                )
            },
            root,
        )
        fake = FakeRunner()

        code, stdout, stderr = _run_main(
            ["create", "--alias", "docs-mcp", "--organization-id", "org_123"],
            fake,
        )

        self.assertEqual(code, 0, stderr)
        payload = json.loads(stdout)
        aliases = state.load_aliases(root)
        self.assertEqual(aliases["docs-mcp"].tunnel_id, payload["tunnel"]["id"])
        self.assertNotEqual(
            aliases["docs-mcp"].tunnel_id, "tunnel_deadbeefdeadbeefdeadbeefdeadbeef"
        )
        history = (root / "history.md").read_text(encoding="utf-8")
        self.assertIn("action=stale-alias", history)

    def test_connect_recovers_from_stale_alias_when_remote_get_fails(self) -> None:
        root = state.ensure_state_dirs()
        state.save_aliases(
            {
                "docs-mcp": state.AliasRecord(
                    alias="docs-mcp",
                    tunnel_id="tunnel_deadbeefdeadbeefdeadbeefdeadbeef",
                    name="docs-mcp",
                    organization_ids=("org_123",),
                )
            },
            root,
        )
        fake = FakeRunner()

        code, stdout, stderr = _run_main(
            [
                "connect",
                "--alias",
                "docs-mcp",
                "--organization-id",
                "org_123",
                "--mcp-command",
                "python server.py",
            ],
            fake,
        )

        self.assertEqual(code, 0, stderr)
        payload = json.loads(stdout)
        self.assertNotEqual(payload["tunnel"]["id"], "tunnel_deadbeefdeadbeefdeadbeefdeadbeef")
        config = json.loads(pathlib.Path(payload["profile_path"]).read_text(encoding="utf-8"))
        self.assertEqual(config["mcp"]["commands"][0]["command"], "python server.py")

    def test_connect_restarts_running_tmux_when_stale_alias_is_replaced(self) -> None:
        root = state.ensure_state_dirs()
        stale_tunnel_id = "tunnel_deadbeefdeadbeefdeadbeefdeadbeef"
        state.save_aliases(
            {
                "docs-mcp": state.AliasRecord(
                    alias="docs-mcp",
                    tunnel_id=stale_tunnel_id,
                    name="docs-mcp",
                    organization_ids=("org_123",),
                )
            },
            root,
        )
        state.save_processes(
            {
                "docs-mcp": state.ProcessRecord(
                    alias="docs-mcp",
                    tunnel_id=stale_tunnel_id,
                    session_name=_expected_tmux_session_name("docs-mcp", root),
                    config_path=str(root / "configs" / "docs-mcp.yaml"),
                    health_url_file=str(root / "health" / "docs-mcp.url"),
                    target_kind="command",
                    target_value="python old_server.py",
                    command="tunnel-client run --config docs-mcp.yaml",
                    started_at="2026-04-17T00:00:00Z",
                )
            },
            root,
        )
        fake = FakeRunner()
        fake.sessions.add(_expected_tmux_session_name("docs-mcp", root))

        code, stdout, stderr = _run_main(
            [
                "connect",
                "--alias",
                "docs-mcp",
                "--organization-id",
                "org_123",
                "--mcp-command",
                "python server.py",
            ],
            fake,
        )

        self.assertEqual(code, 0, stderr)
        payload = json.loads(stdout)
        self.assertTrue(payload["started"])
        self.assertFalse(payload["already_running"])
        self.assertNotEqual(payload["tunnel"]["id"], stale_tunnel_id)
        self.assertIn(_expected_tmux_session_name("docs-mcp", root), fake.sessions)
        kill_calls = [call for call in fake.calls if call[:3] == ["tmux", "kill-session", "-t"]]
        self.assertEqual(len(kill_calls), 1)
        new_session_calls = [call for call in fake.calls if call[:2] == ["tmux", "new-session"]]
        self.assertEqual(len(new_session_calls), 1)
        process = state.load_processes(root)["docs-mcp"]
        self.assertEqual(process.tunnel_id, payload["tunnel"]["id"])
        history = (root / "history.md").read_text(encoding="utf-8")
        self.assertIn("action=stale-alias", history)
        self.assertIn("action=stale-process", history)

    def test_status_reports_stale_alias_without_silent_recreate(self) -> None:
        root = state.ensure_state_dirs()
        os.environ["CONTROL_PLANE_API_KEY"] = "runtime-key"
        state.save_aliases(
            {
                "docs-mcp": state.AliasRecord(
                    alias="docs-mcp",
                    tunnel_id="tunnel_deadbeefdeadbeefdeadbeefdeadbeef",
                    name="docs-mcp",
                    organization_ids=("org_123",),
                )
            },
            root,
        )
        fake = FakeRunner()

        code, stdout, stderr = _run_main(["status", "docs-mcp"], fake)

        self.assertEqual(code, 2, stderr)
        payload = json.loads(stdout)
        self.assertTrue(payload["stale"])
        self.assertEqual(payload["tunnel_id"], "tunnel_deadbeefdeadbeefdeadbeefdeadbeef")
        self.assertEqual(payload["remote_lookup_auth_kind"], "runtime")
        self.assertEqual(payload["remote_lookup_auth_ref"], "env:CONTROL_PLANE_API_KEY")
        create_calls = [call for call in fake.calls if "create" in call]
        self.assertEqual(create_calls, [])
        self.assertIn("scripts/tunnel_mcp connect --alias docs-mcp", payload["repair_command"])

    def test_status_surfaces_local_diagnostics_when_remote_admin_auth_is_missing(self) -> None:
        root = state.ensure_state_dirs()
        os.environ.pop("OPENAI_ADMIN_KEY", None)
        os.environ.pop("CONTROL_PLANE_API_KEY", None)
        os.environ.pop("OPENAI_API_KEY", None)
        health_file = root / "health" / "docs-mcp.url"
        health_file.write_text("", encoding="utf-8")
        profile_path = pathlib.Path(self.temp.name) / "xdg" / "tunnel-client" / "docs-mcp.yaml"
        profile_path.parent.mkdir(parents=True, exist_ok=True)
        profile_path.write_text("{}\n", encoding="utf-8")
        log_path = root / "logs" / "docs-mcp.log"
        log_path.write_text("runtime failed to start\nmissing admin key\n", encoding="utf-8")
        state.save_aliases(
            {
                "docs-mcp": state.AliasRecord(
                    alias="docs-mcp",
                    tunnel_id="tunnel_11111111111111111111111111111111",
                    name="Docs MCP",
                    organization_ids=("org_123",),
                    profile_name="docs-mcp",
                    profile_path=str(profile_path),
                    config_path=str(profile_path),
                    health_url_file=str(health_file),
                )
            },
            root,
        )
        state.save_processes(
            {
                "docs-mcp": state.ProcessRecord(
                    alias="docs-mcp",
                    tunnel_id="tunnel_11111111111111111111111111111111",
                    mode="process",
                    pid=43210,
                    config_path=str(profile_path),
                    profile_name="docs-mcp",
                    profile_path=str(profile_path),
                    health_url_file=str(health_file),
                    target_kind="command",
                    target_value="python server.py",
                    command="tunnel-client run --profile docs-mcp",
                    log_path=str(log_path),
                    started_at="2026-04-17T00:00:00Z",
                )
            },
            root,
        )
        fake = FakeRunner()

        code, stdout, stderr = _run_main(["status", "docs-mcp"], fake)

        self.assertEqual(code, 0, stderr)
        payload = json.loads(stdout)
        self.assertIsNone(payload["remote"])
        self.assertEqual(payload["remote_error"], "")
        self.assertEqual(
            payload["remote_skipped_reason"],
            "environment variable OPENAI_ADMIN_KEY is not set",
        )
        self.assertTrue(payload["next_steps"][0].startswith("tunnel-client doctor "))
        self.assertIn("scripts/tunnel_mcp connect --alias docs-mcp", payload["repair_command"])
        self.assertEqual(payload["local"]["runtime_state"], "stopped")
        self.assertIn("recorded process pid is not running", payload["local"]["issues"])
        self.assertIn("health URL file has not been populated", payload["local"]["issues"])
        self.assertEqual(
            payload["local"]["log"]["tail"],
            "runtime failed to start\nmissing admin key",
        )

    def test_list_merges_remote_inventory_with_local_state(self) -> None:
        root = state.ensure_state_dirs()
        state.save_aliases(
            {
                "local-one": state.AliasRecord(
                    alias="local-one",
                    tunnel_id="tunnel_11111111111111111111111111111111",
                    name="Local One",
                    admin_profile="sandbox",
                    organization_ids=("org_123",),
                )
            },
            root,
        )
        fake = FakeRunner()
        fake.remote["tunnel_11111111111111111111111111111111"] = {
            "id": "tunnel_11111111111111111111111111111111",
            "name": "Local One",
            "description": "local",
            "organization_ids": ["org_123"],
            "workspace_ids": [],
            "tenant_ids": [],
        }
        fake.remote["tunnel_22222222222222222222222222222222"] = {
            "id": "tunnel_22222222222222222222222222222222",
            "name": "Remote Two",
            "description": "remote",
            "organization_ids": ["org_123"],
            "workspace_ids": [],
            "tenant_ids": [],
        }

        code, stdout, stderr = _run_main(["list", "--organization-id", "org_123"], fake)

        self.assertEqual(code, 0, stderr)
        payload = json.loads(stdout)
        local_by_id = {item["id"]: item["local_alias"] for item in payload["remote_tunnels"]}
        admin_by_id = {
            item["id"]: item["local_admin_profile"] for item in payload["remote_tunnels"]
        }
        self.assertEqual(local_by_id["tunnel_11111111111111111111111111111111"], "local-one")
        self.assertEqual(admin_by_id["tunnel_11111111111111111111111111111111"], "sandbox")
        self.assertIsNone(local_by_id["tunnel_22222222222222222222222222222222"])

    def test_status_uses_alias_admin_profile_by_default(self) -> None:
        root = state.ensure_state_dirs()
        state.save_admin_profiles(
            {
                "sandbox": state.AdminProfile(
                    name="sandbox",
                    control_plane_base_url="https://sandbox.example.com",
                    admin_key="env:SANDBOX_ADMIN_KEY",
                )
            },
            root,
            active_profile="sandbox",
        )
        state.save_aliases(
            {
                "docs-mcp": state.AliasRecord(
                    alias="docs-mcp",
                    tunnel_id="tunnel_11111111111111111111111111111111",
                    name="Docs MCP",
                    admin_profile="sandbox",
                    organization_ids=("org_123",),
                )
            },
            root,
        )
        fake = FakeRunner()
        fake.remote["tunnel_11111111111111111111111111111111"] = {
            "id": "tunnel_11111111111111111111111111111111",
            "name": "Docs MCP",
            "description": "remote",
            "organization_ids": ["org_123"],
            "workspace_ids": [],
            "tenant_ids": [],
        }
        os.environ["TUNNEL_MCP_ADMIN_PROFILE"] = "default"
        os.environ["SANDBOX_ADMIN_KEY"] = "sandbox-admin-key"
        os.environ.pop("CONTROL_PLANE_API_KEY", None)
        os.environ.pop("OPENAI_API_KEY", None)

        code, stdout, stderr = _run_main(["status", "docs-mcp"], fake)

        self.assertEqual(code, 0, stderr)
        payload = json.loads(stdout)
        self.assertEqual(payload["admin_profile"], "sandbox")
        self.assertEqual(payload["remote_lookup_auth_kind"], "admin")
        admin_call = _admin_calls(fake)[0]
        self.assertNotIn("--admin-key", admin_call)
        self.assertEqual(fake.admin_envs[0]["OPENAI_ADMIN_KEY"], "sandbox-admin-key")
        self.assertEqual(
            _value_after(admin_call, "--control-plane.base-url"),
            "https://sandbox.example.com",
        )

    def test_status_merges_local_process_and_remote_metadata(self) -> None:
        root = state.ensure_state_dirs()
        os.environ["CONTROL_PLANE_API_KEY"] = "runtime-key"
        health_file = root / "health" / "docs-mcp.url"
        health_file.write_text(self.health_base_url + "/healthz\n", encoding="utf-8")
        state.save_aliases(
            {
                "docs-mcp": state.AliasRecord(
                    alias="docs-mcp",
                    tunnel_id="tunnel_11111111111111111111111111111111",
                    name="Docs MCP",
                    organization_ids=("org_123",),
                    config_path=str(root / "configs" / "docs-mcp.yaml"),
                    health_url_file=str(health_file),
                )
            },
            root,
        )
        state.save_processes(
            {
                "docs-mcp": state.ProcessRecord(
                    alias="docs-mcp",
                    tunnel_id="tunnel_11111111111111111111111111111111",
                    session_name=_expected_tmux_session_name("docs-mcp", root),
                    config_path=str(root / "configs" / "docs-mcp.yaml"),
                    health_url_file=str(health_file),
                    target_kind="server_url",
                    target_value="http://127.0.0.1:3001/mcp",
                    command="tunnel-client run --config docs-mcp.yaml",
                    started_at="2026-04-17T00:00:00Z",
                )
            },
            root,
        )
        fake = FakeRunner()
        fake.sessions.add(_expected_tmux_session_name("docs-mcp", root))
        fake.remote["tunnel_11111111111111111111111111111111"] = {
            "id": "tunnel_11111111111111111111111111111111",
            "name": "Docs MCP",
            "description": "remote",
            "organization_ids": ["org_123"],
            "workspace_ids": [],
            "tenant_ids": [],
        }

        code, stdout, stderr = _run_main(["status", "docs-mcp"], fake)

        self.assertEqual(code, 0, stderr)
        payload = json.loads(stdout)
        self.assertFalse(payload["stale"])
        self.assertTrue(payload["tmux"]["running"])
        self.assertEqual(payload["health_url"], self.health_base_url + "/healthz")
        self.assertEqual(payload["ui_url"], self.health_base_url + "/ui")
        self.assertEqual(payload["runtime_state"], "ready")
        self.assertEqual(payload["remote_lookup_auth_kind"], "runtime")
        self.assertEqual(payload["remote_lookup_auth_ref"], "env:CONTROL_PLANE_API_KEY")
        self.assertEqual(payload["remote"]["name"], "Docs MCP")
        self.assertIn("tunnel-client doctor --config", payload["next_steps"][0])
        admin_call = _admin_calls(fake)[0]
        self.assertNotIn("--admin-key", admin_call)
        self.assertEqual(fake.admin_envs[0]["CONTROL_PLANE_API_KEY"], "runtime-key")

    def test_stop_marks_tmux_runtime_stopped_and_clears_health_url(self) -> None:
        root = state.ensure_state_dirs()
        health_file = root / "health" / "docs-mcp.url"
        health_file.write_text("http://127.0.0.1:4567/healthz\n", encoding="utf-8")
        state.save_aliases(
            {
                "docs-mcp": state.AliasRecord(
                    alias="docs-mcp",
                    tunnel_id="tunnel_11111111111111111111111111111111",
                    name="Docs MCP",
                    profile_name="docs-mcp",
                    profile_path=str(root / "configs" / "docs-mcp.yaml"),
                    config_path=str(root / "configs" / "docs-mcp.yaml"),
                    health_url_file=str(health_file),
                )
            },
            root,
        )
        state.save_processes(
            {
                "docs-mcp": state.ProcessRecord(
                    alias="docs-mcp",
                    tunnel_id="tunnel_11111111111111111111111111111111",
                    mode="tmux",
                    session_name=_expected_tmux_session_name("docs-mcp", root),
                    config_path=str(root / "configs" / "docs-mcp.yaml"),
                    profile_name="docs-mcp",
                    profile_path=str(root / "configs" / "docs-mcp.yaml"),
                    health_url_file=str(health_file),
                    target_kind="command",
                    target_value="python server.py",
                    command="tunnel-client run --profile docs-mcp",
                    started_at="2026-04-17T00:00:00Z",
                )
            },
            root,
        )
        fake = FakeRunner()
        fake.sessions.add(_expected_tmux_session_name("docs-mcp", root))

        code, stdout, stderr = _run_main(["stop", "docs-mcp"], fake)

        self.assertEqual(code, 0, stderr)
        payload = json.loads(stdout)
        self.assertTrue(payload["stopped"])
        self.assertFalse(payload["already_stopped"])
        self.assertEqual(payload["runtime_state"], "stopped")
        self.assertEqual(payload["ui_url"], "")
        self.assertFalse(pathlib.Path(health_file).exists())
        process = state.load_processes(root)["docs-mcp"]
        self.assertEqual(process.mode, "stopped")
        self.assertEqual(process.pid, 0)
        self.assertEqual(process.session_name, "")
        kill_calls = [call for call in fake.calls if call[:3] == ["tmux", "kill-session", "-t"]]
        self.assertEqual(len(kill_calls), 1)

    def test_disconnect_aliases_stop_for_process_mode(self) -> None:
        root = state.ensure_state_dirs()
        profile_path = pathlib.Path(self.temp.name) / "xdg" / "tunnel-client" / "docs-mcp.yaml"
        profile_path.parent.mkdir(parents=True, exist_ok=True)
        profile_path.write_text("{}\n", encoding="utf-8")
        state.save_aliases(
            {
                "docs-mcp": state.AliasRecord(
                    alias="docs-mcp",
                    tunnel_id="tunnel_11111111111111111111111111111111",
                    name="Docs MCP",
                    profile_name="docs-mcp",
                    profile_path=str(profile_path),
                    config_path=str(profile_path),
                    health_url_file=str(root / "health" / "docs-mcp.url"),
                )
            },
            root,
        )
        state.save_processes(
            {
                "docs-mcp": state.ProcessRecord(
                    alias="docs-mcp",
                    tunnel_id="tunnel_11111111111111111111111111111111",
                    mode="process",
                    pid=os.getpid(),
                    config_path=str(profile_path),
                    profile_name="docs-mcp",
                    profile_path=str(profile_path),
                    health_url_file=str(root / "health" / "docs-mcp.url"),
                    target_kind="command",
                    target_value="python server.py",
                    command="tunnel-client run --profile docs-mcp",
                    started_at="2026-04-17T00:00:00Z",
                )
            },
            root,
        )

        original_terminate = runtime.terminate_process
        original_wait = runtime.wait_for_process_exit
        try:
            runtime.terminate_process = lambda pid: None  # type: ignore[assignment]
            runtime.wait_for_process_exit = lambda pid, timeout_seconds=1.0: True  # type: ignore[assignment]
            code, stdout, stderr = _run_main(["disconnect", "docs-mcp"], FakeRunner())
        finally:
            runtime.terminate_process = original_terminate  # type: ignore[assignment]
            runtime.wait_for_process_exit = original_wait  # type: ignore[assignment]

        self.assertEqual(code, 0, stderr)
        payload = json.loads(stdout)
        self.assertTrue(payload["stopped"])
        self.assertEqual(payload["runtime_state"], "stopped")
        process = state.load_processes(root)["docs-mcp"]
        self.assertEqual(process.mode, "stopped")
        self.assertEqual(process.pid, 0)


def _run_main(
    argv: list[str],
    fake: FakeRunner,
    *,
    popen_factory: runtime.PopenFactory = subprocess.Popen,
) -> tuple[int, str, str]:
    stdout = io.StringIO()
    stderr = io.StringIO()
    with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
        code = commands.main(argv, runner=fake, popen_factory=popen_factory)
    return code, stdout.getvalue(), stderr.getvalue()


def _completed(
    args: list[str],
    returncode: int,
    *,
    stdout: str = "",
    stderr: str = "",
) -> subprocess.CompletedProcess[str]:
    return subprocess.CompletedProcess(
        args=args, returncode=returncode, stdout=stdout, stderr=stderr
    )


def _value_after(args: list[str], flag: str) -> str:
    return args[args.index(flag) + 1]


def _values_after(args: list[str], flag: str) -> list[str]:
    values = []
    for idx, arg in enumerate(args):
        if arg == flag:
            values.append(args[idx + 1])
    return values


def _admin_calls(fake: FakeRunner) -> list[list[str]]:
    return [call for call in fake.calls if len(call) >= 2 and call[1] == "admin"]


def _tmux_session_name_arg(args: list[str]) -> str:
    return args[args.index("-s") + 1]


def _tmux_command_arg(args: list[str]) -> str:
    return args[-1]


def _tmux_env_entries(args: list[str]) -> list[str]:
    entries: list[str] = []
    for idx, arg in enumerate(args):
        if arg == "-e":
            entries.append(args[idx + 1])
    return entries


def _expected_tmux_session_name(alias: str, root: pathlib.Path | None = None) -> str:
    return runtime.tmux_session_name(alias, root or state.ensure_state_dirs())


def _alias_from_session_name(session_name: str) -> str:
    return session_name.removeprefix("tunnel-mcp__").rsplit("__", 1)[0]


def _write_health_url_for_alias(alias: str, health_base_url: str) -> None:
    health_dir = pathlib.Path(os.environ["CODEX_HOME"]) / "tunnel-mcp" / "health"
    health_dir.mkdir(parents=True, exist_ok=True)
    (health_dir / f"{alias}.url").write_text(health_base_url + "\n", encoding="utf-8")


class _HealthHandler(http.server.BaseHTTPRequestHandler):
    def do_GET(self) -> None:  # noqa: N802
        if self.path == "/healthz":
            self.send_response(200)
            self.send_header("Content-Type", "text/plain; charset=utf-8")
            self.end_headers()
            self.wfile.write(b"live")
            return
        if self.path == "/readyz":
            self.send_response(200)
            self.send_header("Content-Type", "text/plain; charset=utf-8")
            self.end_headers()
            self.wfile.write(b"ready")
            return
        self.send_response(404)
        self.end_headers()

    def log_message(self, format: str, *args: object) -> None:
        return


def _write_fake_tunnel_client(path: pathlib.Path) -> pathlib.Path:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        """#!/usr/bin/env python3
import json
import sys

if "admin" in sys.argv and "list" in sys.argv:
    print(json.dumps({"tunnels": []}))
    raise SystemExit(0)

print(json.dumps({"id": "tunnel_fake"}))
""",
        encoding="utf-8",
    )
    path.chmod(0o755)
    return path


if __name__ == "__main__":
    unittest.main()
