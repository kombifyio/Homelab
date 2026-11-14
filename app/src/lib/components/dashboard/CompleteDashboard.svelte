<script lang="ts">
  import { tr } from "#lib/i18n.svelte.js";
  import type { Snippet } from "svelte";
  import { MediaQuery } from "svelte/reactivity";

  import NodeMessageLine from "#lib/components/dashboard/NodeMessageLine.svelte";
  import NodeRow from "#lib/components/dashboard/NodeRow.svelte";
  import PeoplePanel from "#lib/components/dashboard/PeoplePanel.svelte";
  import TrademarkNotice from "#lib/components/TrademarkNotice.svelte";
  import {
    groupNodesByDeployment,
    type NodeView,
  } from "#lib/dashboard/homelab-model.js";
  import type { NodeMessage } from "#lib/dashboard/node-messages.js";
  import type {
    DeviceEntry,
    PersonEntry,
    SectionState,
  } from "#lib/dashboard/people.js";

  interface Props {
    nodes: NodeView[];
    deploymentName: (deploymentId: string) => string;
    nodeHref: (node: NodeView) => string;
    onOpenService: (serviceId: string) => void;
    people: SectionState<PersonEntry>;
    devices: SectionState<DeviceEntry>;
    canonicalInventoryUnavailable?: boolean;
    /** What one Node has to say; shown as its fixed status line. */
    nodeMessages?: (node: NodeView) => NodeMessage[];
    /** The details of one Node message; they open as an overlay on click. */
    nodeMessageDetails?: Snippet<[NodeView, NodeMessage]>;
    /** Node-scoped controls; opened from the Node's actions menu. */
    nodeActions?: Snippet<[NodeView]>;
    /** Messages about the list itself, shown in the status line below it. */
    listMessages?: NodeMessage[];
    listMessageDetails?: Snippet<[NodeMessage]>;
  }

  let {
    nodes,
    deploymentName,
    nodeHref,
    onOpenService,
    people,
    devices,
    canonicalInventoryUnavailable = false,
    nodeMessages,
    nodeMessageDetails,
    nodeActions,
    listMessages = [],
    listMessageDetails,
  }: Props = $props();

  // Below the shell's phone breakpoint (48rem) the page head (title, add
  // server) sits in the top bar, so the cards start at the top; the list's
  // heading line is head too and stays for screen readers, its "All
  // services" link follows the cards. The cards and the devices column are
  // the same as on the desktop, in one column.
  const wide = new MediaQuery("min-width: 48rem");
  const groups = $derived(groupNodesByDeployment(nodes, deploymentName));
  const allListMessages = $derived<NodeMessage[]>([
    ...(canonicalInventoryUnavailable
      ? [
          {
            id: "inventory-unavailable",
            kind: "inventory-unavailable",
            tone: "warning" as const,
            summary: tr("ui.dashboard.inventoryUnavailable"),
          },
        ]
      : []),
    ...listMessages,
  ]);
</script>

{#snippet listDetails(message: NodeMessage)}
  {#if message.kind === "inventory-unavailable"}
    <p class="text-sm text-foreground/90" data-testid="dashboard-inventory-unavailable">
      {tr("ui.dashboard.inventoryUnavailable")}
    </p>
  {:else}
    {@render listMessageDetails?.(message)}
  {/if}
{/snippet}

<!-- From 48rem up viewport-bound: the host gives this a bounded height; the
     Node list and the devices column scroll on their own, the page never
     grows. On a phone the Node list flows with the page and takes the screen. -->
<div
  class="flex flex-1 flex-col gap-3"
  data-testid="dashboard-complete"
>
  <div
    class="grid gap-4 md:min-h-[18rem] md:flex-1 md:basis-0 md:grid-rows-[minmax(0,1fr)] xl:grid-cols-[minmax(0,2fr)_minmax(0,1fr)]"
  >
    <section
      class="flex min-h-0 min-w-0 flex-col gap-2"
      aria-labelledby="dashboard-nodes-title"
      data-testid="server-inventory-panel"
    >
      <div
        class="flex shrink-0 items-baseline justify-between gap-2 {wide.current
          ? ''
          : 'sr-only'}"
      >
        <h2
          id="dashboard-nodes-title"
          class="text-base font-semibold text-foreground"
        >
          {tr("ui.completeDashboard.nodesApps")}
        </h2>
        {#if wide.current}
          <a class="text-sm text-primary hover:underline" href="/services"
            >{tr("ui.completeDashboard.allServices")}</a
          >
        {/if}
      </div>

      <!-- The list's left edge is also the cards' edge. The scroll box reaches
           into the page gutter on the left (negative margin, equal padding)
           so a federation bundle can sit there without pushing any card. -->
      <div class="relative flex min-h-0 flex-1 flex-col">
      <div
        class="dashboard-scroll -ml-3.5 flex min-h-0 flex-1 flex-col gap-2.5 pr-1 md:overflow-y-auto pl-3.5 md:-ml-[22px] md:pl-[22px]"
        style:padding-bottom={allListMessages.length > 0 ? "2.75rem" : undefined}
        data-testid="dashboard-node-list"
      >
        {#if nodes.length === 0}
          <p class="text-sm text-muted-foreground">
            {tr("ui.dashboard.noNodes")}
          </p>
        {/if}

        {#each groups as group (group.key)}
          <div class="relative flex shrink-0 flex-col gap-2.5" data-testid="node-group">
            {#if group.federated}
              <!-- A federated deployment: its Nodes stay together, bundled by
                   a thin line in the left gutter carrying the deployment's
                   name. Peripheral context only: it never indents a card. -->
              <div
                class="federation-bundle absolute inset-y-0 -left-3.5 flex w-3.5 flex-col items-center gap-1.5 py-1.5 md:-left-[22px] md:w-[22px]"
                data-testid="federation-bundle"
              >
                <span class="w-0.5 flex-1 rounded-sm"></span>
                <span
                  class="federation-label font-mono text-[10px] tracking-[0.12em] uppercase"
                  >{group.label}</span
                >
                <span class="w-0.5 flex-1 rounded-sm"></span>
              </div>
            {/if}
            {#each group.nodes as node (node.key)}
              <NodeRow
                {node}
                detailsHref={nodeHref(node)}
                {onOpenService}
                messages={nodeMessages?.(node) ?? []}
              >
                {#snippet messageDetails(message)}
                  {@render nodeMessageDetails?.(node, message)}
                {/snippet}
                {#snippet actions()}
                  {@render nodeActions?.(node)}
                {/snippet}
              </NodeRow>
            {/each}
          </div>
        {/each}
      </div>

      <!-- Messages about the list itself float as one line over the list's
           bottom edge, where there is room anyway: nothing above or between
           the cards makes way for them. -->
      {#if allListMessages.length > 0}
        <NodeMessageLine
          class="absolute inset-x-1 bottom-1 rounded-lg border bg-card shadow-md"
          messages={allListMessages}
          details={listDetails}
          label={tr("ui.nodeMessage.listPanel")}
        />
      {/if}
      </div>

      {#if !wide.current}
        <a
          class="shrink-0 self-start text-sm text-primary hover:underline"
          href="/services"
          data-testid="dashboard-all-services">{tr("ui.completeDashboard.allServices")}</a
        >
      {/if}
      <TrademarkNotice class="shrink-0" />
    </section>

    <div
      class="dashboard-scroll block min-h-0 min-w-0 pr-1 md:hidden md:overflow-y-auto xl:block"
      data-testid="dashboard-device-list"
    >
      <PeoplePanel {people} {devices} />
    </div>
  </div>
</div>

<style>
  /* Subtle scroll affordance: a thin bar and a short fade at both edges. */
  .dashboard-scroll {
    scrollbar-width: thin;
    scrollbar-color: color-mix(in oklab, var(--color-muted-foreground) 35%, transparent)
      transparent;
    mask-image: linear-gradient(
      to bottom,
      transparent 0,
      #000 10px,
      #000 calc(100% - 14px),
      transparent 100%
    );
    padding-block: 6px;
  }
  /* Federation is a relation, not a status, so it gets its own hue instead
     of borrowing a semantic token that already means something. */
  .federation-bundle {
    --federation: oklch(0.66 0.17 290);
  }
  .federation-bundle span:not(.federation-label) {
    background: var(--federation);
  }
  .federation-label {
    writing-mode: vertical-rl;
    transform: rotate(180deg);
    color: color-mix(in oklab, var(--federation) 70%, var(--color-foreground));
    text-decoration: none;
  }
</style>
