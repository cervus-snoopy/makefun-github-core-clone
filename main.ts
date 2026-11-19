// Thin Node 22 launcher: the forge itself is the vendored static Go binary
// ./forge (built in-session from the Go sources in this repo). This file only
// spawns it, forwards stdio/env, and propagates its exit code.
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const forgePath = path.join(path.dirname(fileURLToPath(import.meta.url)), 'forge');

const child = spawn(forgePath, [], { stdio: 'inherit', env: process.env });

child.on('error', (err) => {
  process.stderr.write(`[main] failed to start forge: ${err.message}\n`);
  process.exit(1);
});

child.on('exit', (code, signal) => {
  if (signal) {
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code ?? 1);
});

for (const sig of ['SIGINT', 'SIGTERM']) {
  process.on(sig, () => {
    child.kill(sig);
  });
}
