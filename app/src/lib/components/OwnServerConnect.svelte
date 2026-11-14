<script lang="ts">
  import { onMount } from "svelte";
  import { formatDateTime, tr } from "#lib/i18n.svelte.js";
  import { Check, Copy } from "@lucide/svelte";

  let {
    command,
    pairing,
    expiresAt,
    busy = false,
    error = "",
    onRenew,
  }: {
    command: string;
    pairing: string;
    expiresAt: string;
    busy?: boolean;
    error?: string;
    onRenew: () => void;
  } = $props();

  let now = $state(Date.now());
  let copied = $state("");
  let copyFailed = $state(false);
  const ready = $derived(
    Boolean(command && pairing && Date.parse(expiresAt) > now),
  );
  onMount(() => {
    const timer = setInterval(() => {
      now = Date.now();
    }, 1000);
    return () => clearInterval(timer);
  });

  async function copy(value: string, part: string) {
    if (!ready) return;
    try {
      await navigator.clipboard.writeText(value);
      copied = part;
      copyFailed = false;
    } catch {
      copyFailed = true;
    }
  }
</script>

<div class="max-w-3xl" data-testid="own-server-connect">
  <p class="text-sm text-muted-foreground">{tr("connect.intro")}</p>
  <p class="mt-2 text-sm text-muted-foreground">{tr("connect.plan")}</p>
  <div class="mt-5 grid gap-4 sm:grid-cols-[minmax(0,3fr)_minmax(0,2fr)]">
    <section class="min-w-0 rounded-xl border border-border bg-background p-4">
      <h3 class="text-sm font-semibold">{tr("connect.command")}</h3>
      {#if command}
        <pre
          class="my-3 overflow-x-auto whitespace-pre-wrap break-all text-sm text-primary"
          data-testid="connect-command">{command}</pre>
      {:else}
        <p class="my-3 text-sm text-muted-foreground">
          {tr("connect.unavailable")}
        </p>
      {/if}
      <button
        type="button"
        data-kx="control"
        class="inline-flex items-center gap-2 rounded-lg px-3 py-2 text-sm disabled:opacity-50"
        disabled={!ready}
        onclick={() => copy(command, "command")}
      >
        {#if copied === "command"}<Check
            class="h-4 w-4"
            aria-hidden="true"
          />{:else}<Copy class="h-4 w-4" aria-hidden="true" />{/if}
        {copied === "command"
          ? tr("ui.common.copiedCheck")
          : tr("connect.copyCommand")}
      </button>
    </section>
    <section class="min-w-0 rounded-xl border border-border bg-background p-4">
      <h3 class="text-sm font-semibold">{tr("connect.pairing")}</h3>
      <p class="my-3 text-sm text-muted-foreground">{tr("connect.hidden")}</p>
      <button
        type="button"
        data-kx="control"
        class="inline-flex items-center gap-2 rounded-lg px-3 py-2 text-sm disabled:opacity-50"
        disabled={!ready}
        onclick={() => copy(pairing, "pairing")}
      >
        {#if copied === "pairing"}<Check
            class="h-4 w-4"
            aria-hidden="true"
          />{:else}<Copy class="h-4 w-4" aria-hidden="true" />{/if}
        {copied === "pairing"
          ? tr("ui.common.copiedCheck")
          : tr("connect.copyPairing")}
      </button>
    </section>
  </div>
  <div
    class="mt-3 flex flex-wrap items-center gap-x-4 gap-y-2 text-xs text-muted-foreground"
  >
    {#if expiresAt && Number.isFinite(Date.parse(expiresAt))}
      <p>{tr("connect.expires", { time: formatDateTime(expiresAt) })}</p>
    {/if}
    {#if !ready && !busy}<p>{tr("connect.needsPairing")}</p>{/if}
    <button
      type="button"
      class="rounded px-2 py-1 text-primary disabled:opacity-50"
      disabled={busy}
      onclick={() => {
        copied = "";
        onRenew();
      }}>{busy ? tr("connect.preparing") : tr("connect.renew")}</button
    >
  </div>
  <p class="mt-2 min-h-4 text-xs text-muted-foreground" role="status">
    {error ||
      (copyFailed
        ? tr("connect.copyFailed")
        : copied
          ? tr("ui.common.copiedCheck")
          : "")}
  </p>
</div>
