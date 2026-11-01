#!/usr/bin/env bash
# Regenerate registry.json from plugins/*/plugin.json (and vendor/*/*/plugin.json).
#   scripts/registry-build.sh           write registry.json
#   scripts/registry-build.sh --check   fail if registry.json is stale or invalid
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2

mode="${1:-}"

python3 - "$mode" <<'PY'
import json, sys, pathlib, subprocess

root = pathlib.Path(".")
mode = sys.argv[1] if len(sys.argv) > 1 else ""
entries = []
errors = []

meta_files = sorted(root.glob("plugins/*/plugin.json")) + sorted(root.glob("vendor/*/*/plugin.json"))
for meta in meta_files:
    plugin_dir = meta.parent
    try:
        data = json.loads(meta.read_text())
    except Exception as exc:
        errors.append(f"{meta}: invalid JSON ({exc})")
        continue

    pid = data.get("id") or plugin_dir.name
    if pid != plugin_dir.name:
        errors.append(f"{meta}: id '{pid}' does not match directory '{plugin_dir.name}'")

    version = str(data.get("version", "")).strip()
    if not version:
        errors.append(f"{meta}: missing version")

    # plugin.json version must match the Go binary's pluginVersion.
    main_go = plugin_dir / "main.go"
    if main_go.exists():
        for line in main_go.read_text().splitlines():
            stripped = line.strip()
            if stripped.startswith("pluginVersion") and "=" in stripped:
                go_version = stripped.split("=", 1)[1].strip().strip('"')
                if go_version and version and go_version != version:
                    errors.append(f"{meta}: version {version} != main.go pluginVersion {go_version}")
                break

    entry = {
        "id": pid,
        "name": data.get("name", pid),
        "description": data.get("description", ""),
        "author": data.get("author", "The Tea Team"),
        "version": version,
        "repository": data.get("repository", "https://github.com/the-tea-team/cpa-tea-plugins"),
        "homepage": data.get("homepage", "https://github.com/the-tea-team/cpa-tea-plugins"),
        "license": data.get("license", "MIT"),
        "tags": data.get("tags", []),
    }
    if data.get("logo"):
        entry["logo"] = data["logo"]
    if data.get("locales"):
        entry["locales"] = data["locales"]
    if data.get("install"):
        entry["install"] = data["install"]
    entries.append(entry)

if errors:
    print("registry validation failed:", file=sys.stderr)
    for err in errors:
        print("  -", err, file=sys.stderr)
    sys.exit(1)

registry = {"schema_version": 1, "plugins": entries}
rendered = json.dumps(registry, indent=2, ensure_ascii=False) + "\n"
out = root / "registry.json"

if mode == "--check":
    current = out.read_text() if out.exists() else ""
    if current != rendered:
        print("registry.json is stale — run scripts/registry-build.sh", file=sys.stderr)
        sys.exit(1)
    print(f"registry.json ok ({len(entries)} plugin(s))")
else:
    out.write_text(rendered)
    print(f"registry.json written ({len(entries)} plugin(s))")
PY
