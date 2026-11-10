# model-proxy

A small, configuration-driven, OpenAI-compatible proxy for OpenRouter and any
OpenAI-compatible upstream, including
[Private AI Gateway](https://github.com/Dstack-TEE/private-ai-gateway). It is
deliberately not LiteLLM: no UI, database, caching, rate limiting, budgets, or
multi-provider translation layer. One Go binary, one JSON file, and routes to
one or more upstream providers.

## Features

- `POST /v1/chat/completions`, streaming and non-streaming, with OpenRouter's
  usage shape and cost fields.
- `POST /api/v1/chat/completions`, `/api/v1/responses`, and `/api/v1/models`
  aliases for clients that hardcode OpenRouter's base URL.
- `POST /v1/responses` for Responses-native clients such as Codex, streaming
  and non-streaming, with transparent response passthrough.
- `GET /v1/models` backed by a strict alias registry, optionally merged with
  models discovered from providers that allow passthrough.
- OpenRouter-compatible by default: OpenRouter-shaped model listings, usage
  costs, error envelopes, request-field tolerance, path aliases, and CORS.
- `GET /healthz` liveness endpoint.
- JWCC configuration (JSON with comments and trailing commas) with strict
  unknown-field rejection.
- Virtual keys loaded from environment variables, with optional `.env` support.
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
- Subscription billing-mode tagging for providers such as Codex.
- Optional models.dev list-price estimates with a configurable refresh timer.
- Atomic `SIGHUP` reload.

## Quick start

```sh
cp config.example.json config.json
cp .env.example .env
chmod 600 .env
# edit config.json and .env

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

See [`config.example.json`](./config.example.json) for a complete example. The
config is JWCC (JSON with Commas and Comments): `//` line comments, `/* */`
block comments, and trailing commas are accepted. The example shows both policy
modes, a key without a policy, provider/model `extra_body`, optional OpenRouter
attribution headers, and a Private AI Gateway provider with ACI constraints.

```jsonc
// model-proxy configuration.
//
// This is JWCC (JSON with comments and trailing commas). Unknown fields are
// rejected. Durations use Go syntax, e.g. "10s" or "1m30s". Secrets are never
// placed directly in this file; key_env names are resolved from the process
// environment first, then from env_file.
{
  "env_file": ".env", // Optional; relative paths are resolved from this file.

  "server": {
    "listen": "127.0.0.1:8080",
    "max_body_bytes": 1048576,
    "read_header_timeout": "10s",
    "shutdown_timeout": "15s",
  },

  "logging": {
    "format": "json", // json | text
    "level": "info",  // debug | info | warn | error
    "log_usage": true,
  },

  "auth": {
    "enabled": true, // When enabled, at least one virtual key must resolve.
  },

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
  "virtual_keys": [
    {
      "name": "laptop",
      "key_env": "PROXY_KEY_LAPTOP",
      "policy": "basic",
    },
    {
      "name": "ci",
      "key_env": "PROXY_KEY_CI",
      "policy": "no-smart",
      // Optional access-log User-Agent override for requests using this key.
      "user_agent": "Open WebUI",
    },
    {
      "name": "admin",
      "key_env": "PROXY_KEY_ADMIN",
    },
  ],

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

Rules:

- All durations are Go duration strings such as `"10s"` or `"2m"`.
- `env_file` is optional and relative to the config file when not absolute.
- Process environment variables take precedence over `.env` values.
- Virtual keys use `key_env` only; plaintext `key` fields are rejected.
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
- Unknown configuration fields, duplicate virtual key names, duplicate resolved key
  values, missing environment variables, invalid URLs, and zero models fail
  startup.
- `server.listen` and `read_header_timeout` changes require a restart; other
  values can be reloaded with `SIGHUP`.
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

Private AI Gateway is not a separate implementation. Configure it as an
`openai-compatible` provider. The Phala-hosted deployment is available at
`https://inference.phala.com/v1`.

`base_url` is the provider's API root and must include its version prefix. The
proxy appends `/chat/completions`, so `https://inference.phala.com/v1` becomes
`https://inference.phala.com/v1/chat/completions`. A self-hosted gateway works
the same way; use its listener plus `/v1`, for example
`http://gateway:8086/v1`.

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
under `models`. The requested id is sent upstream unchanged, so a new upstream
model needs no config change:

```jsonc
"codex": {
  "type": "codex",
  "base_url": "https://chatgpt.com/backend-api/codex",
  "auth_dir": "~/.codex",
  "passthrough_models": true
}
```

With that config, `{"model": "gpt-5.6-sol"}` is forwarded to the Codex backend
as `gpt-5.6-sol`. Configured aliases always win over passthrough, so keep using
an alias when you want a stable name or request-body overrides.

- **One provider only.** An unlisted id is routed to the single provider with
  `passthrough_models` enabled. If several providers enable it, the id is
  ambiguous and the request fails with `400 invalid_request` naming the
  candidates; add the model to `models` to choose one.
- **Policies still apply.** A `whitelist` key may only use the names it lists, so
  it cannot use passthrough ids. A `blacklist` key may use passthrough ids it
  does not list. Passthrough requests are logged with `"passthrough": true`,
  and the alias equals the upstream model id.
- **Provider defaults apply.** Provider-level `extra_body`, `billing_mode`, and
  `pricing_provider` are used. Model-level `extra_body` and `pricing` overrides
  require a configured alias.
- **`GET /v1/models` includes discovered ids.** For each passthrough provider the
  proxy asks the upstream for its model list and merges it with the aliases,
  honoring the authenticated key's policy. Results are cached for one minute and
  cleared on `SIGHUP`; a failing or unsupported listing is logged and skipped, so
  the aliases are still returned.

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
| Paths | `/api/v1/chat/completions`, `/api/v1/responses`, and `/api/v1/models` serve the same handlers |
| CORS | `Access-Control-Allow-*` headers and `OPTIONS` preflight |

Model metadata for the listing comes from the pricing catalog, keyed by the
provider's `pricing_provider` and the model's `upstream` id. Explicit model
`pricing` in the config wins over the catalog. Models the catalog does not know
are listed without `pricing` and with a minimal architecture rather than
invented numbers. `supported_parameters` is conservative: it reports `tools`
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
successful fetch. `SIGHUP` rebuilds the pricing loop along with the rest of the
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
| Body over `max_body_bytes` | 413 | `request_too_large` |
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

Logs go to stderr as JSON by default. Access-log fields include request ID,
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

When the upstream does not report `usage.cost` and pricing is available, the log
also includes `estimated_api_cost_usd` and `pricing_source`. This is a
models.dev or configured list-price estimate, not actual subscription billing.

Secrets, prompts, and response bodies are never logged.

## Reload and shutdown

```sh
kill -HUP "$(pidof model-proxy)"
```

`SIGHUP` re-reads the JSON file and its `.env`, validates everything, rebuilds
providers, and atomically swaps the config. Invalid reloads leave the old config
active and log an error. In-flight requests keep the config snapshot they
started with.

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
