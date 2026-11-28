<script lang="ts">
  // First start (DESKTOP-FOUNDATION-STANDARD §5.3.0, D-2026-11-27-13): the same one page as in the
  // other desktop clients. "On this computer" is preselected and needs no account; "With my kombify
  // account" opens kombify's own Homelab sign-in; "I have my own server" stays quiet underneath.
  // Homelab's own first decision follows directly: which server runs the home. Its answer is handed
  // to the creation wizard as a prefill the person confirms there (`/stacks/new?placement=`).
  import { tr } from "#lib/i18n.svelte.js";
  import { goto } from "$app/navigation";
  import { OptionTiles } from "@kombiverselabs/ui/settings";
  import { setLocalLANBridge } from "#lib/api/tunnel.js";
  import {
    cloudUiLoginUrl,
    normalizeServerUrl,
    rememberWindowsLocalClientContext,
    windowsLocalClientReturnUrl,
  } from "#lib/client/windows-onboarding.js";
  import { openAccountSignIn } from "#lib/client/shell-bridge.js";
  import { authStore } from "#lib/stores/auth.svelte.js";
  import HomelabBrandLogo from "#lib/components/HomelabBrandLogo.svelte";
  import Button from "#lib/components/ui/Button.svelte";

  type Mode = "local" | "account";
  type Placement = "home" | "rented" | "unsure";

  let step = $state<"mode" | "placement">("mode");
  let mode = $state<Mode>("local");
  let placement = $state<Placement>("home");
  let showServerInput = $state(false);
  let serverUrl = $state("");
  let starting = $state(false);
  let failed = $state(false);
  const normalizedServerUrl = $derived(normalizeServerUrl(serverUrl));

  const modeOptions = $derived([
    {
      value: "local",
      label: tr("ui.clientOnboarding.localWindowsInstallation"),
      hint: tr("ui.clientOnboarding.noAccountNeeded"),
    },
    {
      value: "account",
      label: tr("ui.clientOnboarding.withAccount"),
      hint: tr("ui.clientOnboarding.withAccountHint"),
    },
  ]);
  const placementOptions = $derived([
    { value: "home", label: tr("ui.clientOnboarding.placementHome") },
    { value: "rented", label: tr("ui.clientOnboarding.placementRented") },
    { value: "unsure", label: tr("ui.clientOnboarding.placementUnsure") },
  ]);

  function continueFromMode() {
    if (mode === "account") {
      // The Windows client opens the sign-in in the system browser; a browser goes there itself.
      if (!openAccountSignIn()) window.location.assign(cloudUiLoginUrl);
      return;
    }
    step = "placement";
  }

  async function startOnThisComputer() {
    starting = true;
    failed = false;
    try {
      await setLocalLANBridge(true);
      try {
        rememberWindowsLocalClientContext(window.localStorage);
      } catch {
        // Storage can be unavailable in restricted browser contexts.
      }
      await authStore.init();
      if (authStore.isAuthenticated || authStore.v2SessionActive) {
        await goto(`/stacks/new?placement=${placement}`);
        return;
      }
      await goto(windowsLocalClientReturnUrl);
    } catch {
      failed = true;
    } finally {
      starting = false;
    }
  }
</script>

<svelte:head>
  <title>Homelab Runtime</title>
</svelte:head>

<main
  class="grid min-h-screen place-items-center bg-background px-4 py-10 text-foreground"
  data-testid="client-first-start"
>
  <section class="flex w-full max-w-xl flex-col items-center gap-8 text-center">
    <HomelabBrandLogo collapsed class="size-16 object-contain" />

    {#if step === "mode"}
      <h1 class="text-3xl font-semibold text-foreground">
        {tr("ui.clientOnboarding.welcome")}
      </h1>
      <OptionTiles
        class="w-full"
        label={tr("ui.clientOnboarding.welcome")}
        name="client-first-start-mode"
        columns={2}
        value={mode}
        options={modeOptions}
        onchange={(next) => (mode = next as Mode)}
      />
      <Button
        variant="primary"
        size="lg"
        testId="client-first-start-continue"
        onclick={continueFromMode}
      >
        {tr("ui.clientOnboarding.start")}
      </Button>
      {#if showServerInput}
        <form
          class="flex w-full flex-col gap-3 sm:flex-row"
          onsubmit={(event) => {
            event.preventDefault();
            if (normalizedServerUrl) window.location.assign(normalizedServerUrl);
          }}
        >
          <input
            class="h-10 min-w-0 flex-1 rounded-md border border-border bg-background px-3 text-sm outline-none focus:border-primary"
            bind:value={serverUrl}
            aria-label={tr("ui.clientOnboarding.serverAddress")}
            placeholder="https://homelab.home"
            data-testid="client-first-start-server-url"
          />
          <Button type="submit" disabled={!normalizedServerUrl}>
            {tr("ui.clientOnboarding.connectServer")}
          </Button>
        </form>
      {:else}
        <button
          type="button"
          class="text-sm text-muted-foreground hover:text-foreground"
          data-testid="client-first-start-own-server"
          onclick={() => (showServerInput = true)}
        >
          {tr("ui.clientOnboarding.ownServer")} ›
        </button>
      {/if}
    {:else}
      <h1 class="text-3xl font-semibold text-foreground">
        {tr("ui.clientOnboarding.placementTitle")}
      </h1>
      <OptionTiles
        class="w-full"
        label={tr("ui.clientOnboarding.placementTitle")}
        name="client-first-start-placement"
        columns={3}
        value={placement}
        options={placementOptions}
        onchange={(next) => (placement = next as Placement)}
      />
      <Button
        variant="primary"
        size="lg"
        testId="client-first-start-placement-continue"
        disabled={starting}
        onclick={startOnThisComputer}
      >
        {tr("ui.clientOnboarding.start")}
      </Button>
      {#if failed}
        <p class="text-sm text-destructive" role="alert">
          {tr("ui.clientOnboarding.couldNotStart")}
        </p>
      {/if}
    {/if}
  </section>
</main>
