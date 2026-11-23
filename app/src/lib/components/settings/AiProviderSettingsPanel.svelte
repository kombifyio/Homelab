<script lang="ts">
  /**
   * Settings › AI providers (Windows client, flag `local_ai_providers`): Workbench Desktop's
   * provider-source standard drawn from Homelab's own settings components (workspace
   * `platform/standards/local-ai-runtimes.v1.md` rule 6). Three source cards filter the list;
   * the Local list shows each runtime's standard row, off until switched on, with its server
   * address and status line: this PC's Ollama and the Ollama of the person's homelab servers
   * (rule 2, `ollama-<name>`). "Answers in Homelab" is the answer choice of rule 9: the
   * Companion's questions run on the chosen runtime and model. The runtimes and the choice are
   * the device-wide file every kombify app on this PC shares; a change in another app shows here
   * at once. The homelab-server rows and the answer choice are an agent design (2026-11-23).
   */
  import { onMount } from "svelte";
  import { Bot, Code, Cpu, Laptop, Server, Terminal } from "@lucide/svelte";
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
  import { listCanonicalServices } from "#lib/api/services.js";
  import {
    addLocalAiRuntime,
    readLocalAi,
    removeLocalAiRuntime,
    setLocalAiAnswer,
    watchLocalAi,
    writeLocalAi,
    type LocalAiRefusal,
    type LocalAiRuntime,
    type LocalAiSnapshot,
  } from "#lib/client/local-ai-bridge.js";
  import { localAiReasonText, runtimeName } from "#lib/ai/local-ai-names.js";

  type Source = "kombify" | "other" | "local";
  const SOURCE_KEY = "kombify.homelab.provider-source";
  const RUNTIME_DOMAINS: Record<string, string> = { ollama: "ollama.com" };
  // The value of the answer choice for "answer from the account's AI use".
  const ACCOUNT = "";

  /** An Ollama app one of the person's servers runs, as the homelab inventory reports it. */
  interface HomelabOllama {
    id: string;
    name: string;
    endpoint: string;
  }

  let snapshot = $state<LocalAiSnapshot | null>(null);
  let aiUse = $state<AiUseChoice | null | "loading">("loading");
  let stored = $state<Source | null>(readStoredSource());
  let pending = $state(false);
  let refusal = $state<LocalAiRefusal | null>(null);
  let endpointDrafts = $state<Record<string, string>>({});
  let homelabOllamas = $state<HomelabOllama[] | null>(null);
  let newName = $state("");
  let newEndpoint = $state("");
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
  // The stored choice while it works, else Local while a local answer is chosen, else kombify
  // AI where it works, else Local.
  const source = $derived<Source>(
    stored === "local" || (stored === "kombify" && kombifyAvailable)
      ? stored
      : snapshot?.answer
        ? "local"
        : kombifyAvailable
          ? "kombify"
          : "local",
  );

  const answer = $derived(snapshot?.readable ? snapshot.answer : undefined);
  const answerRuntime = $derived(
    answer ? snapshot?.runtimes.find((r) => r.id === answer.runtime) : undefined,
  );
  // A runtime may answer once it is switched on; the chosen one stays listed while it is off.
  const answerChoices = $derived(
    (snapshot?.runtimes ?? []).filter(
      (runtime) => runtime.enabled || runtime.id === answer?.runtime,
    ),
  );
  const answerModels = $derived(
    answerRuntime?.status?.chatModels ?? answerRuntime?.status?.models ?? [],
  );
  const answerStatus = $derived.by(() => {
    if (!answer) return tr("settings.aiProviders.answer.account");
    const name = runtimeName(answerRuntime ?? answer.runtime);
    if (answer.reason === undefined)
      return tr("settings.aiProviders.status.checking");
    if (answer.reason)
      return localAiReasonText(answer.reason, name, answer.model);
    return tr("settings.aiProviders.answer.ready", {
      name,
      model: answer.resolvedModel || answer.model,
    });
  });
  const addedEndpoints = $derived(
    new Set((snapshot?.runtimes ?? []).map((runtime) => runtime.endpoint)),
  );

  async function refresh() {
    try {
      snapshot = await readLocalAi();
    } catch {
      // The shell did not answer: keep what is shown; the next change or visit reads again.
    }
  }

  async function apply(
    run: () => Promise<LocalAiSnapshot | { refused: LocalAiRefusal }>,
    after?: () => void,
  ) {
    pending = true;
    refusal = null;
    try {
      const result = await run();
      if ("refused" in result) refusal = result.refused;
      else {
        snapshot = result;
        after?.();
      }
    } catch {
      refusal = "failed";
    } finally {
      pending = false;
    }
  }

  function write(
    runtime: LocalAiRuntime,
    change: { enabled?: boolean; endpoint?: string },
  ) {
    return apply(
      () => writeLocalAi(runtime.id, change),
      () => delete endpointDrafts[runtime.id],
    );
  }

  function chooseAnswer(runtime: string, model = "") {
    return apply(() =>
      setLocalAiAnswer(runtime === ACCOUNT ? null : { runtime, model }),
    );
  }

  function addRuntime(name: string, endpoint: string) {
    return apply(
      () => addLocalAiRuntime(name, endpoint),
      () => {
        newName = "";
        newEndpoint = "";
      },
    );
  }

  // The Ollama apps of the person's servers (the canonical services of the homelab inventory):
  // each one's web address is the runtime's server address.
  async function loadHomelabOllamas() {
    try {
      const services = await listCanonicalServices();
      homelabOllamas = services
        .filter(
          (service) =>
            (service.application_key === "ollama" ||
              service.catalog?.id === "ollama") &&
            typeof service.access.url === "string" &&
            /^https?:\/\//.test(service.access.url),
        )
        .map((service) => ({
          id: service.id,
          name:
            service.server_id || service.application_display_name || service.name,
          endpoint: String(service.access.url).replace(/\/+$/, ""),
        }));
    } catch {
      homelabOllamas = [];
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
        : refusal === "invalid"
          ? tr("settings.aiProviders.refused.invalid")
          : refusal === "failed"
            ? tr("settings.aiProviders.refused.failed")
            : "",
  );

  onMount(() => {
    void refresh();
    void fetchAiUse().then((choice) => (aiUse = choice));
    void loadHomelabOllamas();
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

  {#if snapshot}
    <SettingsSection
      id="ai-provider-answer"
      title={tr("settings.aiProviders.answer")}
    >
      <SettingsRow
        label={tr("settings.aiProviders.answer.with")}
        description={tr("settings.aiProviders.answer.description")}
        facts={[
          { label: tr("settings.aiProviders.status"), value: answerStatus },
        ]}
        for="local-ai-answer"
      >
        <select
          id="local-ai-answer"
          data-testid="local-ai-answer"
          class="max-w-64 rounded-lg border border-border bg-card px-3 py-1.5 text-sm text-foreground disabled:cursor-not-allowed disabled:opacity-60"
          disabled={pending || !snapshot.readable}
          value={answer?.runtime ?? ACCOUNT}
          onchange={(event) => void chooseAnswer(event.currentTarget.value)}
        >
          <option value={ACCOUNT}>{tr("settings.aiProviders.answer.kombify")}</option>
          {#each answerChoices as runtime (runtime.id)}
            <option value={runtime.id}
              >{tr("settings.aiProviders.answer.local", {
                name: runtimeName(runtime),
              })}</option
            >
          {/each}
        </select>
      </SettingsRow>
      {#if answer}
        <SettingsRow
          label={tr("settings.aiProviders.answer.model")}
          description={tr("settings.aiProviders.answer.scope")}
          for="local-ai-answer-model"
          narrow="below"
        >
          <select
            id="local-ai-answer-model"
            data-testid="local-ai-answer-model"
            class="max-w-64 rounded-lg border border-border bg-card px-3 py-1.5 text-sm text-foreground disabled:cursor-not-allowed disabled:opacity-60"
            disabled={pending}
            value={answer.model}
            onchange={(event) =>
              void chooseAnswer(answer.runtime, event.currentTarget.value)}
          >
            <option value=""
              >{tr("settings.aiProviders.answer.firstModel")}</option
            >
            {#each answerModels as model (model)}
              <option value={model}>{model}</option>
            {/each}
            {#if answer.model && !answerModels.includes(answer.model)}
              <option value={answer.model}>{answer.model}</option>
            {/if}
          </select>
        </SettingsRow>
      {/if}
    </SettingsSection>
  {/if}

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
          >{answer
            ? tr("settings.aiProviders.kombify.localAnswers")
            : tr("settings.aiProviders.kombify.active")}</span
        >
      </SettingsRow>
    {:else if source === "local" && snapshot}
      {#each snapshot.runtimes as runtime (runtime.id)}
        {@const domain = RUNTIME_DOMAINS[runtime.kind] ?? ""}
        <SettingsRow
          label={runtimeName(runtime)}
          description={runtime.remote
            ? tr("settings.aiProviders.remote.description")
            : tr("settings.aiProviders.ollama.description")}
          facts={[
            {
              label: tr("settings.aiProviders.status"),
              value: statusLine(runtime),
            },
          ]}
          for={`local-ai-${runtime.id}`}
        >
          <span class="flex items-center gap-3">
            {#if runtime.remote}
              <Server class="size-5 text-muted-foreground" aria-hidden="true" />
            {:else if domain}
              <img
                src={localBrandLogoUrl(domain)}
                alt=""
                draggable="false"
                class="size-6 rounded object-contain"
              />
            {/if}
            <Switch
              id={`local-ai-${runtime.id}`}
              testId={`local-ai-${runtime.id}`}
              checked={runtime.enabled}
              label={runtimeName(runtime)}
              disabled={pending || !snapshot.readable}
              onchange={(next) => void write(runtime, { enabled: next })}
              onText={tr("settings.aiProviders.on")}
              offText={tr("settings.aiProviders.off")}
            />
          </span>
        </SettingsRow>
        <SettingsRow
          label={tr("settings.aiProviders.endpoint")}
          description={runtime.remote
            ? tr("settings.aiProviders.homelab.endpoint.description")
            : tr("settings.aiProviders.endpoint.description")}
          for={`local-ai-${runtime.id}-endpoint`}
          narrow="below"
        >
          <span class="flex flex-wrap items-center gap-2">
            <input
              id={`local-ai-${runtime.id}-endpoint`}
              type="url"
              class="w-64 rounded-lg border border-border bg-card px-3 py-1.5 text-sm text-foreground disabled:cursor-not-allowed disabled:opacity-60"
              placeholder={runtime.defaultEndpoint || runtime.endpoint}
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
                      runtime.defaultEndpoint ||
                      runtime.endpoint,
                  })}
              >
                {tr("settings.aiProviders.save")}
              </Button>
            {/if}
            {#if runtime.remote}
              <Button
                variant="ghost"
                size="sm"
                disabled={pending}
                testId={`local-ai-${runtime.id}-remove`}
                onclick={() =>
                  void apply(() => removeLocalAiRuntime(runtime.id))}
              >
                {tr("settings.aiProviders.remove")}
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

  {#if source === "local" && snapshot?.readable}
    <SettingsSection
      id="ai-provider-homelab"
      title={tr("settings.aiProviders.homelab")}
    >
      {#each homelabOllamas ?? [] as found (found.id)}
        <SettingsRow
          label={tr("settings.aiProviders.homelab.found", { name: found.name })}
          description={found.endpoint}
        >
          <Button
            variant="secondary"
            size="sm"
            disabled={pending || addedEndpoints.has(found.endpoint)}
            onclick={() => void addRuntime(found.name, found.endpoint)}
          >
            {addedEndpoints.has(found.endpoint)
              ? tr("settings.aiProviders.homelab.added")
              : tr("settings.aiProviders.homelab.add")}
          </Button>
        </SettingsRow>
      {/each}
      {#if homelabOllamas !== null && homelabOllamas.length === 0}
        <p class="px-1 text-sm text-muted-foreground">
          {tr("settings.aiProviders.homelab.none")}
        </p>
      {/if}
      <SettingsRow
        label={tr("settings.aiProviders.homelab.manual")}
        description={tr("settings.aiProviders.homelab.description")}
        for="local-ai-new-name"
        narrow="below"
      >
        <form
          class="flex flex-wrap items-center gap-2"
          onsubmit={(event) => {
            event.preventDefault();
            void addRuntime(newName.trim(), newEndpoint.trim());
          }}
        >
          <input
            id="local-ai-new-name"
            data-testid="local-ai-new-name"
            class="w-32 rounded-lg border border-border bg-card px-3 py-1.5 text-sm text-foreground"
            maxlength="80"
            placeholder={tr("settings.aiProviders.homelab.namePlaceholder")}
            aria-label={tr("settings.aiProviders.homelab.name")}
            bind:value={newName}
          />
          <input
            type="url"
            data-testid="local-ai-new-endpoint"
            class="w-56 rounded-lg border border-border bg-card px-3 py-1.5 text-sm text-foreground"
            placeholder="http://192.168.1.20:11434"
            aria-label={tr("settings.aiProviders.endpoint")}
            bind:value={newEndpoint}
          />
          <Button
            type="submit"
            variant="secondary"
            size="sm"
            disabled={pending || !newName.trim() || !newEndpoint.trim()}
          >
            {tr("settings.aiProviders.homelab.add")}
          </Button>
        </form>
      </SettingsRow>
    </SettingsSection>
  {/if}
</div>
