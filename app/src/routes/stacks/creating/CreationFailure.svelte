<script lang="ts">
  import { tr } from "#lib/i18n.svelte.js";
  import { Sparkles } from "@lucide/svelte";
  import ErrorAiHandoverButton from "#lib/components/support/ErrorAiHandoverButton.svelte";
  import ErrorCallout from "#lib/components/ui/ErrorCallout.svelte";
  import { creation } from "./creation-controller.svelte.js";

  interface Props {
    /**
     * The failure lives in the progress list instead of replacing it: the
     * help sits inside the failed phase, the actions under the list.
     */
    part: "help" | "actions";
  }

  let { part }: Props = $props();
</script>


{#if part === "help"}
<div class="flex flex-col" data-testid="creation-failure">
{#if creation.postLeaseManagedCloudFailure && creation.lease}
  <div
    class="bg-primary/10 border border-primary/25 rounded-xl p-4 mb-4"
    data-testid="managed-lease-failure-summary"
  >
    <p class="text-sm font-medium text-foreground mb-2">
      {tr("ui.stacksCreatingCreationFailure.existingManagedVmLeaseReferenced")}
    </p>
    <p class="text-sm text-muted-foreground">
      {tr("ui.creationFailure.lease")} {creation.lease.id}{creation.lease.provider
        ? ` · ${creation.lease.provider}`
        : ""}{creation.lease.host || creation.lease.publicIp
        ? ` · ${creation.lease.host || creation.lease.publicIp}`
        : ""}
    </p>
    <p class="text-sm text-muted-foreground mt-2">
      {#if creation.stackKitArtifactOrRoutingFailure}
        {tr("ui.stacksCreatingCreationFailure.vpsProvisioningCompletedOnlyStackkit")}
      {:else}
        {tr("ui.stacksCreatingCreationFailure.theRolloutReachedTheExisting")}
      {/if}
      {tr("ui.stacksCreatingCreationFailure.theExactRetryEndpointValidates")}
    </p>
  </div>
{/if}

{#if creation.connectRemoteStackKitFailed}
  <div
    class="bg-primary/10 border border-primary/25 rounded-xl p-4 mb-4"
    data-testid="connect-remote-stackkit-failure-summary"
  >
    <p class="text-sm font-medium text-foreground mb-2">
      {tr("ui.stacksCreatingCreationFailure.nodeConnectionSucceeded")}
    </p>
    <p class="text-sm text-muted-foreground">
      {tr("ui.stacksCreatingCreationFailure.sshEnrollmentCompletedAndThe")}
    </p>
  </div>
{/if}

{#if creation.retryError}
  <ErrorCallout
    class="mb-4"
    message={creation.retryError}
    testId="creation-retry-error"
  />
{/if}

<!-- Intelligent troubleshooting from failed task -->
{#if creation.failedTask?.troubleshooting && creation.failedTask.troubleshooting.length > 0}
  <div
    class="mb-4"
    data-testid="creation-troubleshooting"
  >
    <p
      class="mb-2 text-sm font-medium text-foreground"
    >
      {tr("ui.stacksCreatingCreationFailure.whatYouCanDo")}
    </p>
    <ol
      class="text-sm text-foreground space-y-2 list-decimal list-inside"
    >
      {#each creation.failedTask.troubleshooting as step (step)}
        <li class="leading-relaxed text-foreground/90">{step}</li>
      {/each}
    </ol>
  </div>
{:else}
  <!-- Fallback suggestions if no specific troubleshooting available -->
  <div class="mb-4">
    <p class="text-sm text-muted-foreground mb-3">
      {tr("ui.stacksCreatingCreationFailure.possibleSolutions")}
    </p>
    <ul class="text-sm text-foreground/80 space-y-2">
      <li class="flex items-start gap-2">
        <span class="text-primary">•</span>
        <span>{tr("ui.stacksCreatingCreationFailure.checkYourNetworkConnection")}</span>
      </li>
      <li class="flex items-start gap-2">
        <span class="text-primary">•</span>
        <span>{tr("creation.failure.tryAgain")}</span>
      </li>
      <li class="flex items-start gap-2">
        <span class="text-primary">•</span>
        <span>{tr("creation.failure.contactSupport")}</span>
      </li>
    </ul>
  </div>
{/if}
<!-- Raw diagnostics are for support: collapsed, with a copy button. -->
{#if creation.failedTask?.errorDetails}
<details
  class="rounded-lg border border-border/70 px-3 py-2"
  data-testid="creation-failure-technical-details"
>
  <summary
    class="cursor-pointer select-none text-xs font-medium text-muted-foreground"
  >
    {tr("creation.technical.summary")}
  </summary>
  <div class="mt-3 flex items-center justify-end gap-3">
    <button
      type="button"
      data-kx="control"
      class="inline-flex items-center justify-center gap-2 whitespace-nowrap rounded-lg px-3 py-1.5 text-sm font-medium"
      data-testid="copy-error-details-button"
      onclick={creation.copyErrorDetails}
    >
      {creation.copiedErrorDetails
        ? tr("ui.common.copiedCheck")
        : tr("ui.stacksCreatingCreationFailure.copyErrorDetails")}
    </button>
  </div>
  <!-- A scrollable code block must be reachable by keyboard. -->
  <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
  <pre
    data-testid="error-details-text"
    tabindex="0"
    class="mt-3 max-h-48 overflow-x-auto overflow-y-auto whitespace-pre-wrap rounded border border-destructive/20 bg-background p-3 text-xs text-foreground">{creation.failedTask.errorDetails}</pre>
  <p class="sr-only" role="status">
    {creation.copiedErrorDetails ? tr("ui.common.copiedCheck") : ""}
  </p>
</details>
{/if}
</div>
{:else}
<!-- The retry on the existing server leads; the Companion handoff
     sits next to it; Operations stays a quiet way out. -->
<div class="flex flex-wrap items-center gap-2">
  {#if creation.failurePrimaryActionAvailable}
    <button
      onclick={creation.handleFailurePrimaryAction}
      disabled={creation.failurePrimaryActionDisabled}
      data-kx="control"
      data-variant="primary"
      class="inline-flex items-center justify-center gap-2 whitespace-nowrap rounded-xl px-5 py-2.5 text-sm font-medium disabled:pointer-events-none disabled:opacity-50"
    >
      {creation.failurePrimaryActionLabel}
    </button>
  {/if}
  {#if creation.companionAvailable && creation.failedTask}
    <button
      onclick={creation.askCompanionAboutFailure}
      data-kx="control"
      data-testid="creation-failure-ask-companion"
      class="inline-flex items-center justify-center gap-2 whitespace-nowrap rounded-xl px-5 py-2.5 text-sm font-medium"
    >
      <Sparkles class="h-4 w-4" aria-hidden="true" />
      {tr("ui.stacksCreatingCreationFailure.askCompanion")}
    </button>
  {:else if creation.failedTask}
    <!-- Embedded in kombify Cloud, Techstack's own Companion is not
         mounted: the host's Companion takes the failure instead. The
         button renders only where such a host exists. -->
    <ErrorAiHandoverButton
      context={creation.failureHandoverContext}
      class="mt-0"
    />
  {/if}
  <button
    onclick={creation.goToStack}
    class="inline-flex items-center justify-center whitespace-nowrap rounded-xl px-3 py-2.5 text-sm font-medium text-muted-foreground transition-colors hover:text-foreground"
  >
    {tr("ui.stacksCreatingCreationFailure.operations")}
  </button>
</div>
{/if}
