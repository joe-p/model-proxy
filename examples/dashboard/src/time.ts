// Time-bucket helpers shared by the SQLite queries and the HTTP layer.
//
// Buckets are UTC so the chart does not move when the server or browser is in
// another timezone. Weeks start on Monday.

export type Bucket = 'day' | 'week' | 'month';

export const DAY_MS = 86_400_000;

const WEEK_MS = 7 * DAY_MS;

// 1970-01-01 was a Thursday. Shift the epoch by three days so week buckets
// start on Monday instead.
const WEEK_OFFSET_MS = 3 * DAY_MS;

export function parseBucket(value: string | null): Bucket {
  if (value === 'week' || value === 'month') return value;
  return 'day';
}

// SQL expression that returns the bucket key for a row's ts_ms column.
export function bucketSqlExpression(bucket: Bucket): string {
  switch (bucket) {
    case 'day':
      return `CAST(ts_ms / ${DAY_MS} AS INTEGER)`;
    case 'week':
      return `CAST((ts_ms + ${WEEK_OFFSET_MS}) / ${WEEK_MS} AS INTEGER)`;
    case 'month':
      return "strftime('%Y-%m', ts_ms / 1000, 'unixepoch')";
  }
}

export function bucketStartFromKey(bucket: Bucket, key: string): number {
  switch (bucket) {
    case 'day':
      return Number(key) * DAY_MS;
    case 'week':
      return Number(key) * WEEK_MS - WEEK_OFFSET_MS;
    case 'month': {
      const match = /^(\d{4})-(\d{2})$/.exec(key);
      if (!match) return Number.NaN;
      return Date.UTC(Number(match[1]), Number(match[2]) - 1, 1);
    }
  }
}

export function startOfBucket(bucket: Bucket, ms: number): number {
  switch (bucket) {
    case 'day':
      return Math.floor(ms / DAY_MS) * DAY_MS;
    case 'week':
      return Math.floor((ms + WEEK_OFFSET_MS) / WEEK_MS) * WEEK_MS - WEEK_OFFSET_MS;
    case 'month': {
      const date = new Date(ms);
      return Date.UTC(date.getUTCFullYear(), date.getUTCMonth(), 1);
    }
  }
}

export function nextBucketStart(bucket: Bucket, startMs: number): number {
  switch (bucket) {
    case 'day':
      return startMs + DAY_MS;
    case 'week':
      return startMs + WEEK_MS;
    case 'month': {
      const date = new Date(startMs);
      return Date.UTC(date.getUTCFullYear(), date.getUTCMonth() + 1, 1);
    }
  }
}
