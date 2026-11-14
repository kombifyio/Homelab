<script lang="ts">
  /**
   * The in-place confirmation of the shared session renewal layer
   * (workspace AUTHENTICATION.md §6). It appears only when renewal needs a
   * gesture (the browser blocked the renewal window). Asking is not an
   * error: the action stays pending, the wording is neutral and nothing the
   * person entered is lost.
   */
  import { sessionRenewal } from "#lib/auth/session-renewal.svelte.js";
  import { tr } from "#lib/i18n.svelte.js";
  import Button from "#lib/components/ui/Button.svelte";

  const request = $derived(sessionRenewal.confirmation);
</script>

{#if request}
  <section
    class="mx-auto w-full max-w-7xl px-4 pt-4 sm:px-6"
    role="status"
    aria-live="polite"
    aria-labelledby="session-renewal-title"
    data-testid="session-renewal-prompt"
  >
    <div class="rounded-xl border border-border bg-card p-4 shadow-sm sm:p-5">
      <h2
        id="session-renewal-title"
        class="text-base font-semibold text-foreground"
      >
        {tr("sessionRenewal.title")}
      </h2>
      <p class="mt-1 text-sm text-muted-foreground">
        {request.blocked
          ? tr("sessionRenewal.blocked")
          : tr("sessionRenewal.body")}
      </p>
      <div class="mt-3 flex flex-wrap gap-2">
        <Button
          type="button"
          variant="primary"
          testId="session-renewal-continue"
          disabled={request.waiting}
          onclick={() => sessionRenewal.confirm()}
        >
          {request.waiting
            ? tr("sessionRenewal.confirming")
            : tr("sessionRenewal.continue")}
        </Button>
        {#if request.blocked}
          <Button
            type="button"
            variant="secondary"
            testId="session-renewal-same-tab"
            onclick={() => sessionRenewal.continueInThisTab()}
          >
            {tr("sessionRenewal.sameTab")}
          </Button>
        {/if}
        <Button
          type="button"
          variant="ghost"
          testId="session-renewal-not-now"
          disabled={request.waiting}
          onclick={() => sessionRenewal.dismiss()}
        >
          {tr("sessionRenewal.notNow")}
        </Button>
      </div>
    </div>
  </section>
{/if}
