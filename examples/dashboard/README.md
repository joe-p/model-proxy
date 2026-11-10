# Usage dashboard example

A small TypeScript collector that turns model-proxy's JSON access log into a
usage dashboard. It parses the log stream into SQLite, then serves a page with
cost totals, a subscription-estimate toggle, and a per-model breakdown.

It has **no runtime dependencies**. It uses `node:sqlite` (Node 22.13+) and
`tsx` only to run TypeScript directly.

> This is an example, not a hardened service. It binds to `127.0.0.1` and has no
> authentication. Put it behind a reverse proxy if you expose it.

## Run it

Install the dev tools once:

```sh
cd examples/dashboard
npm install
```

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

Then open <http://127.0.0.1:8787>. Do not open `public/index.html` directly:
the page needs the collector's `/api/usage` endpoint.

Options: `--db <path>`, `--port <n>`, `--host <addr>`. Run
`npm start -- --help` for the list.

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

The model breakdown groups by `alias` (falling back to `upstream_model`) and
provider. Token counts are the prompt/completion sums from the log.

## How it works

```
proxy stdout ──▶ readline ──▶ parse.ts ──▶ SQLite ──▶ /api/usage ──▶ dashboard
```

- `src/parse.ts` keeps only `msg: "request"` records and drops `/healthz`.
- `src/db.ts` stores rows with `request_id` as the primary key, so replaying a
  log or restarting the collector does not double-count. Inserts are batched in
  a transaction.
- `src/ingest.ts` batches 250 rows or flushes every second.
- `src/server.ts` serves the page and the JSON API.

The collector's own diagnostics go to stderr so they never contaminate the
parsed log stream.

## Notes and limits

- Rows are keyed by `request_id`, so re-ingesting the same file is safe.
- Nothing prunes old rows. For long retention, add a delete job (for example,
  `DELETE FROM usage WHERE ts_ms < ?`) or partition the table by month.
- The dashboard queries on each request with no cache; fine at personal scale.
- Node's `node:sqlite` may print an experimental warning depending on the Node
  version. It is functional and requires no native build step.
