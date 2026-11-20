<script lang="ts">
  import { tr } from "#lib/i18n.svelte.js";
  import {
    HOMELAB_ROSETTE,
    HOMELAB_ROSETTE_DARK,
    HOMELAB_TOOL_LOCKUP,
    HOMELAB_TOOL_LOCKUP_DARK,
    HOMELAB_TOOL_LOCKUP_DARK_SRCSET,
    HOMELAB_TOOL_LOCKUP_SRCSET,
  } from "#lib/brand-assets.js";

  // A lockup needs its rendered CSS width as `sizes` so the srcset keeps the
  // 96 px WebP up to 2x and only switches to the SVG master beyond it.
  type Props = { class?: string } & (
    | { collapsed: true; sizes?: never }
    | { collapsed?: false; sizes: string }
  );

  let { collapsed = false, sizes, class: className = "" }: Props = $props();

  const lightSrc = $derived(
    collapsed ? HOMELAB_ROSETTE : HOMELAB_TOOL_LOCKUP,
  );
  const darkSrc = $derived(
    collapsed ? HOMELAB_ROSETTE_DARK : HOMELAB_TOOL_LOCKUP_DARK,
  );
  const lightSrcset = $derived(
    collapsed ? undefined : HOMELAB_TOOL_LOCKUP_SRCSET,
  );
  const darkSrcset = $derived(
    collapsed ? undefined : HOMELAB_TOOL_LOCKUP_DARK_SRCSET,
  );
</script>

<img
  {sizes}
  srcset={darkSrcset}
  src={darkSrc}
  alt={tr("ui.homelabBrandLogo.kombifyHomelab")}
  class="{className} logo-dark"
/>
<img
  {sizes}
  srcset={lightSrcset}
  src={lightSrc}
  alt={tr("ui.homelabBrandLogo.kombifyHomelab")}
  class="{className} logo-light"
/>

<style>
  .logo-light {
    display: none;
  }

  :global(html[data-theme="light"]) .logo-light {
    display: block;
  }

  :global(html[data-theme="light"]) .logo-dark {
    display: none;
  }
</style>
