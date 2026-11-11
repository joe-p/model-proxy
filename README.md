# model-proxy

A small, configuration-driven, OpenAI-compatible proxy for OpenRouter and any
OpenAI-compatible upstream, including
[Private AI Gateway](https://github.com/Dstack-TEE/private-ai-gateway). It is
deliberately not LiteLLM: no UI, database, caching, rate limiting, budgets, or
multi-provider translation layer. One Go binary, one JSON file, and routes to
one or more upstream providers, plus optional transparent Claude subscription
proxying at `/claude_sub/`.

## Features

- `POST /v1/chat/completions`, streaming and non-streaming, with OpenRouter's
  usage shape and cost fields.
- `POST /api/v1/chat/completions`, `/api/v1/responses`, and `/api/v1/models`
  aliases for clients that hardcode OpenRouter's base URL.
- `POST /v1/responses` for Responses-native clients such as Codex, streaming
  and non-streaming, with transparent response passthrough.
- `POST /v1/images/generations` and `POST /v1/images/edits` for
  OpenAI-compatible image generation and editing, including Open WebUI's image
  tools, with streaming response passthrough and usage/cost logging.
- `GET /backend-api/wham/usage` for Codex subscription usage, forwarded through
  the configured `codex` provider and returned unchanged.
- `/claude_sub/*` transparent Anthropic Messages API proxy for Claude Code
  subscription logins, with usage and API-equivalent cost logging.
- `GET /v1/models` backed by a strict alias registry, optionally merged with
  models discovered from providers that allow passthrough.
- OpenRouter-compatible by default: OpenRouter-shaped model listings, usage
  costs, error envelopes, request-field tolerance, path aliases, and CORS.
- `GET /healthz` liveness endpoint.
- JWCC configuration (JSON with comments and trailing commas) with strict
  unknown-field rejection.
- Virtual keys loaded from a separate `key_file`, with direct `key` values for
  git-ignored files or `key_env` environment lookups.
- Optional per-key access-log `user_agent` overrides.
- Named model policies (whitelist or blacklist) attached to virtual keys.
- Provider-level and model-level extra request-body fields, merged per request.
- Provider-agnostic interface with an OpenRouter implementation and a generic
  OpenAI-compatible implementation.
- Codex CLI OAuth credentials with automatic access-token refresh.
- Transparent upstream response bodies and SSE streams.
- OpenAI-shaped errors for proxy-generated failures; upstream errors pass through.
- OpenRouter-compatible usage normalization for non-OpenRouter providers.
- Token and cost extraction from responses for structured access logs.
- Optional HTTP push of access-log records to a separate collector, with stdout
  logging still enabled.
- Subscription billing-mode tagging for providers such as Codex.
- Optional models.dev list-price estimates with a configurable refresh timer.
- Atomic configuration reload over an admin-only `POST /admin/reload` endpoint,
  with read-only `GET /admin/status` introspection.

## Quick start

```sh
cp config.example.json config.json
cp keys.example.json keys.json
cp .env.example .env
chmod 600 keys.json .env
# edit config.json, keys.json, and .env

make build
./bin/model-proxy -config ./config.json
```

Or with the default `./config.json`:

```sh
go run ./cmd/model-proxy
```

`MODEL_PROXY_CONFIG` can supply the default config path.

```sh
export MODEL_PROXY_CONFIG=/etc/model-proxy/config.json
```

### Example request

```sh
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $PROXY_KEY_LAPTOP" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "fast",
    "messages": [{"role": "user", "content": "Hello"}],
    "stream": false
  }'
```

```sh
curl http://127.0.0.1:8080/v1/models \
  -H "Authorization: Bearer $PROXY_KEY_LAPTOP"
```

## Configuration

See [`config.example.json`](./config.example.json) and
[`keys.example.json`](./keys.example.json) for complete examples. The config is
JWCC (JSON with Commas and Comments): `//` line comments, `/* */` block
comments, and trailing commas are accepted. The example shows both policy modes,
a key without a policy, provider/model `extra_body`, optional OpenRouter
attribution headers, and a Private AI Gateway provider with ACI constraints.

```jsonc
// model-proxy configuration.
//
// This is JWCC (JSON with comments and trailing commas). Unknown fields are
// rejected. Durations use Go syntax, e.g. "10s" or "1m30s". Secrets are not
// placed directly in this file. Virtual keys live in a separate key_file that
// may contain plaintext keys and should be git ignored; provider credentials
// use api_key_env, resolved from the process environment first, then env_file.
{
  "env_file": ".env", // Optional; relative paths are resolved from this file.

  // Virtual keys live in a separate array file so it can be git ignored.
  // Relative paths are resolved from this file. Each entry sets exactly one
  // of "key" or "key_env".
  "key_file": "keys.json",

  "server": {
    "listen": "127.0.0.1:8080",
    "max_body_bytes": 1048576,
    // Image generation/editing bodies carry base64 images or multipart files.
    "max_image_body_bytes": 26214400,
    "read_header_timeout": "10s",
    "shutdown_timeout": "15s",
  },

  "logging": {
    "format": "json", // json | text
    "level": "info",  // debug | info | warn | error
    "log_usage": true,

    // Optional: push access records to an HTTP collector in addition to
    // stdout. The collector receives {"records":[...]} and should return any
    // 2xx. Delivery is asynchronous and best-effort.
    // "access_sink": {
    //   "url": "http://dashboard:8787/ingest",
    //   "batch_size": 250,
    //   "flush_interval": "1s",
    //   "timeout": "5s",
    //   "queue_size": 4096,
    // }
  },

  "auth": {
    "enabled": true, // When enabled, at least one virtual key must resolve.
  },

  // Expose /claude_sub/ for Claude Code subscription logins. The client sends
  // its own OAuth token, which is forwarded to https://api.anthropic.com. No
  // provider, model, or Claude credential is configured here. When true, an
  // empty "models" block is allowed.
  "enable_claude_subscription": false,

  // Optional list-price estimates from models.dev. The catalog is cached on
  // disk and refreshed in the background. Omit this block to disable estimates.
  "pricing": {
    "source": "models.dev",
    "url": "https://models.dev/api.json",
    "cache": "~/.cache/model-proxy/models.dev.json",
    "refresh_interval": "24h",
    "timeout": "30s",
  },

  // A virtual key's "policy" is optional. Keys without a policy may use every
  // configured model alias.

  // Policies restrict which model aliases a virtual key may use. "mode" is
  // "whitelist" (only listed aliases) or "blacklist" (all but listed aliases).
  // Model entries refer to keys from "models" below.
  "policies": [
    {
      "name": "basic",
      "mode": "whitelist",
      "models": ["fast"],
    },
    {
      "name": "no-smart",
      "mode": "blacklist",
      "models": ["smart"],
    },
  ],

  // Provider keys are local names. "type" selects the implementation and may
  // be omitted when it matches the provider name. Supported types are
  // "openrouter", "openai-compatible" (aliases: "openai", "phala",
  // "private-ai-gateway"), and "codex" (alias: "chatgpt").
  "providers": {
    "openrouter": {
      "type": "openrouter",
      "base_url": "https://openrouter.ai/api/v1",
      "api_key_env": "OPENROUTER_API_KEY",
      "dial_timeout": "10s",
      "response_header_timeout": "120s",

      // Optional OpenRouter app-attribution headers.
      "referer": "https://example.com",
      "title": "model-proxy",

      // Optional defaults injected into every upstream request body for models
      // using this provider. These are body fields, not HTTP headers.
      "extra_body": {
        "provider": {
          "zdr": true,
          "data_collection": "deny",
        },
      },
    },
    "phala": {
      "type": "openai-compatible",
      "base_url": "https://inference.phala.com/v1",
      "api_key_env": "PHALA_API_KEY",
      "dial_timeout": "10s",
      "response_header_timeout": "120s",

      // Private AI Gateway reads ACI routing constraints from the request
      // body. aci_verified makes the gateway fail closed unless the selected
      // upstream passed attestation verification.
      "extra_body": {
        "provider": {
          "aci_verified": true,
          "zdr": true,
        },
      },
    },
    // ChatGPT/Codex subscription credentials from the OpenAI Codex CLI.
    // Reads <auth_dir>/auth.json and refreshes expired access tokens. Without
    // auth_dir, $CODEX_HOME or ~/.codex is used.
    // passthrough_models forwards model ids that are not listed below; the
    // upstream model listing then needs a client_version, which defaults to
    // latest_version in <auth_dir>/version.json.
    // "codex": {
    //   "type": "codex",
    //   "base_url": "https://chatgpt.com/backend-api/codex",
    //   "auth_dir": "~/.codex",
    //   "billing_mode": "subscription",
    //   "pricing_provider": "openai",
    //   "passthrough_models": true,
    //   "client_version": "0.167.0",
    // },
  },

  "models": {
    // The model key is the alias clients send in the "model" field.
    "fast": {
      "provider": "openrouter",
      "upstream": "openai/gpt-4o-mini",

      // Model-level extra_body values override provider-level values. Nested
      // objects such as OpenRouter's "provider" object are merged recursively.
      "extra_body": {
        "provider": {
          "order": ["openai"],
          "allow_fallbacks": false,
        },
      },
    },
    "smart": {
      "provider": "openrouter",
      "upstream": "anthropic/claude-sonnet-4",
    },
    "private": {
      "provider": "phala",
      "upstream": "z-ai/glm-5.3",
    },
  },
}
```

The separate key file is JWCC too and contains an array of key objects:

```jsonc
// keys.json - keep this file out of source control.
[
  {"name": "laptop", "key": "a-long-random-value", "policy": "basic"},
  {"name": "ci", "key_env": "PROXY_KEY_CI", "policy": "no-smart"},
  {"name": "admin", "key": "another-long-random-value"},
]
```

Rules:

- All durations are Go duration strings such as `"10s"` or `"2m"`.
- `env_file` is optional and relative to the config file when not absolute.
- `key_file` is optional and relative to the config file when not absolute.
  It accepts the same JWCC syntax and strict unknown-field rejection as the
  main configuration.
- Process environment variables take precedence over `.env` values.
- Key file entries must set exactly one of `key` or `key_env`. A direct `key`
  is used as a literal virtual key; use it only in a key file that is kept out
  of version control.
- `auth.enabled` defaults to `true`. If auth is enabled, at least one virtual
  key must resolve to a non-empty value.
- A virtual key's `policy` is optional. Keys without a policy may use every
  alias.
- A virtual key's `user_agent` is optional. When set, it replaces the
  `user_agent` value in access logs for requests authenticated with that key.
  It does not change any HTTP header sent to the client or upstream.
- Policies require a non-empty `name`, a `mode` of `whitelist` or `blacklist`,
  and a non-empty `models` list of configured aliases. Policy names must be
  unique, and every key `policy` reference must exist.
- Unknown configuration fields, duplicate virtual key names, duplicate resolved
  key values, missing or unreadable key files, missing environment variables,
  invalid URLs, and zero models fail startup. Zero models are allowed when
  `enable_claude_subscription` is true, because `/claude_sub/` forwards model
  ids without an alias registry.
- `server.listen`, `server.read_header_timeout`, `server.admin_listen`, and
  `server.admin_token_env` changes require a restart; other values can be
  reloaded through `POST /admin/reload`.
- `server.admin_listen` enables the administrative API on a second listener and
  requires `server.admin_token_env`. It must differ from `server.listen` and
  defaults to disabled, which leaves reload unavailable. See
  [Reload and shutdown](#reload-and-shutdown).
- For `codex` providers, `auth_dir` defaults to `$CODEX_HOME` or `~/.codex`.
  `auth_dir` is rejected for other provider types.
- `billing_mode` is an optional per-provider log label. It defaults to
  `subscription` for `codex` providers and is omitted for others.
- `pricing` is optional. When configured, the models.dev catalog is cached on
  disk and refreshed every `refresh_interval` (default `24h`). Omit it to
  disable estimates.
- `pricing_provider` selects the models.dev provider namespace. It defaults to
  `openai` for `codex` providers and is otherwise unset.
- A model may set an explicit `pricing` block to override the catalog.
- `enable_claude_subscription` is a single boolean; there is no Claude provider
  to configure. It always uses `https://api.anthropic.com`, subscription
  billing, and the `anthropic` pricing namespace.

### Providers

Each key under `providers` is a local name referenced by models. The optional
`type` field selects the implementation; when omitted, the provider name is
used. Supported types are:

- `openrouter`: OpenAI-compatible transport with optional `referer` and `title`
  attribution headers.
- `openai-compatible`: generic transport for any OpenAI-compatible Chat
  Completions API, including Private AI Gateway, vLLM, and similar servers.
  `openai`, `phala`, and `private-ai-gateway` are accepted aliases for this
  type.
- `codex` (alias `chatgpt`): Responses API transport authenticated with OpenAI
  Codex CLI OAuth credentials. Chat Completions requests are bridged to the
  Responses API. See [Codex auth](#codex-auth).

The Claude subscription proxy is not a provider type. Enable it with the
top-level `enable_claude_subscription` flag; see
[Claude subscription proxy](#claude-subscription-proxy).

Private AI Gateway is not a separate implementation. Configure it as an
`openai-compatible` provider. The Phala-hosted deployment is available at
`https://inference.phala.com/v1`.

`base_url` is the provider's API root and must include its version prefix. The
proxy appends `/chat/completions`, so `https://inference.phala.com/v1` becomes
`https://inference.phala.com/v1/chat/completions`. A self-hosted gateway works
the same way; use its listener plus `/v1`, for example
`http://gateway:8086/v1`.

Image generation is appended the same way. It defaults to
`/images/generations`; set `images_path` when the upstream differs. OpenRouter's
dedicated Image API is served at `/api/v1/images`, so an OpenRouter provider
uses `"images_path": "/images"`. `images_path` is not supported on `codex`
providers.

Image edits default to multipart `POST <base_url>/images/edits`. Set
`images_edit_format` to `"input_references"` for APIs that accept a JSON body
with base64 data URLs instead, such as OpenRouter. In that mode edits are sent
to `images_path`, so OpenRouter needs:

```jsonc
"openrouter": {
  "images_path": "/images",
  "images_edit_format": "input_references"
}
```

`images_edit_format` accepts `"multipart"` (default) or
`"input_references"` and is not supported on `codex` providers.

Private AI Gateway's ACI routing constraints are ordinary request-body fields.
Set them through `extra_body`, for example:

```json
{"provider": {"aci_verified": true, "zdr": true}}
```

`aci_verified` makes the gateway fail closed unless it verified the selected
upstream inside the TEE. `aci_session_ids` can be added on a model to pin
specific attested sessions. These fields are removed by the gateway before the
request reaches the model provider. Responses and verification headers such as
`X-Receipt-Id`, `X-ACI-Identity`, and `X-ACI-Keyset-Digest` pass through
unchanged.

### Codex auth

The `codex` provider (alias `chatgpt`) reads the OAuth credentials written by
the OpenAI Codex CLI. By default it looks in `$CODEX_HOME`, then `~/.codex`, for
`auth.json`. Set `auth_dir` to use another directory; relative paths resolve
from the config file. No `api_key_env` is required.

```json
"codex": {
  "type": "codex",
  "base_url": "https://chatgpt.com/backend-api/codex",
  "auth_dir": "~/.codex",
  "billing_mode": "subscription"
}
```

The proxy re-reads `auth.json` on every request. A valid access token is used
as-is. When it expires, the proxy exchanges the stored refresh token at OpenAI's
OAuth token endpoint and writes the refreshed tokens back to `auth.json`,
preserving unrelated fields in the file. The `ChatGPT-Account-Id` header is
taken from the file's `account_id` or derived from the token claims.

If `api_key_env` is set for a `codex` provider, its value is used as a static
bearer token and the Codex auth file is ignored.

Use one proxy process per `auth.json`. Refresh tokens are single-use, so
separate processes sharing a file can race and invalidate each other's
credentials.

The Codex backend's native API is the Responses API. `POST /v1/responses`
forwards requests and SSE streams unchanged and observes usage only for its
access log.

`POST /v1/chat/completions` is bridged for `codex` providers. The proxy
converts Chat Completions messages, tools, tool calls, and response formats
into a Responses request, always streams from the Codex backend (which rejects
non-streaming Responses requests), and converts the resulting events back into
Chat Completions JSON or SSE. Because the ChatGPT backend rejects most sampling
and attribution parameters, fields such as `temperature`, `top_p`, `stop`,
`seed`, `n`, penalties, and `user` are dropped for Codex requests. The access
log still records tokens and, with a pricing catalog, an
`estimated_api_cost_usd` API-equivalent estimate.

A `codex` provider defaults to `"billing_mode": "subscription"`. Override it
with `billing_mode` if the same provider shape points at an API-billed service.
With a pricing catalog configured, `estimated_api_cost_usd` gives the
API-equivalent list price for the same tokens; it is not a subscription charge.

### Codex usage

`GET /backend-api/wham/usage` forwards ChatGPT's subscription usage payload
through the configured `codex` provider, including token refresh and the
`ChatGPT-Account-Id` header. The upstream JSON is returned unchanged, so clients
written against ChatGPT's usage endpoint (such as `pi-openai-usage`) can point at
the proxy instead.

The endpoint uses model-proxy's normal virtual-key authentication; the caller's
`Authorization` header is the proxy key, not a ChatGPT token. When exactly one
`codex` provider is configured it is selected automatically. With several Codex
providers, pass `?provider=<name>`:

```sh
curl http://127.0.0.1:8080/backend-api/wham/usage?provider=codex \
  -H "Authorization: Bearer $PROXY_KEY_LAPTOP"
```

The upstream usage URL is derived from the provider's `base_url`; for a typical
`https://chatgpt.com/backend-api/codex` base URL, the upstream endpoint is
`https://chatgpt.com/backend-api/wham/usage`.

### Claude subscription proxy

`/claude_sub/` lets Claude Code use a Claude Pro, Max, Team, or Enterprise
subscription through model-proxy. Claude Code signs in locally and sends its own
OAuth token in `Authorization`; the proxy forwards that token, the request, and
the response unchanged. The proxy never stores or refreshes a Claude
credential.

Enable it with one flag:

```jsonc
"enable_claude_subscription": true
```

The path always uses `https://api.anthropic.com`, subscription billing, and the
`anthropic` models.dev pricing namespace. There is no Claude provider entry, no
Claude credential in the config, and the `models` block may be omitted because
this path does not use aliases.

Claude Code requests can be large; raise `server.max_body_bytes` if a session
exceeds the default 1 MiB.

Point Claude Code at the proxy and give it a virtual key in a custom header:

```sh
unset ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN ANTHROPIC_MODEL
export ANTHROPIC_BASE_URL="http://127.0.0.1:8080/claude_sub"
export ANTHROPIC_CUSTOM_HEADERS="x-model-proxy-key: Bearer $PROXY_KEY"
claude
```

If you previously used API-key authentication, remove `apiKeyHelper` and any
`ANTHROPIC_AUTH_TOKEN` / `ANTHROPIC_API_KEY` entries from your Claude Code
settings, then choose **Claude account with subscription** and sign in.

Path rules:

- The `/claude_sub` prefix is stripped before forwarding. Method, path, query,
  body, `Authorization`, `x-api-key`, `anthropic-version`, `anthropic-beta`,
  `user-agent`, and other end-to-end headers pass through. `x-model-proxy-key`
  is consumed by the proxy and never sent upstream.
- When `auth.enabled` is true, the proxy key must come from
  `x-model-proxy-key` (a bare token or `Bearer <token>`). The standard
  `Authorization` header is reserved for the upstream Claude credential and is
  never treated as a proxy key on this path.
- The path is disabled and returns `404` unless `enable_claude_subscription` is
  true.
- A virtual key's policy still applies to the requested model id. A `whitelist`
  key can use only the configured aliases it lists; a `blacklist` key can use
  anything it does not list.
- Response bodies and SSE streams are byte-for-byte transparent. Upstream errors
  are passed through unchanged. Proxy-generated errors use Anthropic's
  `{"type":"error","error":{...}}` envelope.
- Usage is observed only for the access log. Anthropic's `input_tokens`,
  `cache_creation_input_tokens`, and `cache_read_input_tokens` are summed into
  `prompt_tokens`; `output_tokens` becomes `completion_tokens`; the cache
  counters are logged separately. Streaming usage is merged from the
  `message_start` and `message_delta` events.
- With a pricing catalog, the log includes `estimated_api_cost_usd` and
  `pricing_source` at Anthropic's API list prices. No cost fields are injected
  into the response, because a subscription has no per-token charge. The
  catalog is looked up by the requested model id, then by its undated alias.

### Model aliases

Clients send the alias in `model`. The proxy rewrites only that field to the
configured `upstream` before calling the provider. Alias names are exact and
case-sensitive. Unlisted names return `404 model_not_found`, unless a provider
allows [passthrough models](#passthrough-models).

A key with a `whitelist` policy may use only the listed aliases. A key with a
`blacklist` policy may use every alias except those listed. Using a disallowed
alias returns `403 model_not_allowed` and the request is not sent upstream.
`GET /v1/models` is filtered to the aliases allowed by the authenticated key.

Requests containing `models`, `route`, or `plugins` are rejected with `400`.
Those OpenRouter fields can bypass the alias registry or add paid features.
Other fields, including `provider` and `transforms`, are passed through
untouched. There are no proxy-level retries or fallbacks.

### Passthrough models

Set `passthrough_models` on a provider to accept model ids that are not listed
under `models`. Passthrough ids are exposed and routed as
`<provider>/<upstream>`, the same shape as the aliases above, so the provider is
explicit and a new upstream model needs no config change:

```jsonc
"codex": {
  "type": "codex",
  "base_url": "https://chatgpt.com/backend-api/codex",
  "auth_dir": "~/.codex",
  "passthrough_models": true
}
```

With that config, `{"model": "codex/gpt-5.6-sol"}` is forwarded to the Codex
backend as `gpt-5.6-sol`, and `GET /v1/models` lists it as
`codex/gpt-5.6-sol`. The prefix is stripped before the upstream call and any
further slashes belong to the upstream id, so an aggregator-style
`openrouter/deepseek/deepseek-v4` reaches OpenRouter's
`deepseek/deepseek-v4`. Configured aliases always win over passthrough, so keep
using an alias when you want a stable name or request-body overrides.

- **Provider selection.** A prefixed id is routed to the named provider, which
  must have `passthrough_models` enabled; otherwise the request fails with
  `400 invalid_request`. An unlisted id without a prefix is routed to the single
  provider with `passthrough_models` enabled, with the id used unchanged. If
  several providers enable it, an unprefixed id is ambiguous and the request
  fails with `400 invalid_request` naming the candidates; prefix it instead.
- **Policies still apply.** A `whitelist` key may only use the names it lists, so
  it cannot use passthrough ids. A `blacklist` key may use passthrough ids it
  does not list; blacklisting either the prefixed id or its bare upstream id
  denies both forms. Passthrough requests are logged with
  `"passthrough": true`; the alias is the requested id and `upstream_model` is
  the id without the provider prefix.
- **Provider defaults apply.** Provider-level `extra_body`, `billing_mode`, and
  `pricing_provider` are used. Model-level `extra_body` and `pricing` overrides
  require a configured alias.
- **`GET /v1/models` includes discovered ids.** For each passthrough provider the
  proxy asks the upstream for its model list and merges it with the aliases as
  `<provider>/<id>`, honoring the authenticated key's policy. Results are cached
  for one minute and cleared on reload; a failing or unsupported listing is
  logged and skipped, so the aliases are still returned.

The proxy lists models with `GET <base_url>/models`, reading either
`{"data":[{"id":"..."}]}` (OpenAI, OpenRouter) or `{"models":[{"slug":"..."}]}`
(Codex). The Codex endpoint requires a client version, so a codex provider sends
`client_version` from this file, or from `latest_version` in
`<auth_dir>/version.json` when unset. `client_version` is rejected on other
provider types.

### Extra request-body fields

`extra_body` injects or overrides request-body fields before the upstream call.
It may be configured at the provider level (defaults for every model using that
provider) and at the model level (overrides). Nested JSON objects are merged
recursively; scalars and arrays from the more specific config replace the base
value.

```json
{
  "providers": {
    "openrouter": {
      "extra_body": {
        "provider": {"zdr": true, "data_collection": "deny"}
      }
    }
  },
  "models": {
    "fast": {
      "extra_body": {
        "provider": {"order": ["openai"], "allow_fallbacks": false}
      }
    }
  }
}
```

The example above sends this `provider` object for the `fast` alias:

```json
{"provider":{"zdr":true,"data_collection":"deny","order":["openai"],"allow_fallbacks":false}}
```

Configured values win over client-supplied values at the same JSON key, while
unrelated client fields remain intact. For example, a client cannot turn off
`zdr` or set `data_collection` to `allow` when the config denies it. To enforce
a completely fixed object, set every key you care about in the config.

These are request-body fields, not HTTP headers. OpenRouter reads
`provider.zdr` and `provider.data_collection`; Private AI Gateway reads
`provider.aci_verified`, `provider.aci_session_ids`, and `provider.zdr`. `model`,
`messages`, `stream`, `models`, `route`, and `plugins` may not be set in
`extra_body`; config load fails if they appear.

## Behavior

- Success bodies and SSE streams are forwarded byte-for-byte, except for the
  chat-completions usage normalization described below and the Codex Chat
  Completions bridge, which translates between the two dialects.
- `/claude_sub/` forwards Anthropic request and response bodies, headers, and
  errors unchanged. Only the access log is enriched with observed usage.
- Responses API bodies and streams are never normalized. Chat streaming
  requests to non-OpenRouter providers set `stream_options.include_usage` as
  described below.
- `X-Model-Proxy-Alias` and `X-Model-Proxy-Upstream` describe the selected
  route. `X-Request-ID` is accepted from the client if valid, otherwise
  generated, forwarded upstream, echoed to the client, and included in logs.
- Errors are OpenRouter-shaped: proxy-generated errors use
  `{"error":{"message","code":<int>,"type","metadata":{"proxy_code"}}}` and
  upstream failures are rewritten into the same envelope with
  `metadata.provider_name`.
- Client headers forwarded upstream are limited to `Content-Type`, `Accept`, and
  a sanitized `X-Request-ID`. The proxy sets `Authorization`,
  `Content-Type: application/json`, and optional attribution headers.
- Upstream response headers are copied except hop-by-hop headers,
  `Content-Length`, `Set-Cookie`, and proxy-owned headers.
- Automatic transport compression is disabled so response bytes are never
  silently decompressed.
- CORS is enabled for browser clients. The proxy never proxies a client's
  catalog request for routing; the only catalog reads are the models.dev pricing
  cache and passthrough model discovery.

### OpenRouter compatibility

model-proxy always presents the OpenRouter client contract, for every provider.
There is no setting to turn it off: clients written against OpenRouter keep
working when the base URL points at the proxy.

| Surface | Behavior |
| --- | --- |
| `GET /v1/models` | OpenRouter model objects: `id`, `canonical_slug`, `name`, `created`, `context_length`, `architecture`, `pricing`, `top_provider`, `supported_parameters`, plus `total_count` |
| Chat usage | `cost`, `is_byok`, and `cost_details` for API-billed providers |
| Errors | `{"error":{"message","code":<int>,"type","metadata":{"provider_name","proxy_code"}}}`; upstream error bodies are rewritten |
| Request fields | `models`, `route`, `plugins`, and `transforms` are dropped and logged as `dropped_fields` instead of rejected |
| Paths | `/api/v1/chat/completions`, `/api/v1/responses`, `/api/v1/images`, `/api/v1/images/generations`, and `/api/v1/models` serve the same handlers |
| CORS | `Access-Control-Allow-*` headers and `OPTIONS` preflight |

Model metadata for the listing comes from the pricing catalog, keyed by the
provider's `pricing_provider` and the model's `upstream` id. Explicit model
`pricing` in the config wins over the catalog. Models the catalog does not know
are listed without `pricing` and with a minimal architecture rather than
invented numbers. Providers with `billing_mode: "subscription"` are listed
without `pricing` for every model, including explicit model pricing, because a
plan has no per-token list price; catalog rates still feed access-log estimates.
`supported_parameters` is conservative: it reports `tools`
and reasoning parameters only when the catalog confirms them, and omits
`temperature`/`top_p`/`seed` for Codex-style backends that reject them.

Cost injection follows the billing mode:

- **Provider reported a cost** (OpenRouter): the reported `usage.cost` and
  `cost_details` are passed through untouched.
- **API-billed provider** (`billing_mode` empty or not `subscription`): `cost`
  is derived from the model's catalog rates, or from the model's configured
  `pricing`, and reported in OpenRouter's `cost_details` shape. The access log
  keeps `estimated_api_cost_usd` and `pricing_source` so an estimate is never
  presented as a charge.
- **Subscription provider** (`billing_mode: "subscription"`, such as Codex): no
  cost fields are injected, because a subscription has no per-token price.

Request fields the proxy cannot honor are dropped rather than forwarded:
`models` (fallback list), `route`, `plugins`, and `transforms` would either
bypass the alias registry or add paid features. Each drop is logged in
`dropped_fields`. Responses API bodies and streams are not rewritten; only its
errors follow the OpenRouter envelope.

### OpenRouter-compatible usage output

OpenRouter-capable clients expect OpenRouter's usage shape. For providers whose
type is not `openrouter`, model-proxy normalizes the `usage` object in both
buffered and streaming responses:

| OpenRouter field | Source when the provider uses another dialect |
| --- | --- |
| `prompt_tokens_details.cached_tokens` | `cache_read_input_tokens` or `prompt_cache_hit_tokens` |
| `prompt_tokens_details.cache_write_tokens` | `cache_creation_input_tokens` |
| `completion_tokens_details.reasoning_tokens` | top-level `reasoning_tokens` |
| `total_tokens` | `prompt_tokens + completion_tokens` when omitted |

Existing values are never overridden. Costs are handled separately: a
provider-reported `cost` always wins, API-billed providers get a catalog-derived
`cost` and `cost_details`, and subscription providers get none. model-proxy never
invents `cache_discount` or cache counters that the provider did not report.
Private AI Gateway already reports `cost`, standard token counters, and
`cached_tokens`, so those pass through.

For streaming requests to non-OpenRouter providers, model-proxy also sets
`stream_options.include_usage` to `true` unless already true. OpenRouter always
includes terminal usage, and this gives OpenRouter-compatible clients the same
behavior from other providers.

OpenRouter responses are not normalized and remain byte-for-byte transparent.

### Responses API

`POST /v1/responses` forwards requests to `<base_url>/responses` and returns the
upstream status, headers, and body unchanged. The model alias is rewritten,
configured `extra_body` is merged, and virtual-key authentication and policies
apply exactly as for chat completions. The proxy does not normalize Responses
bodies and does not inject `stream_options`, so Responses-native clients keep
their own usage accounting. A `codex` provider also uses this path internally
when a client calls `/v1/chat/completions`.

When `logging.log_usage` is enabled, the proxy observes usage for its access
log from `response.completed` and `response.incomplete` events, or from the
top-level `usage` object of a non-streaming response:

| Access-log field | Responses field |
| --- | --- |
| `prompt_tokens` | `input_tokens` |
| `completion_tokens` | `output_tokens` |
| `total_tokens` | `total_tokens` |
| `cached_tokens` | `input_tokens_details.cached_tokens` |
| `reasoning_tokens` | `output_tokens_details.reasoning_tokens` |

The response body is still forwarded byte-for-byte; only the log records these
values. `cost_usd` is logged only if the upstream includes `usage.cost`.
ChatGPT subscription responses normally report tokens but not a dollar cost.

### Image generation

`POST /v1/images/generations` and `/v1/images` (plus their `/api/v1` aliases)
forward OpenAI-compatible image requests to the provider selected by the
`model` field. `/images` matches OpenRouter's dedicated Image API path.
Alias resolution, virtual-key authentication, policies, and `extra_body`
merging work exactly as they do for chat models, so an image model must be
listed under `models` or reachable through a provider with
`passthrough_models` enabled.

The request body is forwarded with only `model` rewritten. Fields such as
`prompt`, `size`, `n`, `quality`, `response_format`, and provider-specific
options are passed through untouched. Responses are streamed back byte-for-byte
because image payloads are large.

When `logging.log_usage` is enabled, the proxy observes the response's
top-level `usage` object without buffering the image data and logs
`prompt_tokens`, `completion_tokens`, and `total_tokens`. Both usage shapes are
understood: OpenRouter's `prompt_tokens`/`completion_tokens` (with `cost`) and
the OpenAI images API's `input_tokens`/`output_tokens`. A provider-reported
`cost` is logged as `cost_usd`; otherwise the usual pricing estimate can add
`estimated_api_cost_usd` and `pricing_source`. The response body itself is
never rewritten.

Bodies that are not JSON, such as streamed image responses, are still forwarded
unchanged, but usage is not extracted from SSE events. Upstream errors use the
same envelope as other endpoints.

The upstream path defaults to `<base_url>/images/generations`. Override it per
provider with `images_path`; OpenRouter's Image API, for example, lives at
`/api/v1/images`:

```jsonc
"openrouter": {
  "base_url": "https://openrouter.ai/api/v1",
  "images_path": "/images"
}
```

For Open WebUI, open Admin Settings → Images, choose the OpenAI engine, and set:

- **API Base URL**: the proxy root including `/v1`, for example
  `http://127.0.0.1:8080/v1`.
- **API Key**: a configured virtual key.
- **Model**: an image alias from `models`.

Open WebUI sends `POST <API Base URL>/images/generations`, which the proxy
serves as `/v1/images/generations`. Image editing uses the separate
`IMAGES_EDIT_OPENAI_API_BASE_URL` / `IMAGES_EDIT_OPENAI_API_KEY` /
`IMAGE_EDIT_MODEL` settings and posts to `/images/edits`; point those at the
proxy and the same `openrouter/<slug>` model. Image requests use
`server.max_image_body_bytes` (25 MiB by default) instead of the smaller
`max_body_bytes` used for text requests.

### Image edits

`POST /v1/images/edits` (and `/api/v1/images/edits`) accepts the multipart form
that OpenAI clients and Open WebUI send. The `model` field is resolved through
the same alias and policy machinery as every other endpoint.

Two upstream shapes are supported, selected by the provider's
`images_edit_format`:

- `multipart` (default): the form is forwarded to
  `<base_url>/images/edits` with only `model` rewritten and `extra_body`
  merged. Uploaded file parts keep their filename and content type.
- `input_references`: the form is converted to a JSON request for
  `<base_url><images_path>`. File parts become `input_references` entries with
  `data:<content-type>;base64,...` URLs, `n` is sent as a number, and
  `response_format` is dropped because these APIs always return base64. This
  is the shape OpenRouter's Image API expects. Masks are rejected in this
  mode.

Responses use the same streaming passthrough and usage/cost logging as image
generation, so edits through either shape add `prompt_tokens`,
`completion_tokens`, `total_tokens`, and `cost_usd` (or
`estimated_api_cost_usd`) to the access log.

### Pricing estimates

For upstreams that do not report `cost`, model-proxy can estimate an
API-equivalent cost from the [models.dev](https://models.dev) catalog:

```jsonc
"pricing": {
  "source": "models.dev",
  "url": "https://models.dev/api.json",
  "cache": "~/.cache/model-proxy/models.dev.json",
  "refresh_interval": "24h",
  "timeout": "30s"
}
```

The catalog is loaded from the cache at startup and refreshed in the background
every `refresh_interval`. A failed refresh keeps the previous catalog; a missing
cache is not fatal, and estimates are simply unavailable until the first
successful fetch. A reload rebuilds the pricing loop along with the rest of the
configuration.

Provider-to-catalog mapping uses `pricing_provider`, which defaults to `openai`
for `codex` providers:

```jsonc
"codex": {
  "type": "codex",
  "pricing_provider": "openai"
}
```

A model can override the catalog with explicit per-million rates:

```jsonc
"gpt": {
  "provider": "codex",
  "upstream": "gpt-6.1-sol",
  "pricing": {
    "input_per_million": 2,
    "cached_input_per_million": 0.1,
    "output_per_million": 10
  }
}
```

Estimates use:

```text
uncached_input = prompt_tokens - cached_tokens - cache_write_tokens
cost =
  uncached_input * input_rate
  + cached_tokens * cached_input_rate
  + cache_write_tokens * cache_write_rate
  + completion_tokens * output_rate
```

Rates are USD per million tokens. models.dev context tiers are applied from
`prompt_tokens`, and `reasoning_tokens` is not added separately because it is
already included in `completion_tokens`.

When the upstream reports an actual `usage.cost`, it wins and no estimate is
logged. Otherwise the access log includes `estimated_api_cost_usd` and
`pricing_source` (`models.dev` or `config`). This is a list-price estimate, not
the amount charged to a subscription.

### Proxy-generated errors

| Condition | Status | Code |
| --- | --- | --- |
| Missing/invalid virtual key | 401 | `invalid_api_key` |
| Alias disallowed by the key's policy | 403 | `model_not_allowed` |
| Unknown alias | 404 | `model_not_found` |
| Malformed JSON | 400 | `invalid_request` |
| Unlisted model with several passthrough providers | 400 | `invalid_request` |
| Model prefixed with a provider that disallows passthrough | 400 | `invalid_request` |
| Body over `max_body_bytes` (or `max_image_body_bytes` for images) | 413 | `request_too_large` |
| Image edit mask with `input_references` format | 400 | `invalid_request` |
| Upstream unreachable | 502 | `upstream_unavailable` |
| Upstream response-header timeout | 504 | `upstream_timeout` |
| Unexpected internal failure | 500 | `internal_error` |
| Unknown path | 404 | `not_found` |
| Wrong method | 405 | `method_not_allowed` |

Codes above are reported in `metadata.proxy_code`; `error.code` carries the
HTTP status as an integer, matching OpenRouter. Upstream 4xx/5xx responses keep
their status but their body is rewritten into the same envelope with
`metadata.provider_name` and `metadata.upstream_status`.

### Logging

Logs go to stdout as JSON by default. Access-log fields include request ID,
method, path, status, latency, bytes out, user agent, key name, the key's active
policy (`policy`, `policy_mode`, and `policy_models`), alias, provider,
billing mode, upstream model, stream flag, error code, and client-cancellation
state. A virtual key's `user_agent` setting replaces the client-supplied user
agent in the log entry.
When `logging.log_usage` is enabled, the proxy observes the response body
without changing it and adds `prompt_tokens`, `completion_tokens`,
`total_tokens`, and, when the upstream reports them, `cost_usd`,
`cache_discount`, `cached_tokens`, `cache_write_tokens`, and
`reasoning_tokens`. `cost_usd` is the upstream's `usage.cost` (for OpenRouter,
credits, 1:1 with USD); `cached_tokens` and `cache_write_tokens` come from
`usage.prompt_tokens_details`, and `reasoning_tokens` comes from
`usage.completion_tokens_details`; cache values are logged as reported,
including `0` for a cache miss. Fields the upstream omits are not logged at
all.

`billing_mode` is configured per provider. It defaults to `subscription` for
`codex` providers and is omitted otherwise. It tells a log consumer that token
usage did not incur per-token dollar cost, so a missing `cost_usd` is expected.
Requests through `/claude_sub/` are always logged with `provider: claude_sub`,
`billing_mode: subscription`, `passthrough` true, and the requested model id in
`upstream_model`.

When the upstream does not report `usage.cost` and pricing is available, the log
also includes `estimated_api_cost_usd` and `pricing_source`. This is a
models.dev or configured list-price estimate, not actual subscription billing.

Secrets, prompts, and response bodies are never logged.

#### HTTP access-log sink

Set `logging.access_sink.url` to also push access records to an HTTP collector.
This is useful when the collector runs as a separate service and should receive
application-level records directly, rather than reading the proxy's container
logs.

stdout remains enabled. Only records with `msg: "request"` are pushed; startup,
reload, and internal diagnostic logs stay on stdout. Each delivery is a `POST`
to the configured URL:

```json
{"records":[{"time":"...","level":"INFO","msg":"request","request_id":"...","status":200}]}
```

Records use the same schema as stdout, so a consumer should still deduplicate on
`request_id`. Any 2xx response is success; other statuses are retried with
backoff. Delivery is asynchronous and best-effort: a slow or unavailable
collector never blocks a client request. Retry warnings include `retry_in`; the
first few attempts and then periodic failures are logged, and a later success
logs `access log sink recovered`. When the queue is full, records are dropped
and a warning is logged with the cumulative drop count.

| Field | Default | Meaning |
| --- | --- | --- |
| `url` | required | Collector endpoint. `http` or `https`. |
| `batch_size` | `250` | Records per POST. |
| `flush_interval` | `1s` | Maximum delay before a partial batch is sent. |
| `timeout` | `5s` | Per-request HTTP timeout. |
| `queue_size` | `4096` | In-memory records awaiting delivery. |

The collector must implement `POST /ingest` and return any 2xx. The example
dashboard includes such an endpoint; see [`examples/dashboard/`](./examples/dashboard/).
For billing-grade records, treat this as an optimization rather than the only
copy: keep container stdout or another durable log sink enabled.

#### Usage log schema

With `logging.format` set to `json`, every request produces one line-delimited
JSON object on stdout. Access records have `msg` equal to `request` and are
emitted at `INFO`, except `/healthz`, which is `DEBUG`. A log consumer should
filter on `msg == "request"` and skip other records (startup, reload, and
internal errors share the same stream). Reload requests are logged like any
other request, with `path` set to `/admin/reload`.

| Field | Type | Present | Meaning |
| --- | --- | --- | --- |
| `time` | string (RFC 3339) | always | Emit time, added by the JSON handler. |
| `level` | string | always | `INFO` or `DEBUG` (see above). |
| `msg` | string | always | `request` for access records. |
| `request_id` | string | always | Also returned as `X-Request-ID`; use as the dedupe key. |
| `method` | string | always | HTTP method. |
| `path` | string | always | Request path as received. |
| `status` | int | always | HTTP status; `499` means the client closed the request. |
| `duration_ms` | float | always | Wall time from dispatch to completion. |
| `bytes_out` | int | always | Response bytes written. |
| `remote_addr` | string | when known | Client address. |
| `user_agent` | string | when known | Client user agent, or the key's `user_agent` override. |
| `key_name` | string | when authenticated | Virtual key that authorized the request. |
| `policy` | string | when the key has a policy | Attached policy name. |
| `policy_mode` | string | when the key has a policy | `whitelist` or `blacklist`. |
| `policy_models` | string[] | when the key has a policy | Policy's model list. |
| `alias` | string | when resolved | Requested model id (alias or passthrough id). |
| `provider` | string | when resolved | Configured provider name. |
| `billing_mode` | string | when set | For example `subscription`; see below. |
| `upstream_model` | string | when resolved | Model id sent upstream. |
| `passthrough` | bool | when true | Request used a passthrough route. |
| `dropped_fields` | string[] | when non-empty | Request fields removed before the upstream call. |
| `stream` | bool | when true | Response was streamed. |
| `error_code` | string | on proxy-generated errors | Machine-readable error code. |
| `error` | string | on proxy-generated errors | Human-readable error detail. |
| `client_canceled` | bool | when true | Client disconnected before completion. |
| `prompt_tokens` | int | when usage observed | Input tokens. |
| `completion_tokens` | int | when usage observed | Output tokens. |
| `total_tokens` | int | when usage observed | Total tokens. |
| `cost_usd` | float | when the upstream reports it | Upstream `usage.cost`; OpenRouter credits are 1:1 with USD. |
| `estimated_api_cost_usd` | float | when an estimate exists | List-price estimate; not a charge. |
| `pricing_source` | string | with an estimate | Catalog the estimate came from, for example `models.dev`. |
| `cache_discount` | float | when the upstream reports it | Upstream `usage.cache_discount`. |
| `cached_tokens` | int | when the upstream reports it | From `usage.prompt_tokens_details.cached_tokens`; `0` is a cache miss. |
| `cache_write_tokens` | int | when the upstream reports it | From `usage.prompt_tokens_details.cache_write_tokens`. |
| `reasoning_tokens` | int | when the upstream reports it | From `usage.completion_tokens_details.reasoning_tokens`. |

Boolean fields are logged only when true, so their absence means false. Fields
the upstream omits are not logged at all, so an absent token count means
"not observed" — an error, a non-usage endpoint, or `log_usage` disabled — not
a zero. Cache values are logged as reported, including `0`.

Notes for a consumer that stores these rows:

- Read stdout and parse one JSON object per line, or consume the batches posted
  by `logging.access_sink`.
- Deduplicate on `request_id`. Log shipping can retry, and a unique
  constraint plus an idempotent insert avoids double-counting usage.
- Keep `cost_usd` and `estimated_api_cost_usd` in separate columns. Either may
  be present without the other, and the estimate is a list price, not billing.
- Records are not guaranteed to arrive in timestamp order across concurrent
  requests. Sort by `time`, not by arrival.
- `billing_mode: subscription` means token usage did not incur per-token cost,
  so a missing `cost_usd` is expected.

`examples/dashboard/` contains a runnable TypeScript collector that accepts
this stream over `POST /ingest` (or from stdout) and stores it in SQLite, then
serves a dashboard with cost totals, a subscription estimate toggle, stacked
cost/token charts by model, provider, and user-agent with day/week/month
buckets, and a per-model breakdown.

## Reload and shutdown

Reload is exposed only over the administrative API, which listens on its own
port and is disabled by default. Enable it with two settings:

```jsonc
"server": {
  "listen": "127.0.0.1:8080",
  "admin_listen": "127.0.0.1:8081",
  "admin_token_env": "MODEL_PROXY_ADMIN_TOKEN"
}
```

```sh
export MODEL_PROXY_ADMIN_TOKEN=a-long-random-value

curl -fsS -X POST \
  -H "Authorization: Bearer $MODEL_PROXY_ADMIN_TOKEN" \
  http://127.0.0.1:8081/admin/reload
```

```json
{"status":"ok","changed":true,"config_hash":"9c1f...","models":3,"providers":2,"keys":4,"policies":1,"access_sink":false,"generation":1}
```

A reload re-reads the JSON file, its `key_file`, and its `.env`, validates
everything, rebuilds providers, and atomically swaps the config. In-flight
requests keep the config snapshot they started with. Failures are reported
inline and leave the running configuration untouched:

| Status | Meaning |
| --- | --- |
| `200` | Applied, or `"changed": false` when the config and key file are byte-identical to the last applied versions. |
| `400` | The request carried a body. The endpoint only ever re-reads the configured files. |
| `401` | Missing or wrong admin token. |
| `404` | No admin API configured, or the route was called on the public listener. |
| `409` | The change needs a restart: `server.listen`, `server.read_header_timeout`, `server.admin_listen`, or `server.admin_token_env`. |
| `422` | The file is unreadable or invalid. The previous config stays active. |

Notes:

- The admin credential is resolved once, at startup, and is never taken from a
  reloaded file, so configuration cannot grant itself permission to reload.
  Rotating it requires a restart.
- `?force=1` rebuilds even when the file is unchanged, which refreshes provider
  clients and the pricing loop.
- Administrators can be authenticated with `X-Model-Proxy-Admin` instead of
  `Authorization` when a client needs that header for something else.
- Admin routes emit no CORS headers, so they are not reachable from a browser.
- Reload is available only through this endpoint. No signal triggers one.

`GET /admin/status` returns the resolved, non-secret configuration of the
running process — model aliases, provider base URLs, virtual key names,
config/key-file paths, and policies, plus the applied `generation`,
`config_hash`, `last_reload_at`, and `last_reload_error`. API keys and virtual
key values are never included.

```sh
curl -fsS -H "Authorization: Bearer $MODEL_PROXY_ADMIN_TOKEN" \
  http://127.0.0.1:8081/admin/status
```

`SIGINT` and `SIGTERM` stop accepting requests and wait up to
`server.shutdown_timeout` for graceful shutdown.

## Docker

```sh
docker build -t model-proxy .
docker run --rm -p 8080:8080 \
  -v "$PWD/config.json:/etc/model-proxy/config.json:ro" \
  --env-file .env \
  model-proxy
```

Set `server.listen = "0.0.0.0:8080"` in the mounted config when running in a
container.

When the example dashboard runs as a separate service, set its host to
`0.0.0.0`, use the service name in `logging.access_sink.url` (for example
`http://dashboard:8787/ingest`), and keep the service on a private network:
`POST /ingest` is unauthenticated.

## Development

```sh
make check        # gofmt check, go vet, go test
make build
make integration  # opt-in live OpenRouter call, needs OPENROUTER_API_KEY
```

Default tests use a fake upstream `httptest.Server` and make no network calls.

## Non-goals for v1

No UI, database, response cache, rate limiting, key-management API, budgets,
Prometheus endpoint, fallbacks/retries, additional provider families, response model
rewriting, CORS, or live model-catalog fetching.
