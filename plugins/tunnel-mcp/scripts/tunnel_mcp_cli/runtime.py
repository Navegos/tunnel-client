from __future__ import annotations

import dataclasses
import hashlib
import json
import os
import re
import shlex
import shutil
import signal
import subprocess
import time
from pathlib import Path
from typing import Callable, Protocol
from urllib import error as urlerror
from urllib import parse as urlparse
from urllib import request as urlrequest

from tunnel_mcp_cli import state

Runner = Callable[..., subprocess.CompletedProcess[str]]
_PROFILE_NAME_PATTERN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")
_LAUNCH_SETTLE_SECONDS = 0.05
_LAUNCH_HEALTH_TIMEOUT_SECONDS = 2.0
_LAUNCH_HEALTH_POLL_SECONDS = 0.05
_HEALTH_PROBE_TIMEOUT_SECONDS = 0.5
_TERMINATE_WAIT_SECONDS = 1.0
_BIN_HINT_FILENAME = ".tunnel-client-bin"


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
    launched: bool
    started: bool
    running: bool
    healthy: bool
    ready: bool
    already_running: bool
    health_url: str = ""
    session_name: str = ""
    pid: int = 0
    log_path: str = ""
    exit_code: int | None = None


@dataclasses.dataclass(frozen=True)
class EndpointProbe:
    url: str = ""
    ok: bool = False
    status: int = 0
    body: str = ""
    error: str = ""


@dataclasses.dataclass(frozen=True)
class HealthProbe:
    base_url: str = ""
    healthz: EndpointProbe = dataclasses.field(default_factory=EndpointProbe)
    readyz: EndpointProbe = dataclasses.field(default_factory=EndpointProbe)


@dataclasses.dataclass(frozen=True)
class RuntimeObservation:
    running: bool
    health_url: str = ""
    healthy: bool = False
    ready: bool = False
    health_probe: HealthProbe = dataclasses.field(default_factory=HealthProbe)


def default_profile_dir() -> Path:
    override = os.environ.get("TUNNEL_CLIENT_PROFILE_DIR", "").strip()
    if override:
        return Path(override).expanduser()
    xdg_config_home = os.environ.get("XDG_CONFIG_HOME", "").strip()
    if xdg_config_home:
        return Path(xdg_config_home).expanduser() / "tunnel-client"
    return Path.home() / ".config" / "tunnel-client"


def resolve_profile_dir(profile_dir: str | None = None) -> Path:
    override = (profile_dir or "").strip()
    if override:
        return Path(override).expanduser()
    return default_profile_dir()


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
    profile_dir: Path | None,
    state_root: Path,
) -> Path:
    normalized_alias = state.normalize_alias(alias)
    normalized_profile = normalize_profile_name(profile_name, alias=normalized_alias)
    state.reject_inline_secret_material(target.value, field=f"mcp {target.kind}")
    root = state.ensure_state_dirs(state_root)
    config_root = profile_dir or default_profile_dir()
    config_path = config_root / f"{normalized_profile}.yaml"
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


def tmux_session_name(alias: str, state_root: Path | None = None) -> str:
    normalized_alias = state.normalize_alias(alias)
    if state_root is None:
        return f"tunnel-mcp__{normalized_alias}"
    return f"tunnel-mcp__{normalized_alias}__{_session_scope_suffix(state_root)}"


def _session_scope_suffix(state_root: Path) -> str:
    try:
        normalized_root = str(state_root.expanduser().resolve())
    except OSError:
        normalized_root = str(state_root.expanduser().absolute())
    return hashlib.sha256(normalized_root.encode("utf-8")).hexdigest()[:8]


def tunnel_client_args(tunnel_client_bin: str, profile_name: str, profile_dir: Path) -> list[str]:
    return [
        tunnel_client_bin,
        "run",
        "--profile-dir",
        str(profile_dir),
        "--profile",
        profile_name,
    ]


def tunnel_client_command(tunnel_client_bin: str, profile_name: str, profile_dir: Path) -> str:
    return " ".join(
        shlex.quote(part)
        for part in tunnel_client_args(tunnel_client_bin, profile_name, profile_dir)
    )


def resolve_tunnel_client_bin(requested: str | None) -> str:
    configured = (requested or "").strip() or os.environ.get("TUNNEL_CLIENT_BIN", "").strip()
    if configured:
        return str(Path(configured).expanduser())

    hinted = read_tunnel_client_bin_hint()
    if hinted is not None:
        return str(hinted)
    for candidate in _local_tunnel_client_candidates():
        if _is_executable(candidate):
            return str(candidate.resolve())

    on_path = shutil.which("tunnel-client")
    if on_path:
        return on_path
    return "tunnel-client"


def plugin_root() -> Path:
    return Path(__file__).resolve().parents[2]


def tunnel_client_bin_hint_path(root: Path | None = None) -> Path:
    return (root or plugin_root()) / _BIN_HINT_FILENAME


def read_tunnel_client_bin_hint(root: Path | None = None) -> Path | None:
    hint_path = tunnel_client_bin_hint_path(root)
    if not hint_path.is_file():
        return None
    value = hint_path.read_text(encoding="utf-8").strip()
    if not value:
        return None
    candidate = Path(value).expanduser()
    if _is_executable(candidate):
        return candidate.resolve()
    return None


def missing_tunnel_client_message(configured: str) -> str:
    selected = configured.strip()
    location = f" at {selected}" if selected else ""
    return (
        f"tunnel-client binary not found{location}; set --tunnel-client-bin /path/to/tunnel-client "
        "or TUNNEL_CLIENT_BIN=/path/to/tunnel-client. If you are in a tunnel-client source "
        "checkout, build one with `go build -o bin/tunnel-client ./cmd/client` from the "
        "tunnel-client module and rerun the command."
    )


def start_or_reuse(
    alias: str,
    profile_name: str,
    profile_dir: Path,
    tunnel_client_bin: str,
    state_root: Path,
    runner: Runner,
    popen_factory: PopenFactory,
    env_overrides: dict[str, str] | None = None,
    existing_pid: int = 0,
    replace_existing: bool = False,
) -> LaunchResult:
    profile_name = normalize_profile_name(profile_name, alias=alias)
    command = tunnel_client_command(tunnel_client_bin, profile_name, profile_dir)
    session = tmux_session_name(alias, state_root)
    health_file = profile_health_url_file(alias, state_root)
    if tmux_available(runner):
        if tmux_has_session_name(session, runner):
            if replace_existing:
                result = stop_tmux(runner, session_name=session)
                if result.returncode != 0:
                    stderr = (result.stderr or "").strip()
                    stdout = (result.stdout or "").strip()
                    raise RuntimeError(
                        stderr
                        or stdout
                        or f"tmux kill-session failed with exit {result.returncode}"
                    )
            else:
                observation = wait_for_runtime_health(
                    alias,
                    state_root,
                    runner,
                    mode="tmux",
                    session_name=session,
                )
                return LaunchResult(
                    mode="tmux",
                    command=command,
                    launched=False,
                    started=observation.healthy,
                    running=observation.running,
                    healthy=observation.healthy,
                    ready=observation.ready,
                    already_running=True,
                    health_url=observation.health_url,
                    session_name=session,
                )
        _clear_health_url_file(health_file)
        result = start_tmux(
            alias,
            profile_name,
            profile_dir,
            tunnel_client_bin,
            state_root,
            runner,
            env_overrides=env_overrides,
        )
        if result.returncode != 0:
            stderr = (result.stderr or "").strip()
            stdout = (result.stdout or "").strip()
            raise RuntimeError(
                stderr or stdout or f"tmux new-session failed with exit {result.returncode}"
            )
        observation = wait_for_runtime_health(
            alias,
            state_root,
            runner,
            mode="tmux",
            session_name=session,
        )
        return LaunchResult(
            mode="tmux",
            command=command,
            launched=True,
            started=observation.healthy,
            running=observation.running,
            healthy=observation.healthy,
            ready=observation.ready,
            already_running=False,
            health_url=observation.health_url,
            session_name=session,
        )

    if existing_pid and pid_is_running(existing_pid):
        if replace_existing:
            terminate_process(existing_pid)
        else:
            observation = wait_for_runtime_health(
                alias,
                state_root,
                runner,
                mode="process",
                pid=existing_pid,
            )
            return LaunchResult(
                mode="process",
                command=command,
                launched=False,
                started=observation.healthy,
                running=observation.running,
                healthy=observation.healthy,
                ready=observation.ready,
                already_running=True,
                health_url=observation.health_url,
                pid=existing_pid,
                log_path=str(log_path(alias, state_root)),
            )

    _clear_health_url_file(health_file)
    process = start_background_process(
        alias,
        profile_name,
        profile_dir,
        tunnel_client_bin,
        state_root,
        popen_factory,
        env_overrides=env_overrides,
    )
    log_output_path = str(log_path(alias, state_root))
    exit_code = _exit_code_after_launch(process)
    if exit_code is not None:
        return LaunchResult(
            mode="process",
            command=command,
            launched=True,
            started=False,
            running=False,
            healthy=False,
            ready=False,
            already_running=False,
            pid=int(process.pid),
            log_path=log_output_path,
            exit_code=exit_code,
        )
    observation = wait_for_runtime_health(
        alias,
        state_root,
        runner,
        mode="process",
        pid=int(process.pid),
    )
    final_exit_code = process.poll()
    return LaunchResult(
        mode="process",
        command=command,
        launched=True,
        started=observation.healthy,
        running=observation.running,
        healthy=observation.healthy,
        ready=observation.ready,
        already_running=False,
        health_url=observation.health_url,
        pid=int(process.pid),
        log_path=log_output_path,
        exit_code=None if final_exit_code is None else int(final_exit_code),
    )


def tmux_available(runner: Runner) -> bool:
    try:
        result = runner(["tmux", "-V"], check=False, capture_output=True, text=True)
    except FileNotFoundError:
        return False
    return result.returncode == 0


def tmux_has_session(alias: str, runner: Runner, *, state_root: Path | None = None) -> bool:
    return tmux_has_session_name(tmux_session_name(alias, state_root), runner)


def tmux_has_session_name(session: str, runner: Runner) -> bool:
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
    alias: str,
    profile_name: str,
    profile_dir: Path,
    tunnel_client_bin: str,
    state_root: Path,
    runner: Runner,
    *,
    env_overrides: dict[str, str] | None = None,
) -> subprocess.CompletedProcess[str]:
    session = tmux_session_name(alias, state_root)
    command = tunnel_client_command(tunnel_client_bin, profile_name, profile_dir)
    args = ["tmux", "new-session", "-d"]
    for name, value in sorted((env_overrides or {}).items()):
        args.extend(["-e", f"{name}={value}"])
    args.extend(["-s", session, command])
    return runner(
        args,
        check=False,
        capture_output=True,
        text=True,
        env=_child_env(env_overrides),
    )


def stop_tmux(
    runner: Runner,
    *,
    alias: str | None = None,
    state_root: Path | None = None,
    session_name: str | None = None,
) -> subprocess.CompletedProcess[str]:
    session = (session_name or "").strip()
    if not session:
        if alias is None:
            raise ValueError("alias or session_name is required")
        session = tmux_session_name(alias, state_root)
    return runner(
        ["tmux", "kill-session", "-t", f"={session}"],
        check=False,
        capture_output=True,
        text=True,
    )


def _local_tunnel_client_candidates() -> list[Path]:
    candidates: list[Path] = []
    seen: set[str] = set()
    for root in [Path.cwd(), plugin_root(), *plugin_root().parents]:
        for candidate in _candidate_paths_for_root(root):
            key = str(candidate)
            if key in seen:
                continue
            seen.add(key)
            candidates.append(candidate)
    return candidates


def _candidate_paths_for_root(root: Path) -> list[Path]:
    return [
        root / "tunnel-client",
        root / "bin" / "tunnel-client",
        root / "bazel-bin" / "cmd" / "client" / "client",
        root / "bazel-bin" / "api" / "tunnel-client" / "cmd" / "client" / "client",
    ]


def _is_executable(path: Path) -> bool:
    return path.is_file() and os.access(path, os.X_OK)


def log_path(alias: str, state_root: Path) -> Path:
    return state.ensure_state_dirs(state_root) / "logs" / f"{state.normalize_alias(alias)}.log"


def start_background_process(
    alias: str,
    profile_name: str,
    profile_dir: Path,
    tunnel_client_bin: str,
    state_root: Path,
    popen_factory: PopenFactory,
    *,
    env_overrides: dict[str, str] | None = None,
) -> subprocess.Popen[bytes]:
    output_path = log_path(alias, state_root)
    output_path.parent.mkdir(parents=True, exist_ok=True)
    log_file = output_path.open("ab")
    try:
        return popen_factory(
            tunnel_client_args(tunnel_client_bin, profile_name, profile_dir),
            stdin=subprocess.DEVNULL,
            stdout=log_file,
            stderr=subprocess.STDOUT,
            close_fds=True,
            start_new_session=True,
            env=_child_env(env_overrides),
        )
    finally:
        log_file.close()


def _exit_code_after_launch(process: subprocess.Popen[bytes]) -> int | None:
    poll = getattr(process, "poll", None)
    if poll is None:
        return None

    deadline = time.monotonic() + _LAUNCH_SETTLE_SECONDS
    while True:
        exit_code = poll()
        if exit_code is not None:
            return int(exit_code)
        if time.monotonic() >= deadline:
            return None
        time.sleep(0.01)


def _tmux_running_after_launch(session_name: str, runner: Runner) -> bool:
    deadline = time.monotonic() + _LAUNCH_SETTLE_SECONDS
    while True:
        if not tmux_has_session_name(session_name, runner):
            return False
        if time.monotonic() >= deadline:
            return True
        time.sleep(0.01)


def wait_for_runtime_health(
    alias: str,
    state_root: Path,
    runner: Runner,
    *,
    mode: str,
    pid: int = 0,
    session_name: str = "",
) -> RuntimeObservation:
    health_file = profile_health_url_file(alias, state_root)
    deadline = time.monotonic() + _LAUNCH_HEALTH_TIMEOUT_SECONDS
    while True:
        running = _runtime_is_running(
            alias,
            runner,
            mode=mode,
            pid=pid,
            session_name=session_name,
            state_root=state_root,
        )
        raw_health_url = read_health_url(health_file)
        probe = probe_health_endpoints(raw_health_url)
        observation = RuntimeObservation(
            running=running,
            health_url=probe.healthz.url,
            healthy=probe.healthz.ok,
            ready=probe.readyz.ok,
            health_probe=probe,
        )
        if observation.healthy:
            return observation
        if not running:
            return observation
        if time.monotonic() >= deadline:
            return observation
        time.sleep(_LAUNCH_HEALTH_POLL_SECONDS)


def read_health_url(path: Path) -> str:
    if not path.exists() or not path.is_file():
        return ""
    return path.read_text(encoding="utf-8").strip()


def probe_health_endpoints(raw_health_url: str) -> HealthProbe:
    base_url = normalize_health_base_url(raw_health_url)
    if not base_url:
        return HealthProbe()
    healthz_url = base_url.rstrip("/") + "/healthz"
    readyz_url = base_url.rstrip("/") + "/readyz"
    return HealthProbe(
        base_url=base_url,
        healthz=_probe_endpoint(healthz_url),
        readyz=_probe_endpoint(readyz_url),
    )


def normalize_health_base_url(raw_health_url: str) -> str:
    value = (raw_health_url or "").strip()
    if not value:
        return ""
    parsed = urlparse.urlsplit(value)
    path = parsed.path or ""
    if path.endswith("/healthz"):
        path = path[: -len("/healthz")]
    elif path.endswith("/readyz"):
        path = path[: -len("/readyz")]
    return urlparse.urlunsplit(
        (parsed.scheme, parsed.netloc, path.rstrip("/"), "", ""),
    )


def _probe_endpoint(endpoint: str) -> EndpointProbe:
    try:
        with urlrequest.urlopen(endpoint, timeout=_HEALTH_PROBE_TIMEOUT_SECONDS) as response:
            status = int(getattr(response, "status", 0) or 0)
            body = response.read().decode("utf-8", errors="replace").strip()
            return EndpointProbe(
                url=endpoint,
                ok=200 <= status < 300,
                status=status,
                body=body,
            )
    except urlerror.HTTPError as exc:
        body = exc.read().decode("utf-8", errors="replace").strip()
        return EndpointProbe(
            url=endpoint,
            ok=False,
            status=int(exc.code),
            body=body,
            error=str(exc),
        )
    except (urlerror.URLError, TimeoutError, OSError) as exc:
        return EndpointProbe(url=endpoint, ok=False, error=str(exc))


def _child_env(env_overrides: dict[str, str] | None = None) -> dict[str, str] | None:
    if not env_overrides:
        return None
    env = os.environ.copy()
    env.update(env_overrides)
    return env


def _runtime_is_running(
    alias: str,
    runner: Runner,
    *,
    mode: str,
    pid: int = 0,
    session_name: str = "",
    state_root: Path | None = None,
) -> bool:
    if mode == "tmux":
        if session_name:
            return tmux_has_session_name(session_name, runner)
        return tmux_has_session(alias, runner, state_root=state_root)
    if mode == "process":
        return pid_is_running(pid)
    return False


def _clear_health_url_file(path: Path) -> None:
    try:
        path.unlink(missing_ok=True)
    except OSError:
        return


def clear_health_url_file(alias: str, state_root: Path) -> None:
    _clear_health_url_file(profile_health_url_file(alias, state_root))


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


def wait_for_process_exit(pid: int, timeout_seconds: float = _TERMINATE_WAIT_SECONDS) -> bool:
    deadline = time.monotonic() + timeout_seconds
    while True:
        if not pid_is_running(pid):
            return True
        if time.monotonic() >= deadline:
            return False
        time.sleep(0.05)


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
