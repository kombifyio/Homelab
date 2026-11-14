<!--
  Owner confirmation of one application move dropped on the Placement Board.
  The page starts the move; this dialog only confirms the destination.
-->
<script lang="ts">
  import { Loader2 } from "@lucide/svelte";
  import Modal from "#lib/components/Modal.svelte";
  import ErrorCallout from "#lib/components/ui/ErrorCallout.svelte";
  import { tr } from "#lib/i18n.svelte.js";
  import type { RegistryServer } from "#lib/api/registry.js";

  interface Props {
    applicationName: string;
    sourceNodeName: string;
    targets: { server: RegistryServer; label: string }[];
    initialTargetId: string;
    submitting: boolean;
    failure: string | null;
    oncancel: () => void;
    onconfirm: (target: RegistryServer) => void;
  }

  let {
    applicationName,
    sourceNodeName,
    targets,
    initialTargetId,
    submitting,
    failure,
    oncancel,
    onconfirm,
  }: Props = $props();

  // svelte-ignore state_referenced_locally
  let targetId = $state(initialTargetId);
  const target = $derived(
    targets.find((entry) => entry.server.id === targetId),
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
          bind:value={targetId}
          disabled={submitting}
          data-testid="service-move-destination"
        >
          {#each targets as entry (entry.server.id)}
            <option value={entry.server.id}>{entry.label}</option>
          {/each}
        </select>
      </label>
    {/if}

    <p class="text-xs text-muted-foreground">
      {tr("applicationMove.keepSource")}
    </p>

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
