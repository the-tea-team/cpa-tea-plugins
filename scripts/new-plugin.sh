#!/usr/bin/env bash
# Scaffold a new CPA plugin: scripts/new-plugin.sh <id> ["Human Name"]
# Creates plugins/<id>/ with go.mod, main.go, abi.go, plugin.json, README and a test.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2

id="${1:-}"
name="${2:-$1}"

if [ -z "$id" ]; then
  echo "usage: scripts/new-plugin.sh <id> [\"Human Name\"]" >&2
  exit 2
fi
if ! printf '%s' "$id" | grep -qE '^[a-z0-9][a-z0-9._-]{0,127}$'; then
  echo "plugin id must be lowercase alphanumeric with . _ - (got '$id')" >&2
  exit 2
fi
if [ -d "plugins/$id" ]; then
  echo "plugins/$id already exists" >&2
  exit 1
fi

# CPA's Go requirement and current release drive the new module.
cpa_version="${CPA_VERSION:-v8.0.51}"
go_version="$(go env GOVERSION 2>/dev/null | sed 's/^go//')"
if [ -z "$go_version" ]; then
  echo "go is not installed or not on PATH" >&2
  exit 1
fi
go_mod_version="$(printf '%s' "$go_version" | cut -d. -f1,2).0"

dir="plugins/$id"
mkdir -p "$dir"

# Go identifiers cannot contain hyphens.
go_ident="$(printf '%s' "$id" | tr '.-' '__')"

cat > "$dir/go.mod" <<EOF
module github.com/the-tea-team/cpa-tea-plugins/plugins/$id

go $go_mod_version

require github.com/router-for-me/CLIProxyAPI/v8 $cpa_version
EOF

cat > "$dir/plugin.json" <<EOF
{
  "id": "$id",
  "name": "$name",
  "description": "TODO: one sentence on what $id does.",
  "author": "The Tea Team",
  "version": "0.1.0",
  "license": "MIT",
  "tags": []
}
EOF

cat > "$dir/main.go" <<EOF
package main

import (
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const pluginName = "$name"

var pluginVersion = "0.1.0"

// ${go_ident}Plugin implements the capabilities this plugin needs.
// Enable them in buildPlugin below — do not declare capabilities you do not implement.
type ${go_ident}Plugin struct {
	configYAML []byte
	pluginDir  string
}

func buildPlugin(configYAML []byte, pluginDir string) (pluginapi.Plugin, error) {
	// Wire p into Capabilities below as soon as you implement one; until then it is unused.
	_ = &${go_ident}Plugin{
		configYAML: append([]byte(nil), configYAML...),
		pluginDir:  pluginDir,
	}
	return pluginapi.Plugin{
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "The Tea Team",
			GitHubRepository: "https://github.com/the-tea-team/cpa-tea-plugins",
			ConfigFields:     []pluginapi.ConfigField{},
		},
		Capabilities: pluginapi.Capabilities{
			// TODO: enable exactly the capabilities this plugin implements, e.g.
			// Scheduler:   p,
			// ModelRouter: p,
			// ManagementAPI: p,
		},
	}, nil
}

func (p *${go_ident}Plugin) Reconfigure(configYAML []byte) error {
	p.configYAML = append([]byte(nil), configYAML...)
	return nil
}

func (p *${go_ident}Plugin) Shutdown() error { return nil }
EOF

cat > "$dir/main_test.go" <<EOF
package main

import "testing"

func TestBuildPluginMetadata(t *testing.T) {
	plugin, err := buildPlugin(nil, t.TempDir())
	if err != nil {
		t.Fatalf("buildPlugin returned error: %v", err)
	}
	if plugin.Metadata.Version != pluginVersion {
		t.Fatalf("metadata version %q != pluginVersion %q", plugin.Metadata.Version, pluginVersion)
	}
	if plugin.Metadata.Name != pluginName {
		t.Fatalf("metadata name %q != pluginName %q", plugin.Metadata.Name, pluginName)
	}
}
EOF

# The C ABI entry points are shared by every plugin; only the symbol prefix differs.
sed "s/__PLUGIN_IDENT__/${go_ident}/g" scripts/templates/abi.go.tmpl > "$dir/abi.go"

cat > "$dir/README.md" <<EOF
# $name (\`$id\`)

TODO: what this plugin does and why you would install it.

## Capabilities

TODO: list the CPA capabilities used (Scheduler, ModelRouter, Executor, FrontendAuthProvider,
ManagementAPI, QuotaProvider) and what each one does here.

## Configuration

Lives under \`plugins.configs.$id\` in CPA's \`config.yaml\`.

\`\`\`yaml
plugins:
  enabled: true
  configs:
    $id:
      enabled: true
\`\`\`

## Failure behaviour

TODO: what happens when this plugin errors, and whether requests still route without it.
EOF

echo "created $dir"
echo "next: implement capabilities in $dir/main.go, then scripts/check.sh"
