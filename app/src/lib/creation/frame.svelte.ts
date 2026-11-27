/**
 * The state one creation flow keeps while it is on screen, whichever frame shows
 * it: the answers and their draft, the catalog, the recommendations, the owner
 * step and the session. Created during a frame's initialization, because its
 * effects live as long as that frame.
 */
import { untrack, type Snippet } from "svelte";
import {
  layoutRunTarget,
  type LayoutRecommendationView,
} from "#lib/api/layout.js";
import {
  loadUseCaseCatalog,
  type UseCaseCatalogView,
} from "#lib/api/useCaseCatalog.js";
import {
  capacityHint,
  capacityRequestHost,
  type CapacityHint,
  type CapacitySize,
} from "#lib/wizard/capacity-view.js";
import type {
  WizardCapacity,
  WizardRecommendationRequest,
} from "#lib/api/unifier.js";
import type { Worker } from "#lib/api/workers.js";
import { wizardRequestKey } from "#lib/wizard/WizardPreviewController.js";
import {
  LayoutPreviewController,
  acceptedLayoutFromPreview,
  layoutPreviewView,
  type LayoutPreviewState,
} from "#lib/wizard/LayoutPreviewController.js";
import {
  creationGoals,
  type CreationStage,
} from "#lib/wizard/creationGoals.js";
import {
  clearWizardDraft,
  loadWizardDraft,
  mergeOverDefaults,
  saveWizardDraft,
  wizardDraftWritten,
  type WizardDraft,
} from "#lib/wizard/draft-storage.js";
import type {
  HouseholdPersonDraft,
  HouseholdProfile,
} from "#lib/wizard/household-draft.js";
import {
  applyPasskeyFirstOwnerLogin,
  applyWizardDeploymentLane,
  buildLayoutSelection,
  createDefaultConfig,
  deriveServicesFromGoals,
  getEasyAudienceQuestions,
  getEasyAccessQuestions,
  getEasyGoalQuestions,
  getVpnQuestions,
  isValidSubstrateGuest,
  selectedCanonicalUseCasesFromGoals,
  WizardPreviewController,
  type AccessModeValue,
  type AudienceConfigKey,
  type CanonicalUseCaseGoalKey,
  type GoalConfigKey,
  type ManagedProviderID,
  type StackConfig,
  type WizardPreviewState,
} from "#lib/wizard/index.js";
import { OwnerStepState } from "#lib/wizard/owner-state.svelte.js";
import type { WizardIssue } from "#lib/wizard/validation-context.js";
import { tr } from "#lib/i18n.svelte.js";
import { managedProviders } from "#lib/wizard/managed-providers.js";
import {
  managedCreationPauses,
  managedCreationScope,
} from "./creation-pause.svelte";
import type { CreationContext } from "./context";
import { USE_CASE_ACCESS_FLAG } from "./use-case-access";
import {
  createCreationSession,
  creationStepIssues,
  creationShownSteps,
  creationStepNumber,
  creationStepOrder,
  isCreationStepComplete,
  type CreationFacts,
  type CreationIssueField,
  type CreationProcess,
  type CreationSession,
  type CreationSessionState,
  type CreationStepKey,
} from "./core/index";
import type { HomelabServerSelection } from "./homelab-servers";

export interface CreationFrameOptions {
  process: CreationProcess;
  initialConfig?: StackConfig;
  isDeploying: boolean;
  /**
   * Keeps the entered answers and the step under this name, per owner, for
   * every frame and device (a process and its target). Without it nothing is
   * kept.
   */
  draftKey?: string;
  /** The deployment the flow adds to; with a layout flow it asks for the distribution. */
  layoutStackId?: string;
  /** A replacement a superseded run handed back; shown to be confirmed again. */
  layoutReplacement?: LayoutRecommendationView;
  /** Changes when the shown recommendation must be asked for again. */
  layoutRefresh: number;
  /** A recommendation names a StackKit; the frame opens the step it applies to. */
  onRecommendationOpen?: () => void;
  onmanagedproviderselect?: (provider: ManagedProviderID) => void;
  nodeOptions?: Snippet<[StackConfig]>;
  nodeAlternative?: Snippet;
  /** The person's existing servers, offered on the Server step. */
  homelab?: HomelabServerSelection;
}

function cloneConfig(value?: StackConfig): StackConfig | null {
  if (!value) return null;
  return JSON.parse(JSON.stringify(value)) as StackConfig;
}

export class CreationFrame {
  readonly context: CreationContext;
  readonly session: CreationSession;
  readonly options: CreationFrameOptions;
  /** Where this process starts; having moved on counts as work to lose. */
  readonly #startStep: CreationStepKey;

  readonly accessQuestions = getEasyAccessQuestions();
  readonly audienceQuestions = getEasyAudienceQuestions();
  readonly vpnQuestions = getVpnQuestions().filter(
    (option) => option.value !== "none",
  );
  readonly #goalQuestions = getEasyGoalQuestions();

  config = $state<StackConfig>(createDefaultConfig());
  nav = $state<CreationSessionState>(null as unknown as CreationSessionState);
  draftRestored = $state(false);
  /**
   * The kept draft is still being read. The steps wait for it, so a creation
   * continued from another frame opens on its step, not on the first one.
   */
  draftLoading = $state(false);
  /** Show validation only after interaction so initial rendering stays calm. */
  attempted = $state(false);
  /** Set by anything the person changes; ends once the submission is accepted. */
  userEdited = $state(false);
  /**
   * A card applying handed-over answers reports them like a choice; only what
   * follows the person's own first touch counts as something they could lose.
   */
  interacted = false;
  /** The Server step reports its own readiness; the value stays when it is left. */
  serverSelectionReady = $state(true);
  /**
   * Step 1 has two stages: the choice of use cases, then the configuration of
   * the chosen ones. Forward on the choice opens the configuration, Back there
   * returns to the choice; a process that comes for one use case has no choice.
   */
  goalsStage = $state<CreationStage>("pick");

  recommendationState = $state<WizardPreviewState>({ phase: "idle" });
  layoutState = $state<LayoutPreviewState>({ phase: "idle" });
  /** The owner's own server per use case ("Choose another server"). */
  layoutPins = $state<Record<string, string>>({});
  /** The choices before the last change, for undoing a refused one. */
  #previousLayoutPins: Record<string, string> = {};
  /**
   * Which products each use case is built from, straight from the StackKits
   * catalog. Cards render immediately, but only become selectable when this
   * pinned catalog advertises their applications.
   */
  useCaseCatalog = $state<Map<string, UseCaseCatalogView>>(new Map());
  /**
   * Loading, failed and ready are different answers. A failed read is never
   * shown as "Coming soon" and never lets the person continue without it.
   */
  catalogState = $state<"loading" | "ready" | "error">("loading");
  #catalogAttempt = $state(0);
  #workers = $state<Worker[]>([]);

  readonly owner: OwnerStepState;
  readonly #recommendations: WizardPreviewController;
  readonly #layouts: LayoutPreviewController;

  /** `options` reads live: pass getters, so an effect tracks only what it uses. */
  constructor(context: CreationContext, options: CreationFrameOptions) {
    this.context = context;
    this.options = options;

    const initial = untrack(() => ({
      process: options.process,
      draftKey: options.draftKey,
      initialConfig: options.initialConfig,
    }));
    this.draftLoading = Boolean(initial.draftKey);
    this.#initialApplied = Boolean(initial.initialConfig);
    this.config = this.#initialConfig(undefined, initial.initialConfig);

    this.session = createCreationSession({
      process: initial.process,
      isComplete: (key) => isCreationStepComplete(key, this.facts),
    });
    this.#startStep = this.session.activeSteps()[0] ?? "goals";
    const stopSession = this.session.subscribe((state) => {
      this.nav = state;
    });
    $effect(() => stopSession);

    this.owner = new OwnerStepState(() => this.config);
    this.#recommendations = new WizardPreviewController((state) => {
      this.recommendationState = state;
    });
    this.#layouts = new LayoutPreviewController((state) => {
      this.layoutState = state;
    });
    $effect(() => () => this.#recommendations.destroy());
    $effect(() => () => this.#layouts.destroy());

    $effect(() => {
      const process = options.process;
      untrack(() => this.session.setProcess(process));
    });

    this.#restoreDraft();
    this.#keepDraft();
    this.#adoptLateInitialConfig();
    this.#followRecommendations();
    this.#followLayout();
    this.#followOwner();
    this.#applyLaneDefaults();
    this.#followEverythingGoal();
    this.#followCreationPause();
    this.#loadCatalog();
    $effect(() => {
      void this.context.inventory
        .listWorkers()
        .then((workers) => {
          this.#workers = workers;
        })
        .catch(() => {
          this.#workers = [];
        });
    });
  }

  #followCreationPause() {
    $effect(() => {
      const scope = this.creationScope;
      if (scope && this.context.auth.isAuthenticated)
        untrack(() => void managedCreationPauses.refresh(scope));
    });
  }

  #initialConfig(restored?: StackConfig, stored?: StackConfig): StackConfig {
    if (restored) {
      const value = mergeOverDefaults(createDefaultConfig(), restored);
      applyPasskeyFirstOwnerLogin(value);
      return value;
    }
    const copy = cloneConfig(stored);
    if (copy) {
      applyPasskeyFirstOwnerLogin(copy);
      return copy;
    }
    const value = createDefaultConfig();
    if (value.goals) value.goals.files = true;
    return value;
  }

  get process(): CreationProcess {
    return this.options.process;
  }
  get current(): CreationStepKey {
    return this.nav.current;
  }
  /**
   * The Goals step shows its choice and something is chosen: forward opens the
   * configuration. An empty choice has nothing to configure and leaves the
   * step through the usual validation.
   */
  get goalsPicking(): boolean {
    return (
      this.nav.current === "goals" &&
      this.goalsStage === "pick" &&
      !this.process.focusGoal &&
      selectedCanonicalUseCasesFromGoals(this.config.goals).length > 0
    );
  }
  /** The Goals step shows its configuration; back returns to the choice. */
  get goalsConfiguring(): boolean {
    return (
      this.nav.current === "goals" &&
      this.goalsStage === "configure" &&
      !this.process.focusGoal
    );
  }
  get stepNumber(): number {
    return creationStepNumber(
      this.nav.current,
      creationShownSteps(this.process),
    );
  }
  get lastStep(): CreationStepKey {
    void this.nav;
    return this.session.lastStep();
  }
  get reachableSteps(): CreationStepKey[] {
    void this.nav;
    return this.session.reachableSteps();
  }

  readonly primaryGoalQuestions = $derived.by(() =>
    this.#goalQuestions.filter(
      (goal) => (goal.surface ?? "primary") === "primary",
    ),
  );
  readonly advancedGoalQuestions = $derived.by(() =>
    this.#goalQuestions.filter((goal) => goal.surface === "advanced"),
  );
  readonly #availableGoals = $derived.by(() =>
    creationGoals(
      [...this.primaryGoalQuestions, ...this.advancedGoalQuestions],
      this.useCaseCatalog,
    )
      .filter((card) => card.availability === "available")
      .flatMap((card) => (card.question ? [card.question.configKey] : []))
      .filter((goal): goal is CanonicalUseCaseGoalKey => goal !== "everything"),
  );

  // Read at the moment a step or a move asks, so each fact tracks only itself.
  readonly facts: CreationFacts = (() => {
    const frame = this;
    return {
      get requireGoal() {
        return frame.process.requireGoal;
      },
      get hasSelectedGoal() {
        return (
          selectedCanonicalUseCasesFromGoals(frame.config.goals).length > 0
        );
      },
      get catalogState() {
        return frame.catalogState;
      },
      get serverSelectionReady() {
        return frame.serverSelectionReady;
      },
      get existingServer() {
        return Boolean(frame.options.homelab?.selected);
      },
      get serverMode() {
        return frame.config.serverProvisioning.mode;
      },
      get hypervisorGuestValid() {
        return isValidSubstrateGuest(frame.config.serverProvisioning.substrate);
      },
      get remoteHost() {
        return frame.config.serverProvisioning.remote.host;
      },
      get usesWallet() {
        return frame.context.auth.walletSource === "kombify";
      },
      get walletItemChosen() {
        return Boolean(frame.config.serverProvisioning.remote.sshWalletItemId);
      },
      get accessMode() {
        return frame.config.network.accessMode;
      },
      get publicAccess() {
        return frame.config.network.publicAccess;
      },
      get inheritsAccess() {
        return frame.process.inheritsAccess;
      },
      get hasUsers() {
        const { onlyMe, familyFriends, public: open } = frame.config.audience;
        return onlyMe || familyFriends || open;
      },
      get ownerValid() {
        return frame.owner.adminIsValid();
      },
      get ownerIssues() {
        return frame.owner
          .ownerValidationErrors()
          .map((text) => ({ field: frame.#ownerField(text), text }));
      },
    };
  })();

  /** The managed provider and region a new server would use; read again when either changes. */
  readonly creationScope = $derived(
    this.facts.existingServer ? null : managedCreationScope(this.config),
  );
  readonly creationPause = $derived(
    managedCreationPauses.get(this.creationScope),
  );

  #ownerField(message: string): CreationIssueField {
    if (
      message === tr("wizard.login.profile.emailRequired") ||
      message === tr("ui.ownerState.emailIsRequiredForPasswordless")
    )
      return "owner-email";
    if (message === tr("ui.ownerState.recoveryPassphrasesDoNotMatch"))
      return "owner-recovery";
    return "step";
  }

  /** What stands in the way on the current step, blockers first. */
  readonly stepIssues: WizardIssue[] = $derived.by(() =>
    creationStepIssues(this.nav.current, this.facts).map((issue) => ({
      field: issue.field,
      message: issue.text ?? tr(issue.messageKey ?? ""),
    })),
  );
  readonly canGoNext = $derived.by(() =>
    isCreationStepComplete(this.nav.current, this.facts),
  );
  readonly earlierStepIncomplete = $derived.by(() => {
    void this.nav;
    return this.session.firstIncompleteBefore(this.nav.current) !== undefined;
  });
  readonly canDeploy = $derived.by(() => {
    const server = this.config.serverProvisioning;
    return (
      this.nav.current === this.lastStep &&
      !this.earlierStepIncomplete &&
      this.canGoNext &&
      !this.layoutChoiceUnaccepted &&
      !managedCreationPauses.holds(this.creationScope) &&
      (this.facts.existingServer ||
        server.mode !== "hypervisor" ||
        isValidSubstrateGuest(server.substrate))
    );
  });
  /** The first thing in the way, shown beside the blocked forward button. */
  readonly blockedReason = $derived.by(() => {
    const onLast = this.nav.current === this.lastStep;
    if (onLast ? this.canDeploy : this.canGoNext) return "";
    if (onLast && this.earlierStepIncomplete)
      return tr("wizard.stepper.locked");
    if (onLast && this.canGoNext && this.layoutChoiceUnaccepted)
      return tr("layout.pin.blocked");
    const pause = this.creationPause;
    if (onLast && pause && pause.state !== "ready")
      return tr("wizard.server.managed.pause.blocked", {
        provider:
          managedProviders.find((item) => item.value === pause.provider_id)
            ?.label ?? pause.provider_id,
      });
    return (
      this.stepIssues[0]?.message ?? tr("wizard.validation.stepIncomplete")
    );
  });
  /**
   * Protect entered answers against accidental departure. The config carries
   * SSH passwords and recovery material, which the draft never keeps (draftKey);
   * protection ends once the submission is accepted and stays in place after a
   * failed one.
   */
  readonly hasUnsavedAnswers = $derived.by(
    () =>
      (this.userEdited || this.nav.current !== this.#startStep) &&
      !this.options.isDeploying,
  );

  // Recommended distribution: only for a run against an existing deployment
  // with something selected. The preview follows the selection (debounced,
  // stale answers dropped); the run request carries the acceptable one.
  readonly #layoutEnabled = $derived.by(
    () =>
      Boolean(this.options.layoutStackId) &&
      this.process.inheritsAccess &&
      !this.options.nodeAlternative,
  );
  readonly layoutSelection = $derived.by(() =>
    this.#layoutEnabled && this.options.layoutStackId && this.process.layoutFlow
      ? buildLayoutSelection(
          this.config,
          this.options.layoutStackId,
          this.process.layoutFlow,
          this.layoutPins,
        )
      : null,
  );
  readonly layoutAcceptance = $derived.by(() =>
    this.#layoutEnabled
      ? acceptedLayoutFromPreview(this.layoutState, this.layoutSelection)
      : undefined,
  );
  /** The choices the current question carries (selected use cases only). */
  readonly activeLayoutPins = $derived.by(
    () => this.layoutSelection?.pins ?? {},
  );
  /**
   * An owner's server choice that is not (yet) a verified recommendation: the
   * run would ignore it, so it cannot be submitted until it is confirmed,
   * changed or dropped.
   */
  readonly layoutChoiceUnaccepted = $derived.by(
    () =>
      Object.keys(this.activeLayoutPins).length > 0 && !this.layoutAcceptance,
  );
  /** The deployment the accepted recommendation adds to; it may be another one of the homelab. */
  readonly layoutTarget = $derived.by(() =>
    this.layoutAcceptance
      ? layoutRunTarget(layoutPreviewView(this.layoutState))
      : undefined,
  );
  get showsLayoutSummary(): boolean {
    return (
      this.#layoutEnabled &&
      this.nav.current === this.lastStep &&
      this.nav.current !== "goals" &&
      this.layoutState.phase !== "idle"
    );
  }

  retryLayout = () => this.#layouts.retry(this.layoutSelection);
  /**
   * Pin a use case to a server, or drop the pin (null). The changed question
   * is re-solved and re-verified; only that answer can be accepted.
   */
  setLayoutPin = (goal: string, serverId: string | null) => {
    const next = { ...this.layoutPins };
    if (serverId) next[goal] = serverId;
    else delete next[goal];
    this.#previousLayoutPins = this.layoutPins;
    this.layoutPins = next;
  };
  /** Restores the choices before the last change (a refused choice). */
  undoLayoutPin = () => {
    this.layoutPins = this.#previousLayoutPins;
    this.#previousLayoutPins = {};
  };
  clearLayoutPins = () => {
    this.#previousLayoutPins = this.layoutPins;
    this.layoutPins = {};
  };
  reloadCatalog = () => {
    this.#catalogAttempt += 1;
  };

  // -- what the person changes -----------------------------------------------

  /**
   * A decision about a use case. Stored only when set explicitly; the catalog
   * keeps the defaults. The frame owns the answers, the step only reports.
   */
  setUseCaseSetting = (
    goal: GoalConfigKey,
    settingId: string,
    value: string | boolean,
  ) => {
    if (this.interacted) this.userEdited = true;
    this.config.useCaseSettings ??= {};
    this.config.useCaseSettings[goal] = {
      ...(this.config.useCaseSettings[goal] ?? {}),
      [settingId]: value,
    };
  };

  setGoal = (goal: GoalConfigKey, value: boolean) => {
    if (this.interacted) this.userEdited = true;
    const goals = this.config.goals;
    if (!goals) return;
    const available = this.#availableGoals;
    goals[goal] = value;
    // If "everything" is selected, toggle all currently available use cases.
    if (goal === "everything") {
      for (const useCase of available) goals[useCase] = value;
      goals.everything = value;
    } else if (!available.includes(goal)) {
      goals[goal] = false;
      goals.everything = false;
    } else if (!value) {
      goals.everything = false;
    } else if (
      available.length > 0 &&
      available.every((useCase) => goals[useCase])
    ) {
      goals.everything = true;
    }
  };

  setAccessMode = (value: AccessModeValue) => {
    if (this.interacted) this.userEdited = true;
    this.config.network.accessMode = value;
    this.config.network.accessModeSelected = true;
  };

  setAudience = (audience: AudienceConfigKey, value: boolean) => {
    if (this.interacted) this.userEdited = true;
    this.config.audience[audience] = value;
    if (audience === "public") this.config.network.publicAccess = value;
  };

  setHouseholdProfile = (profile: HouseholdProfile) => {
    if (this.interacted) this.userEdited = true;
    this.config.household = {
      profile,
      people: this.config.household?.people ?? [],
    };
    this.config.audience.onlyMe = profile === "solo";
    this.config.audience.familyFriends = profile !== "solo";
  };

  setHouseholdPeople = (people: HouseholdPersonDraft[]) => {
    if (this.interacted) this.userEdited = true;
    this.config.household = {
      profile:
        this.config.household?.profile ??
        (this.config.audience.onlyMe ? "solo" : "shared"),
      people,
    };
  };

  // -- creating --------------------------------------------------------------

  enforceExistingOwnerReuse() {
    if (!this.process.reusesOwner) return;
    const { owner, auth } = this.config;
    owner.bootstrapMode = "none";
    owner.source = "local";
    owner.username = "";
    owner.email = "";
    owner.displayName = "";
    owner.recoveryPassphraseHash = "";
    owner.recoveryMaterialRef = "";
    auth.requirePassword = false;
    auth.requireMfa = false;
    auth.allowPasswordless = false;
  }

  /** The answers as they go into the run; the owner's secrets are ready by now. */
  /** Access per use case is offered only behind its feature flag. */
  get useCaseAccessEnabled(): boolean {
    return this.context.features.isEnabled(USE_CASE_ACCESS_FLAG);
  }

  finalizedConfig(): StackConfig {
    const config = this.config;
    // Without the flag no access decision leaves the wizard, also none a
    // kept draft still carries.
    if (!this.useCaseAccessEnabled) delete config.useCaseAccess;
    config.wizardType = "easy";
    const existingOverlay = config.services.headscale;
    deriveServicesFromGoals(config);
    // A joined Node inherits access from its Homelab; the hidden first-run
    // access default must not turn on a new overlay during goal derivation.
    if (this.process.inheritsAccess)
      config.services.headscale = existingOverlay;
    return config;
  }

  /**
   * Ends the draft together with a save still pending from the last change:
   * the wizard stays on screen while the next page loads, and that save would
   * otherwise write the answers back for the next creation in this tab.
   */
  forgetDraft() {
    this.#forgotten = true;
    clearTimeout(this.#pendingSave);
    this.#pendingSave = undefined;
    const key = this.options.draftKey;
    if (key) void clearWizardDraft(key);
  }

  /**
   * Writes a change still waiting for its debounce now, for a move to another
   * frame ("Continue in Homelab") right after the last answer.
   */
  async flushDraft(): Promise<void> {
    const key = this.options.draftKey;
    if (!key) return;
    const pending = this.#pendingSave;
    if (pending !== undefined) {
      clearTimeout(pending);
      this.#pendingSave = undefined;
      void this.#writeDraft();
    }
    await wizardDraftWritten(key);
  }
  #pendingSave: ReturnType<typeof setTimeout> | undefined;
  /** The person left; nothing writes the ended draft back. */
  #forgotten = false;
  /** A draft was found, or the person started; from then on every change is kept. */
  #keeping = false;
  /** The step the last write carried; a new step is written at once. */
  #writtenStep: CreationStepKey | undefined;
  #initialApplied = false;
  #laneApplied = false;

  #writeDraft(
    config = $state.snapshot(this.config) as StackConfig,
  ): Promise<void> {
    const { draftKey, isDeploying } = this.options;
    if (!draftKey || isDeploying || this.#forgotten) return Promise.resolve();
    const draft: WizardDraft = {
      step: this.nav.current,
      furthest: this.nav.furthest,
      config,
    };
    this.#writtenStep = draft.step;
    return saveWizardDraft(
      draftKey,
      this.process.key,
      draft,
      creationStepOrder(this.process),
    );
  }

  // -- what follows the answers ----------------------------------------------

  /**
   * Reads the draft another frame or device left for this process. It applies
   * only while the person has not started here; otherwise their own answers
   * win and overwrite it with the next change. A link that asks for a use case
   * the draft does not hold starts over: it names a new creation, not this one.
   */
  #restoreDraft() {
    let started = false;
    $effect(() => {
      const key = this.options.draftKey;
      if (!key || started) return;
      const { isAuthenticated, modeDetected } = this.context.auth;
      if (!isAuthenticated) {
        // Without a session there is no owner to keep a draft for.
        if (modeDetected) this.draftLoading = false;
        return;
      }
      // The server choice decides the process and so which kept steps exist:
      // the draft waits until the person's servers were read and one chosen.
      if (this.options.homelab?.loading) return;
      started = true;
      void loadWizardDraft(key, creationStepOrder(this.process)).then(
        (draft) => {
          if (
            draft &&
            !this.userEdited &&
            this.nav.current === this.#startStep &&
            this.#linkFitsDraft(draft)
          )
            this.#applyDraft(draft);
          this.draftLoading = false;
        },
      );
    });
  }

  #linkFitsDraft(draft: WizardDraft): boolean {
    const kept = new Set(
      selectedCanonicalUseCasesFromGoals(draft.config.goals),
    );
    const linked = untrack(() => this.options.initialConfig)?.goals;
    return selectedCanonicalUseCasesFromGoals(linked).every((goal) =>
      kept.has(goal),
    );
  }

  #applyDraft(draft: WizardDraft) {
    this.config = this.#initialConfig(draft.config);
    this.#initialApplied = true;
    this.#laneApplied = true;
    const active = this.session.activeSteps();
    // A frame without the kept step (one step alone) only writes once the
    // person changes something, so opening it never moves the draft back.
    this.#keeping = active.includes(draft.step);
    if (active.includes(draft.furthest)) this.session.open(draft.furthest);
    if (active.includes(draft.step)) this.session.open(draft.step);
    this.draftRestored = true;
  }

  #keepDraft() {
    // Keep the non-secret answers while the person works. Nothing is written
    // before the kept draft was read, so opening a frame never replaces it.
    $effect(() => {
      const { draftKey, isDeploying } = this.options;
      if (!draftKey || isDeploying || this.draftLoading) return;
      const answers = $state.snapshot(this.config) as StackConfig;
      const step = this.nav.current;
      if (!this.userEdited && step === this.#startStep && !this.#keeping)
        return;
      this.#keeping = true;
      untrack(() => {
        // A step change is kept at once, so another frame or a reload right
        // after it opens the same step; typed answers follow after a pause.
        if (step !== this.#writtenStep) {
          this.#pendingSave = undefined;
          void this.#writeDraft(answers);
          return;
        }
        const timer = setTimeout(() => {
          this.#pendingSave = undefined;
          void this.#writeDraft(answers);
        }, 600);
        this.#pendingSave = timer;
      });
      return () => clearTimeout(this.#pendingSave);
    });

    // Leaving the frame keeps a change still waiting for its pause.
    $effect(() => () => {
      if (this.#pendingSave !== undefined) void this.#writeDraft();
    });

    // An accepted submission ends the draft; a failed one keeps it.
    $effect(() => () => {
      if (untrack(() => this.options.isDeploying)) this.forgetDraft();
    });
  }

  #adoptLateInitialConfig() {
    $effect(() => {
      const initial = this.options.initialConfig;
      if (!initial || this.#initialApplied) return;
      this.#initialApplied = true;
      this.config = cloneConfig(initial) ?? createDefaultConfig();
      applyPasskeyFirstOwnerLogin(this.config);
    });
  }

  /** The question the recommendations are asked now; null where none is asked. */
  readonly #recommendationRequest =
    $derived.by<WizardRecommendationRequest | null>(() => {
      if (this.process.reusesOwner) return null;
      const config = this.config;
      const server = config.serverProvisioning;
      const smartHome = config.useCaseSettings?.["smart-home"];
      const selectedServer = this.options.homelab?.selected;
      const selectedWorker = selectedServer
        ? this.#workers.find(
            (worker) =>
              worker.id === selectedServer ||
              worker.server_id === selectedServer,
          )
        : undefined;
      const selectedServerRamMb = selectedWorker?.ram_mb;
      const allowManagedOffering =
        this.current === "server" || Boolean(selectedServer);
      const fitEnabled = this.context.features.isEnabled("server_hardware_fit");
      return {
        smart_home_settings: smartHome,
        smart_home_context: {
          existing: smartHome?.["instance-origin"] === "existing",
          needs_radio: smartHome?.["device-passthrough"] === true,
          ...(server.mode === "hypervisor" &&
          server.substrate?.advisoryInventory
            ? {
                proxmox_available: true,
                lan_reachable: false,
                cpu: server.substrate.advisoryInventory.cpu,
                memory_mib: server.substrate.advisoryInventory.memoryMiB,
                disk_gib: server.substrate.advisoryInventory.diskGiB,
              }
            : {}),
        },
        goals: selectedCanonicalUseCasesFromGoals(config.goals),
        use_case_settings: config.useCaseSettings,
        use_case_capabilities: config.useCaseCapabilities,
        services: [],
        deployment_lane:
          this.context.auth.lane === "saas" ? "saas" : "self-hosted",
        ...(server.mode === "kombify-cloud"
          ? { provider_id: config.providerId }
          : {}),
        ...capacityRequestHost(config, {
          allowManagedOffering,
          selectedServerRamMb,
        }),
        ...(fitEnabled
          ? selectedServer
            ? {
                server_fit: {
                  server_id: selectedServer,
                  source: "guard" as const,
                },
              }
            : server.mode === "kombify-cloud" && config.runtimeOfferingId
              ? {
                  server_fit: {
                    provider_id: config.providerId,
                    offering_id: config.runtimeOfferingId,
                    source: "managed",
                  },
                }
              : server.mode === "hypervisor"
                ? {
                    server_fit: {
                      source: "substrate" as const,
                      ...(server.substrate?.serverId
                        ? { host: server.substrate.serverId }
                        : {}),
                      ...(server.substrate &&
                      (server.substrate.memoryMiB > 0 ||
                        server.substrate.advisoryInventory)
                        ? {
                            observed_capacity: {
                              cpu_cores:
                                server.substrate.cpu ||
                                server.substrate.advisoryInventory?.cpu ||
                                0,
                              memory_mb:
                                server.substrate.memoryMiB ||
                                server.substrate.advisoryInventory?.memoryMiB ||
                                0,
                              disk_gb:
                                server.substrate.diskGiB ||
                                server.substrate.advisoryInventory?.diskGiB ||
                                0,
                              source: "substrate",
                              usage_observed: true,
                            },
                          }
                        : {}),
                    },
                  }
                : server.mode === "connect-remote"
                  ? {
                      server_fit: {
                        source: "ssh" as const,
                        ...(server.remote.host.trim()
                          ? { host: server.remote.host.trim() }
                          : {}),
                      },
                    }
                  : {
                      server_fit: {
                        source: "install-command" as const,
                      },
                    }
          : {}),
        surface: "easy",
      };
    });

  #followRecommendations() {
    $effect(() => {
      const request = this.#recommendationRequest;
      if (request) this.#recommendations.update(request);
    });
  }

  /**
   * Whether the selected use cases fit the chosen server, as advice (never a
   * gate): the capacity of the recommendations answer to exactly this
   * selection and server; while a changed question is being asked there is no
   * answer, never the earlier one.
   */
  readonly #capacityAnswer = $derived.by<
    { assessed: boolean; capacity?: WizardCapacity } | undefined
  >(() => {
    const request = this.#recommendationRequest;
    const state = this.recommendationState;
    // A server whose size is not known yet (to be connected, one-liner) or
    // nothing selected gets no advice: the answer would be about another question.
    if (
      !request ||
      selectedCanonicalUseCasesFromGoals(this.config.goals).length === 0
    )
      return undefined;
    // Only the answer to the question on screen now counts: while a changed
    // selection or server is being asked (or before the state caught up with
    // it) nothing is shown, never the earlier answer.
    if (state.phase === "idle" || state.key !== wizardRequestKey(request))
      return undefined;
    if (state.phase === "resolved")
      return { assessed: true, capacity: state.result.capacity };
    // A failed answer is not a fit, and is said so.
    return state.phase === "degraded" ? { assessed: true } : undefined;
  });
  readonly capacityHint = $derived.by<CapacityHint | undefined>(() => {
    const answer = this.#capacityAnswer;
    if (!answer) return undefined;
    return capacityHint(answer.capacity, {
      currentOfferingId: this.config.runtimeOfferingId,
      managed: this.config.serverProvisioning.mode === "kombify-cloud",
      assessed: answer.assessed,
      existingRamMb:
        this.current === "goals" && !this.options.homelab?.selected
          ? this.#workers
              .filter((worker) => worker.approved && (worker.ram_mb ?? 0) > 0)
              .reduce((total, worker) => total + (worker.ram_mb ?? 0), 0)
          : undefined,
    });
  });
  /** "Use this size": the larger managed size carries the selection. */
  useCapacitySize = (size: CapacitySize) => {
    if (this.interacted) this.userEdited = true;
    this.config.runtimeOfferingId = size.id;
  };
  /** "Leave out": the use case is not selected; the answer follows the selection. */
  leaveOutForCapacity = (goal: string) => {
    const selected = selectedCanonicalUseCasesFromGoals(this.config.goals).find(
      (key) => key === goal,
    );
    if (selected) this.setGoal(selected, false);
  };
  /** Re-runs the current server capacity check, bypassing any cached answer. */
  recheckServerFit = () => {
    const request = this.#recommendationRequest;
    if (request) this.#recommendations.refresh(request);
  };
  /** Applies one actionable fit recovery without erasing the user's choices. */
  applyFitRecovery = (recovery: {
    action: string;
    offering_id?: string;
    profile?: string;
    goal?: string;
  }) => {
    if (this.interacted) this.userEdited = true;
    switch (recovery.action) {
      case "switch_offering":
        if (
          recovery.offering_id === "monthly-runtime-standard" ||
          recovery.offering_id === "monthly-runtime-premium" ||
          recovery.offering_id === "monthly-runtime-large"
        ) {
          this.config.runtimeOfferingId = recovery.offering_id;
        }
        break;
      case "switch_profile":
        if (
          recovery.goal === "photos" &&
          (recovery.profile === "immich-lite" || recovery.profile === "ente-photos")
        ) {
          this.setUseCaseSetting("photos", "backend", recovery.profile);
        } else {
          this.goalsStage = "configure";
          this.session.open("goals");
        }
        break;
      case "omit_use_case":
        if (recovery.goal) this.leaveOutForCapacity(recovery.goal);
        break;
      case "refresh_inventory":
        this.recheckServerFit();
        break;
      case "review_selection":
        this.goalsStage = "configure";
        this.session.open("goals");
        break;
    }
  };

  get recommendedStackKit(): string | undefined {
    const state = this.recommendationState;
    const result =
      state.phase === "resolved"
        ? state.result
        : state.phase === "loading" || state.phase === "degraded"
          ? state.previous
          : undefined;
    return (
      result?.recommendations.find((item) => item.recommended) ??
      result?.recommendations[0]
    )?.stackkit;
  }

  #followLayout() {
    $effect(() => {
      const selection = this.layoutSelection;
      untrack(() => this.#layouts.update(selection));
    });
    $effect(() => {
      const replacement = this.options.layoutReplacement;
      if (!replacement) return;
      untrack(() => {
        if (this.layoutSelection)
          this.#layouts.adopt(this.layoutSelection, replacement);
      });
    });
    let refreshSeen = untrack(() => this.options.layoutRefresh);
    $effect(() => {
      const requested = this.options.layoutRefresh;
      if (requested === refreshSeen) return;
      refreshSeen = requested;
      untrack(() => this.#layouts.retry(this.layoutSelection));
    });
  }

  #followOwner() {
    $effect(() => {
      if (this.process.reusesOwner) return;
      this.owner.setSignedInAccount(this.context.auth.signedInAccount);
    });
    $effect(() => {
      if (!this.process.reusesOwner) return;
      const { owner, auth } = this.config;
      if (
        owner.bootstrapMode === "none" &&
        owner.source === "local" &&
        owner.username === "" &&
        owner.email === "" &&
        owner.displayName === "" &&
        owner.recoveryPassphraseHash === "" &&
        owner.recoveryMaterialRef === "" &&
        !auth.requirePassword &&
        !auth.requireMfa &&
        !auth.allowPasswordless
      )
        return;
      this.enforceExistingOwnerReuse();
    });
  }

  #applyLaneDefaults() {
    $effect(() => {
      if (
        this.#laneApplied ||
        !this.process.appliesLaneDefaults ||
        !this.context.auth.modeDetected
      )
        return;
      applyWizardDeploymentLane(this.config, this.context.auth.lane);
      this.#laneApplied = true;
    });
  }

  #followEverythingGoal() {
    $effect(() => {
      const goals = this.config.goals;
      if (!goals) return;
      // Catalog loading or a temporarily unavailable release must not erase
      // saved wishes. Runtime admission still validates every submitted goal.
      const available = this.#availableGoals;
      const allSelected =
        available.length > 0 && available.every((goal) => goals[goal]);
      if (goals.everything !== allSelected) goals.everything = allSelected;
    });
  }

  #loadCatalog() {
    $effect(() => {
      void this.#catalogAttempt;
      let cancelled = false;
      this.catalogState = "loading";
      loadUseCaseCatalog().then(
        (catalog) => {
          if (cancelled) return;
          this.useCaseCatalog = catalog;
          this.catalogState = "ready";
        },
        () => {
          if (!cancelled) this.catalogState = "error";
        },
      );
      return () => {
        cancelled = true;
      };
    });
  }
}
