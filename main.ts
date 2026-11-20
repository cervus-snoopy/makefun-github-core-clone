import { spawn } from "node:child_process";
import { chmodSync, copyFileSync, unlinkSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

// Thin launcher for the forge binary.
//
// The binary is copied to a fresh file in the OS temp directory and
// made executable before it is spawned. This survives hosts that
// strip the execute bit from uploaded files (for example a GitHub
// web upload turning 100755 into 100644): spawning ./forge
// directly would fail with EACCES, while the temp copy always
// carries mode 0755. If the copy fails for any reason, fall back
// to spawning ./forge directly.

const dir = dirname(fileURLToPath(import.meta.url));
const forgePath = join(dir, "forge");

let runPath = forgePath;
let tmpPath = null;

try {
  tmpPath = join(tmpdir(), `forge-${process.pid}-${Date.now()}`);
  copyFileSync(forgePath, tmpPath);
  chmodSync(tmpPath, 0o755);
  runPath = tmpPath;
} catch {
  tmpPath = null;
  runPath = forgePath;
}

const child = spawn(runPath, [], { stdio: "inherit", env: process.env });

child.on("error", (err) => {
  if (tmpPath) {
    try {
      unlinkSync(tmpPath);
    } catch {
      // Best effort cleanup; the temp copy is harmless if it lingers.
    }
  }
  console.error(`[main] failed to start forge: ${err.message}`);
  process.exit(1);
});

child.on("exit", (code, signal) => {
  if (tmpPath) {
    try {
      unlinkSync(tmpPath);
    } catch {
      // Best effort cleanup; the temp copy is harmless if it lingers.
    }
  }
  if (signal) {
    process.kill(process.pid, signal);
  } else {
    process.exit(code ?? 1);
  }
});

process.on("SIGINT", () => child.kill("SIGINT"));
process.on("SIGTERM", () => child.kill("SIGTERM"));
