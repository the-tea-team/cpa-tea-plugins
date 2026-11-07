# Tea Keys (`tea-keys`)

Per-API-key model allowlists for CLIProxyAPI. A key can call only the models it is
allowed to use, and `/v1/models` shows only those models.

CPA's built-in `access.api-keys` is a flat list of strings, so it cannot express
"this key may use `gpt-4o` and `claude-*`, but nothing else". This plugin adds that
policy without forking CPA.

## What it enforces

| Surface | Behaviour |
|---|---|
| Model execution (`/v1/chat/completions`, `/v1/messages`, `/v1/responses`, `/v1beta/...`, count-tokens, images, video, audio, streams) | Rejected with HTTP 403 before credential selection and before any upstream call. |
| `/v1/models`, `/v1/models/<id>`, `/v1beta/models` | The catalog is filtered to the key's allowlist, so the client sees only permitted models. A detail request for a filtered model returns 404 from CPA's normal catalog lookup. |
| Codex `?client_version=` catalog and the Grok model catalog | Filtered through the same response hook. |
| Missing or unmanaged key in exclusive mode | Rejected with HTTP 401. |

Streaming requests are rejected by the before-auth request interceptor, so a
disallowed stream never reaches an upstream.

## Capabilities

- `FrontendAuthProvider` — authenticates keys listed in this plugin and reports
  the key's `name` as the CPA principal (useful in usage records).
- `FrontendAuthProviderOmitBody` — the auth hook does not need request bodies.
- `RequestInterceptor` — enforces the allowlist before credential selection and
  before the request leaves CPA.
- `ResponseInterceptor` — filters model catalogs returned by CPA's model-list
  endpoints and the Codex/Grok catalogs.
- `FrontendAuthProviderExclusive` — enabled only when `require_managed_keys: true`
  so core `access.api-keys` cannot bypass the policy.

No management API, no extra listener, no background service.

## Configuration

Lives under `plugins.configs.tea-keys` in CPA's `config.yaml`.

### Recommended: tea-keys owns authentication

```yaml
access:
  api-keys: []          # optional; tea-keys is the exclusive auth provider here

plugins:
  enabled: true
  dir: "plugins"
  configs:
    tea-keys:
      enabled: true
      require_managed_keys: true
      keys:
        - name: alice
          key: "sk-alice-..."
          models:
            - "gpt-4o"
            - "gpt-4o-mini"
            - "claude-*"
        - name: bob
          key: "sk-bob-..."
          models:
            - "gemini-2.5-pro"
        - name: service
          key_sha256: "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
          models: ["*"]
```

With `require_managed_keys: true`, tea-keys becomes the only frontend auth provider.
A key that is not in `keys` gets 401. A request without a credential gets CPA's normal
"Missing API key" response. **This is the default**; set it to `false` only for the
migration mode below.

### Gradual adoption

```yaml
access:
  api-keys:
    - "sk-alice-..."     # still authenticated by CPA
    - "sk-legacy-..."    # not managed by tea-keys, therefore unrestricted

plugins:
  enabled: true
  configs:
    tea-keys:
      enabled: true
      require_managed_keys: false
      keys:
        - name: alice
          key: "sk-alice-..."
          models: ["gpt-4o", "claude-*"]
```

With `require_managed_keys: false`, tea-keys authenticates the keys it knows and
leaves unknown keys to CPA's other access providers. Any key that appears in
`keys` is restricted; unmanaged keys keep CPA's previous behaviour. This lets an
operator migrate keys one at a time.

**Migration-mode caveat:** model-list filtering is guaranteed only when tea-keys
sees every credential. If a key is also in `access.api-keys`, CPA may authenticate
it from a query-string credential before tea-keys runs; the model list then cannot
be filtered because the response hook does not receive query parameters. Execution
is still enforced through CPA's caller-scope identity when the entry uses `key`
(plaintext). An entry that uses `key_sha256` relies on a header credential in
migration mode, because the raw key is not available to derive the caller scope.
Use a header credential or switch to `require_managed_keys: true` when
`/v1/models` must be filtered for every request.

### Key entries

Exactly one of `key` and `key_sha256` is required.

| Field | Meaning |
|---|---|
| `name` | Stable identity returned to CPA. Defaults to `key-<8 hex>`. Must be unique. Never put the secret here. |
| `key` | Plaintext API key. It is hashed at load time and the plaintext is not retained. |
| `key_sha256` | Hex SHA-256 of the key. A `sha256:` prefix and uppercase hex are accepted. |
| `models` | Allowlist. See below. |

The plugin never logs key values. The config file still contains the plaintext key
when `key` is used; that is the same exposure as `access.api-keys` today. Use
`key_sha256` when the config file is more widely readable.

### Model allowlists

| Value | Result |
|---|---|
| omitted or `["*"]` | Every model is allowed. |
| `[]` | No model is allowed. The key authenticates but every model request is rejected and its model list is empty. |
| `["gpt-4o"]` | Exact client-facing model ID. |
| `["claude-*"]` | Glob. `*` matches any sequence (including `/`), `?` matches one character. |
| `["*/gpt-4o"]` | Matches `openai/gpt-4o`, `anthropic/gpt-4o`, and similar provider-prefixed IDs. |

Matching is case-sensitive and is applied to the **client-requested model**, not to
the resolved upstream model. An alias is therefore allowed only when the alias itself
is in the list. This is intentional: the allowlist describes what the client may ask
for, and an allowed alias must not become a way to reach an arbitrary upstream model.

The matcher also understands three CPA naming quirks:

- Anthropic model listings may cloak IDs as `claude-fable-5-dd-<reversed model>`.
  The allowlist matches the uncloaked name, so `claude-sonnet-4` works even when
  the catalog shows the cloaked ID.
- A thinking suffix such as `gpt-4o(8192)` is ignored for matching.
- Gemini listing names carry a `models/` prefix; `gemini-2.5-pro` matches
  `models/gemini-2.5-pro`.

## Credential transport

The auth hook accepts the same forms as CPA's config provider:

- `Authorization: Bearer <key>`
- `Authorization: <key>`
- `X-Api-Key: <key>`
- `X-Goog-Api-Key: <key>`

Query-string credentials (`?key=` and `?auth_token=`) are accepted only for
**unmanaged** keys. A managed key sent in the query string is rejected with 401 and
a message asking for a header. CPA's model-list response hook receives request
headers but not the query string, so accepting a managed query key would let that
key see the unfiltered catalog. Use a header for restricted keys.

## Error responses

The plugin returns a protocol-shaped error body:

- OpenAI / Codex:
  ```json
  {"error":{"message":"Model \"gpt-4o-mini\" is not allowed for this API key","type":"invalid_request_error","param":null,"code":"model_not_allowed"}}
  ```
- Claude:
  ```json
  {"type":"error","error":{"type":"permission_error","message":"..."}}
  ```
- Gemini:
  ```json
  {"error":{"code":403,"message":"...","status":"PERMISSION_DENIED"}}
  ```

Status codes:

- `403 model_not_allowed` — the key is valid but the model is not on its allowlist.
- `401 unmanaged_api_key` — `require_managed_keys: true` and the key is not managed.
- `401` with header guidance — a managed key was presented in the query string.

## Examples

List the models Alice can see:

```bash
curl -s http://127.0.0.1:8317/v1/models \
  -H "Authorization: Bearer sk-alice-..." | jq -r '.data[].id'
```

Use an allowed model:

```bash
curl -s http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer sk-alice-..." \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'
```

Try a model outside the allowlist:

```bash
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer sk-alice-..." \
  -H "Content-Type: application/json" \
  -d '{"model":"o3","messages":[{"role":"user","content":"hi"}]}'
# 403, and no upstream request was made
```

## Failure behaviour

- **Invalid configuration**: registration fails and CPA logs the plugin error. The
  plugin is not loaded, so managed-key authentication and restrictions are not
  active. On a failed reload, the previous plugin instance stays active.
- **No runtime surprises**: interceptors never panic and return pass-through when a
  payload is not a recognised model catalog. A malformed catalog is left untouched
  rather than partially rewritten.
- **Exclusive mode with no keys** is rejected at registration: it would lock every
  client out. `require_managed_keys` defaults to true, so a configuration that only
  enables the plugin (no `keys`) fails to load instead of accepting every request.
- **Plugin disabled or missing**: CPA's normal `access.api-keys` behaviour applies.
  There is no fail-closed enforcement without the plugin.

## Limitations

- The allowlist is checked against the requested model name. A model alias is
  governed by the alias name, not the model it resolves to.
- `/v1/models/<id>` is served from the filtered catalog, so a disallowed model
  appears as 404 "Model not found" rather than 403.
- Query-string credentials are intentionally not supported for managed keys, as
  explained above.
- This plugin does not manage keys through CPA-Manager-Plus. Edit
  `plugins.configs.tea-keys.keys` and reload CPA.

## Install

Build and place the shared library where CPA discovers plugins (`<plugins.dir>/` or
`<plugins.dir>/<goos>/<goarch>/`):

```bash
scripts/build.sh tea-keys
cp plugins/tea-keys/tea-keys.so /path/to/cpa/plugins/
```

Then enable it under `plugins.configs.tea-keys` as shown above.
