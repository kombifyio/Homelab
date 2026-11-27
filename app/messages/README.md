# Homelab localization catalogs

`en.json` is the English source catalog. `de.json`, `es.json`, `zh-Hans.json`,
`hi.json` and `ar.json` are complete translations for the active runtime
locales with identical message IDs and placeholders. `fr.json` remains an
inactive draft compiled for export/QA readiness and is not admitted to runtime
locale negotiation (`src/lib/i18n.ts`) until translation QA is admitted.

Paraglide JS compiles `project.inlang/settings.json` through the Vite pipeline
(and `scripts/compile-paraglide.mjs`) into ignored `src/lib/paraglide/` modules.
`src/lib/i18n.ts` and `src/lib/i18n.svelte.ts` provide the thin runtime adapter
preserving `homelab-locale` persistence, browser language negotiation, Arabic
RTL direction, and existing helper signatures.
