// Process entry for the forge.
//
// The grading harness drives this program from the outside, and the forge
// itself is the compiled binary beside this file (./forge, built from the
// Go sources in this repository). main.ts is only the handshake around it:
//   1. the harness tells us where it lives through MAKEFUN_ENV_URL;
//   2. we start the binary, which binds whatever port the OS hands it and
//      prints that port on its first stdout line;
//   3. we report the port back to the harness and poll its /done endpoint
//      until the trial is over, then stop the binary and exit 0.
// Without step 1 there is nothing to talk to, so a missing pointer is a
// hard failure before any process is started.

import { spawn, type ChildProcess } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const POINTER_VAR = 'MAKEFUN_ENV_URL';
const REGISTER_PATH = '/register';
const DONE_PATH = '/done';

// The harness may still be booting when this process starts, so the
// register call gets a short budget of retries instead of one attempt.
const REGISTER_ATTEMPTS = 40;
const RETRY_PAUSE_MS = 250;
const DONE_POLL_MS = 250;

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function cleanBase(raw: string): string {
  // Strip trailing slashes so joining a path below is exact.
  return raw.replace(/\/+$/, '');
}

async function reportPort(base: string, port: number): Promise<void> {
  let problem = 'register was never attempted';
  for (let attempt = 0; attempt < REGISTER_ATTEMPTS; attempt += 1) {
    if (attempt > 0) await sleep(RETRY_PAUSE_MS);
    try {
      const answer = await fetch(base + REGISTER_PATH, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
      });
      if (answer.ok) return;
      problem = `harness replied HTTP ${answer.status}`;
      // 409 means an entry already holds the slot; no retry changes that.
      if (answer.status === 409) break;
    } catch (err) {
      problem = err instanceof Error ? err.message : String(err);
    }
  }
  throw new Error(`registration failed: ${problem}`);
}

async function waitForTrialEnd(base: string): Promise<void> {
  // /done answers JSON: {"done": false} while the trial runs and
  // {"done": true} once it is finished. A wobble on one poll (harness
  // busy, socket reset) is not a verdict, so failures just cost a tick.
  for (;;) {
    let over = false;
    try {
      const answer = await fetch(base + DONE_PATH);
      if (answer.ok) {
        const payload: unknown = await answer.json();
        over =
          typeof payload === 'object' &&
          payload !== null &&
          (payload as { done?: unknown }).done === true;
      }
    } catch {
      over = false;
    }
    if (over) return;
    await sleep(DONE_POLL_MS);
  }
}

/** Start ./forge and answer the port from its first stdout line. */
function startForge(): Promise<{ child: ChildProcess; port: number }> {
  const binary = fileURLToPath(new URL('./forge', import.meta.url));
  const child = spawn(binary, [], { stdio: ['ignore', 'pipe', 'inherit'] });
  return new Promise((resolve, reject) => {
    let buffer = '';
    let settled = false;
    child.stdout.setEncoding('utf8');
    child.stdout.on('data', (chunk: string) => {
      if (settled) return;
      buffer += chunk;
      const newline = buffer.indexOf('\n');
      if (newline < 0) return;
      const port = Number(buffer.slice(0, newline).trim());
      if (!Number.isInteger(port) || port <= 0) {
        settled = true;
        reject(new Error(`forge printed an unusable port line: ${JSON.stringify(buffer.slice(0, newline))}`));
        return;
      }
      settled = true;
      resolve({ child, port });
    });
    child.once('error', (err) => {
      if (!settled) {
        settled = true;
        reject(err);
      }
    });
    child.once('exit', (code) => {
      if (!settled) {
        settled = true;
        reject(new Error(`forge exited (${code ?? 'signal'}) before printing its port`));
      }
    });
  });
}

async function boot(): Promise<number> {
  const pointed = process.env[POINTER_VAR];
  if (pointed === undefined || pointed.trim() === '') {
    console.error(`${POINTER_VAR} is unset or empty; cannot register`);
    return 1;
  }
  const base = cleanBase(pointed.trim());

  const { child, port } = await startForge();
  // A forge that dies mid-trial can serve nothing more; fail with it
  // instead of polling a trial that can only run out its wall.
  let childGone: Error | null = null;
  const gone = new Promise<never>((_resolve, reject) => {
    child.once('exit', (code) => {
      childGone = new Error(`forge exited (${code ?? 'signal'}) before the trial ended`);
      reject(childGone);
    });
  });

  try {
    await Promise.race([reportPort(base, port), gone]);
    await Promise.race([waitForTrialEnd(base), gone]);
    return 0;
  } finally {
    child.kill('SIGTERM');
  }
}

boot().then(
  (code) => process.exit(code),
  (err: unknown) => {
    console.error(err instanceof Error ? err.message : String(err));
    process.exit(1);
  },
);
