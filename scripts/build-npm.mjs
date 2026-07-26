#!/usr/bin/env node
// Build (and optionally publish) the beamd npm packages.
//
//   node scripts/build-npm.mjs <version> [--publish]
//   node scripts/build-npm.mjs <version> --publish-existing [--dry-run]
//
// Produces, under npm/build/:
//   - @beamd/cli-<os>-<cpu>  — one per platform, each shipping the matching
//                              native binary, gated by "os"/"cpu".
//   - @beamd/cli             — the user-facing package: a JS shim (npm/shim.cjs)
//                              plus the platform packages as optionalDependencies,
//                              exposing the `beamd` bin via bin/beamd.cjs.
//
// On `npm i @beamd/cli`, npm installs only the platform package matching the
// host (~one binary, not four). Version comes from the git tag (without "v").

import { execFileSync, spawnSync } from "node:child_process";
import { mkdirSync, rmSync, writeFileSync, copyFileSync, chmodSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const outDir = join(root, "npm", "build");

const version = process.argv[2];
const buildAndPublish = process.argv.includes("--publish");
const publishExisting = process.argv.includes("--publish-existing");
const publish = buildAndPublish || publishExisting;
const dryRun = process.argv.includes("--dry-run");
const distTag = process.env.NPM_DIST_TAG || (version?.includes("-") ? "next" : "");
// The control-plane host the CLI logs into defaults to production in code
// (cmd/beamd/client.go: DefaultHost = "beamd.ai"), so a normal publish needs no
// env. Set BEAMD_DEFAULT_HOST only to bake a *different* default into a build
// (rare — e.g. a private hosted deployment); for staging/dev, users override at
// runtime via the BEAMD_DEFAULT_HOST env var instead.
const defaultHost = process.env.BEAMD_DEFAULT_HOST || "";
if (
  !version ||
  version.startsWith("-") ||
  (buildAndPublish && publishExisting) ||
  (dryRun && !publish)
) {
  console.error(
    "usage: node scripts/build-npm.mjs <version> [--publish | --publish-existing [--dry-run]]",
  );
  process.exit(1);
}

// node platform/arch ↔ Go GOOS/GOARCH
const targets = [
  { os: "darwin", cpu: "arm64", goos: "darwin", goarch: "arm64" },
  { os: "darwin", cpu: "x64", goos: "darwin", goarch: "amd64" },
  { os: "linux", cpu: "arm64", goos: "linux", goarch: "arm64" },
  { os: "linux", cpu: "x64", goos: "linux", goarch: "amd64" },
];
const mainDir = join(outDir, "beamd");

if (!publishExisting) {
  rmSync(outDir, { recursive: true, force: true });
  mkdirSync(outDir, { recursive: true });

  const optionalDependencies = {};
  for (const t of targets) {
    const name = `@beamd/cli-${t.os}-${t.cpu}`; // scoped, org-owned, grouped with @beamd/cli
    const dirName = `cli-${t.os}-${t.cpu}`; // flat build dir; npm reads the name from package.json
    optionalDependencies[name] = version;

    const pkgDir = join(outDir, dirName);
    mkdirSync(join(pkgDir, "bin"), { recursive: true });

    console.error(`building ${name} (${t.goos}/${t.goarch})${defaultHost ? ` [host=${defaultHost}]` : ""} …`);
    const ldflags =
      `-s -w -X main.Version=${version}` + (defaultHost ? ` -X main.DefaultHost=${defaultHost}` : "");
    execFileSync(
      "go",
      ["build", "-trimpath", "-ldflags", ldflags, "-o", join(pkgDir, "bin", "beamd"), "./cmd/beamd"],
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
  mkdirSync(join(mainDir, "bin"), { recursive: true });
  copyFileSync(join(root, "npm", "shim.cjs"), join(mainDir, "bin", "beamd.cjs"));
  chmodSync(join(mainDir, "bin", "beamd.cjs"), 0o755);
  copyFileSync(join(root, "README.md"), join(mainDir, "README.md"));
  copyFileSync(join(root, "LICENSE"), join(mainDir, "LICENSE"));
  writeFileSync(
    join(mainDir, "package.json"),
    JSON.stringify(
      {
        name: "@beamd/cli",
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
} else {
  // Fail closed before publishing: this validates every manifest and binary,
  // and executes the native package through the real npm shim. Most
  // importantly, it does not rebuild or alter npm/build, so publish consumes
  // the exact artifact bytes that passed the preceding smoke step.
  execFileSync(
    process.execPath,
    [join(root, "scripts", "package-smoke.mjs"), version],
    { cwd: root, stdio: "inherit" },
  );
}

// publishPkg publishes one package dir, treating "this version already
// exists" as a skip rather than a failure — so a partial/interrupted publish
// can be safely re-run (idempotent).
function publishPkg(dir, label) {
  console.error(`${dryRun ? "dry-running publish for" : "publishing"} ${label} …`);
  const args = ["publish", "--access", "public"];
  if (distTag) {
    args.push("--tag", distTag);
  }
  if (dryRun) {
    args.push("--dry-run");
  }
  const res = spawnSync("npm", args, { cwd: dir, encoding: "utf8" });
  if (res.stdout) process.stdout.write(res.stdout);
  if (res.status === 0) {
    if (res.stderr) process.stderr.write(res.stderr);
    return;
  }
  const stderr = res.stderr || "";
  if (/EPUBLISHCONFLICT|cannot publish over|previously published|already exists/i.test(stderr)) {
    console.error(`  ↳ ${label}@${version} already published — skipping`);
    return;
  }
  process.stderr.write(stderr);
  throw new Error(`npm publish ${label} failed (exit ${res.status})`);
}

if (publish) {
  // Platform packages first, so the main package's optionalDependencies
  // resolve for anyone installing immediately after.
  for (const t of targets) {
    publishPkg(join(outDir, `cli-${t.os}-${t.cpu}`), `@beamd/cli-${t.os}-${t.cpu}`);
  }
  publishPkg(mainDir, "@beamd/cli");
}
