#!/usr/bin/env node

import { spawnSync } from "node:child_process";
import { existsSync, readFileSync, statSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const defaultAppRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");

function sameFile(left, right) {
  if (!existsSync(left) || !existsSync(right)) return false;
  return readFileSync(left).equals(readFileSync(right));
}

// pnpm 12 writes the environment document of pnpm-lock.yaml (configDependencies, packageManagerDependencies)
// only into the committed file; the installed copy holds the project document alone.
function projectDocument(file) {
  const text = readFileSync(file, "utf8").replaceAll("\r\n", "\n");
  const split = text.startsWith("---\n") ? text.lastIndexOf("\n---\n") : -1;
  return split < 0 ? text : text.slice(split + 5);
}

function sameLockfile(committed, installed) {
  if (!existsSync(committed) || !existsSync(installed)) return false;
  return sameFile(committed, installed) || projectDocument(committed) === projectDocument(installed);
}

function executableReady(binRoot, name) {
  for (const candidate of [
    resolve(binRoot, name),
    resolve(binRoot, `${name}.cmd`),
  ]) {
    try {
      if (statSync(candidate).isFile() && statSync(candidate).size > 0)
        return true;
    } catch {
      // Try the platform-specific candidate.
    }
  }
  return false;
}

function pnpmInvocation(args) {
  const corepackCLI = resolve(
    dirname(process.execPath),
    "node_modules",
    "corepack",
    "dist",
    "corepack.js",
  );
  return existsSync(corepackCLI)
    ? { command: process.execPath, args: [corepackCLI, "pnpm", ...args] }
    : { command: "pnpm", args, shell: process.platform === "win32" }; // Node 25+ ships no corepack; mise provides pnpm
}

export function frozenManifestReady(appRoot = defaultAppRoot, run = spawnSync) {
  const invocation = pnpmInvocation([
    "--dir",
    appRoot,
    "install",
    "--lockfile-only",
    "--offline",
    "--frozen-lockfile",
    "--ignore-scripts",
  ]);
  const result = run(invocation.command, invocation.args, {
    cwd: appRoot,
    stdio: "ignore",
    env: process.env,
    shell: invocation.shell,
  });
  return !result.error && result.status === 0;
}

export function dependenciesReady(
  appRoot = defaultAppRoot,
  verifyManifest = frozenManifestReady,
) {
  const modulesRoot = resolve(appRoot, "node_modules");
  return (
    sameLockfile(
      resolve(appRoot, "pnpm-lock.yaml"),
      resolve(modulesRoot, ".pnpm", "lock.yaml"),
    ) &&
    existsSync(resolve(modulesRoot, ".modules.yaml")) &&
    // Vite+ (D-2026-10-30-06): `vp` is the one binary of Vite, Vitest and Oxc.
    executableReady(resolve(modulesRoot, ".bin"), "vp") &&
    executableReady(resolve(modulesRoot, ".bin"), "svelte-check") &&
    verifyManifest(appRoot)
  );
}

export function ensureDependencies(appRoot = defaultAppRoot) {
  if (dependenciesReady(appRoot)) {
    console.log("Frontend dependencies already match the committed lockfile.");
    return 0;
  }
  const result = spawnSync(
    process.execPath,
    [resolve(appRoot, "scripts", "install-deps.mjs"), "--frozen-lockfile"],
    { cwd: resolve(appRoot, ".."), stdio: "inherit", env: process.env },
  );
  if (result.error) {
    console.error(
      `ERROR: failed to ensure frontend dependencies: ${result.error.message}`,
    );
  }
  return result.status ?? 1;
}

if (
  process.argv[1] &&
  import.meta.url === pathToFileURL(resolve(process.argv[1])).href
) {
  process.exitCode = ensureDependencies();
}
