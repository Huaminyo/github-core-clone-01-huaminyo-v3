// github-core-clone v2 — entrypoint: a thin launcher for the Go core.
//
// The forge itself is `forge`, a static Linux amd64 binary built from the Go
// sources in ./go (stdlib only, CGO disabled). This file only finds a place
// to execute it from and forwards stdio, environment and signals, exiting
// with the child's exit code.
//
// The trial mounts the entry at /bot read-only, and the mount may be noexec,
// so the launcher tries in order:
//   1. exec ./forge directly (works when /bot allows execution),
//   2. copy it to the OS temp dir, chmod +x, exec from there,
//   3. same via /dev/shm (tmpfs, usually exec-allowed even when /tmp is not),
//   4. same via $HOME.
// Node has no memfd API, so a temp-file copy is the portable equivalent.
// The copy is small (~6.5 MB) and the temp file is unlinked on exit.
//
// Only a SPAWN failure (the binary could not be started at all) moves to the
// next location. Once a child is running, its exit — by code or by signal —
// ends the launcher with the same outcome: a child killed by a signal must
// NOT be mistaken for a spawn failure, or the launcher would respawn the
// server instead of dying when the harness signals it to stop.

import { spawn } from 'node:child_process';
import type { ChildProcess } from 'node:child_process';
import { copyFileSync, chmodSync, unlinkSync } from 'node:fs';
import { tmpdir, homedir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const bundled = join(here, 'forge');

function safeDir(fn: () => string): string {
  try {
    return fn();
  } catch {
    return '';
  }
}

function candidates(): string[] {
  const out: string[] = [bundled];
  const name = `forge-${process.pid}`;
  for (const dir of [safeDir(tmpdir), '/dev/shm', safeDir(homedir)]) {
    if (dir) out.push(join(dir, name));
  }
  return out;
}

function stageTo(path: string): boolean {
  try {
    if (path !== bundled) {
      copyFileSync(bundled, path);
      chmodSync(path, 0o755);
    }
    return true;
  } catch {
    return false;
  }
}

function cleanup(path: string): void {
  if (path !== bundled) {
    try {
      unlinkSync(path);
    } catch {
      // Best effort; the OS reclaims temp files.
    }
  }
}

type LaunchResult =
  | { spawnFailed: true }
  | { spawnFailed: false; code: number | null; signal: NodeJS.Signals | null };

function launch(path: string): Promise<LaunchResult> {
  return new Promise((resolve) => {
    let child: ChildProcess;
    try {
      child = spawn(path, [], { stdio: 'inherit', env: process.env });
    } catch {
      resolve({ spawnFailed: true });
      return;
    }
    let settled = false;
    const done = (result: LaunchResult) => {
      if (!settled) {
        settled = true;
        process.removeListener('SIGINT', forward);
        process.removeListener('SIGTERM', forward);
        resolve(result);
      }
    };
    const forward = (signal: NodeJS.Signals) => {
      try {
        child.kill(signal);
      } catch {
        // The child is already gone; its exit will settle the launch.
      }
    };
    process.on('SIGINT', forward);
    process.on('SIGTERM', forward);
    child.on('error', () => done({ spawnFailed: true }));
    child.on('exit', (code, signal) => done({ spawnFailed: false, code, signal }));
  });
}

const paths = candidates();
for (const path of paths) {
  if (!stageTo(path)) continue;
  const result = await launch(path);
  cleanup(path);
  if (result.spawnFailed) {
    // The binary could not be started from here (EACCES/noexec) — try the next location.
    continue;
  }
  if (result.signal) {
    // The child died by a signal: die by the same signal, like a shell would.
    try {
      process.kill(process.pid, result.signal);
    } catch {
      // Fall through to the plain exit below.
    }
    process.exit(1);
  }
  process.exit(result.code ?? 0);
}
console.error('[entry] could not execute the forge binary from any location');
process.exit(1);
