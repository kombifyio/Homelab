<!--
  Owner confirmation of one application move dropped on the Placement Board.
  The page starts the move; this dialog confirms the destination, surfaces
  pre-flight warnings (such as direct TCP:8124 reachability), and lists any
  blocked servers with their reasons.
-->
<script lang="ts">
  import { AlertTriangle, Ban, ChevronDown, Info, Loader2 } from "@lucide/svelte";
  import Modal from "#lib/components/Modal.svelte";
  import ErrorCallout from "#lib/components/ui/ErrorCallout.svelte";
  import { tr } from "#lib/i18n.svelte.js";
  import { moveIneligibleLabel, moveWarningLabel } from "#lib/services/move-copy.js";
  import { placementServerKey } from "#lib/services/placement.js";
  import type {
    RegistryMoveIneligibleCode,
    RegistryMoveWarning,
    RegistryServer,
  } from "#lib/api/registry.js";

  interface BlockedMoveTarget {
    server: RegistryServer;
    label: string;
    code: RegistryMoveIneligibleCode;
  }

  interface Props {
    applicationName: string;
    sourceNodeName: string;
    targets: { server: RegistryServer; label: string }[];
    initialTargetKey: string;
    warnings?: RegistryMoveWarning[];
    blockedTargets?: BlockedMoveTarget[];
    submitting: boolean;
    failure: string | null;
    oncancel: () => void;
    onconfirm: (target: RegistryServer) => void;
  }

  let {
    applicationName,
    sourceNodeName,
    targets,
    initialTargetKey,
    warnings = [],
    blockedTargets = [],
    submitting,
    failure,
    oncancel,
    onconfirm,
  }: Props = $props();

  // svelte-ignore state_referenced_locally
  let targetKey = $state(initialTargetKey);
  const target = $derived(
    targets.find((entry) => placementServerKey(entry.server) === targetKey),
  );
  const activeWarnings = $derived(
    warnings.filter(
      (warning) =>
        !warning.server_id || warning.server_id === target?.server.id,
    ),
  );
  const infoNotes = $derived(
    activeWarnings.filter(
      (warning) => warning.code === "encrypted_relay_transport",
    ),
  );
  const cautionWarnings = $derived(
    activeWarnings.filter(
      (warning) => warning.code !== "encrypted_relay_transport",
    ),
  );
</script>

<Modal title={tr("ui.services.moveApplication")} onClose={oncancel} maxWidth="md">
  <div class="space-y-4" data-testid="service-move-dialog">
    <p class="text-sm text-foreground">
      {tr("applicationMove.summary", {
        application: applicationName,
        source: sourceNodeName,
        destination: target?.label ?? "",
      })}
    </p>

    {#if targets.length > 1}
      <label class="block space-y-1.5 text-sm">
        <span class="font-medium text-foreground"
          >{tr("applicationMove.destination")}</span
        >
        <select
          class="flex h-10 w-full rounded-lg border border-input bg-transparent px-3 py-2 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-50"
          bind:value={targetKey}
          disabled={submitting}
          data-testid="service-move-destination"
        >
          {#each targets as entry (placementServerKey(entry.server))}
            <option value={placementServerKey(entry.server)}>{entry.label}</option>
          {/each}
        </select>
      </label>
    {/if}

    {#if infoNotes.length > 0}
      <div
        class="space-y-1.5 rounded-lg border border-border/70 bg-muted/40 p-3 text-xs text-muted-foreground"
        role="status"
        data-testid="service-move-info-notes"
      >
        <div class="flex items-center gap-1.5 font-medium text-foreground">
          <Info class="h-4 w-4 shrink-0 text-muted-foreground" />
          <span>{tr("applicationMove.relayInfoTitle")}</span>
        </div>
        <ul class="space-y-1 pl-5 list-disc">
          {#each infoNotes as note (note.code + ":" + (note.server_id ?? ""))}
            <li data-info-code={note.code}>
              {moveWarningLabel(note.code)}
            </li>
          {/each}
        </ul>
      </div>
    {/if}

    {#if cautionWarnings.length > 0}
      <div
        class="space-y-2 rounded-lg border border-warning/30 bg-warning/10 p-3 text-xs text-warning"
        role="status"
        data-testid="service-move-warnings"
      >
        <div class="flex items-center gap-1.5 font-semibold">
          <AlertTriangle class="h-4 w-4 shrink-0" />
          <span>{tr("applicationMove.warningsTitle")}</span>
        </div>
        <ul class="space-y-1.5 pl-5 list-disc text-warning/90">
          {#each cautionWarnings as warning (warning.code + ":" + (warning.server_id ?? ""))}
            <li data-warning-code={warning.code}>
              {moveWarningLabel(warning.code)}
            </li>
          {/each}
        </ul>
      </div>
    {/if}

    <p class="text-xs text-muted-foreground">
      {tr("applicationMove.keepSource")}
    </p>

    {#if blockedTargets.length > 0}
      <details
        class="rounded-lg border border-border/70 bg-muted/20 px-3 py-2 text-xs text-muted-foreground"
        data-testid="service-move-blocked-targets"
      >
        <summary
          class="flex cursor-pointer list-none items-center justify-between gap-2 font-medium text-foreground/80"
        >
          <span class="flex items-center gap-1.5">
            <Ban class="h-3.5 w-3.5 text-muted-foreground" />
            {tr("applicationMove.blockedDestinations", {
              count: blockedTargets.length,
            })}
          </span>
          <ChevronDown class="h-3.5 w-3.5 shrink-0" />
        </summary>
        <ul class="mt-2.5 space-y-2 border-t border-border/50 pt-2">
          {#each blockedTargets as blocked (placementServerKey(blocked.server))}
            <li
              class="flex flex-col gap-0.5"
              data-blocked-server={blocked.server.id}
              data-blocked-deployment={blocked.server.kit_deployment_id || undefined}
              data-blocked-code={blocked.code}
            >
              <span class="font-medium text-foreground/90">{blocked.label}</span
              >
              <span class="text-[11px] text-muted-foreground"
                >{moveIneligibleLabel(blocked.code)}</span
              >
            </li>
          {/each}
        </ul>
      </details>
    {/if}

    {#if failure}
      <ErrorCallout
        message={tr("applicationMove.failed")}
        details={failure}
        testId="service-move-error"
      />
    {/if}
  </div>

  {#snippet footer()}
    <button
      type="button"
      class="px-4 py-2 rounded-lg bg-secondary text-secondary-foreground hover:bg-secondary/80 transition-colors disabled:opacity-50 cursor-pointer font-medium"
      onclick={oncancel}
      disabled={submitting}
    >
      {tr("ui.importExportModal.cancel")}
    </button>
    <button
      type="button"
      data-kx="control"
      data-variant="primary"
      data-testid="service-move-confirm"
      class="px-4 py-2 rounded-lg disabled:opacity-50 flex items-center gap-1.5 cursor-pointer font-semibold"
      onclick={() => target && onconfirm(target.server)}
      disabled={submitting || !target}
    >
      {#if submitting}
        <Loader2 class="w-4 h-4 animate-spin" />
      {/if}
      {tr(submitting ? "applicationMove.starting" : "applicationMove.start")}
    </button>
  {/snippet}
</Modal>
