# model-proxy

A small, configuration-driven, OpenAI-compatible proxy for OpenRouter. It is
deliberately not LiteLLM: no UI, database, caching, rate limiting, budgets, or
multi-provider translation layer. One Go binary, one JSON file, one upstream
provider.

## Features

- `POST /v1/chat/completions`, streaming and non-streaming.
- `GET /v1/models` backed by a strict alias registry.
- `GET /healthz` liveness endpoint.
- JWCC configuration (JSON with comments and trailing commas) with strict
  unknown-field rejection.
- Virtual keys loaded from environment variables, with optional `.env` support.
- Named model policies (whitelist or blacklist) attached to virtual keys.
- Provider-level and model-level extra request-body fields, merged per request.
- Provider-agnostic interface; the only implementation is OpenRouter.
- Transparent upstream response bodies and SSE streams.
- OpenAI-shaped errors for proxy-generated failures; upstream errors pass through.
- Token and cost extraction from responses for structured access logs.
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
modes, a key without a policy, provider/model `extra_body`, and optional
OpenRouter attribution headers.

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

  "providers": {
    "openrouter": {
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
- Policies require a non-empty `name`, a `mode` of `whitelist` or `blacklist`,
  and a non-empty `models` list of configured aliases. Policy names must be
  unique, and every key `policy` reference must exist.
- Unknown configuration fields, duplicate virtual key names, duplicate resolved key
  values, missing environment variables, invalid URLs, and zero models fail
  startup.
- `server.listen` and `read_header_timeout` changes require a restart; other
  values can be reloaded with `SIGHUP`.

### Model aliases

Clients send the alias in `model`. The proxy rewrites only that field to the
configured `upstream` before calling OpenRouter. Alias names are exact and
case-sensitive. Unlisted names return `404 model_not_found`.

A key with a `whitelist` policy may use only the listed aliases. A key with a
`blacklist` policy may use every alias except those listed. Using a disallowed
alias returns `403 model_not_allowed` and the request is not sent upstream.
`GET /v1/models` is filtered to the aliases allowed by the authenticated key.

Requests containing `models`, `route`, or `plugins` are rejected with `400`.
Those OpenRouter fields can bypass the alias registry or add paid features.
Other fields, including `provider` and `transforms`, are passed through
untouched. There are no proxy-level retries or fallbacks.

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

These are OpenRouter request-body fields, not HTTP headers; OpenRouter reads
`provider.zdr` and `provider.data_collection` from the body. `model`,
`messages`, `stream`, `models`, `route`, and `plugins` may not be set in
`extra_body`; config load fails if they appear.

## Behavior

- Success bodies and SSE streams are forwarded byte-for-byte.
- The proxy does not inject `usage: {include: true}`; OpenRouter includes usage
  and cost in responses by default.
- `X-Model-Proxy-Alias` and `X-Model-Proxy-Upstream` describe the selected
  route. `X-Request-ID` is accepted from the client if valid, otherwise
  generated, forwarded upstream, echoed to the client, and included in logs.
- Client headers forwarded upstream are limited to `Content-Type`, `Accept`, and
  a sanitized `X-Request-ID`. The proxy sets `Authorization`,
  `Content-Type: application/json`, and optional attribution headers.
- Upstream response headers are copied except hop-by-hop headers,
  `Content-Length`, `Set-Cookie`, and proxy-owned headers.
- Automatic transport compression is disabled so response bytes are never
  silently decompressed.
- There is no CORS support and no upstream model-catalog call.

### Proxy-generated errors

| Condition | Status | Code |
| --- | --- | --- |
| Missing/invalid virtual key | 401 | `invalid_api_key` |
| Alias disallowed by the key's policy | 403 | `model_not_allowed` |
| Unknown alias | 404 | `model_not_found` |
| Malformed JSON or forbidden field | 400 | `invalid_request` |
| Body over `max_body_bytes` | 413 | `request_too_large` |
| Upstream unreachable | 502 | `upstream_unavailable` |
| Upstream response-header timeout | 504 | `upstream_timeout` |
| Unexpected internal failure | 500 | `internal_error` |
| Unknown path | 404 | `not_found` |
| Wrong method | 405 | `method_not_allowed` |

Upstream 4xx/5xx responses are returned with their original status and body.

### Logging

Logs go to stderr as JSON by default. Access-log fields include request ID,
method, path, status, latency, bytes out, user agent, key name, the key's active
policy (`policy`, `policy_mode`, and `policy_models`), alias, provider, upstream
model, stream flag, error code, and client-cancellation state. When
`logging.log_usage` is enabled, the proxy observes the response body without
changing it and adds `prompt_tokens`, `completion_tokens`, `total_tokens`, and
`cost_usd` when available. `cost_usd` is OpenRouter's `usage.cost` (credits,
1:1 with USD).

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
Prometheus endpoint, fallbacks/retries, additional providers, response model
rewriting, CORS, or live model-catalog fetching.
