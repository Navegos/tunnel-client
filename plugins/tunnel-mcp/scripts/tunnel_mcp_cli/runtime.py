from __future__ import annotations

import dataclasses
import json
import os
import re
import shlex
import signal
import subprocess
from pathlib import Path
from typing import Callable, Protocol

from tunnel_mcp_cli import state

Runner = Callable[..., subprocess.CompletedProcess[str]]
_PROFILE_NAME_PATTERN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")


class PopenFactory(Protocol):
    def __call__(self, args: list[str], **kwargs: object) -> subprocess.Popen[bytes]: ...


@dataclasses.dataclass(frozen=True)
class RuntimeTarget:
    kind: str
    value: str


@dataclasses.dataclass(frozen=True)
class LaunchResult:
    mode: str
    command: str
    started: bool
    already_running: bool
    session_name: str = ""
    pid: int = 0
    log_path: str = ""


def default_profile_dir() -> Path:
    override = os.environ.get("TUNNEL_CLIENT_PROFILE_DIR", "").strip()
    if override:
        return Path(override).expanduser()
    xdg_config_home = os.environ.get("XDG_CONFIG_HOME", "").strip()
    if xdg_config_home:
        return Path(xdg_config_home).expanduser() / "tunnel-client"
    return Path.home() / ".config" / "tunnel-client"


def normalize_profile_name(profile_name: str, *, alias: str) -> str:
    name = (profile_name or state.normalize_alias(alias)).strip()
    if not name:
        raise state.StateError("profile name must not be empty")
    if name in {".", ".."} or "/" in name or "\\" in name:
        raise state.StateError("profile name must not contain path separators")
    if not _PROFILE_NAME_PATTERN.match(name):
        raise state.StateError("profile name must use letters, numbers, '.', '_' or '-'")
    return name


def write_runtime_profile(
    alias: str,
    profile_name: str,
    tunnel_id: str,
    base_url: str,
    api_key: str,
    target: RuntimeTarget,
    state_root: Path,
) -> Path:
    normalized_alias = state.normalize_alias(alias)
    normalized_profile = normalize_profile_name(profile_name, alias=normalized_alias)
    state.reject_inline_secret_material(target.value, field=f"mcp {target.kind}")
    root = state.ensure_state_dirs(state_root)
    config_path = default_profile_dir() / f"{normalized_profile}.yaml"
    health_url_file = root / "health" / f"{normalized_alias}.url"

    config: dict[str, object] = {
        "config_version": 1,
        "control_plane": {
            "base_url": base_url,
            "tunnel_id": tunnel_id,
            "api_key": api_key,
        },
        "health": {
            "listen_addr": "127.0.0.1:0",
            "url_file": str(health_url_file),
        },
        "admin_ui": {
            "open_browser": False,
        },
        "log": {
            "level": "info",
            "format": "json",
        },
        "mcp": _mcp_config(target),
    }
    config_path.parent.mkdir(parents=True, exist_ok=True)
    config_path.write_text(json.dumps(config, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    config_path.chmod(0o600)
    return config_path


def profile_health_url_file(alias: str, state_root: Path) -> Path:
    return state.ensure_state_dirs(state_root) / "health" / f"{state.normalize_alias(alias)}.url"


def tmux_session_name(alias: str) -> str:
    return f"tunnel-mcp__{state.normalize_alias(alias)}"


def tunnel_client_args(tunnel_client_bin: str, profile_name: str) -> list[str]:
    return [tunnel_client_bin, "run", "--profile", profile_name]


def tunnel_client_command(tunnel_client_bin: str, profile_name: str) -> str:
    return " ".join(
        shlex.quote(part) for part in tunnel_client_args(tunnel_client_bin, profile_name)
    )


def start_or_reuse(
    alias: str,
    profile_name: str,
    tunnel_client_bin: str,
    state_root: Path,
    runner: Runner,
    popen_factory: PopenFactory,
    existing_pid: int = 0,
    replace_existing: bool = False,
) -> LaunchResult:
    profile_name = normalize_profile_name(profile_name, alias=alias)
    command = tunnel_client_command(tunnel_client_bin, profile_name)
    session = tmux_session_name(alias)
    if tmux_available(runner):
        if tmux_has_session(alias, runner):
            if replace_existing:
                result = stop_tmux(alias, runner)
                if result.returncode != 0:
                    stderr = (result.stderr or "").strip()
                    stdout = (result.stdout or "").strip()
                    raise RuntimeError(
                        stderr
                        or stdout
                        or f"tmux kill-session failed with exit {result.returncode}"
                    )
            else:
                return LaunchResult(
                    mode="tmux",
                    command=command,
                    started=False,
                    already_running=True,
                    session_name=session,
                )
        result = start_tmux(alias, profile_name, tunnel_client_bin, runner)
        if result.returncode != 0:
            stderr = (result.stderr or "").strip()
            stdout = (result.stdout or "").strip()
            raise RuntimeError(
                stderr or stdout or f"tmux new-session failed with exit {result.returncode}"
            )
        return LaunchResult(
            mode="tmux",
            command=command,
            started=True,
            already_running=False,
            session_name=session,
        )

    if existing_pid and pid_is_running(existing_pid):
        if replace_existing:
            terminate_process(existing_pid)
        else:
            return LaunchResult(
                mode="process",
                command=command,
                started=False,
                already_running=True,
                pid=existing_pid,
                log_path=str(log_path(alias, state_root)),
            )

    process = start_background_process(
        alias, profile_name, tunnel_client_bin, state_root, popen_factory
    )
    return LaunchResult(
        mode="process",
        command=command,
        started=True,
        already_running=False,
        pid=int(process.pid),
        log_path=str(log_path(alias, state_root)),
    )


def tmux_available(runner: Runner) -> bool:
    try:
        result = runner(["tmux", "-V"], check=False, capture_output=True, text=True)
    except FileNotFoundError:
        return False
    return result.returncode == 0


def tmux_has_session(alias: str, runner: Runner) -> bool:
    session = tmux_session_name(alias)
    try:
        result = runner(
            ["tmux", "has-session", "-t", f"={session}"],
            check=False,
            capture_output=True,
            text=True,
        )
    except FileNotFoundError:
        return False
    return result.returncode == 0


def start_tmux(
    alias: str, profile_name: str, tunnel_client_bin: str, runner: Runner
) -> subprocess.CompletedProcess[str]:
    session = tmux_session_name(alias)
    command = tunnel_client_command(tunnel_client_bin, profile_name)
    return runner(
        ["tmux", "new-session", "-d", "-s", session, command],
        check=False,
        capture_output=True,
        text=True,
    )


def stop_tmux(alias: str, runner: Runner) -> subprocess.CompletedProcess[str]:
    session = tmux_session_name(alias)
    return runner(
        ["tmux", "kill-session", "-t", f"={session}"],
        check=False,
        capture_output=True,
        text=True,
    )


def log_path(alias: str, state_root: Path) -> Path:
    return state.ensure_state_dirs(state_root) / "logs" / f"{state.normalize_alias(alias)}.log"


def start_background_process(
    alias: str,
    profile_name: str,
    tunnel_client_bin: str,
    state_root: Path,
    popen_factory: PopenFactory,
) -> subprocess.Popen[bytes]:
    output_path = log_path(alias, state_root)
    output_path.parent.mkdir(parents=True, exist_ok=True)
    log_file = output_path.open("ab")
    try:
        return popen_factory(
            tunnel_client_args(tunnel_client_bin, profile_name),
            stdin=subprocess.DEVNULL,
            stdout=log_file,
            stderr=subprocess.STDOUT,
            close_fds=True,
            start_new_session=True,
        )
    finally:
        log_file.close()


def pid_is_running(pid: int) -> bool:
    if pid <= 0:
        return False
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    return True


def terminate_process(pid: int) -> None:
    try:
        os.kill(pid, signal.SIGTERM)
    except ProcessLookupError:
        return
    except PermissionError as exc:
        raise RuntimeError(f"cannot stop existing tunnel-client process {pid}") from exc


def _mcp_config(target: RuntimeTarget) -> dict[str, object]:
    if target.kind == "server_url":
        return {
            "server_urls": [
                {
                    "channel": "main",
                    "url": target.value,
                }
            ]
        }
    if target.kind == "command":
        return {
            "commands": [
                {
                    "channel": "main",
                    "command": target.value,
                }
            ]
        }
    raise ValueError(f"unsupported runtime target kind {target.kind}")
