<script lang="ts">
  import { onMount } from "svelte";
  import {
    ArrowUpRight,
    Check,
    Cpu,
    HardDrive,
    MemoryStick,
    MapPin,
  } from "@lucide/svelte";
  import BrandLogoScope from "../BrandLogoScope.svelte";
  import BrandLogoIcon from "../BrandLogoIcon.svelte";
  import {
    getMonthlyRuntimeOfferings,
    type MonthlyRuntimeOffering,
  } from "#lib/api/stacks.js";
  import type { ServerFit } from "#lib/api/unifier.js";
  import {
    getIonosDatacenterChoices,
    readRuntimeOfferingID,
    type ManagedProviderID,
    type StackConfig,
  } from "#lib/wizard/index.js";
  import { managedProviders } from "#lib/wizard/managed-providers.js";
  import { tr } from "#lib/i18n.svelte.js";
  import { useCreationContext } from "#lib/creation/context.js";
  import { NODE_GUIDES } from "#lib/docs-links.js";
  import InfoTip from "./InfoTip.svelte";
  import ManagedCreationPauseNotice from "./ManagedCreationPauseNotice.svelte";
  import {
    managedCreationPauses,
    managedCreationScope,
  } from "#lib/creation/creation-pause.svelte.js";

  let {
    config = $bindable(),
    providers,
    onselect,
    serverFit,
  }: {
    config: StackConfig;
    providers: ReadonlyArray<(typeof managedProviders)[number]>;
    onselect: (provider: ManagedProviderID) => void;
    serverFit?: ServerFit;
  } = $props();
  const creation = useCreationContext();
  const fitEnabled = $derived(
    creation.features.isEnabled("server_hardware_fit"),
  );
  let offerings = $state<MonthlyRuntimeOffering[]>([]);
  let loading = $state(true);
  let loadFailed = $state(false);
  const provider = $derived(
    providers.find((item) => item.value === config.providerId),
  );
  const availableOfferings = $derived(
    offerings.filter((item) =>
      config.providerId
        ? Boolean(item.provider_packages?.[config.providerId])
        : true,
    ),
  );
  const offering = $derived(
    offerings.find((item) => item.id === config.runtimeOfferingId),
  );
  // The server this provider provisions for the offering. The generic row of
  // the offering can say more (or less) than the provider sells, so it is never
  // shown as the size: without the package no size is shown.
  const providerPackage = $derived(
    config.providerId ? offering?.provider_packages?.[config.providerId] : undefined,
  );
  const matchingFit = $derived(
    serverFit &&
      serverFit.provider_id === config.providerId &&
      serverFit.offering_id === config.runtimeOfferingId &&
      serverFit.specs?.memory_mb > 0
      ? serverFit
      : undefined,
  );
  const exactSpecs = $derived({
    sku:
      matchingFit?.sku ||
      matchingFit?.template ||
      providerPackage?.sku ||
      providerPackage?.template,
    vcpus: matchingFit?.specs.cpu_cores ?? providerPackage?.vcpus,
    memory_mb: matchingFit?.specs.memory_mb ?? providerPackage?.memory_mb,
    disk_gb: matchingFit?.specs.disk_gb ?? providerPackage?.disk_gb,
  });
  const datacenters = getIonosDatacenterChoices();
  const region = $derived(
    matchingFit?.location ||
      (config.providerId === "ionos"
        ? datacenters.find((item) => item.value === config.ionosDatacenter)?.label ||
          providerPackage?.region
        : providerPackage?.region),
  );

  const showsSpecs = $derived(
    Boolean(
      exactSpecs.sku ||
        exactSpecs.vcpus ||
        exactSpecs.memory_mb ||
        exactSpecs.disk_gb ||
        region,
    ),
  );

  const pauseScope = $derived(managedCreationScope(config));
  const pause = $derived(managedCreationPauses.get(pauseScope));

  async function loadOfferings() {
    loading = true;
    loadFailed = false;
    try {
      offerings = await getMonthlyRuntimeOfferings();
    } catch {
      loadFailed = true;
    } finally {
      loading = false;
    }
  }
  onMount(() => {
    void loadOfferings();
  });
</script>

<div class="managed-options" data-testid="managed-provider-selector">
  {#if pause && pause.state !== "ready" && pauseScope}
    <ManagedCreationPauseNotice
      {pause}
      provider={provider?.label ?? pause.provider_id}
      onrefresh={() => managedCreationPauses.refresh(pauseScope)}
    />
  {/if}
  {#if fitEnabled && !loading && !loadFailed}
    <label class="block space-y-1 text-sm">
      <span>{tr("layout.expansion.size")}</span>
      <select class="w-full rounded-md border border-border bg-background p-2" value={config.runtimeOfferingId}
        onchange={(event) => { config.runtimeOfferingId = readRuntimeOfferingID(event.currentTarget.value); }}>
        {#if !availableOfferings.some((item) => item.id === config.runtimeOfferingId)}
          <option value={config.runtimeOfferingId}>{config.runtimeOfferingId}</option>
        {/if}
        {#each availableOfferings as item (item.id)}
          {@const pkg = config.providerId ? item.provider_packages?.[config.providerId] : undefined}
          <option value={item.id}>
            {item.name}{pkg?.sku ? ` · ${pkg.sku}` : ""}{item.id !== "monthly-runtime-standard" ? ` · ${tr("wizard.server.managed.requiresAllYouNeed")}` : ""}
          </option>
        {/each}
      </select>
      <span class="block text-muted-foreground">{tr("layout.expansion.entitlement")}</span>
      {#if serverFit?.refusal_class === "entitlement" || serverFit?.advisories?.some((item) => item.code === "entitlement_required")}
        <span class="block text-destructive" data-testid="managed-offering-entitlement-required">
          {tr("wizard.server.managed.entitlementRequiredNote")}
        </span>
      {/if}
    </label>
  {/if}
  <section class="server-summary">
    <header class="server-identity">
      {#if provider}
        <span class="provider-logo"
          ><BrandLogoScope domain={provider.domain}
            ><BrandLogoIcon
              class="h-9 w-9"
              fallbackLabel={provider.label}
            /></BrandLogoScope
          ></span
        >
      {/if}
      <div>
        <h4>
          {offering?.name || config.runtimeOfferingId}
        </h4>
        {#if exactSpecs.sku}
          <p class="sku-label" data-testid="managed-server-sku">{exactSpecs.sku}</p>
        {/if}
        {#if provider}<a
            href={provider.website}
            target="_blank"
            rel="noopener noreferrer"
            >{tr("wizard.server.managed.poweredBy")}
            {provider.label}<ArrowUpRight size={13} aria-hidden="true" /></a
          >{/if}
      </div>
      {#if !fitEnabled}
        <span class="included"><Check size={13} aria-hidden="true" />{tr("wizard.server.managed.available")}</span>
      {/if}
    </header>
    <p class="description">{tr("wizard.server.cloud.info")}</p>
    {#if loading}
      <p class="loading" role="status">
        {tr("wizard.server.managed.loadingSpecs")}
      </p>
    {:else if showsSpecs}
      <dl class="server-specs" data-testid="managed-server-specs">
        {#if exactSpecs.vcpus}<div>
            <dt><Cpu size={15} aria-hidden="true" />vCPU</dt>
            <dd>{exactSpecs.vcpus}</dd>
          </div>{/if}
        {#if exactSpecs.memory_mb}<div>
            <dt><MemoryStick size={15} aria-hidden="true" />RAM</dt>
            <dd>
              {tr("ui.managed.sizeGiB", {
                size: exactSpecs.memory_mb / 1024,
              })}
            </dd>
          </div>{/if}
        {#if exactSpecs.disk_gb}<div>
            <dt>
              <HardDrive size={15} aria-hidden="true" />{tr(
                "wizard.server.hypervisor.disk",
              )}
            </dt>
            <dd>{tr("ui.managed.sizeGB", { size: exactSpecs.disk_gb })}</dd>
          </div>{/if}
        {#if region}<div>
            <dt>
              <MapPin size={15} aria-hidden="true" />{tr(
                "wizard.server.managed.location",
              )}
            </dt>
            <dd>{region}</dd>
          </div>{/if}
      </dl>
    {:else if loadFailed}
      <p class="loading" role="status">
        {tr("wizard.server.managed.specsUnavailable")}
        <button type="button" onclick={() => void loadOfferings()}
          >{tr("wizard.server.managed.retry")}</button
        >
      </p>
    {/if}
  </section>

  <section class="provider-settings">
    <div class="provider-content">
      <div class="provider-copy">
        <h4 class="tip-heading">
          {tr("wizard.server.managed.provider")}<InfoTip
            topic={tr("wizard.server.managed.provider")}
            text={tr("wizard.tip.managedProvider")}
            docs={NODE_GUIDES.managedProviders}
          />
        </h4>
        <p>
          {tr(
            providers.length > 1
              ? "wizard.server.managed.providerChoice"
              : "wizard.server.managed.providerAssigned",
          )}
        </p>
      </div>
      <fieldset class="providers">
        <legend class="sr-only">{tr("wizard.server.managed.provider")}</legend>
        {#each managedProviders as item (item.value)}
          {@const available = providers.some(
            (provider) => provider.value === item.value,
          )}
          <label
            data-testid={`managed-provider-${item.value}`}
            class:unavailable={!available}
          >
            <input
              type="radio"
              name="managed-provider"
              value={item.value}
              checked={available && config.providerId === item.value}
              disabled={!available}
              onchange={() => onselect(item.value)}
            />
            <BrandLogoScope domain={item.domain}
              ><BrandLogoIcon
                class="h-6 w-6"
                fallbackLabel={item.label}
              /></BrandLogoScope
            >
            <span
              ><strong>{item.label}</strong>
              {#if !available}<small
                  >{tr("wizard.server.managed.providerUnavailable")}</small
                >{/if}
            </span>
          </label>
        {/each}
      </fieldset>
      <p class="scope-note">
        {tr("wizard.server.managed.moreOptionsPending")}
      </p>
    </div>
  </section>
</div>

<style>
  .managed-options {
    display: grid;
    gap: 28px;
    padding-block: 4px 8px;
  }
  .server-summary {
    display: grid;
    gap: 18px;
  }
  .server-identity {
    display: flex;
    gap: 14px;
    flex-wrap: wrap;
    align-items: center;
  }
  .provider-logo {
    display: grid;
    place-items: center;
    width: 42px;
    height: 42px;
  }
  .tip-heading {
    display: flex;
    align-items: center;
    gap: 2px;
  }
  h4 {
    font-size: 17px;
    font-weight: 550;
    letter-spacing: -0.02em;
  }
  .sku-label {
    font-size: 12px;
    font-weight: 550;
    color: var(--foreground);
    margin-top: 2px;
  }
  .server-identity a {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    font-size: 12px;
    color: var(--muted-foreground);
    margin-top: 4px;
    text-underline-offset: 3px;
  }
  .server-identity a:hover {
    color: var(--foreground);
    text-decoration: underline;
  }
  .included {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    margin-inline-start: auto;
    font-size: 11px;
    color: var(--primary);
  }
  .description,
  .provider-content p {
    font-size: 13px;
    line-height: 1.65;
    color: var(--muted-foreground);
    max-width: 70ch;
  }
  .server-specs {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(130px, 1fr));
    border-block: 1px solid var(--border);
  }
  .server-specs div {
    position: relative;
    padding: 17px 20px 17px 0;
  }
  .server-specs div + div {
    padding-inline-start: 20px;
  }
  .server-specs div + div::before {
    content: "";
    position: absolute;
    inset-block: 15px;
    inset-inline-start: 0;
    width: 1px;
    background: var(--border);
  }
  dt {
    display: flex;
    align-items: center;
    gap: 8px;
    color: var(--muted-foreground);
    font-size: 11px;
  }
  dd {
    font-size: 18px;
    font-weight: 550;
    margin-top: 8px;
  }
  .provider-settings {
    display: grid;
    gap: 22px;
  }
  .provider-content {
    display: grid;
    gap: 20px;
  }
  .provider-copy h4 {
    font-size: 14px;
  }
  .provider-copy p {
    margin-top: 5px;
  }
  .providers {
    display: flex;
    flex-wrap: wrap;
    gap: 22px;
    border-bottom: 1px solid var(--border);
  }
  .providers label {
    display: flex;
    align-items: center;
    gap: 9px;
    min-height: 42px;
    padding: 0 2px 10px;
    color: var(--muted-foreground);
    cursor: pointer;
  }
  .providers input {
    accent-color: var(--primary);
  }
  .providers label:has(input:checked) {
    color: var(--foreground);
  }
  .providers label.unavailable {
    cursor: not-allowed;
  }
  .providers small {
    display: block;
    font-size: 11px;
    margin-top: 3px;
  }
  .providers strong {
    font-size: 13px;
  }
  .providers label > :global(svg:last-child) {
    color: var(--primary);
  }
  .scope-note {
    padding-top: 2px;
  }
  .loading {
    color: var(--muted-foreground);
    font-size: 12px;
    padding-block: 12px;
  }
  .loading button {
    color: var(--primary);
    text-decoration: underline;
    text-underline-offset: 3px;
  }
  button:focus-visible,
  a:focus-visible {
    outline: 2px solid var(--ring);
    outline-offset: 4px;
  }
  .providers input:focus-visible {
    outline: 2px solid var(--ring);
    outline-offset: 4px;
  }
  @container (max-width: 660px) {
    .included {
      width: 100%;
      margin-inline-start: 56px;
    }
    .server-specs {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
    .server-specs div:nth-child(odd) {
      padding-inline-start: 0;
    }
    .server-specs div:nth-child(odd)::before {
      display: none;
    }
    .server-specs div:nth-child(n + 3) {
      border-top: 1px solid var(--border);
    }
  }
</style>
