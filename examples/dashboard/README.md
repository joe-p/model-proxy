# Usage dashboard example

A small TypeScript collector that turns model-proxy's JSON access records into a
usage dashboard. It accepts records over HTTP from model-proxy's access sink, or
from stdout/file for offline replay, stores them in SQLite, then serves a page
with cost totals, a subscription-estimate toggle, stacked cost-or-token charts
by model, provider, and user-agent product with day/week/month buckets, and a
per-model breakdown.

It has **no runtime dependencies**. It uses `node:sqlite` (Node 22.13+) and
`tsx` only to run TypeScript directly.

> This is an example, not a hardened service. It binds to `127.0.0.1` and has no
> authentication, including on `POST /ingest`. Put it behind a reverse proxy or
> keep it on a private Docker network if you expose it.

## Run it

Install the dev tools once:

```sh
cd examples/dashboard
npm install
```

Run the collector and dashboard:

```sh
npm start -- --db usage.db
```

Then point model-proxy at its ingest endpoint:

```jsonc
"logging": {
  "format": "json",
  "level": "info",
  "log_usage": true,
  "access_sink": {
    "url": "http://127.0.0.1:8787/ingest"
  }
}
```

The proxy keeps writing access records to stdout too. Only records with
`msg: "request"` are pushed. Delivery is asynchronous and best-effort; see the
project README for `batch_size`, `flush_interval`, `timeout`, and `queue_size`.

In separate Docker services, bind the dashboard to the network interface
(`--host 0.0.0.0`) and use the service name in the sink URL, for example
`http://dashboard:8787/ingest`. No host port is required.

Open <http://127.0.0.1:8787>. Do not open `public/index.html` directly: the page
needs the collector's `/api/usage` endpoint.

### Offline and development modes

Wrap a live proxy (the collector spawns it and reads its stdout):

```sh
npm start -- --db usage.db -- ../../bin/model-proxy -config ../../config.json
```

Replay an existing log file:

```sh
npm start -- --file sample.log
```

Read from a pipe:

```sh
../../bin/model-proxy -config ../../config.json | npm start -- --stdin
```

Options: `--db <path>`, `--port <n>`, `--host <addr>`. Run
`npm start -- --help` for the list.

## HTTP ingest

`POST /ingest` accepts one batch:

```json
{"records":[{"time":"2026-01-01T00:00:00Z","level":"INFO","msg":"request","request_id":"abc","status":200}]}
```

It responds with `202` and `{"accepted":n,"skipped":n}`. A malformed body gets
`400`; an oversized body gets `413`. Rows are inserted in a single transaction.

## HTTP JSON API

`GET /api/usage` returns the data rendered by the page:

```
/api/usage?hours=168&bucket=day&metric=cost&includeSubscription=1
```

`hours` is the lookback window. `bucket` is `day`, `week`, or `month`; buckets
are UTC and weeks start on Monday. `metric` is `cost` or `tokens`; it controls
the charts' stacking order and legend totals.

The response includes `summary`, the model table's `models` array, and chart
data for `timeseries` (model), `providers`, and `userAgents`. The provider and
user-agent objects each contain `groups` and `timeseries`. Each bucket has
`start`, `end`, and `segments` with `label`, `detail`, reported/estimated/total
cost, and token counts. Empty buckets are included so the time axis is
continuous. User-agent segments are grouped by the product token before the
first `/`, so `curl/8.5.0` and `curl/8.4.0` share one series.

## What it computes

| Metric | Source |
| --- | --- |
| Reported cost | `cost_usd` (upstream-reported) |
| API estimate | `estimated_api_cost_usd` on rows with no reported cost and a billing mode other than `subscription` |
| Subscription estimate | `estimated_api_cost_usd` on rows with `billing_mode: "subscription"` |
| Total cost | reported + API estimate + subscription estimate when the toggle is on |

Estimated and reported cost are never summed on the same row, so the total
cannot double-count. The toggle only affects the subscription estimate: that
usage did not incur a per-token charge, so it is off by default.

The model chart groups by `alias` (falling back to `upstream_model`) and
provider, with the provider shown as the segment detail. The provider chart
groups by `provider`. The user-agent chart groups by the user-agent product
before the first `/`. Token counts are the prompt/completion sums from the log.
Each chart stacks total cost or prompt/completion tokens per time bucket, using
the same subscription toggle as the cards when the cost metric is selected.
Hover a segment for its bucket, group, selected metric, the other metric, and
request/error counts.

## How it works

```
stdout ──readline────────────────────┐
POST /ingest ──{"records":[...]}─────┴──▶ parse.ts ──▶ SQLite ──▶ /api/usage
```

- `src/parse.ts` keeps only `msg: "request"` records and drops `/healthz`.
- `src/server.ts` serves the page, the JSON API, and `POST /ingest`.
- `src/time.ts` defines the UTC day/week/month buckets shared by SQLite and the
  API.
- `src/db.ts` stores rows with `request_id` as the primary key, so replaying a
  log, retrying an HTTP batch, or restarting the collector does not double-count.
- `src/ingest.ts` handles stdout/file modes and batches 250 rows or flushes
  every second.

The collector's own diagnostics go to stderr so they never contaminate the
parsed log stream.

## Notes and limits

- Rows are keyed by `request_id`, so re-ingesting the same batch is safe.
- HTTP ingest is unauthenticated. Keep the dashboard on a private network or put
  auth in front of it.
- Nothing prunes old rows. For long retention, add a delete job (for example,
  `DELETE FROM usage WHERE ts_ms < ?`) or partition the table by month.
- The dashboard queries on each request with no cache; fine at personal scale.
- Node's `node:sqlite` may print an experimental warning depending on the Node
  version. It is functional and requires no native build step.
