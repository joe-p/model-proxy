import { readFileSync } from 'node:fs';
import { createServer as createHttpServer, type ServerResponse } from 'node:http';
import type { ModelRow, Summary, UsageStore } from './db.js';

interface Costed {
  reportedCostUsd: number;
  subscriptionEstimateUsd: number;
  apiEstimateUsd: number;
}

type WithTotal<T> = T & { totalCostUsd: number };

// Total = reported cost + estimated API cost + optionally the subscription
// estimate. Estimated and reported cost never overlap on a single row.
function withTotal<T extends Costed>(row: T, includeSubscription: boolean): WithTotal<T> {
  const totalCostUsd =
    row.reportedCostUsd +
    row.apiEstimateUsd +
    (includeSubscription ? row.subscriptionEstimateUsd : 0);
  return { ...row, totalCostUsd };
}

let cachedHtml: string | undefined;

function dashboardHtml(): string {
  cachedHtml ??= readFileSync(new URL('../public/index.html', import.meta.url), 'utf8');
  return cachedHtml;
}

function sendJson(res: ServerResponse, status: number, body: unknown): void {
  const payload = JSON.stringify(body);
  res.writeHead(status, {
    'content-type': 'application/json; charset=utf-8',
    'content-length': Buffer.byteLength(payload),
    'cache-control': 'no-store',
  });
  res.end(payload);
}

function parseHours(raw: string | null): number {
  if (raw === null) return 24;
  const value = Number(raw);
  if (!Number.isFinite(value)) return 24;
  return Math.min(Math.max(Math.trunc(value), 1), 24 * 365);
}

function handleUsage(store: UsageStore, res: ServerResponse, url: URL): void {
  const hours = parseHours(url.searchParams.get('hours'));
  const includeSubscription =
    url.searchParams.get('includeSubscription') === '1' ||
    url.searchParams.get('includeSubscription') === 'true';

  const toMs = Date.now();
  const fromMs = toMs - hours * 60 * 60 * 1000;

  const summary: WithTotal<Summary> = withTotal(store.summary(fromMs, toMs), includeSubscription);
  const models: WithTotal<ModelRow>[] = store
    .models(fromMs, toMs)
    .map((row) => withTotal(row, includeSubscription))
    .sort((a, b) => b.totalCostUsd - a.totalCostUsd);

  sendJson(res, 200, {
    range: {
      hours,
      from: new Date(fromMs).toISOString(),
      to: new Date(toMs).toISOString(),
    },
    includeSubscription,
    summary,
    models,
  });
}

export function createServer(store: UsageStore) {
  return createHttpServer((req, res) => {
    try {
      const url = new URL(req.url ?? '/', 'http://localhost');

      if (req.method !== 'GET' && req.method !== 'HEAD') {
        sendJson(res, 405, { error: 'method not allowed' });
        return;
      }

      if (url.pathname === '/favicon.ico') {
        res.writeHead(204);
        res.end();
        return;
      }

      if (url.pathname === '/') {
        const html = dashboardHtml();
        res.writeHead(200, {
          'content-type': 'text/html; charset=utf-8',
          'content-length': Buffer.byteLength(html),
          'cache-control': 'no-store',
        });
        res.end(html);
        return;
      }

      if (url.pathname === '/healthz') {
        res.writeHead(200, { 'content-type': 'text/plain' });
        res.end('ok');
        return;
      }

      if (url.pathname === '/api/usage') {
        handleUsage(store, res, url);
        return;
      }

      sendJson(res, 404, { error: 'not found' });
    } catch (err) {
      sendJson(res, 500, { error: err instanceof Error ? err.message : String(err) });
    }
  });
}
