<!--
  Three-class placement board (D-2026-11-21-02). The page mounts this only
  while `placement_board_classes` is on. Native apps and Managed Apps are
  areas, not server columns. Dedicated servers reuse the existing column board.
-->
<script lang="ts">
  import type { ServiceStatusKind } from "@kombiverselabs/ui/service";
  import { tr } from "#lib/i18n.svelte.js";
  import Button from "#lib/components/ui/Button.svelte";
  import PlacementBoard from "#lib/components/inventory/PlacementBoard.svelte";
  import type {
    PlacementColumn,
    UnmanagedCard,
  } from "#lib/services/placement.js";
  import type { RegistryServer, RegistryService } from "#lib/api/registry.js";
  import type { CockpitRefreshError } from "#lib/monitoring/cockpit-snapshot.js";

  interface AreaCard {
    key: string;
    name: string;
    meta: string;
    serviceId?: string;
    status: ServiceStatusKind;
    statusLabel?: string;
    native: boolean;
    /** Native cards are 0. Managed cards do not invent a slot count. */
    managedServiceSlots: 0 | null;
  }

  interface Props {
    native: AreaCard[];
    managed: AreaCard[];
    columns: PlacementColumn[];
    loading: boolean;
    registryError: string | null;
    cockpitStale: boolean;
    cockpitError: CockpitRefreshError | null;
    onretry: () => void;
    onopen: (serviceId: string) => void;
    onmove: (service: RegistryService, target: RegistryServer) => void;
    onstack: (card: UnmanagedCard) => void;
  }

  let {
    native,
    managed,
    columns,
    loading,
    registryError,
    cockpitStale,
    cockpitError,
    onretry,
    onopen,
    onmove,
    onstack,
  }: Props = $props();
</script>

{#snippet areaCard(card: AreaCard, testId: string)}
  <article
    class="min-w-0 rounded-xl border border-border/80 bg-card/35 p-3.5"
    role="listitem"
    data-testid={testId}
    data-status={card.status}
    data-managed-slots={card.native ? 0 : undefined}
  >
    {#if card.native}
      <div class="flex flex-wrap gap-1.5">
        <span
          class="rounded-md border border-border bg-muted px-1.5 py-0.5 text-[9px] font-medium uppercase tracking-wider text-muted-foreground"
          data-testid="placement-native-badge"
        >
          {tr("ui.placementClasses.badge.native")}
        </span>
        <span
          class="rounded-md border border-border bg-muted px-1.5 py-0.5 text-[9px] font-medium uppercase tracking-wider text-muted-foreground"
          data-testid="placement-zero-slots"
        >
          {tr("ui.placementClasses.badge.zeroSlots")}
        </span>
      </div>
    {/if}
    <h3
      class="truncate text-sm font-semibold text-foreground"
      class:mt-2={card.native}
    >
      {card.name}
    </h3>
    <p class="mt-1 text-xs text-muted-foreground">{card.meta}</p>
    {#if card.statusLabel}
      <p class="mt-1 text-xs text-muted-foreground">{card.statusLabel}</p>
    {/if}
    {#if card.serviceId}
      <Button
        variant="secondary"
        size="sm"
        class="mt-3"
        onclick={() => card.serviceId && onopen(card.serviceId)}
      >
        {tr("entity.app.details")}
      </Button>
    {/if}
  </article>
{/snippet}

<div class="flex flex-col gap-8" data-testid="placement-class-board">
  <section
    aria-labelledby="placement-native-heading"
    data-testid="placement-class-native"
  >
    <h2
      id="placement-native-heading"
      class="text-sm font-semibold text-foreground"
    >
      {tr("ui.placementClasses.native.title")}
    </h2>
    <p class="mt-1 max-w-3xl text-sm text-muted-foreground">
      {tr("ui.placementClasses.native.summary")}
    </p>
    {#if native.length > 0}
      <div class="mt-4 grid gap-3 sm:grid-cols-2 xl:grid-cols-3" role="list">
        {#each native as card (card.key)}
          {@render areaCard(card, "placement-native-card")}
        {/each}
      </div>
    {:else if !loading && !registryError}
      <p
        class="mt-3 text-sm text-muted-foreground"
        data-testid="placement-native-empty"
      >
        {tr("ui.placementClasses.native.empty")}
      </p>
    {/if}
  </section>

  <section
    aria-labelledby="placement-managed-heading"
    data-testid="placement-class-managed"
  >
    <h2
      id="placement-managed-heading"
      class="text-sm font-semibold text-foreground"
    >
      {tr("ui.placementClasses.managed.title")}
    </h2>
    <p class="mt-1 max-w-3xl text-sm text-muted-foreground">
      {tr("ui.placementClasses.managed.summary")}
    </p>
    {#if managed.length > 0}
      <!-- One area of cards. No server headline, hostname or capacity column. -->
      <div
        class="mt-4 grid gap-3 sm:grid-cols-2 xl:grid-cols-3"
        role="list"
        data-testid="placement-managed-area"
      >
        {#each managed as card (card.key)}
          {@render areaCard(card, "placement-managed-card")}
        {/each}
      </div>
    {:else if !loading && !registryError}
      <p
        class="mt-3 text-sm text-muted-foreground"
        data-testid="placement-managed-empty"
      >
        {tr("ui.placementClasses.managed.empty")}
      </p>
    {/if}
  </section>

  <section
    aria-labelledby="placement-dedicated-heading"
    data-testid="placement-class-dedicated"
  >
    <h2
      id="placement-dedicated-heading"
      class="mb-1 text-sm font-semibold text-foreground"
    >
      {tr("ui.placementClasses.dedicated.title")}
    </h2>
    <p class="mb-4 max-w-3xl text-sm text-muted-foreground">
      {tr("ui.placementClasses.dedicated.summary")}
    </p>
    {#if !loading && !registryError && columns.length === 0}
      <p
        class="text-sm text-muted-foreground"
        data-testid="placement-dedicated-empty"
      >
        {tr("ui.placementClasses.dedicated.empty")}
      </p>
    {:else}
      <PlacementBoard
        {columns}
        {loading}
        {registryError}
        {cockpitStale}
        {cockpitError}
        {onretry}
        {onmove}
        {onstack}
      />
    {/if}
  </section>
</div>
