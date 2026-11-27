<script lang="ts">
  import { ArrowUpRight, Check, Layers, RefreshCw } from "@lucide/svelte";
  import type { DetectedExistingApp } from "#lib/api/unifier.js";
  import { tr } from "#lib/i18n.svelte.js";
  import { isSmartHomeApp } from "#lib/wizard/existing-apps.js";

  let {
    apps,
    smartHomeOrigin,
    onAdopt,
    onOverride,
    compact = false,
  }: {
    apps: DetectedExistingApp[];
    /** Current Smart Home instance-origin setting ("existing" | "new" | undefined). */
    smartHomeOrigin?: string | boolean;
    onAdopt?: (app: DetectedExistingApp) => void;
    onOverride?: (app: DetectedExistingApp) => void;
    compact?: boolean;
  } = $props();

  const firstClassApps = $derived(
    apps.filter((app) => app.spectrum_tier === "first_class"),
  );

  function appIcon(app: DetectedExistingApp): string | undefined {
    return app.logo?.url || app.icon_url || undefined;
  }

  function appDocs(app: DetectedExistingApp): string | undefined {
    return app.docs_url || app.homepage_url || undefined;
  }
</script>

{#if firstClassApps.length > 0}
  <section
    class="rounded-xl border border-primary/30 bg-primary/5 text-sm space-y-3 {compact
      ? 'p-3'
      : 'p-4'}"
    aria-label={tr("wizard.existingApps.title")}
    data-testid="existing-app-takeover-banner"
  >
    <div class="flex items-start gap-2.5">
      <Layers
        size={18}
        class="mt-0.5 shrink-0 text-primary"
        aria-hidden="true"
      />
      <div class="min-w-0 space-y-0.5">
        <p class="font-medium text-foreground">
          {tr("wizard.existingApps.title")}
        </p>
        {#if !compact}
          <p class="text-xs text-muted-foreground">
            {tr("wizard.existingApps.subtitle")}
          </p>
        {/if}
      </div>
    </div>

    <ul class="space-y-2.5">
      {#each firstClassApps as app (app.service_id || app.app_id)}
        {@const icon = appIcon(app)}
        {@const docs = appDocs(app)}
        {@const smartHome = isSmartHomeApp(app)}
        {@const adoptingExisting = smartHome && smartHomeOrigin !== "new"}
        <li
          class="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-border bg-card/80 px-3 py-2.5"
          data-testid={`detected-existing-app-${app.app_id}`}
        >
          <div class="flex min-w-0 items-center gap-3">
            {#if icon}
              <img
                src={icon}
                alt=""
                class="h-8 w-8 shrink-0 rounded-md object-contain"
                loading="lazy"
              />
            {:else}
              <span
                class="grid h-8 w-8 shrink-0 place-items-center rounded-md bg-muted text-xs font-semibold uppercase text-muted-foreground"
                aria-hidden="true"
              >
                {app.display_name.slice(0, 2)}
              </span>
            {/if}
            <div class="min-w-0">
              <div class="flex flex-wrap items-center gap-x-2 gap-y-0.5">
                <strong class="font-medium text-foreground">
                  {app.display_name}
                </strong>
                {#if app.vendor_name}
                  <span class="text-xs text-muted-foreground">
                    {tr("wizard.existingApps.byVendor", {
                      vendor: app.vendor_name,
                    })}
                  </span>
                {/if}
                {#if docs}
                  <a
                    href={docs}
                    target="_blank"
                    rel="noreferrer"
                    class="inline-flex items-center gap-0.5 text-xs text-primary hover:underline"
                  >
                    {tr("wizard.existingApps.docs")}
                    <ArrowUpRight size={12} aria-hidden="true" />
                  </a>
                {/if}
              </div>
              {#if !smartHome}
                <p class="text-xs text-muted-foreground">
                  {tr("wizard.existingApps.stackItNote")}
                </p>
              {/if}
            </div>
          </div>

          {#if smartHome && (onAdopt || onOverride)}
            <div class="flex flex-wrap items-center gap-2">
              <button
                type="button"
                class="inline-flex items-center gap-1.5 rounded-md border px-2.5 py-1 text-xs font-medium transition-colors {adoptingExisting
                  ? 'border-primary bg-primary text-primary-foreground'
                  : 'border-border bg-background text-foreground hover:border-primary'}"
                aria-pressed={adoptingExisting}
                data-testid="existing-app-adopt-btn"
                onclick={() => onAdopt?.(app)}
              >
                {#if adoptingExisting}
                  <Check size={13} aria-hidden="true" />
                  {tr("wizard.existingApps.adopted")}
                {:else}
                  {tr("wizard.existingApps.adoptInPlace")}
                {/if}
              </button>
              <button
                type="button"
                class="inline-flex items-center gap-1.5 rounded-md border px-2.5 py-1 text-xs font-medium transition-colors {!adoptingExisting
                  ? 'border-primary bg-primary/10 text-primary'
                  : 'border-border bg-background text-muted-foreground hover:text-foreground'}"
                aria-pressed={!adoptingExisting}
                data-testid="existing-app-override-btn"
                onclick={() => onOverride?.(app)}
              >
                <RefreshCw size={12} aria-hidden="true" />
                {!adoptingExisting
                  ? tr("wizard.existingApps.freshSelected")
                  : tr("wizard.existingApps.installFresh")}
              </button>
            </div>
          {/if}
        </li>
      {/each}
    </ul>
  </section>
{/if}
