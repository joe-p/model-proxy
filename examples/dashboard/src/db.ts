import { DatabaseSync } from 'node:sqlite';
import type { AccessLog } from './parse.js';
import {
  bucketSqlExpression,
  bucketStartFromKey,
  type Bucket,
} from './time.js';

export interface Summary {
  requests: number;
  errors: number;
  streamed: number;
  reportedCostUsd: number;
  subscriptionEstimateUsd: number;
  apiEstimateUsd: number;
  promptTokens: number;
  completionTokens: number;
  totalTokens: number;
  avgDurationMs: number;
}

export type Breakdown = 'model' | 'provider' | 'userAgent';

export interface GroupRow {
  label: string;
  detail: string;
  requests: number;
  errors: number;
  promptTokens: number;
  completionTokens: number;
  reportedCostUsd: number;
  subscriptionEstimateUsd: number;
  apiEstimateUsd: number;
}

export interface GroupTimeseriesRow extends GroupRow {
  bucketStartMs: number;
}

const SCHEMA = `
CREATE TABLE IF NOT EXISTS usage (
  request_id             TEXT PRIMARY KEY,
  ts_ms                  INTEGER NOT NULL,
  time                   TEXT NOT NULL,
  method                 TEXT NOT NULL,
  path                   TEXT NOT NULL,
  status                 INTEGER NOT NULL,
  duration_ms            REAL NOT NULL,
  bytes_out              INTEGER NOT NULL,
  remote_addr            TEXT,
  user_agent             TEXT,
  key_name               TEXT,
  alias                  TEXT,
  provider               TEXT,
  billing_mode           TEXT,
  upstream_model         TEXT,
  stream                 INTEGER NOT NULL DEFAULT 0,
  prompt_tokens          INTEGER,
  completion_tokens      INTEGER,
  total_tokens           INTEGER,
  cost_usd               REAL,
  estimated_api_cost_usd REAL,
  pricing_source         TEXT,
  error_code             TEXT,
  client_canceled        INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS usage_ts ON usage (ts_ms);
CREATE INDEX IF NOT EXISTS usage_alias_ts ON usage (alias, ts_ms);
`;

const INSERT = `
INSERT OR IGNORE INTO usage (
  request_id, ts_ms, time, method, path, status, duration_ms, bytes_out,
  remote_addr, user_agent, key_name, alias, provider, billing_mode,
  upstream_model, stream, prompt_tokens, completion_tokens, total_tokens,
  cost_usd, estimated_api_cost_usd, pricing_source, error_code, client_canceled
) VALUES (
  :requestId, :tsMs, :time, :method, :path, :status, :durationMs, :bytesOut,
  :remoteAddr, :userAgent, :keyName, :alias, :provider, :billingMode,
  :upstreamModel, :stream, :promptTokens, :completionTokens, :totalTokens,
  :costUsd, :estimatedApiCostUsd, :pricingSource, :errorCode, :clientCanceled
)
`;

// Reported cost is always counted. Estimated cost is only counted on a row
// whose upstream reported no cost, so the two can never double-count. The
// subscription estimate is separated so the dashboard can toggle it.
const SUMMARY = `
SELECT
  COUNT(*) AS requests,
  COALESCE(SUM(CASE WHEN status >= 400 OR error_code IS NOT NULL THEN 1 ELSE 0 END), 0) AS errors,
  COALESCE(SUM(CASE WHEN stream = 1 THEN 1 ELSE 0 END), 0) AS streamed,
  COALESCE(SUM(cost_usd), 0) AS reported_cost_usd,
  COALESCE(SUM(CASE WHEN cost_usd IS NULL AND billing_mode = 'subscription'
                    THEN estimated_api_cost_usd ELSE 0 END), 0) AS subscription_estimate_usd,
  COALESCE(SUM(CASE WHEN cost_usd IS NULL AND COALESCE(billing_mode, '') <> 'subscription'
                    THEN estimated_api_cost_usd ELSE 0 END), 0) AS api_estimate_usd,
  COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens,
  COALESCE(SUM(completion_tokens), 0) AS completion_tokens,
  COALESCE(SUM(total_tokens), 0) AS total_tokens,
  COALESCE(AVG(duration_ms), 0) AS avg_duration_ms
FROM usage
WHERE ts_ms >= ? AND ts_ms < ?
`;

const AGGREGATES = `
  COUNT(*) AS requests,
  COALESCE(SUM(CASE WHEN status >= 400 OR error_code IS NOT NULL THEN 1 ELSE 0 END), 0) AS errors,
  COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens,
  COALESCE(SUM(completion_tokens), 0) AS completion_tokens,
  COALESCE(SUM(cost_usd), 0) AS reported_cost_usd,
  COALESCE(SUM(CASE WHEN cost_usd IS NULL AND billing_mode = 'subscription'
                    THEN estimated_api_cost_usd ELSE 0 END), 0) AS subscription_estimate_usd,
  COALESCE(SUM(CASE WHEN cost_usd IS NULL AND COALESCE(billing_mode, '') <> 'subscription'
                    THEN estimated_api_cost_usd ELSE 0 END), 0) AS api_estimate_usd
`;

interface Dimension {
  label: string;
  detail: string;
}

const DIMENSIONS: Record<Breakdown, Dimension> = {
  model: {
    label: "COALESCE(alias, upstream_model, 'unknown')",
    detail: "COALESCE(provider, '')",
  },
  provider: {
    label: "COALESCE(NULLIF(provider, ''), 'unknown')",
    detail: "''",
  },
  userAgent: {
    // Group by the product token before the first '/', ignoring its version.
    label: `COALESCE(NULLIF(TRIM(CASE
      WHEN user_agent IS NULL OR TRIM(user_agent) = '' THEN 'unknown'
      WHEN INSTR(user_agent, '/') > 0 THEN SUBSTR(user_agent, 1, INSTR(user_agent, '/') - 1)
      ELSE user_agent
    END), ''), 'unknown')`,
    detail: "''",
  },
};

function groupsSql(breakdown: Breakdown): string {
  const { label, detail } = DIMENSIONS[breakdown];
  return `
SELECT
  ${label} AS label,
  ${detail} AS detail,
  ${AGGREGATES}
FROM usage
WHERE ts_ms >= ? AND ts_ms < ?
GROUP BY label, detail
`;
}

function timeseriesSql(bucket: Bucket, breakdown: Breakdown): string {
  const bucketKey = bucketSqlExpression(bucket);
  const { label, detail } = DIMENSIONS[breakdown];
  return `
SELECT
  ${bucketKey} AS bucket_key,
  ${label} AS label,
  ${detail} AS detail,
  ${AGGREGATES}
FROM usage
WHERE ts_ms >= ? AND ts_ms < ?
GROUP BY bucket_key, label, detail
ORDER BY bucket_key, label, detail
`;
}

type Row = Record<string, string | number | null>;

function n(value: string | number | null | undefined): number {
  return typeof value === 'number' ? value : 0;
}

function s(value: string | number | null | undefined): string {
  return typeof value === 'string' ? value : '';
}

export class UsageStore {
  private readonly db: DatabaseSync;
  private readonly insertStmt: ReturnType<DatabaseSync['prepare']>;
  private readonly summaryStmt: ReturnType<DatabaseSync['prepare']>;
  private readonly groupStmts: Record<Breakdown, ReturnType<DatabaseSync['prepare']>>;
  private readonly timeseriesStmts: Record<
    Breakdown,
    Record<Bucket, ReturnType<DatabaseSync['prepare']>>
  >;

  constructor(path: string) {
    this.db = new DatabaseSync(path);
    this.db.exec('PRAGMA journal_mode = WAL');
    this.db.exec('PRAGMA synchronous = NORMAL');
    this.db.exec('PRAGMA busy_timeout = 5000');
    this.db.exec(SCHEMA);
    this.insertStmt = this.db.prepare(INSERT);
    this.summaryStmt = this.db.prepare(SUMMARY);
    this.groupStmts = {
      model: this.db.prepare(groupsSql('model')),
      provider: this.db.prepare(groupsSql('provider')),
      userAgent: this.db.prepare(groupsSql('userAgent')),
    };
    this.timeseriesStmts = {
      model: {
        day: this.db.prepare(timeseriesSql('day', 'model')),
        week: this.db.prepare(timeseriesSql('week', 'model')),
        month: this.db.prepare(timeseriesSql('month', 'model')),
      },
      provider: {
        day: this.db.prepare(timeseriesSql('day', 'provider')),
        week: this.db.prepare(timeseriesSql('week', 'provider')),
        month: this.db.prepare(timeseriesSql('month', 'provider')),
      },
      userAgent: {
        day: this.db.prepare(timeseriesSql('day', 'userAgent')),
        week: this.db.prepare(timeseriesSql('week', 'userAgent')),
        month: this.db.prepare(timeseriesSql('month', 'userAgent')),
      },
    };
  }

  insertMany(rows: AccessLog[]): void {
    if (rows.length === 0) return;
    this.db.exec('BEGIN');
    try {
      for (const row of rows) {
        this.insertStmt.run({
          requestId: row.requestId,
          tsMs: row.tsMs,
          time: row.time,
          method: row.method,
          path: row.path,
          status: row.status,
          durationMs: row.durationMs,
          bytesOut: row.bytesOut,
          remoteAddr: row.remoteAddr ?? null,
          userAgent: row.userAgent ?? null,
          keyName: row.keyName ?? null,
          alias: row.alias ?? null,
          provider: row.provider ?? null,
          billingMode: row.billingMode ?? null,
          upstreamModel: row.upstreamModel ?? null,
          stream: row.stream ? 1 : 0,
          promptTokens: row.promptTokens ?? null,
          completionTokens: row.completionTokens ?? null,
          totalTokens: row.totalTokens ?? null,
          costUsd: row.costUsd ?? null,
          estimatedApiCostUsd: row.estimatedApiCostUsd ?? null,
          pricingSource: row.pricingSource ?? null,
          errorCode: row.errorCode ?? null,
          clientCanceled: row.clientCanceled ? 1 : 0,
        });
      }
      this.db.exec('COMMIT');
    } catch (err) {
      this.db.exec('ROLLBACK');
      throw err;
    }
  }

  summary(fromMs: number, toMs: number): Summary {
    const row = (this.summaryStmt.get(fromMs, toMs) ?? {}) as unknown as Row;
    return {
      requests: n(row.requests),
      errors: n(row.errors),
      streamed: n(row.streamed),
      reportedCostUsd: n(row.reported_cost_usd),
      subscriptionEstimateUsd: n(row.subscription_estimate_usd),
      apiEstimateUsd: n(row.api_estimate_usd),
      promptTokens: n(row.prompt_tokens),
      completionTokens: n(row.completion_tokens),
      totalTokens: n(row.total_tokens),
      avgDurationMs: n(row.avg_duration_ms),
    };
  }

  groups(fromMs: number, toMs: number, breakdown: Breakdown): GroupRow[] {
    const rows = this.groupStmts[breakdown].all(fromMs, toMs) as unknown as Row[];
    return rows.map((row) => ({
      label: s(row.label) || 'unknown',
      detail: s(row.detail),
      requests: n(row.requests),
      errors: n(row.errors),
      promptTokens: n(row.prompt_tokens),
      completionTokens: n(row.completion_tokens),
      reportedCostUsd: n(row.reported_cost_usd),
      subscriptionEstimateUsd: n(row.subscription_estimate_usd),
      apiEstimateUsd: n(row.api_estimate_usd),
    }));
  }

  timeseries(
    fromMs: number,
    toMs: number,
    bucket: Bucket,
    breakdown: Breakdown,
  ): GroupTimeseriesRow[] {
    const rows = this.timeseriesStmts[breakdown][bucket].all(fromMs, toMs) as unknown as Row[];
    const result: GroupTimeseriesRow[] = [];
    for (const row of rows) {
      const bucketStartMs = bucketStartFromKey(bucket, String(row.bucket_key ?? ''));
      if (!Number.isFinite(bucketStartMs)) continue;
      result.push({
        bucketStartMs,
        label: s(row.label) || 'unknown',
        detail: s(row.detail),
        requests: n(row.requests),
        errors: n(row.errors),
        promptTokens: n(row.prompt_tokens),
        completionTokens: n(row.completion_tokens),
        reportedCostUsd: n(row.reported_cost_usd),
        subscriptionEstimateUsd: n(row.subscription_estimate_usd),
        apiEstimateUsd: n(row.api_estimate_usd),
      });
    }
    return result;
  }

  close(): void {
    this.db.close();
  }
}
