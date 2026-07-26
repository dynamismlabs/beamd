#!/usr/bin/env node
// Network-free release-package validation. Run after build-npm.mjs and before
// any npm, GitHub Release, or GHCR publish step.

import { spawnSync } from "node:child_process";
import {
  existsSync,
  lstatSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  symlinkSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const version = process.argv[2];
if (!version || version.startsWith("-")) {
  console.error("usage: node scripts/package-smoke.mjs <version>");
  process.exit(2);
}

const targets = [
  ["darwin", "arm64"],
  ["darwin", "x64"],
  ["linux", "arm64"],
  ["linux", "x64"],
];
const build = join(root, "npm", "build");
const main = JSON.parse(readFileSync(join(build, "beamd", "package.json"), "utf8"));
if (main.name !== "@beamd/cli" || main.version !== version) {
  throw new Error("main npm manifest name/version mismatch");
}

for (const [os, cpu] of targets) {
  const dir = join(build, `cli-${os}-${cpu}`);
  const manifest = JSON.parse(readFileSync(join(dir, "package.json"), "utf8"));
  const expectedName = `@beamd/cli-${os}-${cpu}`;
  if (
    manifest.name !== expectedName ||
    manifest.version !== version ||
    manifest.os?.[0] !== os ||
    manifest.cpu?.[0] !== cpu ||
    main.optionalDependencies?.[expectedName] !== version
  ) {
    throw new Error(`platform manifest mismatch: ${expectedName}`);
  }
  const binary = join(dir, "bin", "beamd");
  if (!existsSync(binary) || !lstatSync(binary).isFile()) {
    throw new Error(`platform binary missing: ${binary}`);
  }
}

// Exercise the actual JS shim against the current runner's matching platform
// package without contacting npm.
const platformDir = join(build, `cli-${process.platform}-${process.arch}`);
if (!existsSync(platformDir)) {
  throw new Error(`release does not support runner ${process.platform}/${process.arch}`);
}
const sandbox = mkdtempSync(join(tmpdir(), "beamd-package-smoke-"));
const scopeDir = join(sandbox, "node_modules", "@beamd");
mkdirSync(scopeDir, { recursive: true });
symlinkSync(platformDir, join(scopeDir, `cli-${process.platform}-${process.arch}`), "dir");
const shim = join(build, "beamd", "bin", "beamd.cjs");
const result = spawnSync(process.execPath, [shim, "version"], {
  cwd: sandbox,
  encoding: "utf8",
  env: { ...process.env, NODE_PATH: join(sandbox, "node_modules") },
});
if (result.status !== 0) {
  throw new Error(`npm shim failed: ${result.stderr || result.stdout}`);
}
if (!`${result.stdout}${result.stderr}`.includes(version)) {
  throw new Error(`npm shim did not execute the ${version} binary`);
}

for (const dockerfileName of ["Dockerfile", "Dockerfile.goreleaser"]) {
  const dockerfile = readFileSync(join(root, dockerfileName), "utf8");
  if (!/EXPOSE\s+443\/tcp\s+443\/udp/.test(dockerfile)) {
    throw new Error(`${dockerfileName} must expose both 443/tcp and 443/udp`);
  }
}
console.log(`package smoke passed for ${version} (${targets.length} binary targets)`);
