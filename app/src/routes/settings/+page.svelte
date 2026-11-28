<script lang="ts">
  import { goto } from "$app/navigation";
  import { page } from "$app/state";
  import { onMount } from "svelte";
  import {
    getAvailableLocales,
    getLocale,
    isLocale,
    setLocale,
    stateLabel,
    tr,
    trParts,
  } from "#lib/i18n.svelte.js";
  import {
    defaultIdentity,
    StackIdentityDisplay,
    StackIdentityEditor,
    stackIdentityEditorLabels,
    stackIdentityLabelFor,
  } from "#lib/components/open-core/index.js";
  import { NotificationPreferences } from "#lib/components/open-core/index.js";
  import CompanionSettingsPanel from "#lib/components/settings/CompanionSettingsPanel.svelte";
  import AiProviderSettingsPanel from "#lib/components/settings/AiProviderSettingsPanel.svelte";
  import { hasLocalAiBridge } from "#lib/client/local-ai-bridge.js";
  import { features } from "#lib/stores/features.js";
  import {
    createCompanionAdapter,
    hasCompanionBridge,
    probeCompanion,
  } from "#lib/client/companion-bridge.js";
  import { parseApiError } from "#lib/api/errors.js";
  import {
    applyOrphanCleanup,
    destroyStack,
    listKitDeployments,
    offlineServersFromDestroyError,
    planOrphanCleanup,
    type KitDeployment,
    type OfflineStackServer,
  } from "#lib/api/stacks.js";
  import { authStore } from "#lib/stores/auth.svelte.js";
  import { requestIntroductionStart } from "#lib/onboarding/introduction.js";
  import SetupReplayDialog from "#lib/components/onboarding/SetupReplayDialog.svelte";
  import { theme, type Theme } from "#lib/stores/theme.js";
  import {
    dashboardPreset,
    dashboardOwnerKey,
  } from "#lib/stores/dashboardPreset.svelte.js";
  import {
    DASHBOARD_PRESETS,
    isPresetAvailable,
    type DashboardPresetId,
  } from "#lib/dashboard/presets.js";
  import {
    FINISHES,
    NAV_FLYOUT_PREFERENCES,
    type Finish,
  } from "@kombiverselabs/design/contract";
  import {
    OptionTiles,
    RecipeSwatch,
    SettingsRow,
    SettingsSection,
    SettingsShell,
    type SettingsCategory,
    type SettingsTab,
  } from "@kombiverselabs/ui/settings";
  import { SegmentedControl, Select } from "@kombiverselabs/ui/primitives";
  import {
    resolveSettingsTab,
    settingsTabHref,
    type SettingsTabSlug,
  } from "#lib/settings/tabs.js";
  import Button from "#lib/components/ui/Button.svelte";
  import ErrorCallout from "#lib/components/ui/ErrorCallout.svelte";
  import Modal from "#lib/components/Modal.svelte";
  import Switch from "#lib/components/ui/Switch.svelte";
  import HomelabSettingsPanel from "#lib/components/settings/HomelabSettingsPanel.svelte";
  import StackImportExportModal from "#lib/components/StackImportExportModal.svelte";
  import { toast } from "#lib/stores/toast.js";
  import {
    shortcutsStore,
    shortcutsEnabled,
  } from "#lib/stores/shortcuts.js";
  import {
    hydrateStackIdentityFromBackend,
    saveStackIdentityToBackend,
    type StackIdentity,
  } from "#lib/stores/stackIdentity.js";

  // The Companion tab exists only in the Windows client while it can show the desktop Companion:
  // its native side answers the Companion bridge. A browser and the hosted web app never get it.
  // "idle": not asked yet, or an aborted probe that has to be asked again.
  let companionProbe = $state<"no" | "idle" | "probing" | "yes">("no");
  const companionRequested = $derived(
    page.url.searchParams.get("tab")?.trim().toLowerCase() === "companion",
  );
  const companionTab = $derived(
    companionProbe === "yes" ||
      ((companionProbe === "probing" || companionProbe === "idle") &&
        companionRequested),
  );
  const companionAdapter = createCompanionAdapter();
  // AI providers: the Windows client only (its shell holds the shared local AI runtimes), behind
  // the flag `local_ai_providers` (off by default).
  const allFeatures = features.allFeatures;
  const aiProvidersTab = $derived(
    hasLocalAiBridge() &&
      ($allFeatures.get("local_ai_providers")?.enabled ?? false),
  );
  const activeTab = $derived(
    resolveSettingsTab(
      page.url.searchParams.get("tab"),
      companionTab,
      aiProvidersTab,
    ),
  );
  // Two headings in the subpanel (KOMBIFY-DESIGN-SYSTEM-STANDARD §10.2.3): what belongs to this
  // person and device, and what belongs to their homelab.
  const settingsCategories: readonly SettingsCategory[] = $derived([
    { id: "general", label: tr("ui.settings.categoryGeneral") },
    { id: "homelab", label: tr("ui.settings.categoryHomelab") },
  ]);
  const settingsTabs: readonly SettingsTab[] = $derived([
    {
      slug: "account",
      label: tr("ui.settings.account"),
      scope: "shared",
      category: "general",
    },
    {
      slug: "identity",
      label: tr("ui.settings.homelabIdentity"),
      scope: "shared",
      category: "homelab",
    },
    {
      slug: "design",
      label: tr("ui.settings.lookAndLanguage"),
      scope: "shared",
      category: "general",
    },
    ...(companionTab
      ? ([
          {
            slug: "companion",
            label: tr("ui.settings.companion"),
            scope: "shared",
            category: "general",
          },
        ] satisfies SettingsTab[])
      : []),
    {
      slug: "notifications",
      label: tr("nav.notifications"),
      scope: "shared",
      category: "general",
    },
    ...(aiProvidersTab
      ? ([
          {
            slug: "ai-providers",
            label: tr("settings.aiProviders.title"),
            scope: "tool",
            category: "general",
          },
        ] satisfies SettingsTab[])
      : []),
    {
      slug: "homelab",
      label: tr("wizard.settings.title"),
      scope: "tool",
      category: "homelab",
    },
  ]);
  const hrefForTab = (slug: string) => settingsTabHref(slug as SettingsTabSlug);

  let replaySetup = $state(false);
  function continueSetupReplay() {
    replaySetup = false;
    requestIntroductionStart({ tourOnly: true });
  }

  // Appearance card state. `deviceFinish` mirrors whether this device holds
  // an explicit finish choice (section-4 tier 2); picking a finish sets it,
  // "Follow account" clears it and the account default (tier 3) shows again.
  let deviceFinishChosen = $state(false);
  const APPEARANCE_MODES: readonly Theme[] = ["dark", "light", "system"];
  const themePreference = theme.preference;
  const resolvedAppearance = theme.resolved;
  // The finish swatches render the real package recipes in miniature, so
  // they need the RESOLVED appearance the page is showing, not the `system`
  // preference — the same resolution the theme store applies to the root.
  const swatchAppearance = $derived($resolvedAppearance);

  function pickFinish(finish: Finish) {
    theme.setFinish(finish);
    deviceFinishChosen = true;
  }

  function followAccount() {
    theme.followAccountFinish();
    deviceFinishChosen = false;
  }

  let stackIdentityLoading = $state(false);
  let stackIdentityLoaded = $state(false);
  let stackIdentityEditable = $state(false);
  let stackIdentityError = $state("");
  let stackIdentityValue = $state<StackIdentity | null>(null);

  // Deployment deletion lives here, not on the dashboard: it is a deliberate,
  // confirmed action rather than a control the operator meets on every visit.
  // The homelab name: generated as "homelab" at creation time, so the owner
  // needs a way to set the name they actually use. It titles the dashboard.
  // The API distinguishes two states the card must not conflate: a 404 (no
  // homelab provisioned at all) and a legal `homelab: null` alongside existing
  // deployments (pre-backfill lanes, PocketBase-only self-host).

  let deployments = $state<KitDeployment[]>([]);
  /** Import or export of a stack specification, opened from its settings row. */
  let transfer = $state<{ mode: "import" | "export"; deploymentId?: string } | null>(null);
  let deploymentsLoading = $state(false);
  let deploymentsError = $state<string | null>(null);
  let deleteCandidate = $state<KitDeployment | null>(null);
  let deleteConfirmName = $state("");
  let deletingDeployment = $state(false);
  let deleteError = $state<string | null>(null);
  // Servers of the stack that stopped answering; the owner confirms them gone
  // in a second step, after the API asked for it.
  let offlineServers = $state<OfflineStackServer[]>([]);
  const deleteConfirmed = $derived(
    deleteCandidate !== null &&
      deleteConfirmName.trim() === (deleteCandidate?.name ?? "").trim(),
  );

  const fallbackIdentity = $derived({
    name: "",
    characterId: defaultIdentity.characterId,
    animationStyle: defaultIdentity.animationStyle,
    savedAt: null,
    animationEnabled: defaultIdentity.animationEnabled,
    iconStyle: defaultIdentity.iconStyle,
    glowColorOverride: defaultIdentity.glowColorOverride,
  });

  async function loadStackIdentitySettings() {
    stackIdentityLoading = true;
    stackIdentityError = "";

    try {
      const response = await hydrateStackIdentityFromBackend();
      stackIdentityValue = response?.stack_identity ?? null;
      stackIdentityEditable = response?.editable ?? false;
      stackIdentityLoaded = true;
    } catch (err) {
      stackIdentityError =
        err instanceof Error ? err.message : tr("ui.settings.failedToLoadHomelabIdentity");
    } finally {
      stackIdentityLoading = false;
    }
  }

  async function handleIdentitySave(updated: StackIdentity) {
    stackIdentityError = "";

    try {
      const response = await saveStackIdentityToBackend(updated);
      stackIdentityValue = response.stack_identity ?? updated;
      stackIdentityEditable = response.editable;
      toast.success(tr("settings.saved"));
    } catch (err) {
      stackIdentityError =
        err instanceof Error ? err.message : tr("ui.settings.failedToSaveHomelabIdentity");
    }
  }

  async function loadDeployments() {
    deploymentsLoading = true;
    deploymentsError = null;
    try {
      deployments = await listKitDeployments();
    } catch (err) {
      deploymentsError =
        parseApiError(err).message || tr("ui.settings.failedToLoadDeployments");
      deployments = [];
    } finally {
      deploymentsLoading = false;
    }
  }

  function openDeleteConfirm(deployment: KitDeployment) {
    if (deployment.demo_anchor) return;
    deleteCandidate = deployment;
    deleteConfirmName = "";
    deleteError = null;
    offlineServers = [];
  }

  function cancelDelete() {
    if (deletingDeployment) return;
    deleteCandidate = null;
    deleteConfirmName = "";
    deleteError = null;
    offlineServers = [];
  }

  async function confirmDelete() {
    const target = deleteCandidate;
    if (!target || deletingDeployment || !deleteConfirmed) return;
    deletingDeployment = true;
    deleteError = null;
    try {
      if (target.legacy) {
        const plan = await planOrphanCleanup(target.id);
        if (plan.candidates.length === 0) {
          throw new Error(
            plan.warnings?.[0] ||
              tr("ui.settings.thisEntryIsNotVerified"),
          );
        }
        await applyOrphanCleanup(plan.digest, target.id);
      } else {
        await destroyStack(target.id, {
          confirmServerIds: offlineServers.map((server) => server.id),
        });
      }
      const removedName = target.name;
      deleteCandidate = null;
      deleteConfirmName = "";
      offlineServers = [];
      toast.success(tr("settings.homelabRemoved", { name: removedName }));
      await loadDeployments();
    } catch (err) {
      const parsed = parseApiError(err);
      const gone = offlineServersFromDestroyError(parsed.details);
      if (gone.length > 0) {
        // Nothing was removed: show the offline servers (again, when one more
        // went silent after the first confirmation) and let the owner say they
        // are gone.
        offlineServers = gone;
        deleteError = null;
      } else {
        // A confirmed server that reported again is no longer offline: the
        // stack is removed through its node, so the first step starts over.
        offlineServers = [];
        deleteError = parsed.message || tr("ui.settings.deletionFailed");
      }
    } finally {
      deletingDeployment = false;
    }
  }

  onMount(() => {
    deviceFinishChosen = theme.hasDeviceFinish();
    void loadStackIdentitySettings();
    void loadDeployments();
  });

  // The probe asks the shell once. An aborted probe (a tab switch while it was in flight) is never
  // "unavailable": it goes back to idle and is asked again.
  let leaving = false;
  const probing = new AbortController();
  function runCompanionProbe() {
    if (leaving || !hasCompanionBridge()) return;
    companionProbe = "probing";
    void probeCompanion(probing.signal).then((available) => {
      if (leaving) return;
      companionProbe = available === null ? "idle" : available ? "yes" : "no";
    });
  }
  onMount(() => {
    if (hasCompanionBridge()) companionProbe = "idle";
    return () => {
      leaving = true;
      probing.abort();
    };
  });
  $effect(() => {
    if (companionProbe === "idle") runCompanionProbe();
  });

  const finishValue = theme.finish;
  const navFlyoutValue = theme.navFlyout;
  // "Follow account" is the first finish tile: chosen while this device holds no finish of its own.
  const FOLLOW_ACCOUNT = "account";
  const finishOptions = $derived([
    { value: FOLLOW_ACCOUNT, label: tr("ui.settings.followAccount") },
    ...FINISHES.map((finish) => ({ value: finish, label: finish })),
  ]);
  const modeItems = $derived([
    { value: "inherit", label: tr("appearance.inherit") },
    ...APPEARANCE_MODES.map((mode) => ({
      value: mode,
      label: tr(`appearance.${mode}`),
    })),
  ]);
  const dashboardOptions = $derived(
    DASHBOARD_PRESETS.filter((preset) =>
      isPresetAvailable(preset.id, { ownerKey: dashboardOwnerKey() }),
    ).map((preset) => ({ value: preset.id, label: preset.label })),
  );
  const languageOptions = $derived(
    getAvailableLocales().map((locale) => ({
      value: locale.code,
      label: locale.name,
    })),
  );
  function pickMode(value: string) {
    if (value === "inherit") theme.followDefaultTheme();
    else theme.set(value as Theme);
  }
  function pickFinishTile(value: string) {
    if (value === FOLLOW_ACCOUNT) followAccount();
    else pickFinish(value as Finish);
  }
  const navFlyoutLabels: Record<
    (typeof NAV_FLYOUT_PREFERENCES)[number],
    string
  > = $derived({
    auto: tr("ui.settings.followFinish"),
    flag: tr("ui.settings.flag"),
    overlap: tr("ui.settings.overlap"),
    tether: tr("ui.settings.tether"),
    float: tr("ui.settings.float"),
  });
</script>

{#if replaySetup}
  <SetupReplayDialog
    onContinue={continueSetupReplay}
    onDismiss={() => (replaySetup = false)}
  />
{/if}

<div class="mx-auto max-w-4xl p-4 md:p-8" data-testid="settings-page">
  <SettingsShell
    title={tr("ui.settings.settings")}
    tabs={settingsTabs}
    categories={settingsCategories}
    current={activeTab}
    hrefFor={hrefForTab}
    tabsLabel={tr("ui.settings.settings")}
  >
    <div class="space-y-6">
      {#if activeTab === "account"}
    <SettingsSection
      id="account"
      title={tr("ui.settings.account")}
      lead={authStore.userEmail || tr("ui.settings.signedIn")}
    >
      <SettingsRow label={tr("setupReplay.restart")}>
        <Button
          variant="secondary"
          testId="settings-repeat-setup"
          onclick={() => (replaySetup = true)}
        >
          {tr("ui.clientOnboarding.start")}
        </Button>
      </SettingsRow>
      <SettingsRow label={tr("introduction.restart")}>
        <Button
          variant="secondary"
          testId="settings-replay-introduction"
          onclick={() => requestIntroductionStart({ tourOnly: true })}
        >
          {tr("ui.clientOnboarding.start")}
        </Button>
      </SettingsRow>
    </SettingsSection>

    <SettingsSection id="keyboard" title={tr("shortcuts.settingsTitle")}>
      <SettingsRow
        label={tr("shortcuts.switchLabel")}
        for="settings-shortcuts-switch"
      >
        <Switch
          id="settings-shortcuts-switch"
          testId="settings-shortcuts-switch"
          checked={$shortcutsEnabled}
          onchange={(next) => shortcutsStore.setEnabled(next)}
          onText={tr("switch.on")}
          offText={tr("switch.off")}
        />
      </SettingsRow>
    </SettingsSection>
      {:else if activeTab === "identity"}
    <SettingsSection
      id="identity"
      title={tr("ui.settings.homelabIdentity")}
    >
      <div>
        {#if stackIdentityLoading}
          <p class="text-sm text-muted-foreground">{tr("ui.login.loading")}</p>
        {:else if stackIdentityEditable && stackIdentityLoaded}
          <StackIdentityEditor
            identity={stackIdentityValue ?? fallbackIdentity}
            labels={stackIdentityEditorLabels()}
            labelFor={stackIdentityLabelFor}
            locale={getLocale()}
            onSave={handleIdentitySave}
          />
        {:else if stackIdentityLoaded && stackIdentityValue}
          <StackIdentityDisplay
            identity={stackIdentityValue}
            labelFor={stackIdentityLabelFor}
          />
          <p
            class="mt-4 rounded-lg border border-border bg-muted/40 p-3 text-sm text-muted-foreground"
          >
            {tr("ui.settings.managedByKombifyCloud")}
          </p>
        {:else if stackIdentityLoaded}
          <p class="text-sm text-muted-foreground">
            {tr("ui.settings.noHomelabIdentityConfigured")}
          </p>
        {/if}

        {#if stackIdentityError}
          <ErrorCallout class="mt-3" message={stackIdentityError} />
        {/if}
      </div>
    </SettingsSection>

      {:else if activeTab === "design"}
    <SettingsSection
      id="appearance"
      class="scroll-mt-6"
      title={tr("ui.settings.lookAndLanguage")}
    >
      <SettingsRow label={tr("ui.settings.mode")}>
        <SegmentedControl
          label={tr("ui.settings.appearanceMode")}
          items={modeItems}
          value={$themePreference ?? "inherit"}
          onchange={pickMode}
        />
      </SettingsRow>
      <SettingsRow label={tr("ui.settings.surface")} layout="stack">
        <OptionTiles
          label={tr("ui.settings.surfaceFinish")}
          name="settings-finish"
          columns={4}
          value={deviceFinishChosen ? $finishValue : FOLLOW_ACCOUNT}
          options={finishOptions}
          onchange={pickFinishTile}
        >
          {#snippet tile(option)}
            <!-- A miniature of the finish itself: the package's real plate and
                 selection recipes at a quarter scale (KOMBIFY-DESIGN-SYSTEM-STANDARD §6). -->
            <RecipeSwatch
              finish={option.value === FOLLOW_ACCOUNT
                ? $finishValue
                : (option.value as Finish)}
              appearance={swatchAppearance}
              size="sm"
            />
          {/snippet}
        </OptionTiles>
      </SettingsRow>
      <SettingsRow label={tr("ui.settings.navigationPreview")}>
        <Select
          label={tr("ui.settings.navigationPreviewShape")}
          options={NAV_FLYOUT_PREFERENCES.map((shape) => ({
            value: shape,
            label: navFlyoutLabels[shape],
          }))}
          value={$navFlyoutValue}
          onchange={(shape) =>
            theme.setNavFlyout(
              shape as (typeof NAV_FLYOUT_PREFERENCES)[number],
            )}
        />
      </SettingsRow>
      <SettingsRow label={tr("ui.settings.dashboard")}>
        <Select
          label={tr("ui.settings.dashboardLayout")}
          options={dashboardOptions}
          value={dashboardPreset.current}
          onchange={(preset) =>
            dashboardPreset.set(preset as DashboardPresetId)}
        />
      </SettingsRow>
      <SettingsRow label={tr("settings.language")}>
        <Select
          label={tr("settings.language")}
          options={languageOptions}
          value={getLocale()}
          onchange={(code) => {
            if (isLocale(code)) setLocale(code);
          }}
        />
      </SettingsRow>
    </SettingsSection>

      {:else if activeTab === "companion"}
    {#if companionTab}
      <CompanionSettingsPanel adapter={companionAdapter} />
    {/if}

      {:else if activeTab === "notifications"}
    <section id="notification-preferences">
      <div>
        <NotificationPreferences
          apiBase="/api/v1/notifications"
          locale={getLocale()}
        />
      </div>
    </section>

      {:else if activeTab === "ai-providers"}
    <AiProviderSettingsPanel />
      {:else if activeTab === "homelab"}
    <SettingsSection id="homelab-settings" title={tr("wizard.settings.title")}>
      <HomelabSettingsPanel />
    </SettingsSection>

    <SettingsSection id="homelab-transfer" title={tr("ui.common.importExport")}>
      <div data-testid="stack-transfer-settings">
        {#each deployments as deployment (deployment.id)}
          <SettingsRow label={deployment.name}>
            <Button
              variant="secondary"
              testId="stack-export-button"
              onclick={() => (transfer = { mode: "export", deploymentId: deployment.id })}
            >
              {tr("ui.homelab.export")}
            </Button>
          </SettingsRow>
        {/each}
        <SettingsRow label={tr("ui.homelab.importSpec")}>
          <Button
            variant="secondary"
            testId="stack-import-button"
            onclick={() => (transfer = { mode: "import" })}
          >
            {tr("ui.importExportModal.import")}
          </Button>
        </SettingsRow>
      </div>
    </SettingsSection>

    <SettingsSection
      id="danger-zone"
      title={tr("ui.settings.dangerZone")}
      tone="danger"
    >
      <div data-testid="settings-delete-deployment-card">
        {#if deploymentsLoading}
          <p class="text-sm text-muted-foreground">
            {tr("ui.settings.loadingDeployments")}
          </p>
        {:else if deploymentsError}
          <ErrorCallout
            message={deploymentsError}
            onRetry={() => void loadDeployments()}
          />
        {:else if deployments.length === 0}
          <p class="text-sm text-muted-foreground">
            {tr("ui.settings.noDeploymentsToDelete")}
          </p>
        {:else}
          {#each deployments as deployment (deployment.id)}
            <SettingsRow
              label={deployment.name}
              description={stateLabel(
                deployment.state || deployment.status || "unknown",
              )}
            >
              <Button
                variant="destructive"
                testId="settings-delete-deployment-button"
                onclick={() => openDeleteConfirm(deployment)}
                disabled={deployment.demo_anchor || deletingDeployment}
                ariaLabel={deployment.demo_anchor
                  ? tr("ui.settings.theDemoDeploymentIsProtected")
                  : tr("ui.settings.deleteThisDeployment")}
              >
                {tr("settings.remove")}
              </Button>
            </SettingsRow>
          {/each}
        {/if}
      </div>
    </SettingsSection>
      {/if}
    </div>
  </SettingsShell>
</div>

{#if transfer}
  <StackImportExportModal
    mode={transfer.mode}
    kitDeploymentId={transfer.deploymentId}
    onClose={() => (transfer = null)}
    onSuccess={(result) => {
      transfer = null;
      goto(
        `/stacks/creating?job_id=${encodeURIComponent(result.jobId)}&stack_id=${encodeURIComponent(result.kitDeploymentId)}`,
      );
    }}
  />
{/if}

{#if deleteCandidate}
  <Modal
    title={tr("ui.settings.deleteNamed", { name: deleteCandidate.name })}
    description={tr("ui.settings.thisDecommissionsTheManagedRuntime")}
    onClose={cancelDelete}
    maxWidth="md"
    role="alertdialog"
    tone="danger"
    testId="settings-delete-deployment-modal"
  >
    <label
      class="mb-1 block text-sm text-muted-foreground"
      for="settings-delete-deployment-name"
    >
      {trParts("ui.settings.typeToConfirm")[0]}<span class="font-medium text-foreground"
        >{deleteCandidate.name}</span
      >{trParts("ui.settings.typeToConfirm")[1]}
    </label>
    <input
      id="settings-delete-deployment-name"
      class="mb-4 w-full rounded-lg border border-border bg-input px-3 py-2 text-foreground"
      data-testid="settings-delete-deployment-input"
      bind:value={deleteConfirmName}
      autocomplete="off"
      data-autofocus
    />

    {#if offlineServers.length > 0}
      <div
        class="mb-4 rounded-lg border border-border bg-muted/40 p-3"
        data-testid="settings-delete-offline-servers"
      >
        <p class="text-sm font-medium text-foreground">
          {tr("ui.settings.serverGoneTitle")}
        </p>
        <p class="mt-1 text-sm text-muted-foreground">
          {tr("ui.settings.serverGoneBody", {
            servers: offlineServers.map((server) => server.name).join(", "),
          })}
        </p>
      </div>
    {/if}

    {#if deleteError}
      <ErrorCallout message={deleteError} />
    {/if}

    {#snippet footer()}
      <Button
        variant="secondary"
        onclick={cancelDelete}
        disabled={deletingDeployment}
      >
        {tr("ui.importExportModal.cancel")}
      </Button>
      <Button
        variant="destructive"
        testId="settings-delete-deployment-confirm"
        onclick={confirmDelete}
        disabled={deletingDeployment || !deleteConfirmed}
      >
        {deletingDeployment
          ? tr("ui.settings.deleting")
          : offlineServers.length > 0
            ? tr("ui.settings.removeWithoutServer")
            : tr("settings.remove")}
      </Button>
    {/snippet}
  </Modal>
{/if}
