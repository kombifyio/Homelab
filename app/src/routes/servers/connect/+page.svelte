<script lang="ts">
  import { onMount } from "svelte";
  import { tr } from "#lib/i18n.svelte.js";
  import OwnServerConnect from "#lib/components/OwnServerConnect.svelte";
  import { createServerConnection } from "#lib/api/tunnel.js";
  import { getGuardBinaryPins } from "#lib/api/workers.js";
  import { generatePinnedConnectCommand } from "#lib/wizard/requirements.js";

  let command = $state("");
  let pairing = $state("");
  let expiresAt = $state("");
  let busy = $state(false);
  let error = $state("");
  let destroyed = false;

  async function prepare() {
    if (busy) return;
    busy = true;
    error = "";
    command = "";
    pairing = "";
    expiresAt = "";
    try {
      const connection = await createServerConnection();
      const nextCommand = connection.lan_pin
        ? generatePinnedConnectCommand(
            connection.registry_url,
            connection.lan_pin,
            await getGuardBinaryPins(),
          )
        : connection.command;
      if (destroyed) return;
      if (
        !nextCommand ||
        !connection.pairing ||
        !Number.isFinite(Date.parse(connection.expires_at))
      ) {
        throw new Error("Connection setup unavailable");
      }
      command = nextCommand;
      pairing = connection.pairing;
      expiresAt = connection.expires_at;
    } catch {
      if (!destroyed) error = tr("connect.unavailable");
    } finally {
      if (!destroyed) busy = false;
    }
  }

  // Choosing Connect server is the explicit preparation action; no deployment
  // or operated change is created by this account-level pairing.
  onMount(() => {
    void prepare();
    return () => {
      destroyed = true;
    };
  });
</script>

<svelte:head
  ><title>{tr("connect.title")} | Homelab Runtime</title></svelte:head
>
<div class="mx-auto w-full max-w-4xl px-4 py-8 sm:px-6 sm:py-12">
  <section
    class="rounded-2xl border border-border bg-card p-5 sm:p-8"
    aria-labelledby="connect-title"
  >
    <h1 id="connect-title" class="mb-4 text-2xl font-semibold text-foreground">
      {tr("connect.title")}
    </h1>
    <OwnServerConnect
      {command}
      {pairing}
      {expiresAt}
      {busy}
      {error}
      onRenew={prepare}
    />
    <a
      href="/servers"
      data-kx="control"
      class="mt-5 inline-flex rounded-lg px-3 py-2 text-sm"
      >{tr("connect.servers")}</a
    >
  </section>
</div>
