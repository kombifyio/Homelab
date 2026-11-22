<script lang="ts">
  import HomelabBrandLogo from "#lib/components/HomelabBrandLogo.svelte";
  import { tr } from "#lib/i18n.svelte.js";
  // Auth redirect is now handled by +page.ts load function
  // This page shows a loading state while the redirect happens
</script>

<div class="min-h-full flex items-center justify-center">
  <div class="text-center">
    <HomelabBrandLogo sizes="240px" class="mx-auto mb-4 w-60 max-w-full" />
    <p class="mb-4 text-sm text-muted-foreground">
      {tr("ui.redirect.homelab")}
    </p>
    <p class="text-muted-foreground mb-6">{tr("ui.redirect.generic")}</p>
    <a href="/login" class="text-primary hover:underline"
      >{tr("ui.configFlow.continueSetup")}</a
    >
  </div>
</div>
