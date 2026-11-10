// A parsed model-proxy access-log record.
//
// Only the fields the dashboard needs are kept. The full schema lives in the
// project README under "Usage log schema".
export interface AccessLog {
  requestId: string;
  time: string;
  tsMs: number;
  method: string;
  path: string;
  status: number;
  durationMs: number;
  bytesOut: number;
  remoteAddr?: string;
  userAgent?: string;
  keyName?: string;
  alias?: string;
  provider?: string;
  billingMode?: string;
  upstreamModel?: string;
  stream: boolean;
  errorCode?: string;
  clientCanceled: boolean;
  promptTokens?: number;
  completionTokens?: number;
  totalTokens?: number;
  costUsd?: number;
  estimatedApiCostUsd?: number;
  pricingSource?: string;
}

type Raw = Record<string, unknown>;

function str(value: unknown): string | undefined {
  return typeof value === 'string' ? value : undefined;
}

function num(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isFinite(value) ? value : undefined;
}

function int(value: unknown): number | undefined {
  const n = num(value);
  return n === undefined ? undefined : Math.trunc(n);
}

function bool(value: unknown): boolean {
  return value === true;
}

// parseRecord validates one decoded JSON object. It returns null for anything
// that is not an access record (startup logs, errors, health checks).
export function parseRecord(value: unknown): AccessLog | null {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) return null;
  const raw = value as Raw;

  if (raw.msg !== 'request') return null;

  const requestId = str(raw.request_id);
  const time = str(raw.time);
  if (!requestId || !time) return null;

  const tsMs = Date.parse(time);
  if (!Number.isFinite(tsMs)) return null;

  const path = str(raw.path) ?? '';
  // /healthz is logged at DEBUG; drop it if the proxy runs at debug level.
  if (path === '/healthz') return null;

  return {
    requestId,
    time,
    tsMs,
    method: str(raw.method) ?? '',
    path,
    status: int(raw.status) ?? 0,
    durationMs: num(raw.duration_ms) ?? 0,
    bytesOut: int(raw.bytes_out) ?? 0,
    remoteAddr: str(raw.remote_addr),
    userAgent: str(raw.user_agent),
    keyName: str(raw.key_name),
    alias: str(raw.alias),
    provider: str(raw.provider),
    billingMode: str(raw.billing_mode),
    upstreamModel: str(raw.upstream_model),
    stream: bool(raw.stream),
    errorCode: str(raw.error_code),
    clientCanceled: bool(raw.client_canceled),
    promptTokens: int(raw.prompt_tokens),
    completionTokens: int(raw.completion_tokens),
    totalTokens: int(raw.total_tokens),
    costUsd: num(raw.cost_usd),
    estimatedApiCostUsd: num(raw.estimated_api_cost_usd),
    pricingSource: str(raw.pricing_source),
  };
}

// parseLine turns one line of stdout into an AccessLog, or returns null for
// anything that is not an access record.
export function parseLine(line: string): AccessLog | null {
  const trimmed = line.trim();
  if (!trimmed.startsWith('{')) return null;

  let raw: unknown;
  try {
    raw = JSON.parse(trimmed);
  } catch {
    return null;
  }
  return parseRecord(raw);
}
