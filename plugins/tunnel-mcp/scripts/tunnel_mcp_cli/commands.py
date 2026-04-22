from __future__ import annotations

import argparse
import dataclasses
import json
import os
import shlex
import subprocess
import sys
from pathlib import Path
from typing import Any, Callable
from urllib.parse import urlparse

from tunnel_mcp_cli import runtime, state

Runner = Callable[..., subprocess.CompletedProcess[str]]
# citadel-ignore: public endpoint example for external tunnel-client config
DEFAULT_BASE_URL = "https://api.openai.com"
DEFAULT_ADMIN_PROFILE = "default"
DEFAULT_ADMIN_KEY_REF = "env:OPENAI_ADMIN_KEY"
DEFAULT_RUNTIME_API_KEY_REF = "env:CONTROL_PLANE_API_KEY"


class TunnelMCPError(RuntimeError):
    pass


class RemoteError(TunnelMCPError):
    def __init__(self, message: str, *, returncode: int):
        super().__init__(message)
        self.returncode = returncode


@dataclasses.dataclass(frozen=True)
class EffectiveAdminProfile:
    name: str
    control_plane_base_url: str
    admin_key: str
    path: str


def main(
    argv: list[str] | None = None,
    *,
    runner: Runner = subprocess.run,
    popen_factory: runtime.PopenFactory = subprocess.Popen,
) -> int:
    parser = _parser()
    args = parser.parse_args(argv)
    args.tunnel_client_bin = runtime.resolve_tunnel_client_bin(args.tunnel_client_bin)
    try:
        if args.command == "create":
            payload = _create(args, runner)
        elif args.command == "connect":
            payload = _connect(args, runner, popen_factory)
        elif args.command == "list":
            payload = _list(args, runner)
        elif args.command == "status":
            payload = _status(args, runner)
        elif args.command in {"stop", "disconnect"}:
            payload = _stop(args, runner)
        else:
            parser.print_help()
            return 2
    except state.StateError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1
    except TunnelMCPError as exc:
        if args.command in {"connect", "status"} and str(exc).startswith("{"):
            print(str(exc))
            return 2
        print(f"error: {exc}", file=sys.stderr)
        return 1
    except FileNotFoundError:
        print(
            f"error: {runtime.missing_tunnel_client_message(args.tunnel_client_bin)}",
            file=sys.stderr,
        )
        return 1

    print(json.dumps(payload, indent=2, sort_keys=True))
    return 0


def _create(args: argparse.Namespace, runner: Runner) -> dict[str, Any]:
    alias = state.normalize_alias(args.alias)
    root = state.ensure_state_dirs()
    previous_alias = state.load_aliases(root).get(alias)
    admin_profile = _resolve_admin_profile(
        args,
        root,
        default_profile_name=previous_alias.admin_profile if previous_alias else "",
    )
    tunnel = _resolve_tunnel(
        alias,
        args,
        runner,
        admin_profile=admin_profile,
        root=root,
        create_if_missing=True,
    )
    aliases = state.load_aliases(root)
    aliases[alias] = state.alias_record_from_tunnel(
        alias=alias,
        tunnel=tunnel,
        admin_profile=admin_profile.name,
        description=args.description or _default_description(alias),
    )
    state.save_aliases(aliases, root)
    state.append_history(
        "create",
        alias,
        tunnel["id"],
        f"name={tunnel.get('name', alias)} admin_profile={admin_profile.name}",
        root,
    )
    return {
        "alias": alias,
        "tunnel": tunnel,
        "admin_profile": admin_profile.name,
        "admin_profile_path": admin_profile.path,
        "state_root": str(root),
    }


def _connect(
    args: argparse.Namespace, runner: Runner, popen_factory: runtime.PopenFactory
) -> dict[str, Any]:
    alias = state.normalize_alias(args.alias)
    profile_name = runtime.normalize_profile_name(args.profile, alias=alias)
    profile_dir = runtime.resolve_profile_dir(getattr(args, "profile_dir", ""))
    root = state.ensure_state_dirs()
    target = _target_from_args(args)
    runtime_api_key = _runtime_api_key_ref(args)
    launch_env_overrides = _runtime_launch_env_overrides(runtime_api_key)
    previous_alias = state.load_aliases(root).get(alias)
    admin_profile = _resolve_admin_profile(
        args,
        root,
        default_profile_name=previous_alias.admin_profile if previous_alias else "",
    )
    remote_error = ""
    if args.tunnel_id:
        try:
            tunnel = _remote_get(
                _validated_tunnel_id(args.tunnel_id),
                args,
                runner,
                admin_profile,
                key_ref=runtime_api_key,
            )
        except RemoteError as exc:
            tunnel = _provided_tunnel(alias, args)
            remote_error = str(exc)
    else:
        try:
            tunnel = _resolve_tunnel(
                alias,
                args,
                runner,
                admin_profile=admin_profile,
                root=root,
                create_if_missing=True,
            )
        except RemoteError as exc:
            if previous_alias is None or not previous_alias.tunnel_id or _is_not_found(exc):
                raise
            tunnel = _local_tunnel_from_alias(previous_alias)
            remote_error = str(exc)
    tunnel_id = str(tunnel["id"])
    replace_existing_runtime = bool(
        previous_alias and previous_alias.tunnel_id and previous_alias.tunnel_id != tunnel_id
    )

    profile_path = runtime.write_runtime_profile(
        alias=alias,
        profile_name=profile_name,
        tunnel_id=tunnel_id,
        base_url=admin_profile.control_plane_base_url,
        api_key=runtime_api_key,
        target=target,
        profile_dir=profile_dir,
        state_root=root,
    )
    health_url_file = runtime.profile_health_url_file(alias, root)

    aliases = state.load_aliases(root)
    aliases[alias] = state.alias_record_from_tunnel(
        alias=alias,
        tunnel=tunnel,
        admin_profile=admin_profile.name,
        description=args.description or _default_description(alias),
        config_path=str(profile_path),
        profile_name=profile_name,
        profile_dir=str(profile_dir),
        profile_path=str(profile_path),
        health_url_file=str(health_url_file),
    )
    state.save_aliases(aliases, root)

    processes = state.load_processes(root)
    existing_process = processes.get(alias)
    if replace_existing_runtime and existing_process:
        state.append_history(
            "stale-process",
            alias,
            existing_process.tunnel_id,
            f"replacing with tunnel_id={tunnel_id}",
            root,
        )
    try:
        launch = runtime.start_or_reuse(
            alias=alias,
            profile_name=profile_name,
            profile_dir=profile_dir,
            tunnel_client_bin=args.tunnel_client_bin,
            state_root=root,
            runner=runner,
            popen_factory=popen_factory,
            env_overrides=launch_env_overrides,
            existing_pid=existing_process.pid if existing_process else 0,
            replace_existing=replace_existing_runtime,
        )
    except RuntimeError as exc:
        raise TunnelMCPError(str(exc)) from exc
    except FileNotFoundError as exc:
        raise TunnelMCPError(runtime.missing_tunnel_client_message(args.tunnel_client_bin)) from exc

    processes[alias] = state.ProcessRecord(
        alias=alias,
        tunnel_id=tunnel["id"],
        admin_profile=admin_profile.name,
        config_path=str(profile_path),
        profile_name=profile_name,
        profile_dir=str(profile_dir),
        profile_path=str(profile_path),
        health_url_file=str(health_url_file),
        target_kind=target.kind,
        target_value=target.value,
        command=launch.command,
        started_at=state.utc_now(),
        mode=launch.mode,
        session_name=launch.session_name,
        pid=launch.pid,
        log_path=launch.log_path,
    )
    state.save_processes(processes, root)
    state.append_history(
        "connect",
        alias,
        tunnel["id"],
        (
            f"mode={launch.mode} session={launch.session_name or '-'} "
            f"pid={launch.pid or '-'} started={launch.started} healthy={launch.healthy} ready={launch.ready}"
        ),
        root,
    )

    payload = _connect_payload(
        alias=alias,
        tunnel=tunnel,
        admin_profile=admin_profile,
        record=aliases[alias],
        process=processes[alias],
        launch=launch,
        runner=runner,
        remote_error=remote_error,
    )
    if not launch.healthy:
        raise TunnelMCPError(json.dumps(payload, indent=2, sort_keys=True))
    return payload


def _list(args: argparse.Namespace, runner: Runner) -> dict[str, Any]:
    root = state.ensure_state_dirs()
    admin_profile = _resolve_admin_profile(args, root)
    aliases = state.load_aliases(root)
    local_aliases = [record.to_dict() for _, record in sorted(aliases.items())]

    payload: dict[str, Any] = {
        "aliases": local_aliases,
        "admin_profile": admin_profile.name,
        "admin_profile_path": admin_profile.path,
        "state_root": str(root),
    }
    if _has_remote_scope(args):
        remote = _remote_list(args, runner, admin_profile)
        by_tunnel_id = {record.tunnel_id: record.alias for record in aliases.values()}
        by_tunnel_admin_profile = {
            record.tunnel_id: record.admin_profile for record in aliases.values()
        }
        merged = []
        for tunnel in remote:
            item = dict(tunnel)
            tunnel_id = str(tunnel.get("id", ""))
            item["local_alias"] = by_tunnel_id.get(tunnel_id)
            item["local_admin_profile"] = by_tunnel_admin_profile.get(tunnel_id)
            merged.append(item)
        payload["remote_tunnels"] = merged
    return payload


def _status(args: argparse.Namespace, runner: Runner) -> dict[str, Any]:
    alias = state.normalize_alias(args.alias)
    root = state.ensure_state_dirs()
    aliases = state.load_aliases(root)
    processes = state.load_processes(root)
    record = aliases.get(alias)
    if record is None:
        raise TunnelMCPError(f"alias {alias} is not known; run create or connect first")
    admin_profile = _resolve_admin_profile(args, root, default_profile_name=record.admin_profile)
    process = processes.get(alias)
    remote = None
    stale = False
    error = ""
    repair_command = _repair_command(alias, record, process)
    remote_lookup_attempted = False
    remote_lookup_auth_kind = ""
    remote_lookup_auth_ref = ""
    remote_skipped_reason = ""

    key_ref, auth_kind = _status_read_only_key_ref(record, process, admin_profile)
    if key_ref:
        available, reason = _secret_reference_available(key_ref)
        if available:
            remote_lookup_attempted = True
            remote_lookup_auth_kind = auth_kind
            remote_lookup_auth_ref = key_ref
            try:
                remote = _remote_get(record.tunnel_id, args, runner, admin_profile, key_ref=key_ref)
            except RemoteError as exc:
                error = str(exc)
                stale = _is_not_found(exc)
        else:
            remote_skipped_reason = reason
    else:
        remote_skipped_reason = _status_remote_lookup_skipped_reason(record, process, admin_profile)

    payload = _status_payload(
        alias=alias,
        record=record,
        process=process,
        admin_profile=admin_profile,
        runner=runner,
        remote=remote,
        stale=stale,
        error=error,
        repair_command=repair_command,
        remote_lookup_attempted=remote_lookup_attempted,
        remote_lookup_auth_kind=remote_lookup_auth_kind,
        remote_lookup_auth_ref=remote_lookup_auth_ref,
        remote_skipped_reason=remote_skipped_reason,
    )
    if stale and process is None:
        raise TunnelMCPError(json.dumps(payload, indent=2, sort_keys=True))
    return payload


def _stop(args: argparse.Namespace, runner: Runner) -> dict[str, Any]:
    alias = state.normalize_alias(args.alias)
    root = state.ensure_state_dirs()
    aliases = state.load_aliases(root)
    processes = state.load_processes(root)
    record = aliases.get(alias)
    if record is None:
        raise TunnelMCPError(f"alias {alias} is not known; run create or connect first")
    admin_profile = _resolve_admin_profile(args, root, default_profile_name=record.admin_profile)
    process = processes.get(alias)
    already_stopped = False
    stop_error = ""
    previous_mode = process.mode if process else ""

    if process is None or process.mode == "stopped":
        already_stopped = True
    elif process.mode == "tmux":
        session_name = process.session_name or runtime.tmux_session_name(alias, root)
        if runtime.tmux_has_session_name(session_name, runner):
            result = runtime.stop_tmux(runner, session_name=session_name)
            if result.returncode != 0:
                stop_error = (result.stderr or result.stdout or "").strip() or (
                    f"tmux kill-session failed with exit {result.returncode}"
                )
        else:
            already_stopped = True
    elif process.mode == "process" and process.pid:
        try:
            runtime.terminate_process(process.pid)
        except RuntimeError as exc:
            stop_error = str(exc)
        if not stop_error and not runtime.wait_for_process_exit(process.pid):
            stop_error = f"process {process.pid} did not exit after SIGTERM"
    else:
        already_stopped = True

    runtime.clear_health_url_file(alias, root)
    if process is not None:
        processes[alias] = state.ProcessRecord(
            alias=process.alias,
            tunnel_id=process.tunnel_id,
            admin_profile=process.admin_profile,
            mode="stopped",
            session_name="",
            pid=0,
            config_path=process.config_path,
            profile_name=process.profile_name,
            profile_dir=process.profile_dir,
            profile_path=process.profile_path,
            health_url_file=process.health_url_file,
            target_kind=process.target_kind,
            target_value=process.target_value,
            command=process.command,
            log_path=process.log_path,
            started_at=process.started_at,
        )
        state.save_processes(processes, root)

    detail = f"previous_mode={previous_mode or '-'} already_stopped={already_stopped}"
    if stop_error:
        detail += f" error={stop_error}"
    state.append_history("stop", alias, record.tunnel_id, detail, root)

    payload = _status_payload(
        alias=alias,
        record=record,
        process=processes.get(alias),
        admin_profile=admin_profile,
        runner=runner,
        remote=None,
        stale=False,
        error=stop_error,
        repair_command=_repair_command(alias, record, processes.get(alias)),
        remote_lookup_attempted=False,
        remote_lookup_auth_kind="",
        remote_lookup_auth_ref="",
        remote_skipped_reason="stop is a local-only operation",
    )
    payload["already_stopped"] = already_stopped
    payload["stopped"] = stop_error == ""
    payload["stop_error"] = stop_error
    if stop_error:
        raise TunnelMCPError(json.dumps(payload, indent=2, sort_keys=True))
    return payload


def _connect_payload(
    *,
    alias: str,
    tunnel: dict[str, Any],
    admin_profile: EffectiveAdminProfile,
    record: state.AliasRecord,
    process: state.ProcessRecord,
    launch: runtime.LaunchResult,
    runner: Runner,
    remote_error: str,
) -> dict[str, Any]:
    local = _local_runtime_details(alias, record, process, runner)
    payload = {
        "alias": alias,
        "tunnel": tunnel,
        "admin_profile": admin_profile.name,
        "admin_profile_path": admin_profile.path,
        "profile_name": record.profile_name,
        "profile_dir": record.profile_dir,
        "profile_path": record.profile_path,
        "profile_exists": local["profile"]["exists"],
        "config_path": record.config_path,
        "health_url_file": record.health_url_file,
        "health_url": local["health"]["url"],
        "ui_url": local["health"]["ui"],
        "runtime_state": local["runtime_state"],
        "healthy": launch.healthy,
        "ready": launch.ready,
        "launched": launch.launched,
        "mode": launch.mode,
        "command": launch.command,
        "session_name": launch.session_name,
        "pid": launch.pid,
        "log_path": launch.log_path,
        "started": launch.started,
        "running": launch.running,
        "already_running": launch.already_running,
        "exit_code": launch.exit_code,
        "remote_error": remote_error,
        "tmux": local["tmux"],
        "process_running": local["process_running"],
        "process": process.to_dict(),
        "local": local,
        "next_steps": _doctor_next_steps(
            profile_name=record.profile_name,
            profile_dir=record.profile_dir,
            config_path=record.config_path,
        ),
    }
    if remote_error:
        payload["remote"] = None
    return payload


def _status_payload(
    *,
    alias: str,
    record: state.AliasRecord,
    process: state.ProcessRecord | None,
    admin_profile: EffectiveAdminProfile,
    runner: Runner,
    remote: dict[str, Any] | None,
    stale: bool,
    error: str,
    repair_command: str,
    remote_lookup_attempted: bool,
    remote_lookup_auth_kind: str,
    remote_lookup_auth_ref: str,
    remote_skipped_reason: str,
) -> dict[str, Any]:
    local = _local_runtime_details(alias, record, process, runner)
    return {
        "alias": alias,
        "tunnel_id": record.tunnel_id,
        "admin_profile": admin_profile.name,
        "admin_profile_path": admin_profile.path,
        "remote": remote,
        "stale": stale,
        "error": error,
        "remote_error": error,
        "repair_command": repair_command,
        "config_path": record.config_path,
        "profile_name": record.profile_name,
        "profile_dir": record.profile_dir,
        "profile_path": record.profile_path,
        "profile_exists": local["profile"]["exists"],
        "health_url_file": record.health_url_file,
        "health_url": local["health"]["url"],
        "ui_url": local["health"]["ui"],
        "runtime_state": local["runtime_state"],
        "healthy": local["health"]["healthz"]["ok"],
        "ready": local["health"]["readyz"]["ok"],
        "remote_lookup_attempted": remote_lookup_attempted,
        "remote_lookup_auth_kind": remote_lookup_auth_kind,
        "remote_lookup_auth_ref": remote_lookup_auth_ref,
        "remote_skipped_reason": remote_skipped_reason,
        "tmux": local["tmux"],
        "process_running": local["process_running"],
        "process": process.to_dict() if process else None,
        "local": local,
        "next_steps": _status_next_steps(
            alias=alias,
            profile_name=record.profile_name,
            profile_dir=record.profile_dir,
            config_path=record.config_path,
            repair_command=repair_command,
        ),
    }


def _local_runtime_details(
    alias: str,
    record: state.AliasRecord,
    process: state.ProcessRecord | None,
    runner: Runner,
) -> dict[str, Any]:
    root = state.ensure_state_dirs()
    health_url_file = (
        process.health_url_file if process and process.health_url_file else record.health_url_file
    )
    profile_name = process.profile_name if process and process.profile_name else record.profile_name
    profile_dir = process.profile_dir if process and process.profile_dir else record.profile_dir
    profile_path = process.profile_path if process and process.profile_path else record.profile_path
    config_path = process.config_path if process and process.config_path else record.config_path
    log_path = process.log_path if process and process.log_path else ""

    health = _path_details(health_url_file)
    if health["exists"]:
        raw_health_url = Path(health_url_file).read_text(encoding="utf-8").strip()
    else:
        raw_health_url = ""
    probe = runtime.probe_health_endpoints(raw_health_url)
    health["raw_url"] = raw_health_url
    health["base_url"] = probe.base_url
    health["url"] = probe.healthz.url
    health["ui"] = probe.base_url.rstrip("/") + "/ui" if probe.base_url else ""
    health["healthz"] = _endpoint_probe_details(probe.healthz)
    health["readyz"] = _endpoint_probe_details(probe.readyz)

    profile = _path_details(profile_path)
    profile["name"] = profile_name
    profile["dir"] = profile_dir
    profile["config_path"] = config_path
    log = _path_details(log_path)
    log["tail"] = _read_log_tail(log_path)

    tmux_session = (
        process.session_name
        if process and process.session_name
        else runtime.tmux_session_name(alias, root)
    )
    tmux_running = runtime.tmux_has_session_name(tmux_session, runner)
    process_running = runtime.pid_is_running(process.pid) if process and process.pid else False
    runtime_running = tmux_running or process_running

    return {
        "runtime_state": _runtime_state(runtime_running, probe),
        "issues": _local_issues(
            process=process,
            tmux_running=tmux_running,
            process_running=process_running,
            profile_exists=profile["exists"],
            health=health,
            log_exists=log["exists"],
        ),
        "profile": profile,
        "health": health,
        "log": log,
        "tmux": {
            "session_name": tmux_session,
            "running": tmux_running,
        },
        "process_running": process_running,
    }


def _local_issues(
    *,
    process: state.ProcessRecord | None,
    tmux_running: bool,
    process_running: bool,
    profile_exists: bool,
    health: dict[str, Any],
    log_exists: bool,
) -> list[str]:
    issues: list[str] = []
    if process and process.mode == "tmux" and process.session_name and not tmux_running:
        issues.append("recorded tmux session is not running")
    if process and process.mode == "process" and process.pid and not process_running:
        issues.append("recorded process pid is not running")
    if process and process.profile_path and not profile_exists:
        issues.append("recorded runtime profile is missing")
    if process and process.health_url_file and not health["raw_url"]:
        issues.append("health URL file has not been populated")
    if process and health["url"] and not health["healthz"]["ok"]:
        issue = f"health endpoint is not healthy at {health['url']}"
        if health["healthz"]["status"]:
            issue += f" (HTTP {health['healthz']['status']})"
        elif health["healthz"]["error"]:
            issue += f" ({health['healthz']['error']})"
        issues.append(issue)
    if process and health["readyz"]["url"] and not health["readyz"]["ok"]:
        issue = f"ready endpoint is not ready at {health['readyz']['url']}"
        if health["readyz"]["status"]:
            issue += f" (HTTP {health['readyz']['status']})"
        elif health["readyz"]["error"]:
            issue += f" ({health['readyz']['error']})"
        elif health["readyz"]["body"]:
            issue += f" ({health['readyz']['body']})"
        issues.append(issue)
    if process and process.log_path and not (tmux_running or process_running) and log_exists:
        issues.append("runtime log exists but no active runtime is running")
    return issues


def _runtime_state(runtime_running: bool, probe: runtime.HealthProbe) -> str:
    if not runtime_running:
        return "stopped"
    if probe.healthz.ok:
        if probe.readyz.ok:
            return "ready"
        return "healthy"
    return "starting"


def _endpoint_probe_details(probe: runtime.EndpointProbe) -> dict[str, Any]:
    return {
        "url": probe.url,
        "ok": probe.ok,
        "status": probe.status,
        "body": probe.body,
        "error": probe.error,
    }


def _path_details(path_value: str) -> dict[str, Any]:
    if not path_value:
        return {
            "path": "",
            "exists": False,
            "size_bytes": 0,
        }
    path = Path(path_value)
    exists = path.exists()
    size_bytes = path.stat().st_size if exists and path.is_file() else 0
    return {
        "path": path_value,
        "exists": exists,
        "size_bytes": size_bytes,
    }


def _read_log_tail(path_value: str, *, max_lines: int = 20) -> str:
    if not path_value:
        return ""
    path = Path(path_value)
    if not path.exists() or not path.is_file():
        return ""
    lines = path.read_text(encoding="utf-8", errors="replace").splitlines()
    return "\n".join(lines[-max_lines:])


def _resolve_tunnel(
    alias: str,
    args: argparse.Namespace,
    runner: Runner,
    *,
    admin_profile: EffectiveAdminProfile,
    root: Path,
    create_if_missing: bool,
) -> dict[str, Any]:
    aliases = state.load_aliases(root)
    existing = aliases.get(alias)
    if existing and existing.tunnel_id:
        existing_admin_profile = _resolve_admin_profile(
            args, root, default_profile_name=existing.admin_profile
        )
        try:
            return _remote_get(existing.tunnel_id, args, runner, existing_admin_profile)
        except RemoteError as exc:
            if not _is_not_found(exc):
                raise
            state.append_history("stale-alias", alias, existing.tunnel_id, str(exc), root)
            aliases.pop(alias, None)
            state.save_aliases(aliases, root)

    scoped_remote = _find_matching_remote(alias, args, runner, admin_profile)
    if scoped_remote is not None:
        return scoped_remote

    if not create_if_missing:
        raise TunnelMCPError(f"alias {alias} is not known")
    if not args.organization_ids and not args.workspace_ids:
        raise TunnelMCPError("creating a tunnel requires --organization-id or --workspace-id")
    return _remote_create(alias, args, runner, admin_profile)


def _find_matching_remote(
    alias: str, args: argparse.Namespace, runner: Runner, admin_profile: EffectiveAdminProfile
) -> dict[str, Any] | None:
    if not _has_remote_scope(args):
        return None
    desired_name = args.name or alias
    try:
        for tunnel in _remote_list_for_lookup(args, runner, admin_profile):
            if str(tunnel.get("name", "")) == desired_name:
                return tunnel
    except RemoteError as exc:
        if not _is_not_found(exc):
            raise
    return None


def _remote_get(
    tunnel_id: str,
    args: argparse.Namespace,
    runner: Runner,
    admin_profile: EffectiveAdminProfile,
    *,
    key_ref: str | None = None,
) -> dict[str, Any]:
    return _run_tunnel_client_json(
        args,
        runner,
        admin_profile,
        ["tunnels", "get", tunnel_id],
        key_ref=key_ref,
    )


def _remote_create(
    alias: str, args: argparse.Namespace, runner: Runner, admin_profile: EffectiveAdminProfile
) -> dict[str, Any]:
    name = args.name or alias
    description = args.description or _default_description(alias)
    command = ["tunnels", "create", "--name", name, "--description", description]
    command.extend(_scope_flags_for_create(args))
    return _run_tunnel_client_json(args, runner, admin_profile, command)


def _remote_list(
    args: argparse.Namespace, runner: Runner, admin_profile: EffectiveAdminProfile
) -> list[dict[str, Any]]:
    command = ["tunnels", "list"]
    command.extend(_single_scope_filter(args))
    return _remote_list_with_command(args, runner, admin_profile, command)


def _remote_list_for_lookup(
    args: argparse.Namespace, runner: Runner, admin_profile: EffectiveAdminProfile
) -> list[dict[str, Any]]:
    command = ["tunnels", "list"]
    command.extend(_first_scope_filter(args))
    return _remote_list_with_command(args, runner, admin_profile, command)


def _remote_list_with_command(
    args: argparse.Namespace,
    runner: Runner,
    admin_profile: EffectiveAdminProfile,
    command: list[str],
) -> list[dict[str, Any]]:
    raw = _run_tunnel_client_json(args, runner, admin_profile, command)
    tunnels = raw.get("tunnels", [])
    if not isinstance(tunnels, list):
        raise TunnelMCPError("tunnel-client list returned malformed tunnels payload")
    return [t for t in tunnels if isinstance(t, dict)]


def _run_tunnel_client_json(
    args: argparse.Namespace,
    runner: Runner,
    admin_profile: EffectiveAdminProfile,
    admin_args: list[str],
    *,
    key_ref: str | None = None,
) -> dict[str, Any]:
    subprocess_env = _admin_subprocess_env(
        admin_profile=admin_profile,
        key_ref=key_ref,
    )
    command = [
        args.tunnel_client_bin,
        "admin",
        "--control-plane.base-url",
        admin_profile.control_plane_base_url,
        "--json",
    ]
    command.extend(admin_args)

    try:
        result = runner(command, check=False, capture_output=True, text=True, env=subprocess_env)
    except FileNotFoundError as exc:
        raise TunnelMCPError(runtime.missing_tunnel_client_message(args.tunnel_client_bin)) from exc
    if result.returncode != 0:
        raise RemoteError(
            _failed_command("tunnel-client admin", result), returncode=result.returncode
        )
    try:
        payload = json.loads(result.stdout or "{}")
    except json.JSONDecodeError as exc:
        raise TunnelMCPError(f"tunnel-client returned invalid JSON: {exc}") from exc
    if not isinstance(payload, dict):
        raise TunnelMCPError("tunnel-client returned non-object JSON")
    return payload


def _admin_subprocess_env(
    *,
    admin_profile: EffectiveAdminProfile,
    key_ref: str | None,
) -> dict[str, str]:
    env = dict(os.environ)
    resolved_ref = (key_ref or admin_profile.admin_key).strip()
    secret_value = _resolve_secret_reference(resolved_ref)
    if key_ref and resolved_ref != admin_profile.admin_key:
        env["CONTROL_PLANE_API_KEY"] = secret_value
        env["OPENAI_API_KEY"] = secret_value
        env.pop("OPENAI_ADMIN_KEY", None)
        return env
    env["OPENAI_ADMIN_KEY"] = secret_value
    return env


def _scope_flags_for_create(args: argparse.Namespace) -> list[str]:
    flags: list[str] = []
    for org in args.organization_ids or []:
        flags.extend(["--organization-id", org])
    for workspace in args.workspace_ids or []:
        flags.extend(["--workspace-id", workspace])
    return flags


def _single_scope_filter(args: argparse.Namespace) -> list[str]:
    provided = [
        ("--organization-id", (args.organization_ids or [None])[0]),
        ("--workspace-id", (args.workspace_ids or [None])[0]),
        ("--tenant-id", args.tenant_id),
    ]
    non_empty = [(flag, value) for flag, value in provided if value]
    if len(non_empty) != 1:
        raise TunnelMCPError(
            "remote list requires exactly one of --organization-id, --workspace-id, or --tenant-id"
        )
    flag, value = non_empty[0]
    return [flag, value]


def _first_scope_filter(args: argparse.Namespace) -> list[str]:
    if args.organization_ids:
        return ["--organization-id", args.organization_ids[0]]
    if args.workspace_ids:
        return ["--workspace-id", args.workspace_ids[0]]
    if getattr(args, "tenant_id", ""):
        return ["--tenant-id", args.tenant_id]
    raise TunnelMCPError("remote lookup requires --organization-id, --workspace-id, or --tenant-id")


def _has_remote_scope(args: argparse.Namespace) -> bool:
    return bool(
        (args.organization_ids or [])
        or (args.workspace_ids or [])
        or getattr(args, "tenant_id", "")
    )


def _target_from_args(args: argparse.Namespace) -> runtime.RuntimeTarget:
    if args.mcp_server_url:
        parsed = urlparse(args.mcp_server_url)
        if parsed.scheme not in {"http", "https"} or not parsed.netloc:
            raise TunnelMCPError("--mcp-server-url must be an http or https URL")
        state.reject_inline_secret_material(args.mcp_server_url, field="mcp server URL")
        return runtime.RuntimeTarget(kind="server_url", value=args.mcp_server_url)
    if args.mcp_command:
        state.reject_inline_secret_material(args.mcp_command, field="mcp command")
        return runtime.RuntimeTarget(kind="command", value=args.mcp_command)
    raise TunnelMCPError("connect requires --mcp-server-url or --mcp-command")


def _provided_tunnel(alias: str, args: argparse.Namespace) -> dict[str, Any]:
    tunnel_id = _validated_tunnel_id(args.tunnel_id)
    return {
        "id": tunnel_id,
        "name": args.name or alias,
        "description": args.description or _default_description(alias),
        "organization_ids": args.organization_ids or [],
        "workspace_ids": args.workspace_ids or [],
        "tenant_ids": [],
    }


def _validated_tunnel_id(value: Any) -> str:
    tunnel_id = str(value)
    _validate_tunnel_id(tunnel_id)
    return tunnel_id


def _local_tunnel_from_alias(record: state.AliasRecord) -> dict[str, Any]:
    return {
        "id": record.tunnel_id,
        "name": record.name or record.alias,
        "description": record.description or _default_description(record.alias),
        "organization_ids": list(record.organization_ids),
        "workspace_ids": list(record.workspace_ids),
        "tenant_ids": list(record.tenant_ids),
    }


def _validate_tunnel_id(tunnel_id: str) -> None:
    if not tunnel_id.startswith("tunnel_") or len(tunnel_id) <= len("tunnel_"):
        raise TunnelMCPError("--tunnel-id must look like tunnel_<id>")


def _runtime_api_key_ref(args: argparse.Namespace) -> str:
    api_key = args.runtime_api_key or DEFAULT_RUNTIME_API_KEY_REF
    state.validate_secret_reference(api_key, field="runtime api_key")
    return api_key


def _runtime_launch_env_overrides(runtime_api_key_ref: str) -> dict[str, str]:
    value = (runtime_api_key_ref or "").strip()
    if not value.startswith("env:"):
        return {}
    env_name = value.removeprefix("env:")
    return {env_name: _resolve_secret_reference(value)}


def _status_read_only_key_ref(
    record: state.AliasRecord,
    process: state.ProcessRecord | None,
    admin_profile: EffectiveAdminProfile,
) -> tuple[str, str]:
    for path_value in [
        process.profile_path if process else "",
        record.profile_path,
        process.config_path if process else "",
        record.config_path,
    ]:
        key_ref = _control_plane_api_key_ref_from_profile(path_value)
        if not key_ref:
            continue
        available, _reason = _secret_reference_available(key_ref)
        if available:
            return key_ref, "runtime"
    for key_ref in [DEFAULT_RUNTIME_API_KEY_REF, "env:OPENAI_API_KEY"]:
        available, _reason = _secret_reference_available(key_ref)
        if available:
            return key_ref, "runtime"
    available, _reason = _secret_reference_available(admin_profile.admin_key)
    if available:
        return admin_profile.admin_key, "admin"
    return "", ""


def _control_plane_api_key_ref_from_profile(path_value: str) -> str:
    if not path_value:
        return ""
    path = Path(path_value)
    if not path.exists() or not path.is_file():
        return ""
    try:
        raw = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return ""
    if not isinstance(raw, dict):
        return ""
    control_plane = raw.get("control_plane")
    if not isinstance(control_plane, dict):
        return ""
    api_key = control_plane.get("api_key")
    if not isinstance(api_key, str):
        return ""
    api_key = api_key.strip()
    if not api_key:
        return ""
    try:
        state.validate_secret_reference(api_key, field="runtime api_key")
    except state.StateError:
        return ""
    return api_key


def _secret_reference_available(secret_ref: str) -> tuple[bool, str]:
    value = (secret_ref or "").strip()
    if not value:
        return False, "secret reference is empty"
    if value.startswith("env:"):
        name = value.removeprefix("env:")
        if os.environ.get(name, "").strip():
            return True, ""
        return False, f"environment variable {name} is not set"
    if value.startswith("file:"):
        path = Path(value.removeprefix("file:")).expanduser()
        if not path.exists() or not path.is_file():
            return False, f"secret file {path} does not exist"
        if path.stat().st_size == 0:
            return False, f"secret file {path} is empty"
        return True, ""
    return False, f"unsupported secret reference {value}"


def _resolve_secret_reference(secret_ref: str) -> str:
    value = (secret_ref or "").strip()
    state.validate_secret_reference(value, field="secret reference")
    if value.startswith("env:"):
        env_name = value.removeprefix("env:")
        resolved = os.environ.get(env_name, "").strip()
        if resolved:
            return resolved
        raise TunnelMCPError(f"environment variable {env_name} is not set")
    if value.startswith("file:"):
        path = Path(value.removeprefix("file:")).expanduser()
        try:
            resolved = path.read_text(encoding="utf-8").strip()
        except OSError as exc:
            raise TunnelMCPError(f"failed to read secret file {path}: {exc}") from exc
        if resolved:
            return resolved
        raise TunnelMCPError(f"secret file {path} is empty")
    raise TunnelMCPError(f"unsupported secret reference {value}")


def _status_remote_lookup_skipped_reason(
    record: state.AliasRecord,
    process: state.ProcessRecord | None,
    admin_profile: EffectiveAdminProfile,
) -> str:
    reasons: list[str] = []
    seen: set[str] = set()
    for key_ref in [
        _control_plane_api_key_ref_from_profile(process.profile_path if process else ""),
        _control_plane_api_key_ref_from_profile(record.profile_path),
        _control_plane_api_key_ref_from_profile(process.config_path if process else ""),
        _control_plane_api_key_ref_from_profile(record.config_path),
        DEFAULT_RUNTIME_API_KEY_REF,
        "env:OPENAI_API_KEY",
        admin_profile.admin_key,
    ]:
        if not key_ref or key_ref in seen:
            continue
        seen.add(key_ref)
        available, reason = _secret_reference_available(key_ref)
        if available:
            return ""
        if reason:
            reasons.append(reason)
    if reasons:
        return reasons[-1]
    return "no runtime or admin key reference is available for read-only lookup"


def _resolve_admin_profile(
    args: argparse.Namespace,
    root: Path,
    *,
    default_profile_name: str = "",
) -> EffectiveAdminProfile:
    requested_name = (
        args.admin_profile
        or default_profile_name
        or os.environ.get("TUNNEL_MCP_ADMIN_PROFILE")
        or DEFAULT_ADMIN_PROFILE
    )
    name = state.normalize_alias(requested_name)
    profiles = state.load_admin_profiles(root)
    existing = profiles.get(name)
    base_url = (
        args.control_plane_base_url
        or (existing.control_plane_base_url if existing else "")
        or DEFAULT_BASE_URL
    )
    admin_key = args.admin_key or (existing.admin_key if existing else "") or DEFAULT_ADMIN_KEY_REF
    state.validate_secret_reference(admin_key, field=f"admin profile {name} admin_key")

    if existing and existing.control_plane_base_url == base_url and existing.admin_key == admin_key:
        profile = existing
    else:
        profile = state.AdminProfile(
            name=name,
            control_plane_base_url=base_url,
            admin_key=admin_key,
            updated_at=state.utc_now(),
        )
        profiles[name] = profile
        state.save_admin_profiles(profiles, root, active_profile=name)
    return EffectiveAdminProfile(
        name=name,
        control_plane_base_url=base_url,
        admin_key=admin_key,
        path=str(state.admin_profiles_path(root)),
    )


def _default_description(alias: str) -> str:
    return f"MCP tunnel for {alias}"


def _failed_command(label: str, result: subprocess.CompletedProcess[str]) -> str:
    stdout = (result.stdout or "").strip()
    stderr = (result.stderr or "").strip()
    detail = stderr or stdout or f"exit {result.returncode}"
    return f"{label} failed: {detail}"


def _is_not_found(exc: RemoteError) -> bool:
    message = str(exc).lower()
    return "404" in message or "not found" in message or "no such tunnel" in message


def _repair_command(
    alias: str, record: state.AliasRecord, process: state.ProcessRecord | None
) -> str:
    command = ["scripts/tunnel_mcp", "connect", "--alias", alias]
    if record.admin_profile:
        command.extend(["--admin-profile", record.admin_profile])
    if record.profile_name:
        command.extend(["--profile", record.profile_name])
    profile_dir = process.profile_dir if process and process.profile_dir else record.profile_dir
    if profile_dir:
        command.extend(["--profile-dir", profile_dir])
    if record.organization_ids:
        command.extend(["--organization-id", record.organization_ids[0]])
    elif record.workspace_ids:
        command.extend(["--workspace-id", record.workspace_ids[0]])
    elif record.tenant_ids:
        command.extend(["--tenant-id", record.tenant_ids[0]])
    if process and process.target_kind == "server_url":
        command.extend(["--mcp-server-url", process.target_value])
    elif process and process.target_kind == "command":
        command.extend(["--mcp-command", process.target_value])
    else:
        command.append("<add --mcp-server-url or --mcp-command>")
    return " ".join(command)


def _doctor_command(
    profile_name: str,
    profile_dir: str,
    config_path: str,
    *,
    explain: bool = True,
) -> str:
    command = ["tunnel-client", "doctor"]
    if profile_name:
        command.extend(["--profile", profile_name])
        if profile_dir:
            command.extend(["--profile-dir", profile_dir])
    elif config_path:
        command.extend(["--config", config_path])
    else:
        return "tunnel-client help quickstart"
    if explain:
        command.append("--explain")
    return " ".join(shlex.quote(part) for part in command)


def _doctor_next_steps(profile_name: str, profile_dir: str, config_path: str) -> list[str]:
    return [_doctor_command(profile_name, profile_dir, config_path, explain=True)]


def _status_next_steps(
    *,
    alias: str,
    profile_name: str,
    profile_dir: str,
    config_path: str,
    repair_command: str,
) -> list[str]:
    steps = _doctor_next_steps(profile_name, profile_dir, config_path)
    if repair_command:
        steps.append(repair_command)
    if alias:
        steps.append(f"scripts/tunnel_mcp status {shlex.quote(alias)}")
    return steps


def _parser() -> argparse.ArgumentParser:
    common = argparse.ArgumentParser(add_help=False)
    common.add_argument("--tunnel-client-bin")
    common.add_argument(
        "--control-plane-base-url",
        default=os.environ.get("CONTROL_PLANE_BASE_URL"),
    )
    common.add_argument(
        "--admin-profile",
        help=(
            "Admin profile name from admin_profiles.yaml; defaults to "
            "TUNNEL_MCP_ADMIN_PROFILE or default"
        ),
    )
    common.add_argument(
        "--admin-key",
        default=os.environ.get("TUNNEL_MCP_ADMIN_KEY"),
        help=(
            "Admin key reference to store in the active admin profile, using env:NAME or file:/path"
        ),
    )
    common.add_argument(
        "--runtime-api-key",
        default=os.environ.get("TUNNEL_MCP_RUNTIME_API_KEY"),
        help=(
            "Runtime tunnel key reference for generated config "
            "control_plane.api_key, using env:NAME or file:/path"
        ),
    )
    common.add_argument("--organization-id", action="append", dest="organization_ids", default=[])
    common.add_argument("--workspace-id", action="append", dest="workspace_ids", default=[])

    parser = argparse.ArgumentParser(prog="tunnel_mcp")
    subcommands = parser.add_subparsers(dest="command", required=True)

    create = subcommands.add_parser("create", parents=[common])
    create.add_argument("--alias", required=True)
    create.add_argument("--name")
    create.add_argument("--description")
    create.set_defaults(tenant_id="")

    connect = subcommands.add_parser("connect", parents=[common])
    connect.add_argument("--alias", required=True)
    connect.add_argument("--name")
    connect.add_argument("--description")
    connect.add_argument("--tunnel-id", help="Attach to an existing tunnel id without admin CRUD")
    connect.add_argument("--profile", help="tunnel-client profile name to write and run")
    connect.add_argument(
        "--profile-dir",
        help=(
            "Directory for generated native tunnel-client profiles; defaults to "
            "TUNNEL_CLIENT_PROFILE_DIR, XDG_CONFIG_HOME/tunnel-client, or ~/.config/tunnel-client"
        ),
    )
    target = connect.add_mutually_exclusive_group(required=True)
    target.add_argument("--mcp-server-url")
    target.add_argument("--mcp-command")
    connect.set_defaults(tenant_id="")

    list_cmd = subcommands.add_parser("list", parents=[common])
    list_cmd.add_argument("--tenant-id", default="")
    list_cmd.set_defaults(alias="", name=None, description=None)

    status = subcommands.add_parser("status", parents=[common])
    status.add_argument("alias")
    status.set_defaults(tenant_id="", name=None, description=None)

    stop = subcommands.add_parser("stop", parents=[common], aliases=["disconnect"])
    stop.add_argument("alias")
    stop.set_defaults(tenant_id="", name=None, description=None)

    return parser
