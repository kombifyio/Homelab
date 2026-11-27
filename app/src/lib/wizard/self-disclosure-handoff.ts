/**
 * kombify Cloud → creation wizard self-disclosure handoff.
 *
 * The Cloud dashboard captures a voluntary self-description and opens
 * `/stacks/new` with the raw answers as query parameters. This module reads
 * them back into the closed Homelab vocabulary, drops anything unknown, and
 * derives the wizard prefill and the review the operator confirms. A link only
 * prefills: anyone can craft one, so it carries no free text and nothing is
 * stored until the operator confirms the answers. It never computes a
 * capability score: the Unifier derives the profile from the stored answers
 * (KOMBIFY-DESIGN-SYSTEM-STANDARD.md §9.4).
 */
import type {
  OperatorSelfDisclosure,
  SelfDisclosureExperience,
} from "#lib/api/operatorProfile.js";
import type { MessageKey } from "#lib/i18n.js";
import {
  CANONICAL_USE_CASE_GOALS,
  getEasyGoalQuestions,
} from "./standardBundle.js";
import { createDefaultConfig, type StackConfig } from "./types.js";

const EXPERIENCES: readonly SelfDisclosureExperience[] = [
  "hands-off",
  "guided",
  "curious",
  "techie",
  "expert",
];
const PLACEMENTS = ["home", "rented", "both", "unsure"] as const;
const HARDWARE = [
  "none-yet",
  "old-pc",
  "mini-pc",
  "nas",
  "raspberry-pi",
  "rented-server",
] as const;
const HOUSEHOLDS = [
  "just-me",
  "household",
  "friends-family",
  "small-team",
] as const;
const MOTIVATIONS = [
  "privacy",
  "replace-subscriptions",
  "learning",
  "family",
  "projects",
] as const;
/** Upper bound of the free-text note the operator types in the review. */
export const SELF_DISCLOSURE_NOTES_MAX_CHARS = 2_000;

/**
 * Query parameters a handoff link may carry. `intent` is free text that older
 * Cloud links sent; it is never read, only stripped from the URL.
 */
export const SELF_DISCLOSURE_PARAMS = [
  "experience",
  "goals",
  "placement",
  "hardware",
  "household",
  "motivations",
  "intent",
] as const;

/** Read access is all the parser needs; SvelteKit's `page.url` is read-only. */
type HandoffParams = Pick<URLSearchParams, "get">;

function one<T extends string>(
  params: HandoffParams,
  key: string,
  allowed: readonly T[],
): T | undefined {
  const value = params.get(key)?.trim().toLowerCase();
  return allowed.find((candidate) => candidate === value);
}

function many<T extends string>(
  params: HandoffParams,
  key: string,
  allowed: readonly T[],
): T[] | undefined {
  const values = (params.get(key) ?? "")
    .split(",")
    .map((value) => value.trim().toLowerCase());
  const result = allowed.filter((candidate) => values.includes(candidate));
  return result.length > 0 ? result : undefined;
}

/**
 * Read a Cloud handoff from the page URL. Returns null when the URL carries no
 * recognizable answer, so a plain `/stacks/new` visit stays untouched. Free
 * text is ignored: notes come only from what the operator types in Homelab.
 */
export function readSelfDisclosureHandoff(
  params: HandoffParams,
): OperatorSelfDisclosure | null {
  const disclosure: OperatorSelfDisclosure = {
    experience: one(params, "experience", EXPERIENCES),
    goals: many(params, "goals", CANONICAL_USE_CASE_GOALS),
    placement: one(params, "placement", PLACEMENTS),
    hardware: many(params, "hardware", HARDWARE),
    household: one(params, "household", HOUSEHOLDS),
    motivations: many(params, "motivations", MOTIVATIONS),
  };
  const answered = Object.values(disclosure).some(
    (value) => value !== undefined,
  );
  return answered ? disclosure : null;
}

/** The canonical creation selection, excluding profile and host parameters. */
export function creationUseCaseHandoffKey(params: HandoffParams): string {
  return one(params, "use-case", CANONICAL_USE_CASE_GOALS) ?? "";
}

/** A use-case navigation choice seeds only this creation draft, never a profile. */
export function creationUseCaseWizardConfig(
  params: HandoffParams,
): StackConfig | undefined {
  const selected = creationUseCaseHandoffKey(params);
  if (!selected) return undefined;
  const config = createDefaultConfig();
  if (config.goals) {
    for (const goal of CANONICAL_USE_CASE_GOALS) {
      config.goals[goal] = goal === selected;
    }
    config.goals.everything = false;
  }
  return config;
}

/** One reviewed question with the message keys of its handed-over answers. */
export interface SelfDisclosureReviewRow {
  question: SelfDisclosureQuestion;
  questionKey: MessageKey;
  answerKeys: MessageKey[];
}

type SelfDisclosureQuestion =
  | "experience"
  | "goals"
  | "placement"
  | "hardware"
  | "household"
  | "motivations";

const EXPERIENCE_KEYS: Record<SelfDisclosureExperience, MessageKey> = {
  "hands-off": "wizard.selfDisclosure.experience.hands-off",
  guided: "wizard.selfDisclosure.experience.guided",
  curious: "wizard.selfDisclosure.experience.curious",
  techie: "wizard.selfDisclosure.experience.techie",
  expert: "wizard.selfDisclosure.experience.expert",
};

const PLACEMENT_KEYS: Record<(typeof PLACEMENTS)[number], MessageKey> = {
  home: "wizard.selfDisclosure.placement.home",
  rented: "wizard.selfDisclosure.placement.rented",
  both: "wizard.selfDisclosure.placement.both",
  unsure: "wizard.selfDisclosure.placement.unsure",
};

const HARDWARE_KEYS: Record<(typeof HARDWARE)[number], MessageKey> = {
  "none-yet": "wizard.selfDisclosure.hardware.none-yet",
  "old-pc": "wizard.selfDisclosure.hardware.old-pc",
  "mini-pc": "wizard.selfDisclosure.hardware.mini-pc",
  nas: "wizard.selfDisclosure.hardware.nas",
  "raspberry-pi": "wizard.selfDisclosure.hardware.raspberry-pi",
  "rented-server": "wizard.selfDisclosure.hardware.rented-server",
};

const HOUSEHOLD_KEYS: Record<(typeof HOUSEHOLDS)[number], MessageKey> = {
  "just-me": "wizard.selfDisclosure.household.just-me",
  household: "wizard.selfDisclosure.household.household",
  "friends-family": "wizard.selfDisclosure.household.friends-family",
  "small-team": "wizard.selfDisclosure.household.small-team",
};

const MOTIVATION_KEYS: Record<(typeof MOTIVATIONS)[number], MessageKey> = {
  privacy: "wizard.selfDisclosure.motivation.privacy",
  "replace-subscriptions":
    "wizard.selfDisclosure.motivation.replace-subscriptions",
  learning: "wizard.selfDisclosure.motivation.learning",
  family: "wizard.selfDisclosure.motivation.family",
  projects: "wizard.selfDisclosure.motivation.projects",
};

const QUESTION_KEYS: Record<SelfDisclosureQuestion, MessageKey> = {
  experience: "wizard.selfDisclosure.question.experience",
  goals: "wizard.selfDisclosure.question.goals",
  placement: "wizard.selfDisclosure.question.placement",
  hardware: "wizard.selfDisclosure.question.hardware",
  household: "wizard.selfDisclosure.question.household",
  motivations: "wizard.selfDisclosure.question.motivations",
};

/**
 * The handed-over answers as message keys, in capture order, so the operator
 * sees exactly what a confirmation would store.
 */
export function selfDisclosureReviewRows(
  disclosure: OperatorSelfDisclosure,
): SelfDisclosureReviewRow[] {
  const goalTitles = new Map<string, MessageKey>(
    getEasyGoalQuestions().map((question) => [question.key, question.titleKey]),
  );
  const rows: [SelfDisclosureQuestion, MessageKey[]][] = [
    [
      "experience",
      disclosure.experience ? [EXPERIENCE_KEYS[disclosure.experience]] : [],
    ],
    [
      "goals",
      (disclosure.goals ?? []).flatMap((goal) => {
        const key = goalTitles.get(goal);
        return key ? [key] : [];
      }),
    ],
    [
      "placement",
      disclosure.placement && disclosure.placement in PLACEMENT_KEYS
        ? [
            PLACEMENT_KEYS[
              disclosure.placement as (typeof PLACEMENTS)[number]
            ],
          ]
        : [],
    ],
    [
      "hardware",
      (disclosure.hardware ?? []).flatMap((item) =>
        item in HARDWARE_KEYS
          ? [HARDWARE_KEYS[item as (typeof HARDWARE)[number]]]
          : [],
      ),
    ],
    [
      "household",
      disclosure.household && disclosure.household in HOUSEHOLD_KEYS
        ? [
            HOUSEHOLD_KEYS[
              disclosure.household as (typeof HOUSEHOLDS)[number]
            ],
          ]
        : [],
    ],
    [
      "motivations",
      (disclosure.motivations ?? []).flatMap((item) =>
        item in MOTIVATION_KEYS
          ? [MOTIVATION_KEYS[item as (typeof MOTIVATIONS)[number]]]
          : [],
      ),
    ],
  ];
  return rows
    .filter(([, answerKeys]) => answerKeys.length > 0)
    .map(([question, answerKeys]) => ({
      question,
      questionKey: QUESTION_KEYS[question],
      answerKeys,
    }));
}

/**
 * Wizard prefill for a handed-off self-disclosure. Only answers that map onto
 * an existing wizard question are applied; everything stays editable.
 */
export function selfDisclosureWizardConfig(
  disclosure: OperatorSelfDisclosure,
): StackConfig {
  const config = createDefaultConfig();
  if (config.goals && disclosure.goals?.length) {
    for (const goal of CANONICAL_USE_CASE_GOALS) {
      config.goals[goal] = disclosure.goals.includes(goal);
    }
  } else if (config.goals) {
    config.goals.files = true;
  }
  if (disclosure.household && disclosure.household !== "just-me") {
    config.household = {
      profile: "shared",
      people: config.household?.people ?? [],
    };
  }
  return config;
}

/** The page URL without the handoff parameters, for history replacement. */
export function withoutSelfDisclosureParams(url: Pick<URL, "href">): string {
  const next = new URL(url.href);
  for (const key of SELF_DISCLOSURE_PARAMS) next.searchParams.delete(key);
  return `${next.pathname}${next.search}${next.hash}`;
}
