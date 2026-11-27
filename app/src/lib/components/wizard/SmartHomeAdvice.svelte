<script lang="ts">
  import { isMessageKey, tr, type MessageKey } from "#lib/i18n.svelte.js";
  import type { SmartHomeRecommendation } from "#lib/api/unifier.js";
  let {
    recommendation,
    compact = false,
  }: { recommendation?: SmartHomeRecommendation; compact?: boolean } = $props();

  const REQUIREMENT_KEYS: Record<string, MessageKey> = {
    "apps-require-haos": "wizard.smartHome.apps-require-haos",
    "detect-installed-form-and-api-capabilities":
      "wizard.smartHome.detect-installed-form-and-api-capabilities",
    "identify-and-exclusively-authorize-radio":
      "wizard.smartHome.identify-and-exclusively-authorize-radio",
    "separate-management-grant": "wizard.smartHome.separate-management-grant",
    "verified-proxmox-lan-and-guest-capacity":
      "wizard.smartHome.verified-proxmox-lan-and-guest-capacity",
    "verify-local-device-reachability":
      "wizard.smartHome.verify-local-device-reachability",
  };

  function requirementLabel(requirement: string): string {
    const known = REQUIREMENT_KEYS[requirement];
    if (known) return tr(known);
    const key = `wizard.smartHome.${requirement}`;
    return isMessageKey(key) ? tr(key) : requirement;
  }
</script>

{#if recommendation}
  <aside
    class="rounded-lg border border-border bg-muted/30 p-4 text-sm space-y-2"
    aria-label={tr("wizard.smartHome.advice")}
  >
    {#if !compact}<p class="font-medium">
        {tr("wizard.smartHome.advice")}
      </p>{/if}
    {#if recommendation.auto_detected}
      <p class="font-medium text-primary" data-testid="smart-home-auto-detected">
        {tr("wizard.smartHome.autoDetected")}
      </p>
    {/if}
    <p>
      {recommendation.management_scope === "observed"
        ? tr("wizard.smartHome.preserve")
        : recommendation.operating_form === "haos"
          ? tr("wizard.smartHome.haos")
          : tr("wizard.smartHome.container")}
    </p>
    {#if recommendation.requirements.length}
      <ul class="list-disc pl-5 space-y-1">
        {#each recommendation.requirements as requirement (requirement)}
          <li>{requirementLabel(requirement)}</li>
        {/each}
      </ul>
    {/if}
  </aside>
{/if}
