# model-proxy

A small, configuration-driven, OpenAI-compatible proxy for OpenRouter. It is
deliberately not LiteLLM: no UI, database, caching, rate limiting, budgets, or
multi-provider translation layer. One Go binary, one TOML file, one upstream
provider.

## Features

- `POST /v1/chat/completions`, streaming and non-streaming.
- `GET /v1/models` backed by a strict alias registry.
- `GET /healthz` liveness endpoint.
- TOML configuration with strict unknown-field rejection.
- Virtual keys loaded from environment variables, with optional `.env` support.
- Named model policies (whitelist or blacklist) attached to virtual keys.
- Provider-agnostic interface; the only implementation is OpenRouter.
- Transparent upstream response bodies and SSE streams.
- OpenAI-shaped errors for proxy-generated failures; upstream errors pass through.
- Token and cost extraction from responses for structured access logs.
- Atomic `SIGHUP` reload.

## Quick start

```sh
cp config.example.toml config.toml
cp .env.example .env
chmod 600 .env
# edit config.toml and .env

make build
./bin/model-proxy -config ./config.toml
```

Or with the default `./config.toml`:

```sh
go run ./cmd/model-proxy
```

`MODEL_PROXY_CONFIG` can supply the default config path.

```sh
export MODEL_PROXY_CONFIG=/etc/model-proxy/config.toml
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

See [`config.example.toml`](./config.example.toml) for the full schema.

```toml
env_file = ".env"

[server]
listen = "127.0.0.1:8080"
max_body_bytes = 1048576
read_header_timeout = "10s"
shutdown_timeout = "15s"

[logging]
format = "json"          # json | text
level = "info"           # debug | info | warn | error
log_usage = true

[auth]
enabled = true

[[virtual_keys]]
name = "laptop"
key_env = "PROXY_KEY_LAPTOP"
policy = "basic"          # optional

# Optional: restrict which model aliases a key may use.
[[policies]]
name = "basic"
mode = "whitelist"        # whitelist | blacklist
models = ["fast"]

[providers.openrouter]
base_url = "https://openrouter.ai/api/v1"
api_key_env = "OPENROUTER_API_KEY"
dial_timeout = "10s"
response_header_timeout = "120s"
# referer = "https://example.com"
# title = "model-proxy"

[models.fast]
provider = "openrouter"
upstream = "openai/gpt-4o-mini"
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
- Unknown TOML fields, duplicate key names, duplicate resolved key values,
  missing environment variables, invalid URLs, and zero models fail startup.
- `[server].listen` and `read_header_timeout` changes require a restart; other
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

`SIGHUP` re-reads the TOML file and its `.env`, validates everything, rebuilds
providers, and atomically swaps the config. Invalid reloads leave the old config
active and log an error. In-flight requests keep the config snapshot they
started with.

`SIGINT` and `SIGTERM` stop accepting requests and wait up to
`server.shutdown_timeout` for graceful shutdown.

## Docker

```sh
docker build -t model-proxy .
docker run --rm -p 8080:8080 \
  -v "$PWD/config.toml:/etc/model-proxy/config.toml:ro" \
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
