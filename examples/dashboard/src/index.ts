import { spawn, type ChildProcess } from 'node:child_process';
import { createReadStream } from 'node:fs';
import { UsageStore } from './db.js';
import { ingest, type IngestStats } from './ingest.js';
import { createServer } from './server.js';

interface Options {
  db: string;
  host: string;
  port: number;
  file?: string;
  useStdin: boolean;
  command: string[];
  help: boolean;
}

const HELP = `model-proxy usage dashboard

Accepts model-proxy JSON access records over POST /ingest or stdout, stores
them in SQLite, and serves a dashboard with cost totals, a stacked cost/token
chart, and a per-model breakdown.

Usage:
  npm start -- [options]                         serve dashboard + POST /ingest
  npm start -- [options] -- <proxy command...>   spawn and wrap the proxy
  npm start -- [options] --file access.log      replay an existing log
  npm start -- [options] --stdin                read logs from stdin

Options:
  --db <path>    SQLite database file (default: usage.db)
  --port <n>     dashboard port (default: 8787)
  --host <addr>  bind address (default: 127.0.0.1)
  -h, --help     show this help
`;

function log(message: string): void {
  // Keep wrapper diagnostics off stdout: stdout is the log stream being parsed.
  process.stderr.write(`[dashboard] ${message}\n`);
}

function parseArgs(argv: string[]): Options {
  const options: Options = {
    db: 'usage.db',
    host: '127.0.0.1',
    port: 8787,
    useStdin: false,
    command: [],
    help: false,
  };

  let i = 0;
  while (i < argv.length) {
    const arg = argv[i];
    if (arg === '--') {
      options.command = argv.slice(i + 1);
      break;
    }
    switch (arg) {
      case '--db':
        options.db = argv[++i] ?? options.db;
        break;
      case '--host':
        options.host = argv[++i] ?? options.host;
        break;
      case '--port': {
        const value = Number(argv[++i]);
        if (!Number.isInteger(value) || value <= 0 || value > 65535) {
          throw new Error('--port must be an integer between 1 and 65535');
        }
        options.port = value;
        break;
      }
      case '--file':
        options.file = argv[++i];
        if (!options.file) throw new Error('--file requires a path');
        break;
      case '--stdin':
        options.useStdin = true;
        break;
      case '-h':
      case '--help':
        options.help = true;
        break;
      default:
        throw new Error(`unknown argument: ${arg}`);
    }
    i++;
  }

  return options;
}

async function listen(server: ReturnType<typeof createServer>, host: string, port: number): Promise<void> {
  await new Promise<void>((resolve, reject) => {
    const onError = (err: Error) => reject(err);
    server.once('error', onError);
    server.listen(port, host, () => {
      server.off('error', onError);
      resolve();
    });
  });
}

async function main(): Promise<void> {
  let options: Options;
  try {
    options = parseArgs(process.argv.slice(2));
  } catch (err) {
    log(err instanceof Error ? err.message : String(err));
    process.stderr.write(`\n${HELP}`);
    process.exitCode = 2;
    return;
  }

  if (options.help) {
    process.stdout.write(HELP);
    return;
  }

  const store = new UsageStore(options.db);
  const server = createServer(store);
  await listen(server, options.host, options.port);
  log(`dashboard on http://${options.host}:${options.port} (db: ${options.db})`);

  let child: ChildProcess | undefined;
  let ingestion: Promise<IngestStats> | undefined;

  if (options.command.length > 0) {
    const [command, ...args] = options.command;
    log(`spawning: ${options.command.join(' ')}`);
    child = spawn(command, args, { stdio: ['ignore', 'pipe', 'inherit'] });
    child.on('error', (err) => log(`proxy failed to start: ${err.message}`));
    child.on('exit', (code, signal) =>
      log(`proxy exited (code=${code ?? 'null'}, signal=${signal ?? 'none'})`),
    );
    if (!child.stdout) {
      throw new Error('spawned proxy has no stdout pipe');
    }
    let seen = 0;
    const noDataWarning = setTimeout(() => {
      if (seen === 0) {
        log(
          'no access records after 10s: the proxy may predate stdout logging (run make build), or it is writing logs elsewhere',
        );
      }
    }, 10_000);
    noDataWarning.unref();
    ingestion = ingest(child.stdout, store, {
      onProgress: (stats) => {
        seen = stats.parsed;
        if (seen > 0) clearTimeout(noDataWarning);
      },
    });
  } else if (options.file) {
    log(`reading ${options.file}`);
    ingestion = ingest(createReadStream(options.file), store);
  } else if (options.useStdin) {
    log('reading stdin');
    ingestion = ingest(process.stdin, store);
  } else {
    log(`waiting for access records on POST http://${options.host}:${options.port}/ingest`);
  }

  if (ingestion) {
    ingestion.then(
      ({ parsed, skipped }) => log(`ingest finished: ${parsed} records, ${skipped} lines skipped`),
      (err) => log(`ingest failed: ${err instanceof Error ? err.message : String(err)}`),
    );
  }

  let stopping = false;
  const shutdown = (signal: NodeJS.Signals): void => {
    if (stopping) return;
    stopping = true;
    log(`shutting down (${signal})`);
    if (child && child.exitCode === null && child.signalCode === null) {
      child.kill(signal);
    }
    server.close(() => {
      store.close();
      process.exit(0);
    });
    // Do not hang forever on keep-alive connections.
    setTimeout(() => {
      store.close();
      process.exit(1);
    }, 5000).unref();
  };

  process.on('SIGINT', () => shutdown('SIGINT'));
  process.on('SIGTERM', () => shutdown('SIGTERM'));
}

main().catch((err) => {
  log(err instanceof Error ? err.stack ?? err.message : String(err));
  process.exitCode = 1;
});
