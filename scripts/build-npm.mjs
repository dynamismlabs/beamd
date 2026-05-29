#!/usr/bin/env node
// Build (and optionally publish) the beamd npm packages.
//
//   node scripts/build-npm.mjs <version> [--publish]
//
// Produces, under npm/build/:
//   - beamd-<os>-<cpu>  — one per platform, each shipping the matching
//                          native binary, gated by "os"/"cpu".
//   - beamd             — the user-facing package: a JS shim (npm/shim.cjs)
//                          plus the platform packages as optionalDependencies.
//
// On `npm i beamd`, npm installs only the platform package matching the host
// (~one binary, not four). Version comes from the git tag (without the "v").

import { execFileSync } from "node:child_process";
import { mkdirSync, rmSync, writeFileSync, copyFileSync, chmodSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const outDir = join(root, "npm", "build");

const version = process.argv[2];
const publish = process.argv.includes("--publish");
if (!version || version.startsWith("-")) {
  console.error("usage: node scripts/build-npm.mjs <version> [--publish]");
  process.exit(1);
}

// node platform/arch ↔ Go GOOS/GOARCH
const targets = [
  { os: "darwin", cpu: "arm64", goos: "darwin", goarch: "arm64" },
  { os: "darwin", cpu: "x64", goos: "darwin", goarch: "amd64" },
  { os: "linux", cpu: "arm64", goos: "linux", goarch: "arm64" },
  { os: "linux", cpu: "x64", goos: "linux", goarch: "amd64" },
];

rmSync(outDir, { recursive: true, force: true });
mkdirSync(outDir, { recursive: true });

const optionalDependencies = {};
for (const t of targets) {
  const name = `beamd-${t.os}-${t.cpu}`;
  optionalDependencies[name] = version;

  const pkgDir = join(outDir, name);
  mkdirSync(join(pkgDir, "bin"), { recursive: true });

  console.error(`building ${name} (${t.goos}/${t.goarch}) …`);
  execFileSync(
    "go",
    ["build", "-trimpath", "-ldflags", `-s -w -X main.Version=${version}`, "-o", join(pkgDir, "bin", "beamd"), "./cmd/beamd"],
    { cwd: root, env: { ...process.env, GOOS: t.goos, GOARCH: t.goarch, CGO_ENABLED: "0" }, stdio: ["ignore", "ignore", "inherit"] },
  );
  chmodSync(join(pkgDir, "bin", "beamd"), 0o755);

  writeFileSync(
    join(pkgDir, "package.json"),
    JSON.stringify(
      {
        name,
        version,
        description: `beamd prebuilt binary for ${t.os} ${t.cpu}`,
        repository: { type: "git", url: "git+https://github.com/dynamismlabs/beamd.git" },
        license: "Apache-2.0",
        os: [t.os],
        cpu: [t.cpu],
        files: ["bin/beamd"],
      },
      null,
      2,
    ) + "\n",
  );
}

// Main package: shim + platform packages as optionalDependencies.
const mainDir = join(outDir, "beamd");
mkdirSync(join(mainDir, "bin"), { recursive: true });
copyFileSync(join(root, "npm", "shim.cjs"), join(mainDir, "bin", "beamd.cjs"));
chmodSync(join(mainDir, "bin", "beamd.cjs"), 0o755);
copyFileSync(join(root, "README.md"), join(mainDir, "README.md"));
copyFileSync(join(root, "LICENSE"), join(mainDir, "LICENSE"));
writeFileSync(
  join(mainDir, "package.json"),
  JSON.stringify(
    {
      name: "beamd",
      version,
      description: "Self-hostable, instant-URL HTTPS tunnel for multi-app dev.",
      repository: { type: "git", url: "git+https://github.com/dynamismlabs/beamd.git" },
      homepage: "https://github.com/dynamismlabs/beamd",
      license: "Apache-2.0",
      bin: { beamd: "bin/beamd.cjs" },
      optionalDependencies,
      files: ["bin/beamd.cjs", "README.md", "LICENSE"],
    },
    null,
    2,
  ) + "\n",
);

console.error(`\nnpm packages built in ${outDir} (version ${version})`);

if (publish) {
  // Platform packages first, so the main package's optionalDependencies
  // resolve for anyone installing immediately after.
  for (const t of targets) {
    console.error(`publishing beamd-${t.os}-${t.cpu} …`);
    execFileSync("npm", ["publish", "--access", "public"], { cwd: join(outDir, `beamd-${t.os}-${t.cpu}`), stdio: "inherit" });
  }
  console.error("publishing beamd …");
  execFileSync("npm", ["publish", "--access", "public"], { cwd: mainDir, stdio: "inherit" });
}
