<script lang="ts">
  import { creation } from "./creation-controller.svelte.js";
  import { tr } from "#lib/i18n.svelte.js";
  import OwnServerConnect from "#lib/components/OwnServerConnect.svelte";
  import GuardReadinessLadder from "#lib/components/guard/GuardReadinessLadder.svelte";
  import { authStore } from "#lib/stores/auth.svelte.js";
  import WalletServerAccessPicker from "#lib/kombify-wallet/components/WalletServerAccessPicker.svelte";

  // Hosted: the server access item to rebind the stack to on the next retry.
  let rebindItemId = $state("");
</script>

<!-- Cards that need the user while the run waits: an overdue managed
     runtime, a remote SSH enrollment or a pairing command. Ordinary step
     detail lives in the active phase of the progress reel. -->
{#if creation.waitingForManagedRuntime}
  <!-- A scheduled wait is a normal part of the run; only an overdue
       one that needs the resume action is shown as attention. -->
  {@const attention = creation.waitingRecoveryAvailable}
  <section
    class="rounded-2xl border p-6 {attention
      ? 'border-warning/30 bg-warning/5'
      : 'border-primary/20 bg-primary/5'}"
    data-attention={attention}
    data-testid="waiting-enrollment-card"
    role="status"
    aria-live="polite"
  >
    <div
      class="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between"
    >
      <div>
        <p
          class="text-xs font-semibold uppercase tracking-wide {attention
            ? 'text-warning'
            : 'text-primary'}"
        >
          {tr("ui.stacksCreatingCreationRunStatus.provisioningInProgress")}
        </p>
        <h2 class="mt-1 text-xl font-semibold text-foreground">
          {tr("ui.stacksCreatingCreationProgress.nodeProvisioningIsStillIn")}
        </h2>
        <p class="mt-2 max-w-2xl text-sm text-muted-foreground">
          {#if creation.waitingForProviderProvision}
            {tr(
              "ui.stacksCreatingCreationRunStatus.theProviderOperationHasNot",
            )}
          {:else}
            {tr("ui.stacksCreatingCreationRunStatus.theManagedRuntimeIsNot")}
          {/if}
        </p>
        {#if creation.nextResumeLabel}
          <p
            class="mt-3 text-xs text-muted-foreground"
            data-testid="waiting-enrollment-next-resume"
          >
            {tr("ui.stacksCreatingCreationRunStatus.nextScheduledCheck")}
            <time datetime={creation.jobNextResumeAt}
              >{creation.nextResumeLabel}</time
            >
          </p>
        {/if}
        <p class="mt-3 max-w-2xl text-xs text-muted-foreground">
          {#if creation.jobResumeAvailableAt}
            {tr("ui.creationRun.resumeStartingAt", {
              time: creation.resumeAvailableLabel,
            })}
          {:else}
            {tr("ui.stacksCreatingCreationRunStatus.aManualResumeIsEnabled")}
          {/if}
          {tr("ui.stacksCreatingCreationRunStatus.thisDoesNotCreateAnother")}
        </p>
      </div>
      <span
        class="inline-flex shrink-0 items-center gap-2 rounded-full border px-3 py-1 text-xs font-medium {attention
          ? 'border-warning/30 bg-warning/10 text-warning'
          : 'border-primary/20 bg-primary/10 text-primary'}"
        data-testid="waiting-enrollment-status"
      >
        <span
          class="h-2 w-2 animate-pulse rounded-full {attention
            ? 'bg-warning'
            : 'bg-primary'}"
        ></span>
        {tr("ui.creationRun.notReachableYet")}
      </span>
    </div>

    {#if creation.currentStepMessage}
      <div
        class="mt-5 rounded-lg border px-3 py-2 {attention
          ? 'border-warning/20 bg-warning/10'
          : 'border-primary/20 bg-primary/10'}"
      >
        <p
          class="text-xs font-medium {attention
            ? 'text-warning'
            : 'text-primary'}"
        >
          {tr("ui.stacksCreatingCreationRunStatus.currentStatus")}
        </p>
        <p class="mt-1 text-sm text-foreground">
          {creation.currentStepMessage}
        </p>
      </div>
    {/if}

    <div
      class="mt-5 grid gap-2 sm:grid-cols-3"
      data-testid="waiting-enrollment-access"
    >
      <button
        type="button"
        disabled
        aria-disabled="true"
        data-kx="control"
        class="inline-flex cursor-not-allowed items-center justify-center gap-2 whitespace-nowrap rounded-lg px-3 py-1.5 text-sm font-medium opacity-50"
        data-testid="waiting-dashboard-disabled"
      >
        {tr("ui.settings.dashboard")}
      </button>
      <button
        type="button"
        disabled
        aria-disabled="true"
        data-kx="control"
        class="inline-flex cursor-not-allowed items-center justify-center gap-2 whitespace-nowrap rounded-lg px-3 py-1.5 text-sm font-medium opacity-50"
        data-testid="waiting-services-disabled"
      >
        {tr("ui.services.services")}
      </button>
      <button
        type="button"
        disabled
        aria-disabled="true"
        data-kx="control"
        class="inline-flex cursor-not-allowed items-center justify-center gap-2 whitespace-nowrap rounded-lg px-3 py-1.5 text-sm font-medium opacity-50"
        data-testid="waiting-access-disabled"
      >
        {tr("ui.stacksCreatingCreationRunStatus.nodeAccess")}
      </button>
    </div>
    <p class="mt-3 text-xs text-muted-foreground">
      {tr("ui.stacksCreatingCreationRunStatus.theseAccessPathsAreEnabled")}
    </p>
    {#if creation.waitingRecoveryAvailable}
      <div
        class="mt-4 rounded-lg border border-info/30 bg-info/10 p-3"
        data-testid="waiting-enrollment-recovery"
      >
        <p class="text-xs text-muted-foreground">
          {tr("ui.stacksCreatingCreationRunStatus.theScheduledCheckIsOverdue")}
        </p>
        <button
          type="button"
          data-kx="control"
          data-variant="primary"
          class="mt-3 inline-flex items-center justify-center gap-2 whitespace-nowrap rounded-lg px-3 py-1.5 text-sm font-medium disabled:pointer-events-none disabled:opacity-50"
          disabled={creation.retryingRollout}
          onclick={creation.resumeOverdueManagedRuntime}
          data-testid="waiting-enrollment-retry"
        >
          {creation.retryingRollout
            ? tr("ui.stacksCreatingCreationRunStatus.validatingTheExistingVm")
            : creation.waitingForProviderProvision
              ? tr(
                  "ui.stacksCreatingCreationRunStatus.continueRolloutOnTheExisting",
                )
              : tr(
                  "ui.stacksCreatingCreationRunStatus.resumeEnrollmentOnTheExisting",
                )}
        </button>
        {#if creation.retryError}
          <p
            class="mt-2 text-xs text-destructive"
            data-testid="waiting-enrollment-retry-error"
          >
            {creation.retryError}
          </p>
        {/if}
      </div>
    {/if}
  </section>
{:else if creation.serverProvisioningMode === "connect-remote" && creation.agentPairingWaiting}
  <section data-kx="plate" class="p-6" data-testid="remote-ssh-enrollment-card">
    <div
      class="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between"
    >
      <div>
        <p class="text-xs font-semibold uppercase tracking-wide text-primary">
          {tr("ui.stacksCreatingCreationRunStatus.connectMyNode")}
        </p>
        <h2 class="mt-1 text-xl font-semibold text-foreground">
          {#if creation.remoteEnrollmentActive}
            {tr("ui.stacksCreatingCreationRunStatus.connectingViaSsh")}
          {:else if creation.remoteEnrollmentFailed}
            {tr("ui.stacksCreatingCreationRunStatus.remoteSshConnectionFailed")}
          {:else}
            {tr("ui.stacksCreatingCreationRunStatus.waitingForGuardHeartbeat")}
          {/if}
        </h2>
        <p class="mt-2 max-w-2xl text-sm text-muted-foreground">
          {#if creation.remoteEnrollmentActive}
            {tr(
              "ui.stacksCreatingCreationRunStatus.kombifyIsInstallingAndEnrolling",
            )}
          {:else if creation.remoteEnrollmentFailed}
            {creation.pairingRecoveryError ||
              tr(
                "ui.stacksCreatingCreationRunStatus.checkSshHostCredentialsAnd",
              )}
          {:else}
            {tr(
              "ui.stacksCreatingCreationRunStatus.enrollmentOverSshFinishedThis",
            )}
          {/if}
        </p>
      </div>
      <span
        class="inline-flex shrink-0 items-center gap-2 rounded-full border border-warning/30 bg-warning/10 px-3 py-1 text-xs font-medium text-warning"
        data-testid="remote-ssh-enrollment-status"
      >
        <span class="h-2 w-2 rounded-full bg-warning"></span>
        {#if creation.remoteEnrollmentActive}
          {tr("ui.stacksCreatingCreationRunStatus.enrolling")}
        {:else if creation.remoteEnrollmentFailed}
          {tr("ui.stacksCreatingCreationRunStatus.failed")}
        {:else}
          {tr("ui.stacksCreatingCreationRunStatus.awaitingHeartbeat")}
        {/if}
      </span>
    </div>

    {#if creation.remoteServerHost}
      <div class="mt-5 rounded-lg border border-border bg-muted/40 p-3">
        <p class="text-xs text-muted-foreground">
          {tr("ui.stacksCreatingCreationRunStatus.remoteTarget")}
        </p>
        <p class="mt-1 font-mono text-sm text-foreground">
          {creation.remoteServerUser
            ? `${creation.remoteServerUser}@`
            : ""}{creation.remoteServerHost}{creation.remoteServerPort
            ? `:${creation.remoteServerPort}`
            : ""}
        </p>
      </div>
    {/if}

    {#if creation.remoteEnrollmentActive}
      <div class="mt-5">
        <div
          class="h-2 overflow-hidden rounded-full bg-muted"
          aria-hidden="true"
        >
          <div
            class="h-full rounded-full bg-primary transition-all duration-500"
            style={`width: ${Math.max(8, creation.remoteEnrollmentProgress)}%`}
          ></div>
        </div>
        <p
          class="mt-3 text-sm text-muted-foreground"
          data-testid="remote-ssh-enrollment-message"
        >
          {creation.remoteEnrollmentMessage ||
            tr("ui.stacksCreatingCreationRunStatus.connectingToYourNodeOver")}
        </p>
      </div>
    {:else if creation.remoteEnrollmentFailed}
      <div class="mt-5">
        {#if authStore.walletSource === "kombify"}
          <div class="mb-4 max-w-md" data-testid="remote-ssh-rebind">
            <WalletServerAccessPicker
              id="remote-ssh-rebind-item"
              value={rebindItemId}
              onchange={(itemId) => (rebindItemId = itemId)}
              host={creation.remoteServerHost}
              label={tr("kwallet.picker.rebind_label")}
              disabled={creation.retryingRollout}
            />
          </div>
        {/if}
        <button
          type="button"
          data-kx="control"
          data-variant="primary"
          class="inline-flex items-center justify-center gap-2 whitespace-nowrap rounded-lg px-3 py-1.5 text-sm font-medium disabled:pointer-events-none disabled:opacity-50"
          disabled={creation.retryingRollout}
          onclick={() =>
            creation.retryRemoteSSHEnrollment(rebindItemId || undefined)}
          data-testid="remote-ssh-enrollment-retry"
        >
          {creation.retryingRollout
            ? tr("ui.stacksCreatingCreationRunStatus.retryingSshConnection")
            : tr(
                "ui.stacksCreatingCreationRunStatus.retryConnectionOnSavedServer",
              )}
        </button>
        {#if creation.pairingRecoveryError}
          <p
            class="mt-2 text-xs text-destructive"
            data-testid="remote-ssh-enrollment-retry-error"
          >
            {creation.pairingRecoveryError}
          </p>
        {/if}
      </div>
    {/if}
    <GuardReadinessLadder
      server={creation.connectedServer}
      stackId={creation.stackId}
      pairingError={creation.pairingRecoveryError}
    />

    {#if creation.connectionPollError}
      <p
        class="mt-4 rounded-lg border border-warning/30 bg-warning/10 p-3 text-xs text-muted-foreground"
        role="status"
        data-testid="remote-ssh-connection-poll-error"
      >
        {creation.connectionPollError}
      </p>
    {/if}
  </section>
{:else if creation.agentPairingWaiting}
  <section data-kx="plate" class="p-6" data-testid="guard-pairing-card">
    <div
      class="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between"
    >
      <div>
        <p class="text-xs font-semibold uppercase tracking-wide text-primary">
          {tr("ui.stacksCreatingCreationRunStatus.waitingForRealConnection")}
        </p>
        <h2 class="mt-1 text-xl font-semibold text-foreground">
          {tr("ui.stacksCreatingCreationRunStatus.pairingCommandReady")}
        </h2>
        <p class="mt-2 max-w-2xl text-sm text-muted-foreground">
          {tr("ui.stacksCreatingCreationRunStatus.runThisOneLinerOn")}
        </p>
      </div>
      <span
        class="inline-flex shrink-0 items-center gap-2 rounded-full border border-warning/30 bg-warning/10 px-3 py-1 text-xs font-medium text-warning"
        data-testid="guard-pairing-status"
      >
        <span class="h-2 w-2 rounded-full bg-warning"></span>
        {tr("ui.creationRun.notConnectedYet")}
      </span>
    </div>

    {#if creation.remoteServerHost}
      <div class="mt-5 rounded-lg border border-border bg-muted/40 p-3">
        <p class="text-xs text-muted-foreground">
          {tr("ui.stacksCreatingCreationRunStatus.plannedRemoteTarget")}
        </p>
        <p class="mt-1 font-mono text-sm text-foreground">
          {creation.remoteServerUser
            ? `${creation.remoteServerUser}@`
            : ""}{creation.remoteServerHost}{creation.remoteServerPort
            ? `:${creation.remoteServerPort}`
            : ""}
        </p>
        <p class="mt-1 text-xs text-muted-foreground">
          {tr("ui.stacksCreatingCreationRunStatus.theSshDetailsArePlanning")}
        </p>
      </div>
    {/if}

    <OwnServerConnect
      command={creation.installCommand}
      pairing={creation.registrationToken}
      expiresAt={creation.pairingTokenExpiresAt}
      busy={creation.pairingRecoveryBusy}
      error={creation.pairingRecoveryError}
      onRenew={creation.recoverPairingCommand}
    />

    <GuardReadinessLadder
      server={creation.connectedServer}
      stackId={creation.stackId}
      pairingError={creation.pairingRecoveryError}
    />

    {#if creation.connectionPollError}
      <p
        class="mt-4 rounded-lg border border-warning/30 bg-warning/10 p-3 text-xs text-muted-foreground"
        role="status"
      >
        {creation.connectionPollError}
      </p>
    {/if}
  </section>
{/if}
