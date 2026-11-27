<script lang="ts">
  /**
   * Shown where a page's action waits because the person dismissed the
   * session renewal confirmation (workspace AUTHENTICATION.md §6). Neutral:
   * the action is pending, nothing is wrong with the account, and "Continue"
   * renews in place and then runs the page's action again.
   */
  import { sessionRenewal } from "#lib/auth/session-renewal.svelte.js";
  import { tr, type MessageKey } from "#lib/i18n.svelte.js";
  import Button from "#lib/components/ui/Button.svelte";

  interface Props {
    /** Message id for the body copy; defaults to the pending explanation. */
    bodyKey?: MessageKey;
    /** Runs the page's action again once the session is renewed. */
    onRenewed?: () => void | Promise<void>;
  }

  const { bodyKey = "sessionRenewal.pending", onRenewed }: Props = $props();

  let renewing = $state(false);

  async function continueAction() {
    renewing = true;
    try {
      // Called straight from the click so the renewal window counts it as
      // its gesture.
      const renewed = await sessionRenewal.renewWithGesture();
      if (renewed) await onRenewed?.();
    } finally {
      renewing = false;
    }
  }
</script>

<!-- Self-hides while the shared confirmation is visible so prompts never stack. -->
{#if !sessionRenewal.confirmation}
  <div
    class="rounded-lg border border-border bg-card p-4 text-sm text-foreground"
    data-testid="session-renewal-panel"
  >
    <p class="font-medium">{tr("sessionRenewal.title")}</p>
    <p class="mt-1 text-muted-foreground">
      {tr(bodyKey)}
    </p>
    <div class="mt-3 flex flex-wrap gap-2">
      <Button
        variant="primary"
        testId="session-renewal-continue-action"
        disabled={renewing}
        onclick={continueAction}
      >
        {renewing
          ? tr("sessionRenewal.confirming")
          : tr("sessionRenewal.continue")}
      </Button>
    </div>
  </div>
{/if}
