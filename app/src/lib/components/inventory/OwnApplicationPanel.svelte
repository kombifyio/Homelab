<!--
  An app StackKits runs as an own application (adopted with Stack it): its
  signed state, health from the last status observation, image, and the data
  copy of its latest tag update with its size, Restore and Discard. Every
  change is reviewed on the server first and approved once.
-->
<script lang="ts">
  import {
    isMessageKey,
    tr,
    formatDateTime,
    type MessageKey,
  } from "#lib/i18n.svelte.js";
  import Button from "#lib/components/ui/Button.svelte";
  import ErrorCallout from "#lib/components/ui/ErrorCallout.svelte";
  import type { CanonicalService } from "#lib/api/services.js";
  import {
    ownApplicationGrants,
    ownApplicationHealth,
    ownApplicationReason,
    refreshOwnApplicationStatus,
    runOwnApplicationBackup,
    runOwnApplicationCheckpoint,
    runOwnApplicationPower,
  } from "#lib/services/own-application.js";
  import { byteSize } from "#lib/services/stack-it-plan.js";

  let {
    component,
    onChanged,
  }: {
    component: CanonicalService;
    onChanged: () => Promise<void>;
  } = $props();

  let busy = $state(false);
  let error = $state<string | null>(null);

  const own = $derived(component.own_application);
  const status = $derived(own?.status);
  const checkpoint = $derived(status?.checkpoint);
  const health = $derived(ownApplicationHealth(component));
  const showCheckpoint = $derived(
    checkpoint && checkpoint.state !== "discarded" && checkpoint.state !== "planned",
  );

  async function act(run: () => Promise<string | null>) {
    busy = true;
    error = null;
    try {
      error = await run();
      await onChanged();
    } finally {
      busy = false;
    }
  }

  const HEALTH_KEYS: Record<string, MessageKey> = {
    healthy: "ui.ownApp.health.healthy",
    running: "ui.ownApp.health.running",
    starting: "ui.ownApp.health.starting",
    stopped: "ui.ownApp.health.stopped",
    unhealthy: "ui.ownApp.health.unhealthy",
    unknown: "ui.ownApp.health.unknown",
  };
  const CHECKPOINT_STATE_KEYS: Record<string, MessageKey> = {
    kept: "ui.ownApp.checkpointState.kept",
    restored: "ui.ownApp.checkpointState.restored",
    restoring: "ui.ownApp.checkpointState.restoring",
    retained: "ui.ownApp.checkpointState.retained",
    taken: "ui.ownApp.checkpointState.taken",
  };

  function checkpointStateLabel(state: string): string {
    const key = CHECKPOINT_STATE_KEYS[state];
    return key ? tr(key) : state;
  }

  function stateLabel(state?: string): string {
    if (!state) return tr("ui.ownApp.health.unknown");
    const key = `ui.ownApp.state.${state}`;
    return isMessageKey(key) ? tr(key) : state;
  }
</script>

{#if own}
  <section
    class="space-y-3 rounded-xl border border-border/70 bg-card p-4 text-sm"
    data-testid="own-application-panel"
  >
    <div class="flex items-center justify-between gap-3">
      <h2 class="text-base font-semibold text-foreground">{tr("ui.ownApp.title")}</h2>
      {#if ownApplicationGrants(component, "status")}
        <Button
          variant="secondary"
          size="sm"
          disabled={busy}
          onclick={() => act(() => refreshOwnApplicationStatus(component))}
          testId="own-app-status">{tr("ui.ownApp.checkStatus")}</Button
        >
      {/if}
    </div>

    {#if own.reason_code && own.actions.length === 0}
      <p class="text-muted-foreground">{ownApplicationReason(own.reason_code)}</p>
    {/if}

    <dl class="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5">
      <dt class="text-muted-foreground">{tr("ui.ownApp.state")}</dt>
      <dd>{stateLabel(status?.state)}</dd>
      <dt class="text-muted-foreground">{tr("ui.ownApp.health")}</dt>
      <dd data-testid="own-app-health">
        {tr(HEALTH_KEYS[health] ?? "ui.ownApp.health.unknown")}
        {#if status?.at}<span class="text-xs text-muted-foreground">
            · {formatDateTime(status.at)}</span
          >{/if}
      </dd>
      {#if status?.image}
        <dt class="text-muted-foreground">{tr("ui.ownApp.image")}</dt>
        <dd class="break-all font-mono text-xs">{status.image}</dd>
      {/if}
    </dl>

    {#if showCheckpoint && checkpoint}
      <div
        class="space-y-2 rounded-lg border border-border/70 bg-background/40 p-3"
        data-testid="own-app-checkpoint"
      >
        <p class="font-medium text-foreground">{tr("ui.ownApp.checkpoint")}</p>
        <p>
          {tr("ui.ownApp.checkpointLine", {
            size: byteSize(checkpoint.retained_bytes),
            date: checkpoint.at ? formatDateTime(checkpoint.at) : "?",
          })}
        </p>
        <p class="text-xs text-muted-foreground">
          {checkpointStateLabel(checkpoint.state)}
        </p>
        <div class="flex flex-wrap gap-2">
          {#if ownApplicationGrants(component, "checkpoint_rollback")}
            <Button
              variant="secondary"
              size="sm"
              disabled={busy}
              onclick={() => act(() => runOwnApplicationCheckpoint(component, "rollback"))}
              testId="own-app-restore-data">{tr("ui.ownApp.restoreData")}</Button
            >
          {/if}
          {#if ownApplicationGrants(component, "checkpoint_discard")}
            <Button
              variant="destructive"
              size="sm"
              disabled={busy}
              onclick={() => act(() => runOwnApplicationCheckpoint(component, "discard"))}
              testId="own-app-discard">{tr("ui.ownApp.discard")}</Button
            >
          {/if}
        </div>
      </div>
    {/if}

    <div class="flex flex-wrap gap-2">
      {#if ownApplicationGrants(component, "rollback")}
        <Button
          variant="secondary"
          size="sm"
          disabled={busy}
          onclick={() => act(() => runOwnApplicationPower(component, "rollback"))}
          testId="own-app-rollback">{tr("ui.ownApp.rollback")}</Button
        >
      {/if}
      {#if ownApplicationGrants(component, "backup")}
        <Button
          variant="secondary"
          size="sm"
          disabled={busy}
          onclick={() => act(() => runOwnApplicationBackup(component))}
          testId="own-app-backup">{tr("ui.ownApp.backup")}</Button
        >
      {/if}
    </div>

    {#if busy}<p role="status" class="text-muted-foreground">
        {tr("ui.ownApp.waiting")}
      </p>{/if}
    {#if error}<ErrorCallout message={error} />{/if}
  </section>
{/if}
