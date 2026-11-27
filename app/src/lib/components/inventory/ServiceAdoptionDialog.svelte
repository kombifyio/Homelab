<script lang="ts">
  import { onMount } from "svelte";
  import { AlertTriangle, Info } from "@lucide/svelte";
  import { tr, type MessageKey } from "#lib/i18n.svelte.js";
  import Modal from "#lib/components/Modal.svelte";
  import Button from "#lib/components/ui/Button.svelte";
  import { getJob } from "#lib/api/jobs.js";
  import {
    prepareApplicationAdoption,
    readApplicationAdoption,
    executeApplicationAdoption,
    releaseApplicationAdoption,
    type ApplicationAdoptionNotice,
    type CanonicalService,
    type ApplicationAdoptionPlan,
  } from "#lib/api/services.js";
  let {
    service,
    release = false,
    onClose,
    onComplete,
  }: {
    service: CanonicalService;
    release?: boolean;
    onClose: () => void;
    onComplete: () => Promise<void>;
  } = $props();
  let plan = $state<ApplicationAdoptionPlan | null>(null);
  let busy = $state(false);
  let error = $state<string | null>(null);
  let phase = $state("preview");
  let jobId = $state("");
  const previewKey = crypto.randomUUID();
  const executeKey = crypto.randomUUID();
  let active = true;
  async function waitJob(id: string) {
    while (active) {
      const job = await getJob(id);
      phase = job.state;
      if (job.state === "completed") return;
      if (["failed", "canceled", "cancelled"].includes(job.state))
        throw new Error(job.error || tr("ui.services.adoptionFailed"));
      await new Promise((resolve) => setTimeout(resolve, 1500));
    }
  }
  async function prepare() {
    busy = true;
    try {
      const job = await prepareApplicationAdoption(service, previewKey);
      jobId = job.job_id || "";
      await waitJob(jobId);
      if (active) {
        plan = await readApplicationAdoption(service.id, jobId);
        phase = "review";
      }
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    } finally {
      busy = false;
    }
  }
  async function execute() {
    busy = true;
    error = null;
    try {
      const job = release
        ? await releaseApplicationAdoption(service, executeKey)
        : await executeApplicationAdoption(service, plan!, executeKey);
      jobId = job.job_id || "";
      await waitJob(jobId);
      if (active) {
        await onComplete();
        if (active) phase = "verified";
      }
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    } finally {
      busy = false;
    }
  }
  const KNOWN_NOTICE_KEYS: Record<string, MessageKey> = {
    custom_components_preserved:
      "ui.services.adoptionNotice.custom_components_preserved",
    external_recorder_preserved:
      "ui.services.adoptionNotice.external_recorder_preserved",
    image_version_drift: "ui.services.adoptionNotice.image_version_drift",
    usb_serial_devices_preserved:
      "ui.services.adoptionNotice.usb_serial_devices_preserved",
    host_network_preserved: "ui.services.adoptionNotice.host_network_preserved",
  };

  function noticeText(notice: ApplicationAdoptionNotice): string {
    const key = KNOWN_NOTICE_KEYS[notice.code];
    return key ? tr(key) : notice.message;
  }

  onMount(() => {
    if (!release) void prepare();
    else phase = "review";
    return () => {
      active = false;
    };
  });
</script>

<Modal
  title={release ? tr("ui.services.releaseBinding") : tr("ui.services.stackIt")}
  {onClose}
>
  <div class="space-y-4" data-testid="service-adoption-dialog">
    <p class="text-sm">{service.name}</p>
    <p class="text-sm text-muted-foreground">
      {release
        ? tr("ui.services.releasePreservesRuntime")
        : tr("ui.services.adoptionScope")}
    </p>
    {#if plan?.assessment}
      <dl class="grid grid-cols-[auto_1fr] gap-2 text-sm">
        <dt>{tr("ui.services.adoptionSource")}</dt>
        <dd class="break-all font-mono text-xs">
          {plan.assessment.source.containerId}
        </dd>
        <dt>{tr("ui.services.adoptionData")}</dt>
        <dd class="break-all">{plan.assessment.source.configMount}</dd>
      </dl>
      {#if plan.assessment.source.networkMode === "host" || (plan.assessment.source.preservedDevices?.length ?? 0) > 0 || plan.assessment.source.versionDrift}
        <div
          class="flex flex-wrap gap-1.5"
          data-testid="service-adoption-badges"
        >
          {#if plan.assessment.source.networkMode === "host"}
            <span
              class="inline-flex items-center gap-1 rounded-md border border-border/70 bg-muted/40 px-2 py-0.5 text-xs text-foreground"
              data-adoption-badge="host_network"
            >
              <Info class="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
              {tr("ui.services.adoptionBadgeHostNetwork")}
            </span>
          {/if}
          {#if plan.assessment.source.preservedDevices && plan.assessment.source.preservedDevices.length > 0}
            <span
              class="inline-flex items-center gap-1 rounded-md border border-border/70 bg-muted/40 px-2 py-0.5 text-xs text-foreground"
              data-adoption-badge="preserved_devices"
            >
              <Info class="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
              {tr("ui.services.adoptionBadgePreservedDevices", {
                devices: plan.assessment.source.preservedDevices.join(", "),
              })}
            </span>
          {/if}
          {#if plan.assessment.source.versionDrift}
            <span
              class="inline-flex items-center gap-1 rounded-md border border-warning/30 bg-warning/10 px-2 py-0.5 text-xs text-warning"
              data-adoption-badge="version_drift"
            >
              <AlertTriangle class="h-3.5 w-3.5 shrink-0" />
              {tr("ui.services.adoptionBadgeVersionDrift")}
            </span>
          {/if}
        </div>
      {/if}
      {#if plan.assessment.notices && plan.assessment.notices.length > 0}
        <ul
          class="space-y-2"
          data-testid="service-adoption-notices"
        >
          {#each plan.assessment.notices as notice (notice.code)}
            <li
              class="flex items-start gap-2 rounded-lg border p-2.5 text-xs {notice.severity ===
              'warning'
                ? 'border-warning/30 bg-warning/10 text-warning'
                : 'border-border/70 bg-muted/40 text-foreground'}"
              data-notice-code={notice.code}
              data-notice-severity={notice.severity}
            >
              {#if notice.severity === "warning"}
                <AlertTriangle class="mt-0.5 h-3.5 w-3.5 shrink-0" />
              {:else}
                <Info class="mt-0.5 h-3.5 w-3.5 shrink-0 text-muted-foreground" />
              {/if}
              <span>{noticeText(notice)}</span>
            </li>
          {/each}
        </ul>
      {/if}
      <p class="text-sm">{tr("ui.services.adoptionPreserves")}</p>
      <p class="text-sm text-muted-foreground">
        {tr("ui.services.adoptionOperations")}
      </p>
    {/if}
    {#if jobId}<a
        class="text-sm text-primary underline"
        href={`/jobs/${encodeURIComponent(jobId)}`}
        >{tr("ui.services.adoptionProgress")}</a
      >{/if}
    {#if busy && phase !== "verified"}<p role="status">
        {tr("ui.services.adoptionChecking")} ({phase})
      </p>{/if}
    {#if error}<p class="text-sm text-destructive" role="alert">{error}</p>{/if}
    {#if phase === "verified"}<p role="status">
        {release
          ? tr("ui.services.bindingReleased")
          : tr("ui.services.adoptionVerified")}
      </p>{/if}
  </div>
  {#snippet footer()}
    <Button variant="secondary" onclick={onClose}
      >{tr("ui.importExportModal.cancel")}</Button
    >
    {#if phase === "review" && !error}<Button
        variant="primary"
        onclick={execute}
        disabled={busy}
        testId="approve-service-adoption"
        >{release
          ? tr("ui.services.releaseBinding")
          : tr("ui.services.approveStackIt")}</Button
      >{/if}
  {/snippet}
</Modal>
