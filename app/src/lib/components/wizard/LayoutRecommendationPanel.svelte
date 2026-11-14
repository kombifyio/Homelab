<!--
  Recommended distribution (Unifier Layout Planner, phase L0).

  Shows where kombify suggests running each app: grouped per server with plain
  reasons, checked against the owner's hardware. It is information plus
  implicit acceptance: finishing the wizard run accepts a verified
  recommendation, so the panel asks nothing and offers no button of its own.

  `variant="full"` is the panel on the Goals step; `variant="summary"` is the
  compact reminder on the last active step. Advice that could not be verified
  is shown honestly and is never offered for acceptance.

  The planner looks at every server of the homelab, so the advice can go to
  another deployment than the one asked from, or to a free server as a new
  deployment; that target is the first thing the panel says.

  "Choose another server" (full variant, add-use-case): each app can be moved
  to another server of the owner's deployments. The choice is a pin the
  planner re-solves and StackKits re-verifies; only that new answer can be
  accepted, and "Use the recommendation" drops every choice.
-->
<script lang="ts">
  import { CircleCheck, Info } from "@lucide/svelte";
  import ErrorCallout from "#lib/components/ui/ErrorCallout.svelte";
  import LoadingNote from "#lib/components/ui/LoadingNote.svelte";
  import { acceptableLayoutRecommendation } from "#lib/api/layout.js";
  import { formatNumber, getLocale, tr } from "#lib/i18n.svelte.js";
  import {
    layoutPreviewView,
    type LayoutPreviewState,
  } from "#lib/wizard/LayoutPreviewController.js";
  import { layoutReasonText } from "#lib/wizard/layout-reasons.js";
  import {
    appLabel,
    layoutNodeGroups,
    memoryHint,
  } from "#lib/wizard/layout-view.js";
  import type {
    LayoutAssignmentView,
    LayoutServerChoice,
  } from "#lib/api/layout.js";

  interface Props {
    state: LayoutPreviewState;
    variant?: "full" | "summary";
    /** Asks the same question again after the preview was unavailable. */
    onretry?: () => void;
    /** The owner's server per use case. */
    pins?: Record<string, string>;
    /** Pins a use case to a server; without it the panel offers no choice. */
    onpin?: (goal: string, serverId: string | null) => void;
    /** Restores the choices before a refused one. */
    onundopin?: () => void;
    /** Drops every choice and asks for the plain recommendation again. */
    onclearpins?: () => void;
  }

  let {
    state,
    variant = "full",
    onretry,
    pins = {},
    onpin,
    onundopin,
    onclearpins,
  }: Props = $props();

  const uid = $props.id();
  const view = $derived(layoutPreviewView(state));
  const loading = $derived(state.phase === "loading");
  const groups = $derived(view ? layoutNodeGroups(view) : []);
  const verified = $derived(view?.verification.status === "verified");
  const acceptable = $derived(
    !loading && acceptableLayoutRecommendation(view) !== undefined,
  );
  const verificationReasons = $derived(view?.verification.reasons ?? []);
  // Notes beside a verified result are Techstack's own, localized codes.
  const verifiedNotes = $derived(
    verified
      ? verificationReasons.filter((reason) => !reason.code.startsWith("stackkits:"))
      : [],
  );
  // Where the selection goes, when that is not the deployment asked from.
  const TARGET_NOTES = new Set([
    "extends_other_deployment",
    "joins_bare_server",
    "goes_to_chosen_deployment",
    "found_on_bare_server",
  ]);
  const targetNote = $derived(
    view?.notes?.find((note) => TARGET_NOTES.has(note.code)),
  );
  const notes = $derived(
    view?.notes?.filter((note) => !TARGET_NOTES.has(note.code)) ?? [],
  );
  const nodeName = $derived(
    new Map((view?.nodes ?? []).map((node) => [node.id, node.name])),
  );

  function list(values: string[]): string {
    try {
      return new Intl.ListFormat(getLocale(), {
        style: "long",
        type: "conjunction",
      }).format(values);
    } catch {
      return values.join(", ");
    }
  }

  function gb(value: number): string {
    return formatNumber(value, { maximumFractionDigits: 1 });
  }

  // Servers an app can be moved to, grouped by deployment.
  const choices = $derived(view?.servers ?? []);
  // Only an extension of a deployment takes a choice: a free server is not
  // choosable, and an add-on stays with the use case it adds to.
  const canChoose = $derived(
    Boolean(onpin) && choices.length > 1 && view?.target?.action === "extend",
  );
  function choosable(assignment: LayoutAssignmentView): boolean {
    return (
      canChoose &&
      Boolean(assignment.goal) &&
      assignment.reasons[0]?.code !== "with_use_case" &&
      choices.some((server) => server.id === assignment.node_id)
    );
  }
  const choiceGroups = $derived.by(() => {
    const groups: { id: string; name: string; servers: LayoutServerChoice[] }[] = [];
    for (const server of choices) {
      let group = groups.find((candidate) => candidate.id === server.stack_id);
      if (!group) {
        group = { id: server.stack_id, name: server.stack_name, servers: [] };
        groups.push(group);
      }
      group.servers.push(server);
    }
    return groups;
  });
  const hasPins = $derived(Object.keys(pins).length > 0);

  function choose(assignment: LayoutAssignmentView, serverId: string) {
    if (serverId === (pins[assignment.goal] ?? assignment.node_id)) return;
    onpin?.(assignment.goal, serverId);
  }

  const verificationKey = $derived(`layout.verification.${view?.verification.status ?? "unavailable"}`);
</script>

{#snippet serverOption(server: LayoutServerChoice)}
  <option value={server.id} disabled={!server.connected}>
    {server.connected
      ? server.name
      : tr("layout.pin.notConnected", { server: server.name })}
  </option>
{/snippet}

{#if state.phase === "loading" && !view}
  <LoadingNote
    label={tr("layout.panel.loading")}
    testid="layout-recommendation-loading"
  />
{:else if state.phase === "failed" && state.reason === "pinRefused"}
  <ErrorCallout
    tone="info"
    message={tr("layout.state.pinRefused")}
    onRetry={onundopin ?? onclearpins ?? null}
    retryLabel={tr(onundopin ? "layout.pin.undo" : "layout.pin.reset")}
    testId="layout-pin-refused"
  />
{:else if state.phase === "failed"}
  <ErrorCallout
    tone="info"
    message={tr(`layout.state.${state.reason}`)}
    onRetry={state.reason === "unavailable" ? onretry : null}
    testId="layout-recommendation-unavailable"
  />
{:else if view && variant === "summary"}
  <section
    data-kx="plate"
    data-testid="layout-recommendation-summary"
    data-layout-verification={view.verification.status}
    aria-labelledby="{uid}-title"
    aria-busy={loading}
    class="rounded-xl border border-border bg-card p-4"
  >
    <h3 id="{uid}-title" class="text-sm font-semibold text-foreground">
      {tr("layout.summary.title")}
      {#if loading}
        <span class="ms-2 text-xs font-normal text-muted-foreground">
          {tr("layout.panel.updating")}
        </span>
      {/if}
    </h3>
    {#if targetNote}
      <p
        class="mt-2 text-sm font-medium text-foreground"
        data-testid="layout-target"
        data-layout-target={view.target?.action}
      >
        {layoutReasonText(targetNote)}
      </p>
    {/if}
    {#if groups.some((group) => group.assignments.length > 0)}
      <ul class="mt-2 space-y-1 text-sm text-foreground">
        {#each groups.filter((group) => group.assignments.length > 0) as group (group.node.id)}
          <li>
            {tr("layout.summary.line", {
              node: group.node.name,
              apps: list(group.assignments.map((assignment) => assignment.title)),
            })}
          </li>
        {/each}
      </ul>
    {/if}
    {#if acceptable}
      <div class="mt-3 space-y-0.5 text-sm" role="status">
        <p class="flex items-start gap-2 text-foreground">
          <CircleCheck
            class="mt-0.5 size-4 shrink-0 text-success"
            aria-hidden="true"
          />
          <span>{tr("layout.verification.verified")}</span>
        </p>
        {#each verifiedNotes as note, index (index)}
          <p class="ps-6 text-xs text-muted-foreground">
            {layoutReasonText(note)}
          </p>
        {/each}
        <p class="ps-6 text-muted-foreground">
          {tr("layout.summary.applies")}
        </p>
      </div>
    {:else if !loading}
      <p
        class="mt-3 flex items-start gap-2 text-sm text-muted-foreground"
        role="status"
      >
        <Info class="mt-0.5 size-4 shrink-0" aria-hidden="true" />
        <span>
          {verified ? tr("layout.panel.notCurrent") : tr(verificationKey)}
        </span>
      </p>
    {/if}
  </section>
{:else if view}
  <section
    data-kx="plate"
    data-testid="layout-recommendation-panel"
    data-layout-outcome={view.outcome}
    data-layout-verification={view.verification.status}
    aria-labelledby="{uid}-title"
    aria-busy={loading}
    class="rounded-xl border border-border bg-card p-4 sm:p-5"
  >
    <header class="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1">
      <h3 id="{uid}-title" class="text-base font-semibold text-foreground">
        {tr("layout.panel.title")}
      </h3>
      {#if loading}
        <span class="text-xs text-muted-foreground" role="status">
          {tr("layout.panel.updating")}
        </span>
      {/if}
    </header>
    {#if acceptable}
      <p class="mt-1 text-sm text-muted-foreground">
        {tr("layout.panel.intro")}
      </p>
    {/if}
    {#if hasPins && variant === "full"}
      <div
        class="mt-3 flex flex-wrap items-center justify-between gap-2 text-sm"
        data-testid="layout-pins"
      >
        <p class="text-foreground">{tr("layout.pin.chosen")}</p>
        {#if onclearpins}
          <button
            type="button"
            class="rounded-md text-sm font-medium text-primary underline-offset-2 hover:underline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring"
            onclick={onclearpins}
            data-testid="layout-pins-reset"
          >
            {tr("layout.pin.reset")}
          </button>
        {/if}
      </div>
    {/if}
    {#if targetNote}
      <p
        class="mt-3 text-sm font-medium text-foreground"
        data-testid="layout-target"
        data-layout-target={view.target?.action}
      >
        {layoutReasonText(targetNote)}
      </p>
    {/if}

    {#if view.state !== "open" && view.state !== "unverified"}
      <p class="mt-3 text-sm text-muted-foreground">
        {tr("layout.panel.notCurrent")}
      </p>
    {/if}

    {#if view.outcome !== "feasible"}
      <p class="mt-3 text-sm text-foreground">
        {tr(`layout.outcome.${view.outcome}`)}
      </p>
    {/if}

    {#if groups.length > 0}
      <div class="mt-4 space-y-3">
        {#each groups as group (group.node.id)}
          {@const memory = memoryHint(group.node)}
          <section
            class="rounded-lg border border-border/70 p-3"
            aria-label={group.node.name}
            data-testid="layout-node"
          >
            <div class="flex flex-wrap items-center gap-x-2 gap-y-1">
              <h4 class="font-medium text-foreground">{group.node.name}</h4>
              {#if group.node.primary}
                <span
                  data-kx="tag"
                  class="rounded px-1.5 py-0.5 text-xs text-muted-foreground"
                  >{tr("layout.panel.mainServer")}</span
                >
              {/if}
              {#if group.node.planned}
                <span
                  data-kx="tag"
                  class="rounded px-1.5 py-0.5 text-xs text-muted-foreground"
                  >{tr("layout.panel.newServer")}</span
                >
              {/if}
              {#if group.node.availability.class === "always-on" || group.node.availability.class === "part-time"}
                <span
                  data-kx="status"
                  data-status={group.node.availability.class === "always-on"
                    ? "ok"
                    : "warn"}
                  class="inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium"
                  >{tr(`layout.availability.${group.node.availability.class}`)}</span
                >
              {/if}
              {#if memory}
                <span class="text-xs text-muted-foreground">
                  {tr("layout.panel.reserved", {
                    used: gb(memory.used),
                    total: gb(memory.total),
                  })}
                </span>
              {/if}
            </div>

            {#if group.assignments.length > 0}
              <ul class="mt-3 space-y-3">
                {#each group.assignments as assignment (assignment.workload)}
                  {@const [first, ...rest] = assignment.reasons}
                  <li data-testid="layout-assignment">
                    <p class="text-sm font-medium text-foreground">
                      {assignment.title || appLabel(assignment.workload)}
                    </p>
                    {#if first}
                      <p class="text-sm text-muted-foreground">
                        {layoutReasonText(first)}
                      </p>
                    {/if}
                    {#if choosable(assignment)}
                      <label
                        class="mt-2 flex flex-wrap items-center gap-2 text-xs text-muted-foreground"
                      >
                        <span>{tr("layout.pin.choose")}</span>
                        <select
                          class="rounded-md border border-border bg-background px-2 py-1 text-sm text-foreground"
                          value={pins[assignment.goal] ?? assignment.node_id}
                          disabled={loading}
                          aria-label={tr("layout.pin.label", {
                            app: assignment.title || appLabel(assignment.workload),
                          })}
                          onchange={(event) =>
                            choose(assignment, event.currentTarget.value)}
                          data-testid="layout-pin-select"
                        >
                          {#if choiceGroups.length > 1}
                            {#each choiceGroups as group (group.id)}
                              <optgroup label={group.name}>
                                {#each group.servers as server (server.id)}
                                  {@render serverOption(server)}
                                {/each}
                              </optgroup>
                            {/each}
                          {:else}
                            {#each choices as server (server.id)}
                              {@render serverOption(server)}
                            {/each}
                          {/if}
                        </select>
                      </label>
                    {/if}
                    {#if rest.length > 0}
                      <details class="mt-1 text-sm">
                        <summary
                          class="cursor-pointer select-none text-xs text-primary"
                        >
                          {tr("layout.panel.moreReasons")}
                        </summary>
                        <ul
                          class="mt-1 list-disc space-y-1 ps-5 text-muted-foreground"
                        >
                          {#each rest as reason, index (index)}
                            <li>{layoutReasonText(reason)}</li>
                          {/each}
                        </ul>
                      </details>
                    {/if}
                  </li>
                {/each}
              </ul>
            {:else}
              <p class="mt-2 text-sm text-muted-foreground">
                {tr("layout.panel.nothingNew")}
              </p>
            {/if}
          </section>
        {/each}
      </div>
    {/if}

    {#if view.existing && view.existing.length > 0}
      <div class="mt-4" data-testid="layout-existing">
        <h4 class="text-sm font-medium text-foreground">
          {tr("layout.panel.staysTitle")}
        </h4>
        <ul class="mt-1 space-y-0.5 text-sm text-muted-foreground">
          {#each view.existing as item (item.workload)}
            <li>
              {tr("layout.panel.staysLine", {
                app: appLabel(item.workload),
                node: nodeName.get(item.node_id) ?? item.node_id,
              })}
            </li>
          {/each}
        </ul>
      </div>
    {/if}

    {#if view.conflicts && view.conflicts.length > 0}
      <div class="mt-4" data-testid="layout-conflicts">
        <h4 class="text-sm font-medium text-foreground">
          {tr("layout.panel.conflictsTitle")}
        </h4>
        <ul class="mt-1 list-disc space-y-1 ps-5 text-sm text-muted-foreground">
          {#each view.conflicts as conflict, index (index)}
            {#each conflict.reasons as reason, reasonIndex (reasonIndex)}
              <li>{layoutReasonText(reason)}</li>
            {/each}
          {/each}
        </ul>
      </div>
    {/if}

    {#if notes.length > 0}
      <div class="mt-4" data-testid="layout-notes">
        <h4 class="text-sm font-medium text-foreground">
          {tr("layout.panel.notesTitle")}
        </h4>
        <ul class="mt-1 list-disc space-y-1 ps-5 text-sm text-muted-foreground">
          {#each notes as note, index (index)}
            <li>{layoutReasonText(note)}</li>
          {/each}
        </ul>
      </div>
    {/if}

    <div
      class="mt-4 border-t border-border/60 pt-3 text-sm"
      data-testid="layout-verification"
    >
      {#if verified}
        <p class="flex items-start gap-2 text-foreground" role="status">
          <CircleCheck
            class="mt-0.5 size-4 shrink-0 text-success"
            aria-hidden="true"
          />
          <span>{tr("layout.verification.verified")}</span>
        </p>
        {#if verifiedNotes.length > 0}
          <ul
            class="mt-1 list-disc space-y-1 ps-11 text-xs text-muted-foreground"
            data-testid="layout-verification-notes"
          >
            {#each verifiedNotes as reason, index (index)}
              <li>{layoutReasonText(reason)}</li>
            {/each}
          </ul>
        {/if}
      {:else}
        <p class="flex items-start gap-2 text-muted-foreground" role="status">
          <Info class="mt-0.5 size-4 shrink-0" aria-hidden="true" />
          <span>{tr(verificationKey)}</span>
        </p>
        {#if verificationReasons.length > 0}
          <details class="mt-1 ps-6 text-xs text-muted-foreground">
            <summary class="cursor-pointer select-none">
              {tr("layout.verification.details")}
            </summary>
            <ul class="mt-1 list-disc space-y-1 ps-5">
              {#each verificationReasons as reason, index (index)}
                <li>{layoutReasonText(reason)}</li>
              {/each}
            </ul>
          </details>
        {/if}
      {/if}
    </div>
  </section>
{/if}
