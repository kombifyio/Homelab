/** kombify-Brand commit the vendored /brand files were taken from. */
const ASSET_REVISION = "b29d3daa";

/**
 * Homelab assets from kombify-Brand/assets/logo/system/homelab.
 * The WebP web tier (128 px rosette, 96 px tall lockup) serves UI chrome; the
 * lockup SVG master stays in the srcset for renders the web tier cannot cover.
 */
export const HOMELAB_ROSETTE = `/brand/homelab-rosette-128.webp?v=${ASSET_REVISION}`;
export const HOMELAB_ROSETTE_DARK = `/brand/homelab-rosette-128-dark.webp?v=${ASSET_REVISION}`;
export const HOMELAB_TOOL_LOCKUP = `/brand/homelab-tool-lockup-96.webp?v=${ASSET_REVISION}`;
export const HOMELAB_TOOL_LOCKUP_DARK = `/brand/homelab-tool-lockup-96-dark.webp?v=${ASSET_REVISION}`;
export const HOMELAB_TOOL_LOCKUP_SRCSET = `${HOMELAB_TOOL_LOCKUP} 681w, /brand/homelab-tool-lockup.svg?v=${ASSET_REVISION} 2269w`;
export const HOMELAB_TOOL_LOCKUP_DARK_SRCSET = `${HOMELAB_TOOL_LOCKUP_DARK} 681w, /brand/homelab-tool-lockup-dark.svg?v=${ASSET_REVISION} 2269w`;

/** Vendored umbrella wordmark for global Kombify footers; it never includes a rosette or a remote request. */
export const KOMBIFY_MAIN_WORDMARK =
  "/brand/kombify-brand-wordmark.png?v=main-wordmark-v1";
