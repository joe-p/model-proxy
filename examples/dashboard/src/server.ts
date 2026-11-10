import { readFileSync } from 'node:fs';
import {
  createServer as createHttpServer,
  type IncomingMessage,
  type ServerResponse,
} from 'node:http';
import type { ModelRow, Summary, TimeseriesRow, UsageStore } from './db.js';
import { parseRecord, type AccessLog } from './parse.js';
import {
  nextBucketStart,
  parseBucket,
  startOfBucket,
  type Bucket,
} from './time.js';

interface Costed {
  reportedCostUsd: number;
  subscriptionEstimateUsd: number;
  apiEstimateUsd: number;
}

type WithTotal<T> = T & { totalCostUsd: number };

type Metric = 'cost' | 'tokens';

interface TimeseriesBucket {
  start: string;
  end: string;
  segments: WithTotal<ModelRow>[];
}

const MAX_INGEST_BYTES = 8 * 1024 * 1024;

class HttpError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
  }
}

// Total = reported cost + estimated API cost + optionally the subscription
// estimate. Estimated and reported cost never overlap on a single row.
function withTotal<T extends Costed>(row: T, includeSubscription: boolean): WithTotal<T> {
  const totalCostUsd =
    row.reportedCostUsd +
    row.apiEstimateUsd +
    (includeSubscription ? row.subscriptionEstimateUsd : 0);
  return { ...row, totalCostUsd };
}

function modelKey(model: string, provider: string): string {
  return `${model}\u0000${provider}`;
}

function modelMetric(row: WithTotal<ModelRow>, metric: Metric): number {
  return metric === 'tokens' ? row.promptTokens + row.completionTokens : row.totalCostUsd;
}

function toSegment(row: TimeseriesRow, includeSubscription: boolean): WithTotal<ModelRow> {
  return withTotal(
    {
      model: row.model,
      provider: row.provider,
      requests: row.requests,
      errors: row.errors,
      promptTokens: row.promptTokens,
      completionTokens: row.completionTokens,
      reportedCostUsd: row.reportedCostUsd,
      subscriptionEstimateUsd: row.subscriptionEstimateUsd,
      apiEstimateUsd: row.apiEstimateUsd,
    },
    includeSubscription,
  );
}

function buildTimeseries(
  rows: TimeseriesRow[],
  fromMs: number,
  toMs: number,
  bucket: Bucket,
  includeSubscription: boolean,
  modelOrder: Map<string, number>,
): TimeseriesBucket[] {
  const byStart = new Map<number, TimeseriesRow[]>();
  for (const row of rows) {
    const list = byStart.get(row.bucketStartMs);
    if (list) list.push(row);
    else byStart.set(row.bucketStartMs, [row]);
  }

  const buckets: TimeseriesBucket[] = [];
  const last = startOfBucket(bucket, toMs);
  for (
    let start = startOfBucket(bucket, fromMs);
    start <= last;
    start = nextBucketStart(bucket, start)
  ) {
    const segments = (byStart.get(start) ?? [])
      .map((row) => toSegment(row, includeSubscription))
      .sort((a, b) => {
        const rankA = modelOrder.get(modelKey(a.model, a.provider)) ?? Number.MAX_SAFE_INTEGER;
        const rankB = modelOrder.get(modelKey(b.model, b.provider)) ?? Number.MAX_SAFE_INTEGER;
        return (
          rankA - rankB ||
          a.model.localeCompare(b.model) ||
          a.provider.localeCompare(b.provider)
        );
      });

    buckets.push({
      start: new Date(start).toISOString(),
      end: new Date(nextBucketStart(bucket, start)).toISOString(),
      segments,
    });
  }
  return buckets;
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

function parseMetric(raw: string | null): Metric {
  return raw === 'tokens' ? 'tokens' : 'cost';
}

function handleUsage(store: UsageStore, res: ServerResponse, url: URL): void {
  const hours = parseHours(url.searchParams.get('hours'));
  const bucket = parseBucket(url.searchParams.get('bucket'));
  const metric = parseMetric(url.searchParams.get('metric'));
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

  const modelOrder = new Map(
    [...models]
      .sort((a, b) => modelMetric(b, metric) - modelMetric(a, metric))
      .map((row, index) => [modelKey(row.model, row.provider), index]),
  );
  const timeseries = buildTimeseries(
    store.timeseries(fromMs, toMs, bucket),
    fromMs,
    toMs,
    bucket,
    includeSubscription,
    modelOrder,
  );

  sendJson(res, 200, {
    range: {
      hours,
      from: new Date(fromMs).toISOString(),
      to: new Date(toMs).toISOString(),
    },
    bucket,
    metric,
    includeSubscription,
    summary,
    models,
    timeseries,
  });
}

async function readBody(req: IncomingMessage): Promise<string> {
  const chunks: Buffer[] = [];
  let size = 0;
  for await (const chunk of req) {
    const buffer = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
    size += buffer.length;
    if (size > MAX_INGEST_BYTES) {
      throw new HttpError(413, 'request body too large');
    }
    chunks.push(buffer);
  }
  return Buffer.concat(chunks).toString('utf8');
}

function extractRecords(body: unknown): unknown[] | null {
  if (Array.isArray(body)) return body;
  if (body !== null && typeof body === 'object') {
    const records = (body as Record<string, unknown>).records;
    if (Array.isArray(records)) return records;
  }
  return null;
}

async function handleIngest(
  store: UsageStore,
  req: IncomingMessage,
  res: ServerResponse,
): Promise<void> {
  const body = await readBody(req);

  let payload: unknown;
  try {
    payload = JSON.parse(body);
  } catch {
    throw new HttpError(400, 'invalid JSON');
  }

  const rawRecords = extractRecords(payload);
  if (!rawRecords) {
    throw new HttpError(400, 'expected {"records":[...]}');
  }

  const rows: AccessLog[] = [];
  for (const raw of rawRecords) {
    const row = parseRecord(raw);
    if (row) rows.push(row);
  }
  store.insertMany(rows);

  sendJson(res, 202, {
    accepted: rows.length,
    skipped: rawRecords.length - rows.length,
  });
}

async function handleRequest(
  store: UsageStore,
  req: IncomingMessage,
  res: ServerResponse,
): Promise<void> {
  try {
    const url = new URL(req.url ?? '/', 'http://localhost');

    // model-proxy pushes batches here. The endpoint is intentionally separate
    // from the read-only dashboard API.
    if (url.pathname === '/ingest') {
      if (req.method !== 'POST') {
        sendJson(res, 405, { error: 'method not allowed' });
        return;
      }
      await handleIngest(store, req, res);
      return;
    }

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
    if (res.headersSent) {
      res.destroy();
      return;
    }
    const status = err instanceof HttpError ? err.status : 500;
    sendJson(res, status, { error: err instanceof Error ? err.message : String(err) });
  }
}

export function createServer(store: UsageStore) {
  return createHttpServer((req, res) => {
    void handleRequest(store, req, res);
  });
}
