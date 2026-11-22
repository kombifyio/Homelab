<script lang="ts">
  /**
   * Settings › AI providers (Windows client, flag `local_ai_providers`): Workbench Desktop's
   * provider-source standard drawn from Homelab's own settings components (workspace
   * `platform/standards/local-ai-runtimes.v1.md` rule 6). Three source cards filter the list;
   * the Local list shows each runtime's standard row, off until switched on, with its server
   * address and status line. The runtimes are the device-wide file every kombify app on this
   * PC shares; a change in another app shows here at once. Homelab Runtime runs no AI on a
   * local runtime yet, and the row says so.
   */
  import { onMount } from "svelte";
  import { Bot, Code, Cpu, Laptop, Terminal } from "@lucide/svelte";
  import { brandMarkRosetteSrc } from "@kombiverselabs/ui/shell";
  import {
    OptionTiles,
    SettingsRow,
    SettingsSection,
    type OptionTileSpec,
  } from "@kombiverselabs/ui/settings";
  import { tr } from "#lib/i18n.svelte.js";
  import { localBrandLogoUrl } from "#lib/brand-logo.js";
  import { theme } from "#lib/stores/theme.js";
  import Button from "#lib/components/ui/Button.svelte";
  import Switch from "#lib/components/ui/Switch.svelte";
  import { fetchAiUse, type AiUseChoice } from "#lib/ai/ai-use.js";
  import {
    readLocalAi,
    watchLocalAi,
    writeLocalAi,
    type LocalAiRefusal,
    type LocalAiRuntime,
    type LocalAiSnapshot,
  } from "#lib/client/local-ai-bridge.js";

  type Source = "kombify" | "other" | "local";
  const SOURCE_KEY = "kombify.homelab.provider-source";
  const RUNTIME_NAMES: Record<string, { name: string; domain: string }> = {
    ollama: { name: "Ollama", domain: "ollama.com" },
  };

  let snapshot = $state<LocalAiSnapshot | null>(null);
  let aiUse = $state<AiUseChoice | null | "loading">("loading");
  let stored = $state<Source | null>(readStoredSource());
  let pending = $state(false);
  let refusal = $state<LocalAiRefusal | null>(null);
  let endpointDrafts = $state<Record<string, string>>({});
  const resolved = theme.resolved;

  function readStoredSource(): Source | null {
    try {
      const value = globalThis.localStorage?.getItem(SOURCE_KEY);
      return value === "kombify" || value === "other" || value === "local"
        ? value
        : null;
    } catch {
      return null;
    }
  }

  function choose(value: string) {
    const next = value as Source;
    stored = next;
    try {
      globalThis.localStorage?.setItem(SOURCE_KEY, next);
    } catch {
      // No storage: the choice applies while the page is open.
    }
  }

  // kombify AI works while signed in and the account's AI use allows it (AI Platform refuses
  // kombify AI for `own` and `none`); other providers are not offered by Homelab Runtime yet.
  const kombifyGuidance = $derived(
    aiUse === null || aiUse === "loading"
      ? tr("settings.aiProviders.source.kombify.signedOut")
      : aiUse === "own"
        ? tr("settings.aiProviders.source.kombify.own")
        : aiUse === "none"
          ? tr("settings.aiProviders.source.kombify.none")
          : "",
  );
  const kombifyAvailable = $derived(kombifyGuidance === "");
  const options: OptionTileSpec[] = $derived([
    {
      value: "kombify",
      label: tr("settings.aiProviders.source.kombify"),
      hint: tr("settings.aiProviders.source.kombify.description"),
      ...(kombifyAvailable
        ? {}
        : { availability: "disabled" as const, guidance: kombifyGuidance }),
    },
    {
      value: "other",
      label: tr("settings.aiProviders.source.other"),
      hint: tr("settings.aiProviders.source.other.description"),
      availability: "disabled",
      guidance: tr("settings.aiProviders.source.other.unavailable"),
    },
    {
      value: "local",
      label: tr("settings.aiProviders.source.local"),
      hint: tr("settings.aiProviders.source.local.description"),
    },
  ]);
  // The stored choice while it works, else kombify AI where it works, else Local.
  const source = $derived<Source>(
    stored === "local" || (stored === "kombify" && kombifyAvailable)
      ? stored
      : kombifyAvailable
        ? "kombify"
        : "local",
  );

  async function refresh() {
    try {
      snapshot = await readLocalAi();
    } catch {
      // The shell did not answer: keep what is shown; the next change or visit reads again.
    }
  }

  async function write(
    runtime: LocalAiRuntime,
    change: { enabled?: boolean; endpoint?: string },
  ) {
    pending = true;
    refusal = null;
    try {
      const answer = await writeLocalAi(runtime.id, change);
      if ("refused" in answer) refusal = answer.refused;
      else {
        snapshot = answer;
        delete endpointDrafts[runtime.id];
      }
    } catch {
      refusal = "failed";
    } finally {
      pending = false;
    }
  }

  function statusLine(runtime: LocalAiRuntime): string {
    if (!runtime.enabled) return tr("settings.aiProviders.off");
    if (!runtime.status) return tr("settings.aiProviders.status.checking");
    if (!runtime.status.reachable)
      return tr("settings.aiProviders.status.unreachable", {
        endpoint: runtime.endpoint,
      });
    return runtime.status.models.length > 0
      ? tr("settings.aiProviders.status.available", {
          models: runtime.status.models.join(", "),
        })
      : tr("settings.aiProviders.status.noModels");
  }

  const refusalText = $derived(
    refusal === "invalid_endpoint"
      ? tr("settings.aiProviders.refused.endpoint")
      : refusal === "unreadable"
        ? tr("settings.aiProviders.refused.unreadable")
        : refusal === "failed"
          ? tr("settings.aiProviders.refused.failed")
          : "",
  );

  onMount(() => {
    void refresh();
    void fetchAiUse().then((choice) => (aiUse = choice));
    return watchLocalAi(() => void refresh());
  });
</script>

{#snippet thumb(option: OptionTileSpec)}
  <span
    aria-hidden="true"
    class="flex h-14 w-full select-none items-center justify-center gap-1.5 overflow-hidden rounded-lg border border-border bg-background"
  >
    {#if option.value === "kombify"}
      <img
        src={brandMarkRosetteSrc(
          "kombify",
          $resolved === "dark" ? "dark" : "light",
        )}
        alt=""
        draggable="false"
        class="size-7 object-contain"
      />
    {:else if option.value === "other"}
      <Bot class="size-4 text-muted-foreground" />
      <Code class="size-4 text-muted-foreground" />
      <Terminal class="size-4 text-muted-foreground" />
    {:else}
      <span class="relative">
        <Laptop class="size-7 text-muted-foreground" />
        <Cpu
          class="absolute start-1/2 top-[38%] size-3 -translate-x-1/2 -translate-y-1/2 text-primary rtl:translate-x-1/2"
        />
      </span>
    {/if}
  </span>
{/snippet}

<div class="space-y-6" data-testid="ai-provider-settings" data-source={source}>
  <SettingsSection
    id="ai-provider-source"
    title={tr("settings.aiProviders.source")}
  >
    <OptionTiles
      label={tr("settings.aiProviders.source")}
      name="ai-provider-source"
      value={source}
      {options}
      columns={3}
      tile={thumb}
      onchange={choose}
    />
  </SettingsSection>

  <SettingsSection
    id="ai-provider-list"
    title={tr("settings.aiProviders.title")}
  >
    {#if source === "kombify"}
      <SettingsRow
        label={tr("settings.aiProviders.source.kombify")}
        description={tr("settings.aiProviders.kombify.description")}
      >
        <span class="text-sm text-muted-foreground"
          >{tr("settings.aiProviders.kombify.active")}</span
        >
      </SettingsRow>
    {:else if source === "local" && snapshot}
      {#each snapshot.runtimes as runtime (runtime.id)}
        {@const meta = RUNTIME_NAMES[runtime.id] ?? {
          name: runtime.id,
          domain: "",
        }}
        <SettingsRow
          label={meta.name}
          description={tr("settings.aiProviders.ollama.description")}
          facts={[
            { label: tr("settings.aiProviders.status"), value: statusLine(runtime) },
          ]}
          for={`local-ai-${runtime.id}`}
        >
          <span class="flex items-center gap-3">
            {#if meta.domain}
              <img
                src={localBrandLogoUrl(meta.domain)}
                alt=""
                draggable="false"
                class="size-6 rounded object-contain"
              />
            {/if}
            <Switch
              id={`local-ai-${runtime.id}`}
              testId={`local-ai-${runtime.id}`}
              checked={runtime.enabled}
              label={meta.name}
              disabled={pending || !snapshot.readable}
              onchange={(next) => void write(runtime, { enabled: next })}
              onText={tr("settings.aiProviders.on")}
              offText={tr("settings.aiProviders.off")}
            />
          </span>
        </SettingsRow>
        <SettingsRow
          label={tr("settings.aiProviders.endpoint")}
          description={tr("settings.aiProviders.endpoint.description")}
          for={`local-ai-${runtime.id}-endpoint`}
          narrow="below"
        >
          <span class="flex flex-wrap items-center gap-2">
            <input
              id={`local-ai-${runtime.id}-endpoint`}
              type="url"
              class="w-64 rounded-lg border border-border bg-card px-3 py-1.5 text-sm text-foreground disabled:cursor-not-allowed disabled:opacity-60"
              placeholder={runtime.defaultEndpoint}
              disabled={pending || !snapshot.readable}
              value={endpointDrafts[runtime.id] ?? runtime.endpoint}
              oninput={(event) =>
                (endpointDrafts[runtime.id] = event.currentTarget.value)}
            />
            {#if endpointDrafts[runtime.id] !== undefined && endpointDrafts[runtime.id] !== runtime.endpoint}
              <Button
                variant="secondary"
                size="sm"
                disabled={pending}
                onclick={() =>
                  void write(runtime, {
                    endpoint:
                      endpointDrafts[runtime.id]?.trim() ||
                      runtime.defaultEndpoint,
                  })}
              >
                {tr("settings.aiProviders.save")}
              </Button>
            {/if}
          </span>
        </SettingsRow>
      {/each}
      {#if refusalText || !snapshot.readable}
        <p class="text-sm text-destructive" role="status">
          {refusalText || tr("settings.aiProviders.refused.unreadable")}
        </p>
      {/if}
    {/if}
  </SettingsSection>
</div>
