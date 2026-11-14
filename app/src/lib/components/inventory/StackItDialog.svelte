<!--
  "Stack it": rebuild one unmanaged container as an own application. The
  dialog plans first (read-only), shows the reviewed rows, and approves
  exactly that plan digest. Restore and remove act on the kept original.
-->
<script lang="ts">
  import { onMount } from "svelte";
  import { tr, formatDate } from "#lib/i18n.svelte.js";
  import Modal from "#lib/components/Modal.svelte";
  import Button from "#lib/components/ui/Button.svelte";
  import { getJob } from "#lib/api/jobs.js";
  import {
    prepareApplicationAdoption,
    readApplicationAdoption,
    executeApplicationAdoption,
    resolveStackedOriginal,
    type CanonicalService,
    type ApplicationAdoptionPlan,
    type StackItPlan,
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

  let plan = $state<ApplicationAdoptionPlan | null>(null);
  let busy = $state(false);
  let error = $state<string | null>(null);
  let phase = $state<"planning" | "review" | "running" | "done">("planning");
  const previewKey = crypto.randomUUID();
  const executeKey = crypto.randomUUID();
  let active = true;

  const stackPlan = $derived<StackItPlan | undefined>(plan?.stack_plan);
  const refused = $derived(
    stackPlan?.rows.filter((row) => row.status === "refused") ?? [],
  );
  const approvable = $derived(
    mode !== "stack" ||
      (stackPlan?.outcome === "planned" &&
        stackPlan.admissible &&
        Boolean(stackPlan.planDigest)),
  );
  const retainUntil = $derived(service.adoption?.stack_it?.retain_until);

  const title = $derived(
    mode === "restore_original"
      ? tr("ui.services.restoreOriginal")
      : mode === "release_original"
        ? tr("ui.services.removeOriginal")
        : tr("ui.services.stackIt"),
  );

  const size = byteSize;

  function port(p: StackItPlan["publishedPorts"][number]): string {
    const host = p.hostIP ? `${p.hostIP}:` : "*:";
    return `${host}${p.hostPort} → ${p.containerPort}/${p.protocol}`;
  }

  async function waitJob(id: string) {
    while (active) {
      const job = await getJob(id);
      if (job.state === "completed") return;
      if (["failed", "canceled", "cancelled"].includes(job.state))
        throw stackItJobError(job.result);
      await new Promise((resolve) => setTimeout(resolve, 1500));
    }
  }

  async function prepare() {
    busy = true;
    try {
      const job = await prepareApplicationAdoption(
        service,
        previewKey,
        "stack_it",
      );
      await waitJob(job.job_id || "");
      if (active) {
        plan = await readApplicationAdoption(service.id, job.job_id || "");
        phase = "review";
      }
    } catch (e) {
      error = stackItFailure(e);
    } finally {
      busy = false;
    }
  }

  async function approve() {
    busy = true;
    error = null;
    phase = "running";
    try {
      const job =
        mode === "stack"
          ? await executeApplicationAdoption(service, plan!, executeKey)
          : await resolveStackedOriginal(service, mode, executeKey);
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

  onMount(() => {
    if (mode === "stack") void prepare();
    else phase = "review";
    return () => {
      active = false;
    };
  });
</script>

<Modal {title} {onClose} maxWidth="xl" testId="stack-it-dialog">
  <div class="space-y-4 text-sm">
    <p class="font-medium text-foreground">{service.name}</p>

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
                >{tr(`ui.services.stackItRow.${row.status}`)}</span
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
