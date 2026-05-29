#!/usr/bin/env node
"use strict";

// Entry point for the `beamd` npm package. The real binary ships in a
// per-platform optional dependency (beamd-<os>-<cpu>); npm installs only
// the one matching the host. This shim resolves it and execs it,
// passing through argv, stdio, and the exit code.

const { spawnSync } = require("node:child_process");
const path = require("node:path");

function binaryPath() {
  const pkg = `beamd-${process.platform}-${process.arch}`;
  let pkgJson;
  try {
    // Resolve via the package's manifest (always resolvable), then join
    // the binary path — more robust than resolving an extension-less file.
    pkgJson = require.resolve(`${pkg}/package.json`);
  } catch {
    throw new Error(
      `beamd: no prebuilt binary for ${process.platform}-${process.arch}.\n` +
        `Expected optional dependency "${pkg}" to be installed.\n` +
        `If your platform isn't supported, build from source:\n` +
        `  https://github.com/dynamismlabs/beamd`
    );
  }
  return path.join(path.dirname(pkgJson), "bin", "beamd");
}

let bin;
try {
  bin = binaryPath();
} catch (err) {
  console.error(err.message);
  process.exit(1);
}

const result = spawnSync(bin, process.argv.slice(2), { stdio: "inherit" });
if (result.error) {
  console.error(`beamd: ${result.error.message}`);
  process.exit(1);
}
process.exit(result.status === null ? 1 : result.status);
