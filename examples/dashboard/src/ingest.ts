import { createInterface } from 'node:readline';
import type { Readable } from 'node:stream';
import type { UsageStore } from './db.js';
import { parseLine, type AccessLog } from './parse.js';

export interface IngestStats {
  parsed: number;
  skipped: number;
}

export interface IngestOptions {
  batchSize?: number;
  flushIntervalMs?: number;
  // onProgress fires after each batch is committed, so a caller can tell
  // whether records are actually arriving.
  onProgress?: (stats: IngestStats) => void;
}

// ingest reads newline-delimited JSON, keeps the access records, and writes
// them to the store in batches. Non-access lines are counted and skipped.
export function ingest(
  input: Readable,
  store: UsageStore,
  options: IngestOptions = {},
): Promise<IngestStats> {
  const batchSize = options.batchSize ?? 250;
  const flushIntervalMs = options.flushIntervalMs ?? 1000;

  const lines = createInterface({ input, crlfDelay: Infinity });
  let pending: AccessLog[] = [];
  let parsed = 0;
  let skipped = 0;
  let failure: Error | undefined;

  const flush = (): void => {
    if (pending.length === 0 || failure) return;
    const batch = pending;
    pending = [];
    try {
      store.insertMany(batch);
      options.onProgress?.({ parsed, skipped });
    } catch (err) {
      failure = err instanceof Error ? err : new Error(String(err));
      lines.close();
    }
  };

  const timer = setInterval(flush, flushIntervalMs);
  // Do not keep the process alive just to flush an idle batch.
  timer.unref();

  lines.on('line', (line) => {
    if (failure) return;
    const record = parseLine(line);
    if (!record) {
      skipped++;
      return;
    }
    pending.push(record);
    parsed++;
    if (pending.length >= batchSize) flush();
  });

  return new Promise<IngestStats>((resolve, reject) => {
    lines.on('close', () => {
      clearInterval(timer);
      flush();
      if (failure) reject(failure);
      else resolve({ parsed, skipped });
    });
    lines.on('error', (err) => {
      clearInterval(timer);
      reject(err);
    });
    input.on('error', (err) => {
      clearInterval(timer);
      reject(err);
    });
  });
}
