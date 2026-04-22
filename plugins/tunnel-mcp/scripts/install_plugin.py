#!/usr/bin/env python3

from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import sys
from pathlib import Path
from typing import Optional

DEFAULT_MARKETPLACE = "debug"
DEFAULT_VERSION = "local"
BIN_HINT_FILENAME = ".tunnel-client-bin"
FALLBACK_MANIFEST_PATHS = (
    Path("cursor.json"),
    Path(".cursor-plugin/plugin.json"),
    Path("claude.json"),
)
SAFE_PLUGIN_SEGMENT = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description=(
            "Install this local Codex plugin into the plugin cache and update config.toml."
        )
    )
    parser.add_argument(
        "--source",
        help="Path to the source plugin directory. Defaults to the plugin containing this script.",
    )
    parser.add_argument(
        "--marketplace",
        default=DEFAULT_MARKETPLACE,
        help=(
            "Marketplace name to use in the cache path and config key. "
            f"Default: {DEFAULT_MARKETPLACE}"
        ),
    )
    parser.add_argument(
        "--codex-home",
        help="Override CODEX_HOME. Defaults to $CODEX_HOME or ~/.codex.",
    )
    parser.add_argument(
        "--tunnel-client-bin",
        help=(
            "Path to the tunnel-client binary to persist into the installed plugin bundle. "
            "Defaults to auto-detecting a local build, an existing bundle hint, or "
            "$TUNNEL_CLIENT_BIN."
        ),
    )
    return parser.parse_args()


def default_source() -> Path:
    return Path(__file__).resolve().parents[1]


def resolve_codex_home(arg_value: Optional[str]) -> Path:
    if arg_value:
        return Path(arg_value).expanduser().resolve()
    env_value = os.environ.get("CODEX_HOME")
    if env_value:
        return Path(env_value).expanduser().resolve()
    return (Path.home() / ".codex").resolve()


def tunnel_client_bin_hint_path(root: Path) -> Path:
    return root / BIN_HINT_FILENAME


def read_tunnel_client_bin_hint(root: Path) -> Optional[Path]:
    hint_path = tunnel_client_bin_hint_path(root)
    if not hint_path.is_file():
        return None
    value = hint_path.read_text(encoding="utf-8").strip()
    if not value:
        return None
    return Path(value).expanduser()


def is_executable_file(path: Path) -> bool:
    return path.is_file() and os.access(path, os.X_OK)


def validate_tunnel_client_bin(path_value: str, *, field: str) -> Path:
    candidate = Path(path_value).expanduser().resolve()
    if not is_executable_file(candidate):
        raise ValueError(f"{field} is not an executable file: {candidate}")
    return candidate


def tunnel_client_bin_candidates(source: Path) -> list[Path]:
    candidates: list[Path] = []
    seen: set[str] = set()
    for root in [source, *source.parents]:
        for candidate in (
            root / "tunnel-client",
            root / "bin" / "tunnel-client",
            root / "bazel-bin" / "cmd" / "client" / "client",
            root / "bazel-bin" / "api" / "tunnel-client" / "cmd" / "client" / "client",
        ):
            key = str(candidate)
            if key in seen:
                continue
            seen.add(key)
            candidates.append(candidate)
    return candidates


def discover_tunnel_client_bin(source: Path, explicit: Optional[str]) -> Optional[Path]:
    if explicit and explicit.strip():
        return validate_tunnel_client_bin(explicit.strip(), field="--tunnel-client-bin")

    env_value = os.environ.get("TUNNEL_CLIENT_BIN", "").strip()
    if env_value:
        return validate_tunnel_client_bin(env_value, field="TUNNEL_CLIENT_BIN")

    hinted = read_tunnel_client_bin_hint(source)
    if hinted is not None and is_executable_file(hinted):
        return hinted.resolve()

    for candidate in tunnel_client_bin_candidates(source):
        if is_executable_file(candidate):
            return candidate.resolve()
    return None


def validate_plugin_segment(value: str, *, field: str) -> str:
    normalized = value.strip()
    if not normalized:
        raise ValueError(f"{field} must be a non-empty string")
    if (
        value != normalized
        or normalized in {".", ".."}
        or not SAFE_PLUGIN_SEGMENT.fullmatch(normalized)
    ):
        raise ValueError(
            f"{field} must use letters, numbers, '.', '_' or '-' and must not contain path separators"
        )
    return normalized


def load_plugin_name(source: Path) -> str:
    manifest_path = source / ".codex-plugin" / "plugin.json"
    if not manifest_path.is_file():
        create_minimal_codex_manifest(source, manifest_path)

    try:
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as err:
        raise ValueError(f"invalid plugin manifest: {manifest_path}: {err}") from err

    plugin_name = manifest.get("name")
    if not isinstance(plugin_name, str) or not plugin_name.strip():
        raise ValueError(f"plugin manifest must contain a non-empty string name: {manifest_path}")
    return validate_plugin_segment(plugin_name, field=f"plugin manifest name in {manifest_path}")


def create_minimal_codex_manifest(source: Path, manifest_path: Path) -> None:
    plugin_name = None

    for relative_path in FALLBACK_MANIFEST_PATHS:
        candidate = source / relative_path
        if not candidate.is_file():
            continue

        try:
            manifest = json.loads(candidate.read_text(encoding="utf-8"))
        except json.JSONDecodeError as err:
            raise ValueError(f"invalid plugin manifest: {candidate}: {err}") from err

        raw_plugin_name = manifest.get("name")
        if isinstance(raw_plugin_name, str) and raw_plugin_name.strip():
            plugin_name = validate_plugin_segment(
                raw_plugin_name, field=f"plugin manifest name in {candidate}"
            )
            break

    if plugin_name is None:
        raise ValueError(
            f"missing plugin manifest: {manifest_path}; expected .codex-plugin/plugin.json, "
            "cursor.json, .cursor-plugin/plugin.json, or claude.json with a non-empty name"
        )

    manifest_path.parent.mkdir(parents=True, exist_ok=True)
    manifest_path.write_text(json.dumps({"name": plugin_name}, indent=2) + "\n", encoding="utf-8")


def split_sections(text: str) -> list[tuple[Optional[str], list[str]]]:
    sections: list[tuple[Optional[str], list[str]]] = []
    current_name: Optional[str] = None
    current_lines: list[str] = []

    for line in text.splitlines():
        if line.startswith("[") and line.endswith("]"):
            sections.append((current_name, current_lines))
            current_name = line[1:-1]
            current_lines = [line]
        else:
            current_lines.append(line)

    sections.append((current_name, current_lines))
    return sections


def render_sections(sections: list[tuple[Optional[str], list[str]]]) -> str:
    rendered_chunks = []
    for _, lines in sections:
        chunk = "\n".join(lines).strip("\n")
        if chunk:
            rendered_chunks.append(chunk)
    return "\n\n".join(rendered_chunks) + "\n"


def update_config(config_path: Path, plugin_name: str, marketplace: str) -> None:
    plugin_key = f"{plugin_name}@{marketplace}"
    section_name = f'plugins."{plugin_key}"'

    existing_text = config_path.read_text(encoding="utf-8") if config_path.exists() else ""
    sections = split_sections(existing_text)
    filtered_sections = [(name, lines) for name, lines in sections if name != section_name]
    filtered_sections.append((section_name, [f"[{section_name}]", "enabled = true"]))

    config_path.parent.mkdir(parents=True, exist_ok=True)
    config_path.write_text(render_sections(filtered_sections), encoding="utf-8")


def install_plugin(
    source: Path,
    codex_home: Path,
    marketplace: str,
    *,
    tunnel_client_bin: Optional[str] = None,
) -> tuple[str, Path, Path, Optional[Path]]:
    if not source.is_dir():
        raise ValueError(f"plugin source path is not a directory: {source}")

    plugin_name = load_plugin_name(source)
    target = codex_home / "plugins" / "cache" / marketplace / plugin_name / DEFAULT_VERSION
    target.parent.mkdir(parents=True, exist_ok=True)
    if target.exists():
        shutil.rmtree(target)
    shutil.copytree(source, target)
    resolved_tunnel_client_bin = discover_tunnel_client_bin(source, tunnel_client_bin)
    if resolved_tunnel_client_bin is not None:
        tunnel_client_bin_hint_path(target).write_text(
            str(resolved_tunnel_client_bin) + "\n", encoding="utf-8"
        )

    config_path = codex_home / "config.toml"
    update_config(config_path, plugin_name, marketplace)
    return plugin_name, target, config_path, resolved_tunnel_client_bin


def main() -> int:
    args = parse_args()
    source = Path(args.source).expanduser().resolve() if args.source else default_source()
    codex_home = resolve_codex_home(args.codex_home)
    marketplace = validate_plugin_segment(
        args.marketplace.strip() or DEFAULT_MARKETPLACE,
        field="--marketplace",
    )

    try:
        plugin_name, target, config_path, binary_hint = install_plugin(
            source,
            codex_home,
            marketplace,
            tunnel_client_bin=args.tunnel_client_bin,
        )
    except ValueError as err:
        print(str(err), file=sys.stderr)
        return 1

    print(f"Installed {plugin_name}@{marketplace}")
    print(f"Target: {target}")
    print(f"Config: {config_path}")
    if binary_hint is not None:
        print(f"Tunnel client: {binary_hint}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
