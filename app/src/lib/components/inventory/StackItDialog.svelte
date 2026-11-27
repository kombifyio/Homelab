<!--
  "Stack it": rebuild one unmanaged container as an own application. The
  dialog plans first (read-only), shows the enriched app identity and reviewed
  rows, lets the user adjust overrides and re-plan, and approves exactly that
  plan digest. Restore and remove act on the kept original.
-->
<script lang="ts">
  import { onMount } from "svelte";
  import { tr, formatDate } from "#lib/i18n.svelte.js";
  import Modal from "#lib/components/Modal.svelte";
  import Button from "#lib/components/ui/Button.svelte";
  import { getJob } from "#lib/api/jobs.js";
  import {
    getServiceEnrichment,
    prepareApplicationAdoption,
    readApplicationAdoption,
    executeApplicationAdoption,
    resolveStackedOriginal,
    type CanonicalService,
    type ApplicationAdoptionPlan,
    type ServiceEnrichment,
    type StackItPlan,
    type StackItPlanOverrides,
  } from "#lib/api/services.js";
  import {
    byteSize,
    stackItFailure,
    stackItJobError,
    stackItRowText,
  } from "#lib/services/stack-it-plan.js";

  type Mode = "stack" | "restore_original" | "release_original";
  let {
    service,
    mode = "stack",
    onClose,
    onComplete,
  }: {
    service: CanonicalService;
    mode?: Mode;
    onClose: () => void;
    onComplete: () => Promise<void>;
  } = $props();

  const componentId = $props.id();
  const customizePanelId = `${componentId}-customize`;

  let plan = $state<ApplicationAdoptionPlan | null>(null);
  let enrichment = $state<ServiceEnrichment | null>(null);
  let iconFailed = $state(false);
  let busy = $state(false);
  let error = $state<string | null>(null);
  let phase = $state<"planning" | "review" | "running" | "done">("planning");
  let active = true;

  // User-editable proposal overrides
  let customizeOpen = $state(false);
  let ownApplicationName = $state("");
  let httpPortText = $state("");
  let noOutbound = $state(false);
  let memoryMiBText = $state("");
  let cpusText = $state("");
  let adoptComposeProject = $state(false);
  let httpServiceText = $state("");
  let plannedSignature = $state("");
  let executeKey = $state(crypto.randomUUID());

  const COMPOSE_SERVICE_PATTERN = /^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$/;

  const stackPlan = $derived<StackItPlan | undefined>(plan?.stack_plan);
  const proposal = $derived<ServiceEnrichment | null>(
    enrichment ?? plan?.catalog_proposal ?? null,
  );
  const refused = $derived(
    stackPlan?.rows.filter((row) => row.status === "refused") ?? [],
  );
  const hasActionableRefusal = $derived(
    refused.some(
      (row) =>
        row.code === "refused_http_port_ambiguous" ||
        row.code === "refused_review_outbound" ||
        row.code === "refused_compose_project" ||
        row.code === "refused_http_service_ambiguous",
    ),
  );
  const currentSignature = $derived(
    JSON.stringify({
      ownApplicationName: ownApplicationName.trim(),
      httpPortText: httpPortText.trim(),
      noOutbound,
      memoryMiBText: memoryMiBText.trim(),
      cpusText: cpusText.trim(),
      adoptComposeProject,
      httpServiceText: httpServiceText.trim(),
    }),
  );
  const overridesDirty = $derived(
    mode === "stack" &&
      plannedSignature !== "" &&
      currentSignature !== plannedSignature,
  );
  const approvable = $derived(
    mode !== "stack" ||
      (!overridesDirty &&
        stackPlan?.outcome === "planned" &&
        stackPlan.admissible &&
        Boolean(stackPlan.planDigest)),
  );
  const retainUntil = $derived(service.adoption?.stack_it?.retain_until);

  const displayName = $derived(
    proposal?.display_name ||
      service.catalog?.display_name ||
      service.application_display_name ||
      service.name,
  );
  const vendorName = $derived(
    proposal?.vendor_name || service.catalog?.vendor?.name || "",
  );
  const homepageUrl = $derived(
    safeHttps(
      proposal?.homepage_url ||
        service.catalog?.homepage_url ||
        service.catalog?.vendor?.website,
    ),
  );
  const docsUrl = $derived(
    safeHttps(proposal?.docs_url || service.catalog?.vendor?.docs),
  );
  const sourceRepoUrl = $derived(safeHttps(proposal?.source_repo));
  const logoRemoved = $derived(
    Boolean(proposal?.logo_removed || service.catalog?.logo_removed),
  );
  const logoUrl = $derived.by(() => {
    if (logoRemoved || iconFailed) return "";
    const candidate =
      proposal?.logo?.url ||
      service.catalog?.logo?.url ||
      proposal?.icon_url ||
      "";
    return safeHttps(candidate);
  });
  const monogram = $derived(
    (displayName.trim().charAt(0) || "?").toUpperCase(),
  );

  const title = $derived(
    mode === "restore_original"
      ? tr("ui.services.restoreOriginal")
      : mode === "release_original"
        ? tr("ui.services.removeOriginal")
        : tr("ui.services.stackIt"),
  );

  const size = byteSize;

  function safeHttps(value?: string): string {
    return typeof value === "string" && value.startsWith("https://")
      ? value
      : "";
  }

  function port(p: StackItPlan["publishedPorts"][number]): string {
    const host = p.hostIP ? `${p.hostIP}:` : "*:";
    return `${host}${p.hostPort} → ${p.containerPort}/${p.protocol}`;
  }

  function buildOverrides(): {
    overrides?: StackItPlanOverrides;
    valid: boolean;
  } {
    const overrides: StackItPlanOverrides = {};
    const trimmedName = ownApplicationName.trim();
    if (trimmedName) {
      overrides.own_application_name = trimmedName;
    }
    if (httpPortText.trim() !== "") {
      const parsed = Number(httpPortText.trim());
      if (!Number.isInteger(parsed) || parsed < 1 || parsed > 65535) {
        return { valid: false };
      }
      overrides.http_port = parsed;
    }
    if (noOutbound) {
      overrides.no_outbound = true;
    }
    if (memoryMiBText.trim() !== "") {
      const parsed = Number(memoryMiBText.trim());
      if (!Number.isInteger(parsed) || parsed < 64 || parsed > 8192) {
        return { valid: false };
      }
      overrides.memory_mib = parsed;
    }
    if (cpusText.trim() !== "") {
      const parsed = Number(cpusText.trim());
      if (!Number.isFinite(parsed) || parsed < 0.1 || parsed > 4.0) {
        return { valid: false };
      }
      overrides.cpus = parsed;
    }
    if (adoptComposeProject) {
      overrides.adopt_compose_project = true;
    }
    const trimmedHttpService = httpServiceText.trim();
    if (trimmedHttpService !== "") {
      if (!COMPOSE_SERVICE_PATTERN.test(trimmedHttpService)) {
        return { valid: false };
      }
      overrides.http_service = trimmedHttpService;
    }
    return {
      overrides: Object.keys(overrides).length > 0 ? overrides : undefined,
      valid: true,
    };
  }

  const JOB_POLL_INTERVAL_MS = 1500;
  const JOB_DEADLINE_MS = 10 * 60 * 1000;
  const JOB_MAX_TRANSIENT_FAILURES = 5;

  // Polls until the job completes. Bounded by a deadline; a few consecutive
  // read failures are retried with backoff before the last error surfaces.
  async function waitJob(id: string) {
    if (!id) throw new Error("job id missing");
    const deadline = Date.now() + JOB_DEADLINE_MS;
    let failures = 0;
    while (active) {
      if (Date.now() > deadline) throw new Error("job wait timed out");
      let job: Awaited<ReturnType<typeof getJob>> | undefined;
      try {
        job = await getJob(id);
        failures = 0;
      } catch (e) {
        failures += 1;
        if (failures >= JOB_MAX_TRANSIENT_FAILURES) throw e;
      }
      if (job) {
        if (job.state === "completed") return;
        if (["failed", "canceled", "cancelled"].includes(job.state))
          throw stackItJobError(job.result);
      }
      const delay = JOB_POLL_INTERVAL_MS * 2 ** Math.min(failures, 3);
      await new Promise((resolve) => setTimeout(resolve, delay));
    }
  }

  async function loadEnrichment() {
    try {
      const result = await getServiceEnrichment(service.id);
      if (active) {
        enrichment = result;
        iconFailed = false;
      }
    } catch (e) {
      // Enrichment is optional: the dialog falls back to catalog/plan
      // metadata, but the failure stays visible in the console.
      if (active) console.warn("stack-it: enrichment unavailable", e);
    }
  }

  async function prepare() {
    const built = buildOverrides();
    if (!built.valid) {
      error = tr("ui.services.stackItInvalidOverride");
      return;
    }
    busy = true;
    error = null;
    phase = "planning";
    const key = crypto.randomUUID();
    const signatureSnapshot = currentSignature;
    try {
      const job = await prepareApplicationAdoption(
        service,
        key,
        "stack_it",
        built.overrides,
      );
      await waitJob(job.job_id || "");
      if (active) {
        plan = await readApplicationAdoption(service.id, job.job_id || "");
        plannedSignature = signatureSnapshot;
        executeKey = crypto.randomUUID();
        phase = "review";
      }
    } catch (e) {
      error = stackItFailure(e);
      phase = "review";
    } finally {
      busy = false;
    }
  }

  async function startApproval() {
    if (mode !== "stack") {
      return resolveStackedOriginal(service, mode, executeKey);
    }
    if (!plan) throw new Error("no reviewed plan to approve");
    return executeApplicationAdoption(service, plan, executeKey);
  }

  async function approve() {
    busy = true;
    error = null;
    phase = "running";
    try {
      const job = await startApproval();
      await waitJob(job.job_id || "");
      if (active) {
        await onComplete();
        if (active) phase = "done";
      }
    } catch (e) {
      error = stackItFailure(e);
      phase = "review";
    } finally {
      busy = false;
    }
  }

  $effect(() => {
    if (hasActionableRefusal) {
      customizeOpen = true;
    }
  });

  onMount(() => {
    void loadEnrichment();
    if (mode === "stack") void prepare();
    else phase = "review";
    return () => {
      active = false;
    };
  });
</script>

<Modal {title} {onClose} maxWidth="xl" testId="stack-it-dialog">
  <div class="space-y-4 text-sm">
    <!-- App identity header -->
    <div
      class="flex items-start gap-3 rounded-lg border border-border/70 bg-muted/20 p-3"
      data-testid="stack-it-identity"
    >
      <div
        class="flex h-10 w-10 shrink-0 items-center justify-center overflow-hidden rounded-lg border border-border/60 bg-background"
      >
        {#if logoUrl}
          <img
            src={logoUrl}
            alt={displayName}
            class="h-7 w-7 object-contain"
            onerror={() => {
              iconFailed = true;
            }}
          />
        {:else}
          <span class="text-sm font-semibold text-muted-foreground"
            >{monogram}</span
          >
        {/if}
      </div>
      <div class="min-w-0 flex-1 space-y-1">
        <div class="flex flex-wrap items-center gap-2">
          <p class="truncate font-medium text-foreground">{displayName}</p>
          {#if vendorName && vendorName.toLowerCase() !== displayName.toLowerCase()}
            <span class="text-xs text-muted-foreground">· {vendorName}</span>
          {/if}
          {#if proposal?.category && proposal.category !== "unknown"}
            <span
              class="rounded bg-muted px-1.5 py-0.5 text-[10px] font-medium uppercase tracking-wide text-muted-foreground"
              >{proposal.category}</span
            >
          {/if}
        </div>
        {#if service.name !== displayName}
          <p class="truncate font-mono text-xs text-muted-foreground">
            {service.name}
          </p>
        {/if}
        {#if homepageUrl || docsUrl || sourceRepoUrl}
          <div class="flex flex-wrap items-center gap-3 pt-0.5 text-xs">
            {#if homepageUrl}
              <a
                href={homepageUrl}
                target="_blank"
                rel="noopener noreferrer"
                class="text-primary underline-offset-2 hover:underline"
                >{tr("ui.services.stackItHomepage")}</a
              >
            {/if}
            {#if docsUrl}
              <a
                href={docsUrl}
                target="_blank"
                rel="noopener noreferrer"
                class="text-primary underline-offset-2 hover:underline"
                >{tr("ui.services.stackItDocs")}</a
              >
            {/if}
            {#if sourceRepoUrl}
              <a
                href={sourceRepoUrl}
                target="_blank"
                rel="noopener noreferrer"
                class="text-primary underline-offset-2 hover:underline"
                >{tr("ui.services.stackItSource")}</a
              >
            {/if}
          </div>
        {/if}
      </div>
    </div>

    {#if mode === "stack"}
      {#if phase === "planning" && !error}
        <p role="status" class="text-muted-foreground">
          {tr("ui.services.stackItPlanning")}
        </p>
      {:else if stackPlan}
        <p class="text-muted-foreground">{tr("ui.services.stackItRecreated")}</p>
        <dl
          class="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5"
          data-testid="stack-it-facts"
        >
          <dt class="text-muted-foreground">{tr("ui.services.stackItName")}</dt>
          <dd class="font-mono text-xs">own-{stackPlan.name ?? ""}</dd>
          {#if stackPlan.downtimeSeconds}
            <dt class="text-muted-foreground">
              {tr("ui.services.stackItDowntime")}
            </dt>
            <dd>
              {tr("ui.services.stackItSeconds", {
                seconds: stackPlan.downtimeSeconds,
              })}
            </dd>
          {/if}
          {#if stackPlan.data}
            <dt class="text-muted-foreground">
              {tr("ui.services.stackItData")}
            </dt>
            <dd>
              {tr("ui.services.stackItDisk", {
                size: size(stackPlan.data.bytes),
                free: size(stackPlan.data.freeBytes),
              })}
            </dd>
          {/if}
          {#if stackPlan.publishedPorts.length > 0}
            <dt class="text-muted-foreground">
              {tr("ui.services.stackItPorts")}
            </dt>
            <dd class="font-mono text-xs">
              {#each stackPlan.publishedPorts as p (port(p))}<div>
                  {port(p)}
                </div>{/each}
            </dd>
          {/if}
          {#if stackPlan.hiddenKeys.length > 0}
            <dt class="text-muted-foreground">
              {tr("ui.services.stackItSecrets")}
            </dt>
            <dd class="break-words font-mono text-xs">
              {stackPlan.hiddenKeys.join(", ")}
            </dd>
          {/if}
        </dl>

        <ul
          class="max-h-64 divide-y divide-border/60 overflow-auto rounded-lg border border-border/70"
          data-testid="stack-it-rows"
        >
          {#each stackPlan.rows as row, index (`${row.code}-${index}`)}
            <li class="flex items-start gap-3 px-3 py-2">
              <span
                class="mt-0.5 shrink-0 rounded px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide {row.status ===
                'refused'
                  ? 'bg-destructive/15 text-destructive'
                  : row.status === 'changed'
                    ? 'bg-warning/15 text-warning'
                    : 'bg-muted text-muted-foreground'}"
                >{row.status === "changed" ||
                row.status === "kept" ||
                row.status === "refused"
                  ? tr(`ui.services.stackItRow.${row.status}`)
                  : row.status}</span
              >
              <div class="min-w-0">
                <p class="text-xs text-foreground" data-row-code={row.code}>
                  {stackItRowText(row)}
                </p>
                {#if row.observed || row.adopted}<p
                    class="break-words font-mono text-[11px] text-muted-foreground"
                  >
                    {row.adopted && row.adopted !== row.observed
                      ? `${row.observed ?? ""} → ${row.adopted}`
                      : (row.observed ?? row.adopted)}
                  </p>{/if}
              </div>
            </li>
          {/each}
        </ul>
        {#if refused.length > 0}
          <p class="text-destructive" role="alert">
            {tr("ui.services.stackItRefused")}
          </p>
        {/if}
      {/if}

      {#if phase === "review"}
        <div
          class="rounded-lg border border-border/70"
          data-testid="stack-it-customize"
        >
          <button
            type="button"
            class="flex w-full items-center justify-between px-3 py-2 text-left text-xs font-medium text-foreground hover:bg-muted/40"
            aria-expanded={customizeOpen}
            aria-controls={customizePanelId}
            data-testid="stack-it-customize-toggle"
            onclick={() => {
              customizeOpen = !customizeOpen;
            }}
          >
            <span>{tr("ui.services.stackItCustomize")}</span>
            <span class="text-muted-foreground">{customizeOpen ? "−" : "+"}</span>
          </button>

          {#if customizeOpen}
            <div
              id={customizePanelId}
              class="space-y-3 border-t border-border/60 p-3"
            >
              <p class="text-xs text-muted-foreground">
                {tr("ui.services.stackItCustomizeHint")}
              </p>
              <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <label class="block space-y-1">
                  <span class="text-xs font-medium text-foreground"
                    >{tr("ui.services.stackItName")}</span
                  >
                  <input
                    type="text"
                    bind:value={ownApplicationName}
                    placeholder={stackPlan?.name ??
                      service.adoption?.stack_it?.name ??
                      ""}
                    maxlength="40"
                    disabled={busy}
                    data-testid="stack-it-input-name"
                    class="w-full rounded-md border border-border bg-background px-2.5 py-1.5 font-mono text-xs text-foreground placeholder:text-muted-foreground focus:border-primary focus:outline-none"
                  />
                </label>

                <label class="block space-y-1">
                  <span class="text-xs font-medium text-foreground"
                    >{tr("ui.services.stackItHttpPort")}</span
                  >
                  <input
                    type="text"
                    inputmode="numeric"
                    autocomplete="off"
                    bind:value={httpPortText}
                    placeholder={tr("ui.services.stackItHttpPortPlaceholder")}
                    disabled={busy}
                    data-testid="stack-it-input-http-port"
                    class="w-full rounded-md border border-border bg-background px-2.5 py-1.5 font-mono text-xs text-foreground placeholder:text-muted-foreground focus:border-primary focus:outline-none"
                  />
                </label>

                <label class="block space-y-1">
                  <span class="text-xs font-medium text-foreground"
                    >{tr("ui.services.stackItMemoryMiB")}</span
                  >
                  <input
                    type="text"
                    inputmode="numeric"
                    autocomplete="off"
                    bind:value={memoryMiBText}
                    placeholder="64–8192"
                    disabled={busy}
                    data-testid="stack-it-input-memory"
                    class="w-full rounded-md border border-border bg-background px-2.5 py-1.5 font-mono text-xs text-foreground placeholder:text-muted-foreground focus:border-primary focus:outline-none"
                  />
                </label>

                <label class="block space-y-1">
                  <span class="text-xs font-medium text-foreground"
                    >{tr("ui.services.stackItCpus")}</span
                  >
                  <input
                    type="text"
                    inputmode="decimal"
                    autocomplete="off"
                    bind:value={cpusText}
                    placeholder="0.1–4.0"
                    disabled={busy}
                    data-testid="stack-it-input-cpus"
                    class="w-full rounded-md border border-border bg-background px-2.5 py-1.5 font-mono text-xs text-foreground placeholder:text-muted-foreground focus:border-primary focus:outline-none"
                  />
                </label>
              </div>

              <label class="flex items-center gap-2 text-xs text-foreground">
                <input
                  type="checkbox"
                  bind:checked={noOutbound}
                  disabled={busy}
                  data-testid="stack-it-input-no-outbound"
                  class="h-4 w-4 rounded border-border"
                />
                <span>{tr("ui.services.stackItNoOutbound")}</span>
              </label>

              <div class="space-y-2 border-t border-border/50 pt-2.5">
                <label class="flex items-center gap-2 text-xs text-foreground">
                  <input
                    type="checkbox"
                    bind:checked={adoptComposeProject}
                    disabled={busy}
                    data-testid="stack-it-input-adopt-compose-project"
                    class="h-4 w-4 rounded border-border"
                  />
                  <span>{tr("ui.services.stackItAdoptComposeProject")}</span>
                </label>

                <label class="block space-y-1">
                  <span class="text-xs font-medium text-foreground"
                    >{tr("ui.services.stackItHttpService")}</span
                  >
                  <input
                    type="text"
                    bind:value={httpServiceText}
                    placeholder={tr("ui.services.stackItHttpServicePlaceholder")}
                    maxlength="63"
                    disabled={busy}
                    data-testid="stack-it-input-http-service"
                    class="w-full rounded-md border border-border bg-background px-2.5 py-1.5 font-mono text-xs text-foreground placeholder:text-muted-foreground focus:border-primary focus:outline-none"
                  />
                </label>
              </div>

              <div class="flex flex-wrap items-center justify-between gap-2 pt-1">
                {#if overridesDirty}
                  <p class="text-xs text-warning" role="status">
                    {tr("ui.services.stackItOverridesChanged")}
                  </p>
                {:else}
                  <span></span>
                {/if}
                <Button
                  variant="secondary"
                  size="sm"
                  onclick={prepare}
                  disabled={busy}
                  testId="replan-stack-it"
                  >{tr("ui.services.stackItReplan")}</Button
                >
              </div>
            </div>
          {/if}
        </div>
      {/if}
    {:else if mode === "restore_original"}
      <p class="text-muted-foreground">
        {tr("ui.services.restoreOriginalBody")}
      </p>
      {#if retainUntil}<p>
          {tr("ui.services.originalKeptUntil", {
            date: formatDate(retainUntil),
          })}
        </p>{/if}
    {:else}
      <p class="text-muted-foreground">
        {tr("ui.services.removeOriginalBody")}
      </p>
    {/if}

    {#if phase === "running"}<p role="status" class="text-muted-foreground">
        {tr("ui.services.adoptionChecking")}
      </p>{/if}
    {#if error}<p class="text-destructive" role="alert">{error}</p>{/if}
    {#if phase === "done"}<p role="status">
        {mode === "stack"
          ? tr("ui.services.stackItDone")
          : mode === "restore_original"
            ? tr("ui.services.originalRestored")
            : tr("ui.services.originalRemoved")}
      </p>{/if}
  </div>
  {#snippet footer()}
    <Button variant="secondary" onclick={onClose}
      >{phase === "done"
        ? tr("ui.common.close")
        : tr("ui.importExportModal.cancel")}</Button
    >
    {#if phase === "review"}<Button
        variant={mode === "release_original" ? "destructive" : "primary"}
        onclick={approve}
        disabled={busy || !approvable}
        testId="approve-stack-it"
        >{mode === "stack" ? tr("ui.services.approveStackIt") : title}</Button
      >{/if}
  {/snippet}
</Modal>
