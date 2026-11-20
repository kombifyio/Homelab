// Export the effective Homelab web dictionaries for exact-source localization QA.
// French remains a draft and is not admitted to runtime locale negotiation here.
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import "./i18n-ts-loader.mjs";

const { translations } = await import("../src/lib/i18n.ts");
const { frPending } = await import("../src/lib/locales/fr.pending.ts");
if (Object.hasOwn(translations, "fr")) throw new Error("French is now a runtime locale; export its actual dictionary instead of the inactive draft");
const catalogs = { ...translations, fr: frPending };
const root = join(dirname(fileURLToPath(import.meta.url)), "..", "localization", "exports");
const mode = process.argv[2];
if (!["--write", "--check"].includes(mode) || process.argv.length !== 3) {
  throw new Error("usage: node scripts/export-localization-catalogs.mjs --write|--check");
}
const keys = Object.keys(catalogs.en).sort();
const tokens = text => (text.match(/\{[^{}]*\}/g) ?? []).sort().join("|");
if (mode === "--write") mkdirSync(root, { recursive: true });
for (const [locale, catalog] of Object.entries(catalogs)) {
  const actual = Object.keys(catalog).sort();
  if (JSON.stringify(actual) !== JSON.stringify(keys)) throw new Error(`${locale}: catalog keys differ from English`);
  for (const key of keys) {
    if (typeof catalog[key] !== "string" || !catalog[key].trim() || tokens(catalog[key]) !== tokens(catalogs.en[key])) {
      throw new Error(`${locale}: empty or mismatched message ${key}`);
    }
  }
  const output = `${JSON.stringify(Object.fromEntries(keys.map(key => [key, catalog[key]])), null, 2)}\n`;
  const path = join(root, `${locale}.json`);
  if (mode === "--write") writeFileSync(path, output);
  else if (readFileSync(path, "utf8") !== output) throw new Error(`${locale}: export differs from its source`);
  console.log(`${locale}: ${keys.length} messages`);
}
