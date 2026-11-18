// Structural guard for Techstack's settings pages (KOMBIFY-DESIGN-SYSTEM-STANDARD.md §10.3, run first
// by `pnpm check`): a settings page is drawn only from the product's own settings components and
// layout, so it looks like every other settings page. It fails on the shape that once made the
// Companion tab look foreign (StackKits #3405): another package's own settings renderer, a stylesheet
// of its own, or a page that no longer sits inside the settings shell.
//
//   1. No file of the app imports the renderer entries of `@kombiverselabs/ui/companion-settings`
//      (the unreleased Brand section with its own Svelte/React renderer and stylesheet). Its
//      framework-neutral state logic is ported next to the page (`src/lib/settings/companion/`).
//   2. A settings surface (`src/routes/settings/**`, `src/lib/components/settings/**`,
//      `src/lib/settings/**`) holds no stylesheet, imports none, and has no `<style>` block: spacing,
//      colours, radii and typography come from the settings components and the app theme.
//   3. The settings route page renders through `SettingsShell` and `SettingsSection` of
//      `@kombiverselabs/ui/settings`.
import fs from "node:fs";
import path from "node:path";

const root = path.resolve(import.meta.dirname, "..");
const src = path.join(root, "src");
const SURFACES = [
  "src/routes/settings",
  "src/lib/components/settings",
  "src/lib/settings",
];
const failures = [];

function* walk(dir) {
  if (!fs.existsSync(dir)) return;
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const file = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      if (entry.name !== "node_modules" && entry.name !== ".svelte-kit")
        yield* walk(file);
    } else yield file;
  }
}

const rel = (file) => path.relative(root, file).split(path.sep).join("/");
const inSurface = (file) =>
  SURFACES.some((surface) => rel(file).startsWith(`${surface}/`));
const isCode = (file) =>
  /\.(ts|js|mjs|svelte)$/u.test(file) && !file.endsWith(".d.ts");

/** The code of a Svelte or script file without its comments, so commented-out markup proves nothing. */
function withoutComments(text) {
  return text
    .replace(/<!--[\s\S]*?-->/gu, "")
    .replace(/\/\*[\s\S]*?\*\//gu, "")
    .replace(/(^|[^:"'`\w])\/\/[^\n]*/gu, "$1");
}

/** Import specifiers of a source file: static, side-effect and dynamic imports, and re-exports. */
function specifiers(text) {
  const found = [];
  const pattern =
    /(?:\bimport\s+(?:[^"'`;]*?\sfrom\s+)?|\bexport\s+[^"'`;]*?\sfrom\s+|\bimport\s*\(\s*)(["'])([^"'\n]+)\1/gu;
  for (const match of text.matchAll(pattern)) found.push(match[2]);
  return found;
}

for (const file of walk(src)) {
  const where = rel(file);
  if (inSurface(file) && file.endsWith(".css")) {
    failures.push(
      `${where}: a settings surface holds no stylesheet of its own`,
    );
  }
  if (!isCode(file)) continue;
  const text = fs.readFileSync(file, "utf8");
  for (const specifier of specifiers(text)) {
    if (/^@[^/]+\/ui\/companion-settings(\/|$)/u.test(specifier)) {
      failures.push(
        `${where}: imports ${specifier}; the Companion tab is drawn from Techstack's own settings components (state logic: src/lib/settings/companion/)`,
      );
    }
    if (inSurface(file) && /\.css(\?.*)?$/u.test(specifier)) {
      failures.push(
        `${where}: a settings surface imports a stylesheet (${specifier})`,
      );
    }
  }
  if (
    inSurface(file) &&
    file.endsWith(".svelte") &&
    /<style[\s>]/u.test(text)
  ) {
    failures.push(
      `${where}: a settings component has no <style> block of its own`,
    );
  }
}

const pagePath = path.join(src, "routes/settings/+page.svelte");
if (!fs.existsSync(pagePath)) {
  failures.push("src/routes/settings/+page.svelte: missing");
} else {
  const page = withoutComments(fs.readFileSync(pagePath, "utf8"));
  for (const name of ["SettingsShell", "SettingsSection"]) {
    const imported = new RegExp(
      `import\\s*\\{[^}]*\\b${name}\\b[^}]*\\}\\s*from\\s*["']@kombiverselabs/ui/settings["']`,
      "u",
    ).test(page);
    if (!imported || !new RegExp(`<${name}[\\s>]`, "u").test(page)) {
      failures.push(
        `src/routes/settings/+page.svelte: renders without ${name} of @kombiverselabs/ui/settings`,
      );
    }
  }
}

if (failures.length > 0) {
  console.error(`check-settings-structure: ${failures.length} finding(s)`);
  for (const failure of failures) console.error(`  ${failure}`);
  process.exit(1);
}
console.log(
  "check-settings-structure: settings pages use only Techstack's own settings components",
);
