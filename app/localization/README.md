# Localization review exports

The JSON catalogs in `exports/` are deterministic projections of the Paraglide
source catalogs in `messages/`, including the inactive French draft. Regenerate
them with `node scripts/export-localization-catalogs.mjs --write`; the app's
localization check rejects stale exports, missing keys and changed interpolation
tokens.

These files prepare exact-source review and do not enable French, attest its
translation quality, or establish rendered-layout and product-journey evidence.
The Windows resource drafts have their own separate review scope.
