#!/usr/bin/env node
import { spawnSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import { createRequire } from "node:module";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const require = createRequire(import.meta.url);
const appRoot = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "..",
);
const paraglideEntry = require.resolve("@inlang/paraglide-js");
const paraglideCli = path.join(
  path.dirname(path.dirname(paraglideEntry)),
  "bin",
  "run.js",
);

export async function verifyCompiledCatalog({
  cwd = appRoot,
  outdir = "./src/lib/paraglide",
  baseCatalog = "./messages/en.json",
} = {}) {
  const enPath = path.resolve(cwd, baseCatalog);
  const enCatalog = JSON.parse(readFileSync(enPath, "utf8"));
  const expectedKeys = Object.keys(enCatalog);
  if (expectedKeys.length === 0) {
    throw new Error(`[paraglide] base catalog is empty: ${enPath}`);
  }
  const messagesPath = path.resolve(cwd, outdir, "messages.js");
  if (!existsSync(messagesPath)) {
    throw new Error(
      `[paraglide] compiled messages module missing: ${messagesPath}`,
    );
  }
  const compiled = await import(
    `${pathToFileURL(messagesPath).href}?v=${Date.now()}`
  );
  const m = compiled?.m;
  if (!m || typeof m !== "object") {
    throw new Error(
      `[paraglide] compiled messages export missing in ${messagesPath}`,
    );
  }
  const missing = expectedKeys.filter((key) => typeof m[key] !== "function");
  if (missing.length > 0) {
    throw new Error(
      `[paraglide] compiled catalog missing ${missing.length}/${expectedKeys.length} keys (first: ${missing.slice(0, 5).join(", ")})`,
    );
  }
}

export async function compileParaglide({
  cwd = appRoot,
  project = "./project.inlang",
  outdir = "./src/lib/paraglide",
  stdio = "inherit",
} = {}) {
  const result = spawnSync(
    process.execPath,
    [paraglideCli, "compile", "--project", project, "--silent"],
    { cwd, shell: false, stdio, windowsHide: true },
  );

  if (result.error) {
    console.error(`[paraglide] compile failed: ${result.error.message}`);
    return 1;
  }
  if ((result.status ?? 1) !== 0) {
    return result.status ?? 1;
  }

  try {
    await verifyCompiledCatalog({ cwd, outdir });
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error));
    return 1;
  }
  return 0;
}

if (
  process.argv[1] &&
  path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  process.exitCode = await compileParaglide();
}
