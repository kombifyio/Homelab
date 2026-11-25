<script lang="ts">
  import { onDestroy } from "svelte";
  import { tr } from "#lib/i18n.svelte.js";
  import Button from "#lib/components/ui/Button.svelte";
  import ErrorCallout from "#lib/components/ui/ErrorCallout.svelte";
  import { parseApiError } from "#lib/api/errors.js";
  import {
    denialReason,
    readGuardPaperworkSignIn,
    setGuardPaperworkSignIn,
    setGuardPublicReach,
    type GuardPaperworkSignIn,
  } from "#lib/api/guard.js";
  import type { CanonicalGuardReach } from "#lib/api/registry.js";
  import { confirmInApp } from "#lib/dialogs/in-app-dialog.js";
  import {
    createErrorAiHandoverContext,
    type ErrorAiHandoverContext,
  } from "#lib/support/error-handover.js";
  import GuardStepUpNotice from "./GuardStepUpNotice.svelte";

  interface Props {
    serverId: string;
    reach?: CanonicalGuardReach;
    onChanged?: () => void | Promise<void>;
  }

  let { serverId, reach, onChanged }: Props = $props();

  let service = $state("");
  let port = $state<number | null>(null);
  let busy = $state(false);
  let error = $state<string | null>(null);
  let stepUp = $state(false);
  let paperworkError = $state<ErrorAiHandoverContext | null>(null);
  let paperworkUpdate = $state.raw<{
    serverId: string;
    previousReach: CanonicalGuardReach | undefined;
    signIn: GuardPaperworkSignIn | undefined;
  }>();
  let mounted = true;
  onDestroy(() => {
    mounted = false;
  });

  const published = $derived(reach?.public?.enabled === true ? reach.public : null);
  const publicReachVisible = $derived(
    reach !== undefined &&
      reach.state !== "revoked" &&
      reach.state !== "not_enrolled",
  );
  const valid = $derived(
    /^[a-z]([a-z0-9-]{0,12}[a-z0-9])?$/.test(service.trim().toLowerCase()) &&
      Number.isInteger(port) &&
      (port ?? 0) >= 1 &&
      (port ?? 0) <= 65535,
  );
  const paperworkSignIn = $derived(
    paperworkUpdate &&
      paperworkUpdate.serverId === serverId &&
      paperworkUpdate.previousReach === reach
      ? paperworkUpdate.signIn
      : readGuardPaperworkSignIn(reach),
  );
  const paperworkState = $derived(paperworkSignIn?.state);
  const paperworkOff = $derived(
    paperworkState === "off" || paperworkState === "revoked",
  );
  const canEnablePaperwork = $derived(
    paperworkOff && paperworkSignIn?.can_enable === true,
  );
  const canDisablePaperwork = $derived(
    paperworkState === "pending" ||
      paperworkState === "pending_reconcile" ||
      paperworkState === "delivered",
  );

  async function change(request: Parameters<typeof setGuardPublicReach>[1]) {
    busy = true;
    error = null;
    stepUp = false;
    try {
      await setGuardPublicReach(serverId, request);
      await onChanged?.();
    } catch (cause) {
      const parsed = parseApiError(cause);
      if (
        parsed.isForbidden &&
        denialReason(parsed.details) === "server_reach_step_up_required"
      ) {
        stepUp = true;
      } else {
        error = parsed.message;
      }
    } finally {
      busy = false;
    }
  }

  async function changePaperworkSignIn(enabled: boolean) {
    if (busy || !(enabled ? canEnablePaperwork : canDisablePaperwork)) return;
    const targetServerId = serverId;
    const action = enabled ? "enable" : "disable";
    const actionLabel = tr(`guard.reach.paperworkSignIn.${action}`);

    busy = true;
    paperworkError = null;
    try {
      const confirmed = await confirmInApp({
        title: actionLabel,
        message: tr(`guard.reach.paperworkSignIn.${action}Confirm`),
        confirmText: actionLabel,
        tone: enabled ? "primary" : "warning",
      });
      if (
        !confirmed ||
        !mounted ||
        serverId !== targetServerId ||
        !(enabled ? canEnablePaperwork : canDisablePaperwork)
      ) return;

      const updated = await setGuardPaperworkSignIn(targetServerId, {
        enabled,
        owner_approved: true,
      });
      if (!mounted || serverId !== targetServerId) return;
      paperworkUpdate = {
        serverId: targetServerId,
        previousReach: reach,
        signIn: readGuardPaperworkSignIn(updated),
      };
      await onChanged?.();
    } catch (cause) {
      if (!mounted || serverId !== targetServerId) return;
      const parsed = parseApiError(cause);
      paperworkError = createErrorAiHandoverContext({
        surface: "guard-reach-paperwork-sign-in",
        title: actionLabel,
        userMessage: tr("guard.reach.paperworkSignIn.error"),
        status: parsed.status,
        code: parsed.code,
        reasonCode: parsed.outcome?.reasonCode,
        resourceId: targetServerId,
        action,
      });
    } finally {
      busy = false;
    }
  }
</script>

{#if publicReachVisible || paperworkSignIn}
  <section
    class="rounded-lg border border-border bg-muted/20 p-4"
    aria-labelledby={publicReachVisible ? "guard-reach-title" : "guard-paperwork-sign-in-title"}
    data-testid="guard-reach-control"
  >
    {#if publicReachVisible}
      {@render publicReach()}
    {/if}

    {#if paperworkSignIn}
      <div
        class:mt-4={publicReachVisible}
        class:border-t={publicReachVisible}
        class:border-border={publicReachVisible}
        class:pt-4={publicReachVisible}
        data-testid="guard-paperwork-sign-in"
      >
        <div class="flex flex-wrap items-center justify-between gap-3">
          <div>
            <h3
              id="guard-paperwork-sign-in-title"
              class="text-sm font-semibold text-foreground"
            >
              {tr("guard.reach.paperworkSignIn.title")}
            </h3>
            <p class="mt-1 text-xs text-muted-foreground" role="status">
              {tr(
                `guard.reach.paperworkSignIn.${paperworkSignIn.state === "pending_reconcile" ? "pending" : paperworkSignIn.state}`,
              )}
              {#if paperworkOff && !canEnablePaperwork}
                {tr("guard.reach.paperworkSignIn.unavailable")}
              {/if}
            </p>
          </div>
          {#if paperworkOff || canDisablePaperwork}
            <Button
              variant="secondary"
              size="sm"
              testId={paperworkOff
                ? "guard-paperwork-sign-in-enable"
                : "guard-paperwork-sign-in-disable"}
              disabled={busy || (paperworkOff && !canEnablePaperwork)}
              onclick={() => void changePaperworkSignIn(paperworkOff)}
            >
              {tr(paperworkOff
                ? "guard.reach.paperworkSignIn.enable"
                : "guard.reach.paperworkSignIn.disable")}
            </Button>
          {/if}
        </div>
        {#if paperworkError?.resourceId === serverId}
          <ErrorCallout
            class="mt-3"
            message={paperworkError.userMessage}
            aiHandoverContext={paperworkError}
          />
        {/if}
      </div>
    {/if}
  </section>
{/if}

{#snippet publicReach()}
  <div>
    <h3 id="guard-reach-title" class="text-sm font-semibold text-foreground">
      {tr("guard.reach.publish.title")}
    </h3>
    <p class="mt-1 text-xs text-muted-foreground">
      {tr("guard.reach.publish.intro")}
    </p>

    {#if published}
      <p class="mt-3 text-xs text-foreground" data-testid="guard-reach-current">
        {tr("guard.reach.publish.current", {
          service: published.service ?? "",
          port: published.target_port ?? 0,
        })}
      </p>
      <div class="mt-2">
        <Button
          variant="secondary"
          size="sm"
          testId="guard-reach-disable"
          disabled={busy}
          onclick={() => void change({ enabled: false })}
        >
          {tr("guard.reach.publish.disable")}
        </Button>
      </div>
    {:else}
      <form
        class="mt-3 flex flex-wrap items-end gap-3"
        onsubmit={(event) => {
          event.preventDefault();
          if (valid && !busy) {
            void change({
              enabled: true,
              service: service.trim().toLowerCase(),
              target_port: port ?? undefined,
            });
          }
        }}
      >
        <label class="flex flex-col gap-1 text-xs font-medium text-foreground">
          {tr("guard.reach.publish.service")}
          <input
            class="w-40 rounded-md border border-border bg-background px-3 py-2 text-sm"
            bind:value={service}
            maxlength="14"
            autocomplete="off"
            disabled={busy}
            data-testid="guard-reach-service"
          />
        </label>
        <label class="flex flex-col gap-1 text-xs font-medium text-foreground">
          {tr("guard.reach.publish.port")}
          <input
            class="w-28 rounded-md border border-border bg-background px-3 py-2 text-sm"
            type="number"
            min="1"
            max="65535"
            bind:value={port}
            disabled={busy}
            data-testid="guard-reach-port"
          />
        </label>
        <Button
          variant="primary"
          size="sm"
          type="submit"
          testId="guard-reach-enable"
          disabled={busy || !valid}
        >
          {tr("guard.reach.publish.enable")}
        </Button>
      </form>
    {/if}

    {#if stepUp}
      <GuardStepUpNotice
        class="mt-3"
        testId="guard-reach-stepup"
        body={tr("guard.reach.publish.stepUp")}
        onConfirmed={() => (stepUp = false)}
      />
    {/if}
    {#if error}
      <p class="mt-3 text-xs text-destructive" role="alert">{error}</p>
    {/if}
  </div>
{/snippet}
