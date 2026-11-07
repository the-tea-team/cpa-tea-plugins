# cpa-tea-plugins

Tea Team's plugin suite for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA).

Each directory under `plugins/` is an independent, native CPA plugin built as a Go `c-shared`
library. They install into an unmodified CPA server — no fork, no patched core.

Read **[AGENTS.md](AGENTS.md)** before creating or editing a plugin. It carries the layout rules,
the CPA plugin contract, and the commands.

## Layout

```
plugins/<id>/     one plugin per directory, own go.mod, own version
vendor/<id>/      third-party plugins we have adopted and maintain
registry.json     the CPA plugin-store registry served from this repo
scripts/          build, test, registry and dev-loop tooling
```

## Using the suite

Add this repository as an extra CPA plugin-store source in `config.yaml`:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  store-sources:
    - "https://raw.githubusercontent.com/the-tea-team/cpa-tea-plugins/main/registry.json"
```

CPA's official registry is always included, so our plugins appear alongside the official ones in
CPA-Manager-Plus and EasyCLIProxyAPI.

## Status

One plugin is published:

| Plugin | Purpose |
|---|---|
| `tea-keys` | Per-API-key model allowlists, enforced on requests and on `/v1/models` listings |

Planned next:

| Plugin | Purpose |
|---|---|
| `tea-combos` | Named model combos with ordered fallback across providers/models |
| `tea-quota-router` | Quota-aware round-robin credential selection |
