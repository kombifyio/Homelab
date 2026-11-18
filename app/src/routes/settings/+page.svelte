<script lang="ts">
  import { goto } from "$app/navigation";
  import { page } from "$app/state";
  import { onMount } from "svelte";
  import { getLocale, stateLabel, tr, trParts } from "#lib/i18n.svelte.js";
  import {
    defaultIdentity,
    StackIdentityDisplay,
    StackIdentityEditor,
    stackIdentityEditorLabels,
    stackIdentityLabelFor,
  } from "#lib/components/open-core/index.js";
  import { NotificationPreferences } from "#lib/components/open-core/index.js";
  import CompanionSettingsPanel from "#lib/components/settings/CompanionSettingsPanel.svelte";
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
    type StackPruneResponse,
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
  } from "#lib/dashboard/presets.js";
  import {
    FINISHES,
    NAV_FLYOUT_PREFERENCES,
    type Finish,
  } from "@kombiverselabs/design/contract";
  import {
    RecipeSwatch,
    SettingsRow,
    SettingsSection,
    SettingsShell,
    type SettingsTab,
  } from "@kombiverselabs/ui/settings";
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
  import LanguageSwitcher from "#lib/components/ui/LanguageSwitcher.svelte";
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
  const activeTab = $derived(
    resolveSettingsTab(page.url.searchParams.get("tab"), companionTab),
  );
  const settingsTabs: readonly SettingsTab[] = $derived([
    { slug: "account", label: tr("ui.settings.account"), scope: "shared" },
    { slug: "identity", label: tr("ui.settings.homelabIdentity"), scope: "shared" },
    { slug: "design", label: tr("ui.settings.appearance"), scope: "shared" },
    ...(companionTab
      ? ([
          { slug: "companion", label: tr("ui.settings.companion"), scope: "shared" },
        ] satisfies SettingsTab[])
      : []),
    { slug: "notifications", label: tr("nav.notifications"), scope: "shared" },
    { slug: "homelab", label: tr("wizard.settings.title"), scope: "tool" },
  ]);
  const hrefForTab = (slug: string) => settingsTabHref(slug as SettingsTabSlug);

  // The QA clean-up tool is for kombify staff. The session carries one role
  // signal for that: `admin`. `is_admin` is not used on purpose, because the
  // v2 session also sets it for the homelab `owner`. The portal-SSO session
  // still maps an owner to `admin`, so there owners see the tool too; the
  // backend scopes it to the owner's own tenant either way.
  const isStaff = $derived(
    authStore.cloudUser?.roles?.includes("admin") === true,
  );

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

  let showOrphanPruneConfirm = $state(false);
  let pruningOrphans = $state(false);
  let orphanPruneError = $state<string | null>(null);
  let orphanPruneSuccess = $state(false);
  let orphanPrunePlan = $state<StackPruneResponse | null>(null);

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

  async function openOrphanPruneConfirm() {
    if (!isStaff) return;
    pruningOrphans = true;
    orphanPruneError = null;
    orphanPruneSuccess = false;
    orphanPrunePlan = null;
    try {
      orphanPrunePlan = await planOrphanCleanup();
      showOrphanPruneConfirm = true;
    } catch (err) {
      const parsed = parseApiError(err);
      orphanPruneError = parsed.message || tr("ui.settings.cleanupReviewFailed");
    } finally {
      pruningOrphans = false;
    }
  }

  function cancelOrphanPrune() {
    if (pruningOrphans) return;
    showOrphanPruneConfirm = false;
    orphanPrunePlan = null;
  }

  async function confirmOrphanPrune() {
    pruningOrphans = true;
    orphanPruneError = null;

    try {
      const plan = orphanPrunePlan;
      if (!plan || plan.candidates.length === 0) {
        showOrphanPruneConfirm = false;
        orphanPruneSuccess = true;
        return;
      }
      await applyOrphanCleanup(plan.digest);
      orphanPruneSuccess = true;
      showOrphanPruneConfirm = false;
      orphanPrunePlan = null;
      // SvelteKit 3 deprecates invalidateAll in favour of refreshAll.
      await goto("/dashboard", { refreshAll: true });
    } catch (err) {
      const parsed = parseApiError(err);
      orphanPruneError = parsed.message || tr("ui.settings.cleanupFailed");
    } finally {
      pruningOrphans = false;
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

  async function handleLogout() {
    await authStore.logout({ manualLogin: true });
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
      <div class="flex flex-wrap gap-2">
        <Button variant="secondary" onclick={() => (replaySetup = true)}>
          {tr("setupReplay.restart")}
        </Button>
        <Button
          variant="secondary"
          onclick={() => requestIntroductionStart({ tourOnly: true })}
        >
          {tr("introduction.restart")}
        </Button>
        <Button variant="secondary" onclick={handleLogout}>
          {tr("ui.settings.logout")}
        </Button>
      </div>
    </SettingsSection>

    <SettingsSection
      id="language"
      title={tr("settings.language")}
      lead={tr("settings.language.description")}
    >
      <LanguageSwitcher
        class="rounded-lg border border-border bg-card px-3 py-1.5 text-sm text-foreground"
      />
    </SettingsSection>
    <SettingsSection
      id="keyboard"
      title={tr("shortcuts.settingsTitle")}
      lead={tr("shortcuts.settingsLead")}
    >
      <SettingsRow
        label={tr("shortcuts.switchLabel")}
        description={tr("shortcuts.switchHint")}
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
      title={tr("ui.settings.appearance")}
      lead={tr("ui.settings.lightOrDarkAndThe")}
    >
      <div data-testid="settings-appearance-card">
        <div class="flex flex-col gap-4">
          <div class="flex flex-wrap items-center justify-between gap-3">
            <span class="text-sm font-medium text-foreground">{tr("ui.settings.mode")}</span>
            <div
              class="flex flex-wrap gap-1"
              role="radiogroup"
              aria-label={tr("ui.settings.appearanceMode")}
              data-testid="settings-appearance-mode"
            >
              <button
                type="button"
                role="radio"
                aria-checked={$themePreference === null}
                data-kx={$themePreference === null
                  ? "control selection"
                  : "control"}
                class="rounded-lg px-3 py-1.5 text-sm"
                onclick={() => theme.followDefaultTheme()}
                >{tr("appearance.inherit")}</button
              >
              {#each APPEARANCE_MODES as mode (mode)}
                <button
                  type="button"
                  role="radio"
                  aria-checked={$themePreference === mode}
                  data-kx={$themePreference === mode
                    ? "control selection"
                    : "control"}
                  class="rounded-lg px-3 py-1.5 text-sm capitalize"
                  onclick={() => theme.set(mode)}
                >
                  {tr(`appearance.${mode}`)}
                </button>
              {/each}
            </div>
          </div>
          <p class="-mt-2 text-xs text-muted-foreground" data-testid="settings-appearance-session-hint">
            {tr("appearance.sessionHint")}
          </p>
          <div class="flex flex-wrap items-center justify-between gap-3">
            <div class="flex items-center gap-3">
              <span class="text-sm font-medium text-foreground">{tr("ui.settings.surface")}</span>
              {#if deviceFinishChosen}
                <button
                  type="button"
                  data-kx="control"
                  class="rounded-lg px-2 py-1 text-xs text-muted-foreground"
                  data-testid="settings-finish-follow-account"
                  onclick={followAccount}
                >
                  {tr("ui.settings.followAccount")}
                </button>
              {/if}
            </div>
            <div
              class="flex flex-wrap gap-1"
              role="radiogroup"
              aria-label={tr("ui.settings.surfaceFinish")}
              data-testid="settings-finish"
            >
              {#each FINISHES as finishOption (finishOption)}
                <button
                  type="button"
                  role="radio"
                  aria-checked={$finishValue === finishOption}
                  data-kx={$finishValue === finishOption
                    ? "control selection"
                    : "control"}
                  class="flex items-center gap-2 rounded-lg px-3 py-1.5 text-sm capitalize"
                  onclick={() => pickFinish(finishOption)}
                >
                  <!-- A miniature of the finish itself. The swatch stamps the
                       finish and the resolved appearance on its own subtree, so
                       the package's real plate and selection recipes render at a
                       quarter scale — nothing here draws a second version of a
                       design (KOMBIFY-DESIGN-SYSTEM-STANDARD §6). -->
                  <RecipeSwatch
                    finish={finishOption}
                    appearance={swatchAppearance}
                    size="sm"
                    class="shrink-0"
                  />
                  {finishOption}
                </button>
              {/each}
            </div>
          </div>
          <div class="flex flex-wrap items-center justify-between gap-3">
            <span class="text-sm font-medium text-foreground">
              {tr("ui.settings.navigationPreview")}
            </span>
            <div
              class="flex flex-wrap gap-1"
              role="radiogroup"
              aria-label={tr("ui.settings.navigationPreviewShape")}
              data-testid="settings-nav-flyout"
            >
              {#each NAV_FLYOUT_PREFERENCES as shape (shape)}
                <button
                  type="button"
                  role="radio"
                  aria-checked={$navFlyoutValue === shape}
                  data-kx={$navFlyoutValue === shape
                    ? "control selection"
                    : "control"}
                  class="rounded-lg px-3 py-1.5 text-sm"
                  onclick={() => theme.setNavFlyout(shape)}
                >
                  {navFlyoutLabels[shape]}
                </button>
              {/each}
            </div>
          </div>
          <div class="flex flex-col gap-2">
            <div class="flex flex-wrap items-center justify-between gap-3">
              <span class="text-sm font-medium text-foreground">{tr("ui.settings.dashboard")}</span>
              <div
                class="flex flex-wrap gap-1"
                role="radiogroup"
                aria-label={tr("ui.settings.dashboardLayout")}
                data-testid="settings-dashboard-preset"
              >
                {#each DASHBOARD_PRESETS.filter( (preset) => isPresetAvailable( preset.id, { ownerKey: dashboardOwnerKey() } ) ) as preset (preset.id)}
                  <button
                    type="button"
                    role="radio"
                    aria-checked={dashboardPreset.current === preset.id}
                    data-kx={dashboardPreset.current === preset.id
                      ? "control selection"
                      : "control"}
                    class="rounded-lg px-3 py-1.5 text-sm"
                    title={preset.description}
                    onclick={() => dashboardPreset.set(preset.id)}
                  >
                    {preset.label}
                  </button>
                {/each}
              </div>
            </div>
            <p class="text-xs text-muted-foreground">
              {DASHBOARD_PRESETS.find(
                (preset) => preset.id === dashboardPreset.current,
              )?.description} {tr("ui.settings.savedOnDevice")}
            </p>
          </div>
        </div>
      </div>
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

      {:else if activeTab === "homelab"}
    <SettingsSection
      id="homelab-settings"
      title={tr("wizard.settings.title")}
      lead={tr("wizard.settings.intro")}
    >
      <HomelabSettingsPanel />
    </SettingsSection>

    <SettingsSection
      id="homelab-transfer"
      title={tr("ui.common.importExport")}
      lead={tr("ui.homelab.transferHint")}
    >
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
      {#if isStaff}
        <div
          class="rounded-lg border border-destructive/30 bg-destructive/5 p-4"
          data-testid="settings-prune-orphans-card"
        >
          <div
            class="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between"
          >
            <div class="min-w-0">
              <h3 class="font-medium text-foreground">{tr("ui.settings.cleanUpTestResidue")}</h3>
              <p class="mt-1 text-sm text-muted-foreground">
                {tr("ui.settings.reviewsExactOwnerScopedE2e")}
              </p>
            </div>
            <Button
              variant="secondary"
              class="shrink-0"
              testId="settings-prune-orphans-button"
              onclick={openOrphanPruneConfirm}
              disabled={pruningOrphans}
            >
              {pruningOrphans ? tr("ui.settings.reviewing") : tr("ui.settings.reviewCleanup")}
            </Button>
          </div>

          {#if orphanPruneError}
            <ErrorCallout class="mt-3" message={orphanPruneError} />
          {/if}
          {#if orphanPruneSuccess}
            <p class="mt-3 text-sm text-success">
              {tr("ui.settings.noVerifiedTestResidueRemains")}
            </p>
          {/if}
        </div>
      {/if}

      <div
        class="mt-4 rounded-lg border border-destructive/30 bg-destructive/5 p-4"
        data-testid="settings-delete-deployment-card"
      >
        <div class="min-w-0">
          <h3 class="font-medium text-foreground">{tr("ui.settings.deleteADeployment")}</h3>
          <p class="mt-1 text-sm text-muted-foreground">
            {tr("ui.settings.decommissionsTheDeploymentSManaged")}
          </p>
        </div>

        {#if deploymentsLoading}
          <p class="mt-3 text-sm text-muted-foreground">
            {tr("ui.settings.loadingDeployments")}
          </p>
        {:else if deploymentsError}
          <ErrorCallout
            class="mt-3"
            message={deploymentsError}
            onRetry={() => void loadDeployments()}
          />
        {:else if deployments.length === 0}
          <p class="mt-3 text-sm text-muted-foreground">
            {tr("ui.settings.noDeploymentsToDelete")}
          </p>
        {:else}
          <ul class="mt-3 space-y-2">
            {#each deployments as deployment (deployment.id)}
              <li
                class="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-border bg-card p-3"
                data-testid="settings-deployment-row"
              >
                <div class="min-w-0">
                  <p class="truncate font-medium text-foreground">
                    {deployment.name}
                  </p>
                  <p class="text-xs text-muted-foreground">
                    {stateLabel(deployment.state || deployment.status || "unknown")}
                  </p>
                </div>
                <Button
                  variant="secondary"
                  class="shrink-0"
                  testId="settings-delete-deployment-button"
                  onclick={() => openDeleteConfirm(deployment)}
                  disabled={deployment.demo_anchor || deletingDeployment}
                  ariaLabel={deployment.demo_anchor
                    ? tr("ui.settings.theDemoDeploymentIsProtected")
                    : tr("ui.settings.deleteThisDeployment")}
                >
                  {tr("settings.remove")}
                </Button>
              </li>
            {/each}
          </ul>
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

{#if showOrphanPruneConfirm && isStaff}
  <Modal
    title={tr("ui.settings.reviewVerifiedTestResidue")}
    description={tr("ui.settings.thisExactPlanIsBound")}
    onClose={cancelOrphanPrune}
    maxWidth="md"
    testId="settings-prune-orphans-modal"
  >
    <div class="mb-4 rounded-lg border border-border bg-muted/40 p-3">
      <p class="text-sm text-foreground">
        {tr("ui.settings.pruneQualifies", { owner: "e2e-*", strict: "runtime-cloud-<provider>-<timestamp>" })}
      </p>
    </div>

    {#if orphanPrunePlan?.candidates.length}
      <ul class="space-y-2" data-testid="settings-prune-orphans-plan">
        {#each orphanPrunePlan.candidates as candidate (candidate.resource_type + candidate.id)}
          <li class="rounded-lg border border-border bg-card p-3">
            <p class="font-medium text-foreground">{candidate.name}</p>
            <p class="mt-1 break-all text-xs text-muted-foreground">
              {candidate.resource_type} · {candidate.id} · {candidate.class}
            </p>
          </li>
        {/each}
      </ul>
    {:else}
      <p
        class="text-sm text-success"
        data-testid="settings-prune-orphans-empty"
      >
        {tr("ui.settings.noVerifiedTestResidueWas")}
      </p>
    {/if}

    {#if orphanPruneError}
      <ErrorCallout class="mt-4" message={orphanPruneError} />
    {/if}

    {#snippet footer()}
      <Button
        variant="secondary"
        onclick={cancelOrphanPrune}
        disabled={pruningOrphans}
      >
        {orphanPrunePlan?.candidates.length ? tr("ui.importExportModal.cancel") : tr("ui.groupedTaskList.done")}
      </Button>
      {#if orphanPrunePlan?.candidates.length}
        <Button
          variant="primary"
          testId="settings-prune-orphans-confirm"
          onclick={confirmOrphanPrune}
          disabled={pruningOrphans}
        >
          {pruningOrphans ? tr("ui.settings.applying") : tr("ui.settings.applyExactPlan")}
        </Button>
      {/if}
    {/snippet}
  </Modal>
{/if}
