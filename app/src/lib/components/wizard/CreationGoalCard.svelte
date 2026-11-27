<script lang="ts">
  import { tick } from "svelte";
  import { Check, ArrowUpRight } from "@lucide/svelte";
  import {
    experienceLevel,
    setupAnswersFor,
    setupChanges,
    setupFlowFromCatalog,
    useCaseSetupLabels,
    UseCaseArt,
    UseCaseCardCompact,
    UseCaseGuidedSetup,
    type SetupHardwareHints,
    type UseCaseSetupAnswers,
    type UseCaseSetupFlow,
    type UseCaseSetupResult,
  } from "@kombiverselabs/ui/usecase";
  import { KfDetailSheet } from "@kombiverselabs/ui/card";
  import {
    backendChoices,
    selectedBackend,
    supportsCreationChoice,
    type CreationGoal,
  } from "#lib/wizard/creationGoals.js";
  import { goalDescription, goalTitle } from "#lib/wizard/creationGoalText.js";
  import type { GoalConfigKey } from "#lib/wizard/index.js";
  import {
    installsService,
    maturityLabelled,
    type UseCaseCatalogView,
    type UseCaseCatalogComponent,
  } from "#lib/api/useCaseCatalog.js";
  import { brandDomainForTool } from "#lib/brand-logo.js";
  import { useCaseGuideUrl } from "#lib/docs-links.js";
  import { getLocale, isMessageKey, tr, type MessageKey } from "#lib/i18n.svelte.js";
  import UseCaseServicePicker from "./UseCaseServicePicker.svelte";
  import UseCaseSettings from "./UseCaseSettings.svelte";
  import BrandLogoIcon from "../BrandLogoIcon.svelte";
  import BrandLogoScope from "../BrandLogoScope.svelte";
  import { useCreationContext } from "#lib/creation/context.js";
  import type { GoalsSlice } from "#lib/creation/core/index.js";
  import SmartHomeAdvice from "./SmartHomeAdvice.svelte";
  import ExistingAppTakeoverBanner from "./ExistingAppTakeoverBanner.svelte";
  import type {
    DetectedExistingApp,
    SmartHomeRecommendation,
  } from "#lib/api/unifier.js";

  /**
   * The large card of one use case: its painting, the application choice and
   * the guided setup, at a fixed size. Step 1 shows it on the configuration
   * stage beside the list of chosen use cases, so opening it moves nothing.
   */
  let {
    card,
    config = $bindable(),
    runtimeCatalog,
    reviewMode = false,
    onGoalChange,
    onSettingChange,
    hardware = {},
    oncomplete,
    smartHomeRecommendation,
    detectedExistingApps = [],
    onAdoptExistingApp,
    onOverrideExistingApp,
  }: {
    card: CreationGoal;
    config: GoalsSlice;
    runtimeCatalog: Map<string, UseCaseCatalogView>;
    reviewMode?: boolean;
    onGoalChange: (goal: GoalConfigKey, value: boolean) => void;
    onSettingChange: (
      goal: GoalConfigKey,
      setting: string,
      value: string | boolean,
    ) => void;
    hardware?: SetupHardwareHints;
    /** The guided setup was applied; the stage moves on. */
    oncomplete?: () => void;
    smartHomeRecommendation?: SmartHomeRecommendation;
    detectedExistingApps?: DetectedExistingApp[];
    onAdoptExistingApp?: (app: DetectedExistingApp) => void;
    onOverrideExistingApp?: (app: DetectedExistingApp) => void;
  } = $props();

  const creation = useCreationContext();
  const appearance = creation.appearance;
  let advancedOpen = $state(false);
  let serviceInfo = $state<{
    component: UseCaseCatalogComponent;
    trigger: HTMLButtonElement;
  } | null>(null);

  const title = $derived(goalTitle(card));
  const selected = $derived(
    !!card.question && config.goals?.[card.question.configKey] === true,
  );
  const values = $derived(config.useCaseSettings?.[card.id] ?? {});
  const catalogView = $derived(
    reviewMode ? card.catalog : runtimeCatalog.get(card.id),
  );
  /** The guided setup. The service picker above already chooses the app, so
   * the setup starts with what the app should do. */
  const flow = $derived.by((): UseCaseSetupFlow | undefined => {
    const built = setupFlowFromCatalog(card.id, catalogView, {
      locale: getLocale(),
      hardware,
    });
    if (!built) return undefined;
    const questions = built.questions.filter((entry) => entry.id !== "backend");
    return questions.length > 0 ? { ...built, questions } : undefined;
  });
  /** How many steps the person sees follows the detail level they chose in
   * the kombify Cloud account setup; every other step keeps its
   * recommendation and opens under the setup's "More settings". */
  const level = $derived(experienceLevel(creation.experience.level));
  const setupLabels = $derived(useCaseSetupLabels(getLocale()));
  const chosen = $derived(selectedBackend(card.catalog, values.backend));
  const notInRelease = $derived(
    !reviewMode && runtimeCatalog.get(card.id)?.deliverable === false,
  );
  const matchingExistingApps = $derived(
    detectedExistingApps.filter(
      (app) =>
        app.spectrum_tier === "first_class" && app.use_case_slug === card.id,
    ),
  );
  /** Free-text settings (a mail domain) are not questions; they stay in the
   * settings drawer below the setup. */
  const choiceSettings = $derived(
    (catalogView?.settings ?? [])
      .filter((setting) => setting.kind !== "text")
      .map((setting) => setting.id),
  );
  const hasTextSettings = $derived(
    (catalogView?.settings ?? []).some((setting) => setting.kind === "text"),
  );

  function canChoose(setting: string, value?: string | boolean) {
    return (
      reviewMode ||
      supportsCreationChoice(runtimeCatalog.get(card.id), setting, value)
    );
  }
  function answers() {
    return setupAnswersFor(flow, values, config.useCaseSetupAnswers?.[card.id]);
  }
  function keepSetupAnswers(next: UseCaseSetupAnswers) {
    config.useCaseSetupAnswers = {
      ...config.useCaseSetupAnswers,
      [card.id]: Object.fromEntries(
        Object.entries(next).map(([id, answer]) => [
          id,
          Array.isArray(answer) ? [...answer] : [answer],
        ]),
      ),
    };
  }
  function choose(id: string) {
    if (!card.question || card.availability !== "available") return;
    if (!canChoose("backend", id)) return;
    keepSetupAnswers({ ...answers(), backend: [id] });
    onSettingChange(card.question.configKey, "backend", id);
    onGoalChange(card.question.configKey, true);
  }
  /** Applying the setup stores its decisions and keeps the use case. */
  function applySetup(result: UseCaseSetupResult) {
    if (!card.question) return;
    const changes = setupChanges(catalogView, result, values);
    for (const [id, value] of changes.settings)
      if (canChoose(id, value))
        onSettingChange(card.question.configKey, id, value);
    if (catalogView?.capabilities?.length)
      config.useCaseCapabilities = {
        ...(config.useCaseCapabilities ?? {}),
        [card.id]: changes.capabilities,
      };
    keepSetupAnswers({ ...answers(), ...changes.answers });
    onGoalChange(card.question.configKey, true);
    oncomplete?.();
  }
  function preferenceNote() {
    return tr(
      reviewMode
        ? "wizard.preview.preferenceOnly"
        : "wizard.creation.preferenceOnly",
    );
  }
  /** Stack badges: apps the user can pick that this release only records. */
  function laterMarkers() {
    const markers: Record<string, string> = {};
    for (const component of backendChoices(card.catalog)) {
      if (notInRelease)
        markers[component.id] = tr("wizard.goals.notInThisRelease");
      else if (
        component.role === "alternative" &&
        (reviewMode || !installsService(component))
      )
        markers[component.id] = tr("wizard.preview.savedForLater");
    }
    return markers;
  }
  async function closeServiceInfo() {
    const trigger = serviceInfo?.trigger;
    serviceInfo = null;
    await tick();
    trigger?.focus();
  }
  const ROLE_HELP_KEYS: Record<string, MessageKey> = {
    primary: "wizard.preview.roleHelp.primary",
    alternative: "wizard.preview.roleHelp.alternative",
    supporting: "wizard.preview.roleHelp.supporting",
    bridge: "wizard.preview.roleHelp.bridge",
    connector: "wizard.preview.roleHelp.connector",
  };
  const PREVIEW_HELP_KEYS: Partial<Record<CreationGoal["id"], MessageKey>> = {
    files: "wizard.preview.files.help",
    mail: "wizard.preview.mail.help",
    game: "wizard.preview.game.help",
    dev: "wizard.preview.dev.help",
  };
  function serviceDescription(id: string, role: string) {
    const key = `wizard.preview.service.${id}`;
    if (isMessageKey(key)) return tr(key);
    const roleKey = ROLE_HELP_KEYS[role];
    return roleKey ? tr(roleKey) : "";
  }
</script>

<div
  class="goal-card"
  class:guided={Boolean(flow)}
  class:advanced-open={advancedOpen}
  data-card-id={card.id}
  data-testid={`creation-goal-card-${card.id}`}
>
  {#snippet visual()}
    <UseCaseArt
      class="goal-art"
      useCase={card.id}
      dark={$appearance === "dark"}
      shade={true}
      priority={true}
    />
  {/snippet}
  {#snippet controls()}
    {#if maturityLabelled(card.catalog?.maturity)}
      <!-- The release's own status of this use case (owner decision
           2026-10-07): beta and experimental stay installable. -->
      <span
        class="maturity-chip"
        data-maturity={card.catalog?.maturity}
        data-testid={`maturity-${card.id}`}
        title={tr("wizard.status.hint")}
        >{tr(`wizard.status.${card.catalog?.maturity}`)}</span
      >
    {/if}
    {#if !flow}
      <p class="eyebrow">
        {tr("wizard.preview.serviceFor")}
        {title}
      </p>
    {/if}
    <UseCaseServicePicker
      summary={Boolean(flow)}
      describe={(component) => serviceDescription(component.id, component.role)}
      components={backendChoices(card.catalog)}
      selectedId={chosen?.id}
      label={`${tr("wizard.preview.serviceFor")} ${title}`}
      compact={false}
      savedForLater={laterMarkers()}
      openLabel={tr("wizard.preview.explore")}
      disabledIds={backendChoices(card.catalog)
        .filter((component) => !canChoose("backend", component.id))
        .map((component) => component.id)}
      onselect={choose}
      oninfo={(component, trigger) => {
        serviceInfo = { component, trigger };
      }}
    />
    <!-- The app stack marks these; the sentences live here. -->
    {#if values.backend && chosen?.role === "alternative" && (reviewMode || !installsService(chosen))}
      <p class="preference-note">{preferenceNote()}</p>
    {/if}
    {#if notInRelease}
      <p class="preference-note">
        {tr("wizard.goals.notInThisReleaseLong")}
      </p>
    {/if}
  {/snippet}
  {#snippet details()}
    <div class="detail-content">
      <div class="goal-details">
        <div class="detail-story">
          <p class="eyebrow">{tr("wizard.preview.aboutUseCase")}</p>
          <p>
            {PREVIEW_HELP_KEYS[card.id]
              ? tr(PREVIEW_HELP_KEYS[card.id]!)
              : card.question?.helpKey
                ? tr(card.question.helpKey)
                : goalDescription(card)}
          </p>
          {#if card.question?.tipKey && !["mail", "game", "dev"].includes(card.id)}<p
              class="detail-tip"
            >
              <Check size={15} aria-hidden="true" />{tr(card.question.tipKey)}
            </p>{/if}
          {#if useCaseGuideUrl(card.catalog?.docsPath)}<a
              href={useCaseGuideUrl(card.catalog?.docsPath)}
              target="_blank"
              rel="noreferrer"
              >{tr("wizard.goals.learnMore")}<ArrowUpRight
                size={14}
                aria-hidden="true"
              /></a
            >{/if}
        </div>
        {#if matchingExistingApps.length > 0}
          <ExistingAppTakeoverBanner
            apps={matchingExistingApps}
            smartHomeOrigin={config.useCaseSettings?.["smart-home"]?.[
              "instance-origin"
            ]}
            onAdopt={onAdoptExistingApp}
            onOverride={onOverrideExistingApp}
            compact
          />
        {/if}
        {#if card.id === "smart-home" && smartHomeRecommendation}
          <details
            class="smart-home-advice"
            open={smartHomeRecommendation.auto_detected}
          >
            <summary>{tr("wizard.smartHome.advice")}</summary>
            <SmartHomeAdvice recommendation={smartHomeRecommendation} compact />
          </details>
        {/if}
        {#if flow}
          <UseCaseGuidedSetup
            class="goal-setup"
            {flow}
            {level}
            answers={answers()}
            label={title}
            labels={setupLabels}
            onchange={(result) => keepSetupAnswers(result.answers)}
            oncomplete={applySetup}
          />
        {/if}
      </div>
    </div>
    {#if hasTextSettings}
      <UseCaseSettings
        inlay={true}
        open={advancedOpen}
        onopenchange={(open) => {
          advancedOpen = open;
        }}
        catalog={catalogView}
        exclude={choiceSettings}
        preview={reviewMode}
        {values}
        onchange={(id, value) => {
          if (card.question)
            onSettingChange(card.question.configKey, id, value);
        }}
      />
    {/if}
  {/snippet}
  <UseCaseCardCompact
    name={title}
    description={goalDescription(card)}
    planState={selected ? "included" : "available"}
    layout="tile"
    {visual}
    {controls}
    {details}
    expanded={true}
    selectionLabel={`${tr(selected ? "wizard.goals.remove" : "wizard.goals.add")} ${title}`}
    planLabel={tr(
      selected ? "wizard.preview.selected" : "wizard.preview.optional",
    )}
    onToggle={(next) => {
      if (card.question) onGoalChange(card.question.configKey, next);
    }}
  />
</div>

{#if serviceInfo}
  {@const service = serviceInfo.component}
  {@const domain = brandDomainForTool(service.id, service.name)}
  <KfDetailSheet
    open={true}
    onclose={closeServiceInfo}
    title={service.name}
    meta={title}
    tabs={[{ id: "overview", label: tr("wizard.preview.aboutService") }]}
  >
    {#snippet header()}<div class="service-hero">
        <BrandLogoScope {domain}
          ><BrandLogoIcon
            class="h-12 w-12"
            fallbackLabel={service.name}
          /></BrandLogoScope
        ><span>{tr(`wizard.preview.role.${service.role}`)}</span>
      </div>{/snippet}
    {#snippet body()}
      <div class="service-dossier">
        <p>{serviceDescription(service.id, service.role)}</p>
        <dl>
          <div>
            <dt>{tr("wizard.preview.partOf")}</dt>
            <dd>{title}</dd>
          </div>
        </dl>
        {#if service.role === "alternative" && (reviewMode || !installsService(service))}<p
            class="service-disclaimer"
          >
            {preferenceNote()}
          </p>{/if}
        {#if domain}<a
            href={`https://${domain}`}
            target="_blank"
            rel="noreferrer"
            >{tr("wizard.preview.website")}<ArrowUpRight
              size={16}
              aria-hidden="true"
            /></a
          >{/if}
      </div>
    {/snippet}
    {#snippet actionBar()}
      {#if service.role === "primary" || service.role === "alternative"}<button
          type="button"
          class="use-service"
          data-kx="control"
          data-variant="primary"
          disabled={!canChoose("backend", service.id)}
          onclick={() => {
            choose(service.id);
            void closeServiceInfo();
          }}>{tr("wizard.preview.useService")} {service.name}</button
        >{/if}
    {/snippet}
  </KfDetailSheet>
{/if}

<style>
  /* The card is a container of its own: wide enough, it is the 2x2 card with
     the painting on top and the description beside the application choice;
     narrow, it stacks and grows in height. */
  .goal-card {
    container: goal-card / inline-size;
    position: relative;
    min-width: 0;
    --goal-art-height: 200px;
  }
  .goal-card :global(.kf-uc-enhanced-main) {
    gap: 8px 12px;
    padding: 14px;
  }
  .goal-card :global(.kf-uc-enhanced-visual) {
    min-height: var(--goal-art-height);
    height: var(--goal-art-height);
    border: 0;
    border-radius: 9px;
  }
  /* The title lies on the artwork. */
  .goal-card :global(.kf-uc-enhanced-name) {
    position: absolute;
    z-index: 3;
    top: calc(14px + var(--goal-art-height) - 10px);
    left: 26px;
    width: calc(100% - 82px);
    transform: translateY(-100%);
    color: white;
    font-size: 30px;
    line-height: 1.15;
    -webkit-text-stroke: 0.6px #000;
    paint-order: stroke fill;
    text-shadow: 0 1px 3px #000;
    pointer-events: none;
  }
  .goal-card :global(.kf-uc-enhanced-copy) {
    display: contents;
  }
  .goal-card :global(.kf-uc-enhanced-desc) {
    display: none;
  }
  .goal-card :global(.kf-uc-enhanced-selection) {
    grid-area: auto;
    position: absolute;
    z-index: 3;
    top: 24px;
    right: 24px;
  }
  .goal-card :global(.kf-uc-select) {
    color: white;
    background: rgb(15 22 32 / 72%);
    box-shadow: inset 0 0 0 1px rgb(255 255 255 / 40%);
    backdrop-filter: blur(8px);
  }
  .goal-card :global(.kf-uc-select[data-on="true"]) {
    background: var(--primary);
    box-shadow: 0 2px 12px rgb(0 0 0 / 30%);
  }
  .goal-card :global(.service-picker) {
    flex-direction: column;
    flex-wrap: nowrap;
  }
  .goal-card :global(.service-option) {
    flex: none;
  }
  .goal-card :global(.kf-uc-enhanced-foot) {
    padding-top: 9px;
  }
  .goal-card :global(.goal-art img) {
    object-position: center 45%;
  }
  .maturity-chip {
    align-self: flex-start;
    display: inline-flex;
    align-items: center;
    padding: 1px 8px;
    border-radius: 999px;
    font-size: 11px;
    font-weight: 600;
    letter-spacing: 0.02em;
    color: var(--foreground);
    background: color-mix(in oklab, var(--warning, #d97706) 18%, transparent);
    border: 1px solid
      color-mix(in oklab, var(--warning, #d97706) 45%, transparent);
  }
  .maturity-chip[data-maturity="experimental"],
  .maturity-chip[data-maturity="deprecated"] {
    background: color-mix(
      in oklab,
      var(--destructive, #dc2626) 14%,
      transparent
    );
    border-color: color-mix(
      in oklab,
      var(--destructive, #dc2626) 40%,
      transparent
    );
  }
  .eyebrow {
    font-size: 10px;
    font-weight: 650;
    text-transform: uppercase;
    letter-spacing: 0.12em;
    color: var(--muted-foreground);
    margin: 0 0 6px;
  }
  .goal-details {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 26px;
    padding-block: 8px 22px;
  }
  .detail-story > p:not(.eyebrow) {
    font-size: 14px;
    line-height: 1.65;
    color: var(--muted-foreground);
  }
  .detail-story > p:not(.eyebrow, .detail-tip) {
    color: var(--foreground);
    font-size: 14.5px;
    line-height: 1.6;
  }
  .detail-story .detail-tip {
    display: flex;
    align-items: flex-start;
    gap: 7px;
    margin-top: 14px;
  }
  .detail-tip :global(svg) {
    flex-shrink: 0;
    margin-top: 4px;
    color: var(--primary);
  }
  a {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    color: var(--primary);
    font-size: 12px;
    text-underline-offset: 3px;
  }
  a:hover {
    text-decoration: underline;
  }
  .detail-story a {
    margin-top: 14px;
  }
  .preference-note {
    font-size: 11px;
    line-height: 1.55;
    color: var(--muted-foreground);
    margin-top: 8px;
  }

  /* With a guided setup the card stacks: the chosen application on top, the
     setup steps below across the full card width. */
  .goal-card.guided .detail-story > :not(a) {
    display: none;
  }
  .goal-card.guided .detail-story a {
    margin-top: 0;
  }
  .goal-card.guided .detail-story {
    display: flex;
    justify-content: flex-end;
    gap: 8px;
  }
  .goal-card.guided .detail-story:not(:has(a)) {
    display: none;
  }
  .goal-card.guided .goal-details {
    gap: 0;
  }
  .goal-card.guided :global(.kf-uc-setup) {
    gap: 10px;
  }
  .goal-card.guided :global(.kf-uc-setup-choices) {
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 160px), 1fr));
    gap: 7px;
    margin-top: 8px;
  }
  .goal-card.guided :global(.kf-uc-setup-choice) {
    gap: 8px;
    padding: 8px 10px;
  }
  .goal-card.guided :global(.kf-uc-setup-choice-copy) {
    gap: 2px;
  }
  /* The check sits in the choice's corner, so the title, its chips and the
     description get the whole choice width. */
  .goal-card.guided :global(.kf-uc-setup-choice-mark) {
    position: absolute;
    top: 8px;
    inset-inline-end: 8px;
    width: 18px;
    height: 18px;
  }
  .goal-card.guided :global(.kf-uc-setup-choice-head) {
    gap: 4px;
    padding-inline-end: 20px;
  }
  .goal-card.guided :global(.kf-uc-setup-choice small) {
    font-size: 11px;
    line-height: 1.38;
  }
  .goal-card.guided :global(.kf-uc-setup-choice-mark svg) {
    width: 11px;
    height: 11px;
  }
  .goal-card.guided :global(.kf-uc-setup-chip) {
    padding: 0 4px;
    font-size: 8.5px;
    letter-spacing: 0.02em;
  }
  .goal-card.guided :global(.kf-uc-setup-help) {
    margin-top: 3px;
  }

  /* Wide enough, the card has one fixed size: a short artwork band on top,
     below it the description left and the application choice right. With a
     guided setup the setup takes the full width below the choice. Nothing
     inside makes the card grow. */
  @container goal-card (min-width: 560px) {
    .goal-card :global(.kf-uc-enhanced) {
      --goal-art-height: clamp(120px, 24cqi, 172px);
      display: grid;
      height: var(--goal-card-height, 620px);
      grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
      grid-template-rows: var(--goal-art-height) minmax(0, 1fr) auto auto;
      grid-template-areas:
        "visual visual"
        "details controls"
        "foot foot"
        "advanced advanced";
      gap: 14px 28px;
      padding: 14px;
      overflow: hidden;
    }
    .goal-card :global(.kf-uc-enhanced-main),
    .goal-card :global(.kf-uc-enhanced-details) {
      display: contents;
    }
    .goal-card :global(.kf-uc-enhanced-controls),
    .goal-card .detail-content {
      min-height: 0;
      overflow-y: auto;
      overscroll-behavior: contain;
      scrollbar-width: thin;
      scrollbar-color: color-mix(in oklch, var(--foreground) 22%, transparent)
        transparent;
    }
    .goal-card .detail-content {
      grid-area: details;
      padding: 0;
      border-top: 0;
    }
    .goal-card .goal-details {
      gap: 20px;
      padding-block: 8px 4px;
    }
    .goal-card.guided :global(.kf-uc-enhanced) {
      --goal-art-height: clamp(76px, 16cqi, 116px);
      grid-template-columns: minmax(0, 1fr);
      grid-template-rows: var(--goal-art-height) auto minmax(0, 1fr) auto auto;
      grid-template-areas:
        "visual"
        "controls"
        "details"
        "foot"
        "advanced";
      gap: 10px;
    }
    /* The artwork carries the plan button, so the footer would repeat it. */
    .goal-card.guided :global(.kf-uc-enhanced-foot) {
      display: none;
    }
    /* The release status rides on the artwork instead of taking a row. */
    .goal-card.guided .maturity-chip {
      position: absolute;
      z-index: 3;
      top: 24px;
      left: 24px;
      background: color-mix(in oklab, var(--warning, #d97706) 30%, var(--card));
    }
    /* The rail names the current step. */
    .goal-card.guided :global(.kf-uc-setup-question .kf-uc-setup-step) {
      display: none;
    }
    .goal-card.guided :global(.kf-uc-enhanced-controls) {
      overflow: visible;
    }
    /* The guide link rides on the artwork, beside the plan button. */
    .goal-card.guided .detail-story {
      position: absolute;
      z-index: 3;
      top: 24px;
      right: 66px;
    }
    .goal-card.guided .detail-story a {
      box-sizing: border-box;
      height: 32px;
      padding: 0 12px;
      color: white;
      font-size: 12px;
      background: rgb(15 22 32 / 72%);
      border-radius: 999px;
      box-shadow: inset 0 0 0 1px rgb(255 255 255 / 40%);
      backdrop-filter: blur(8px);
    }
    .goal-card.guided :global(.kf-uc-setup-row) {
      padding-block: 6px;
    }
    /* The rail and the Back/Next row always stay in view; only a step with
       many long choices lets its choices scroll. */
    .goal-card.guided .detail-content,
    .goal-card.guided .goal-details {
      display: grid;
      min-height: 0;
      overflow: hidden;
      padding-block: 0;
    }
    .goal-card.guided .goal-details {
      grid-template-rows: minmax(0, 1fr);
    }
    .goal-card.guided :global(.kf-uc-setup) {
      grid-template-rows: auto minmax(0, 1fr) auto;
      min-height: 0;
    }
    .goal-card.guided :global(.kf-uc-setup-body) {
      min-height: 0;
      overflow-y: auto;
      overscroll-behavior: contain;
      scrollbar-width: thin;
    }
  }
  /* The settings drawer replaces the body while open. */
  .goal-card.advanced-open :global(.kf-uc-enhanced) {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    grid-template-rows: var(--goal-art-height) minmax(0, 1fr);
    grid-template-areas: "visual" "advanced";
    gap: 12px;
    padding: 14px;
  }
  .goal-card.advanced-open :global(.kf-uc-enhanced-main),
  .goal-card.advanced-open :global(.kf-uc-enhanced-details) {
    display: contents;
  }
  .goal-card.advanced-open :global(.kf-uc-enhanced-controls),
  .goal-card.advanced-open :global(.kf-uc-enhanced-foot),
  .goal-card.advanced-open .detail-content {
    display: none;
  }
  .service-hero {
    display: flex;
    align-items: center;
    gap: 16px;
    padding-block: 10px;
  }
  .service-hero span {
    color: var(--muted-foreground);
    font-size: 12px;
  }
  .service-dossier {
    display: grid;
    gap: 24px;
    font-size: 14px;
    line-height: 1.75;
  }
  .service-dossier dl {
    font-size: 12px;
  }
  .service-dossier dl > div {
    display: flex;
    justify-content: space-between;
    gap: 18px;
    padding: 12px 0;
    border-bottom: 1px solid var(--border);
  }
  dt {
    color: var(--muted-foreground);
  }
  .service-disclaimer {
    padding: 14px;
    background: var(--muted);
    border-radius: var(--radius-lg, 12px);
    font-size: 12px;
  }
  .use-service {
    width: 100%;
    border-radius: var(--radius-control, 10px);
    padding: 12px 16px;
    font-size: 13px;
  }
  button:focus-visible,
  a:focus-visible {
    outline: 2px solid var(--ring);
    outline-offset: 4px;
  }
</style>
