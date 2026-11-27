import {
  type CreationGoal,
  type CreationGoalId,
} from "./creationGoals.js";
import { installsService } from "#lib/api/useCaseCatalog.js";
import type { GoalConfigKey } from "./standardBundle.js";

/** Selection templates (profiles) above the use case cards: one click selects
 * the profile's fixed set of cards (workspace KOMBIFY-DESIGN-SYSTEM-STANDARD.md
 * §9). A template only ever selects cards; it never decides deployment
 * eligibility, so a goal the release catalog does not offer yet is skipped and
 * reported. Several profiles may be on together; each has its own colour, and
 * a chosen card's ring shows the colours of the profiles that hold it. */
export type CreationTemplateIcon = "house" | "folder" | "briefcase" | "rocket";

export const CREATION_TEMPLATES = [
  {
    id: "family",
    icon: "house",
    titleKey: "wizard.templates.family.title",
    descriptionKey: "wizard.templates.family.description",
    color: "oklch(0.72 0.16 60)",
    goals: ["photos", "files", "media", "smart-home", "vault"],
  },
  {
    id: "documents-media",
    icon: "folder",
    titleKey: "wizard.templates.documentsMedia.title",
    descriptionKey: "wizard.templates.documentsMedia.description",
    color: "oklch(0.6 0.17 250)",
    goals: ["files", "photos", "media"],
  },
  {
    id: "self-employed",
    icon: "briefcase",
    titleKey: "wizard.templates.selfEmployed.title",
    descriptionKey: "wizard.templates.selfEmployed.description",
    color: "oklch(0.56 0.2 300)",
    goals: ["files", "vault", "mail", "ai", "automation"],
  },
  {
    // "To infinity and beyond": the full homelab in one click, every use case
    // the catalog offers. It reuses the existing `everything` goal, which the
    // sizing requirements already read.
    id: "infinity",
    icon: "rocket",
    titleKey: "wizard.goals.everything.title",
    descriptionKey: "wizard.goals.everything.description",
    color: "oklch(0.66 0.17 150)",
    goals: "all",
  },
] as const satisfies readonly {
  id: string;
  icon: CreationTemplateIcon;
  titleKey: string;
  descriptionKey: string;
  /** The profile's own colour: its chip and its share of a card's ring. */
  color: string;
  goals: readonly CreationGoalId[] | "all";
}[];

/** A small, executable starting point; the remaining intentions stay visible. */
export const STANDARD_STARTS: Record<
  CreationTemplateId,
  {
    goals: readonly CreationGoalId[];
    expansion: readonly CreationGoalId[];
  }
> = {
  family: {
    goals: ["photos", "files", "media", "vault"],
    expansion: ["smart-home"],
  },
  "documents-media": { goals: ["files", "photos", "media"], expansion: [] },
  "self-employed": {
    goals: ["files", "vault", "mail", "automation"],
    expansion: ["ai"],
  },
  infinity: {
    goals: ["files", "vault", "mail", "dev"],
    expansion: [
      "photos",
      "media",
      "smart-home",
      "ai",
      "game",
      "network",
      "automation",
      "marketplace",
    ],
  },
};

export function profileExpansionGoals(
  id: CreationTemplateId,
): readonly CreationGoalId[] {
  return STANDARD_STARTS[id].expansion;
}

export type CreationTemplate = (typeof CREATION_TEMPLATES)[number];
export type CreationTemplateId = CreationTemplate["id"];

export interface TemplateSelection {
  /** Goal keys the template turns on, in card order. */
  goals: GoalConfigKey[];
  /** Template goals the current catalog cannot offer yet. */
  skipped: CreationGoalId[];
}

export interface TemplateStartOptions {
  /** Target host has verified LAN/device reach for local Smart Home. */
  lanReachable?: boolean;
  /** Target host meets the >=12 GiB / 4 vCPU local AI floor. */
  supportsLocalAI?: boolean;
}

export function templateSelection(
  template: CreationTemplate,
  cards: CreationGoal[],
  standardStart = false,
  options: TemplateStartOptions = {},
): TemplateSelection {
  const startGoals = (id: CreationTemplateId): readonly CreationGoalId[] => {
    const base = STANDARD_STARTS[id].goals;
    if (id === "family" && options.lanReachable) {
      return [...base, "smart-home"];
    }
    if (id === "self-employed" && options.supportsLocalAI) {
      return [...base, "ai"];
    }
    return base;
  };
  const wanted = (id: CreationGoalId) =>
    standardStart
      ? startGoals(template.id).includes(id)
      : template.goals === "all" ||
        (template.goals as readonly CreationGoalId[]).includes(id);
  const eligible = (card: CreationGoal) =>
    card.availability === "available" &&
    !!card.question &&
    (!standardStart ||
      card.id !== "photos" ||
      card.catalog?.components.some((component) => installsService(component)));
  const goals = cards
    .filter((card) => eligible(card) && wanted(card.id))
    .flatMap((card) => (card.question ? [card.question.configKey] : []));
  const skipped = cards
    .filter((card) => wanted(card.id) && !eligible(card))
    .map((card) => card.id);
  return {
    goals:
      !standardStart && template.goals === "all"
        ? [...goals, "everything"]
        : goals,
    skipped,
  };
}

const EMPTY: TemplateSelection = { goals: [], skipped: [] };

/** A template's own goals, without the "everything" marker. */
function ownGoals(selection: TemplateSelection | undefined): GoalConfigKey[] {
  return (selection ?? EMPTY).goals.filter((goal) => goal !== "everything");
}

/** The template's own set holds this goal. */
export function templateHolds(
  selection: TemplateSelection | undefined,
  goal: GoalConfigKey,
): boolean {
  return ownGoals(selection).includes(goal);
}

/** Every goal the template would select is selected. */
export function templateComplete(
  selection: TemplateSelection | undefined,
  selected: Partial<Record<GoalConfigKey, boolean>> | undefined,
): boolean {
  const own = ownGoals(selection);
  return own.length > 0 && own.every((goal) => selected?.[goal] === true);
}

/**
 * The profiles that are on: the ones the person switched on whose goals are
 * all still selected. A use case taken away by hand ends every profile that
 * holds it; adding another use case ends none.
 */
export function liveTemplates(
  switchedOn: readonly CreationTemplateId[],
  selections: ReadonlyMap<CreationTemplateId, TemplateSelection>,
  selected: Partial<Record<GoalConfigKey, boolean>> | undefined,
): CreationTemplateId[] {
  return CREATION_TEMPLATES.map((template) => template.id).filter(
    (id) =>
      switchedOn.includes(id) && templateComplete(selections.get(id), selected),
  );
}

export interface TemplateToggle {
  /** The profiles on after the click. */
  on: CreationTemplateId[];
  /** Goals to switch on, in card order. */
  select: GoalConfigKey[];
  /** Goals to switch off: the profile's own that no other profile holds. */
  deselect: GoalConfigKey[];
}

/**
 * One click on a profile. Switching it on selects exactly its fixed set;
 * switching it off removes its set except what another profile that is on
 * still holds, so profiles never undo each other.
 */
export function toggleTemplate(
  id: CreationTemplateId,
  live: readonly CreationTemplateId[],
  selections: ReadonlyMap<CreationTemplateId, TemplateSelection>,
): TemplateToggle {
  const selection = selections.get(id);
  if (!selection) return { on: [...live], select: [], deselect: [] };
  if (!live.includes(id)) {
    return { on: [...live, id], select: ownGoals(selection), deselect: [] };
  }
  const on = live.filter((other) => other !== id);
  const held = new Set(on.flatMap((other) => ownGoals(selections.get(other))));
  return {
    on,
    select: [],
    deselect: ownGoals(selection).filter((goal) => !held.has(goal)),
  };
}

/** The profiles that are on and hold a goal, in profile order. */
export function goalTemplates(
  goal: GoalConfigKey,
  live: readonly CreationTemplateId[],
  selections: ReadonlyMap<CreationTemplateId, TemplateSelection>,
): CreationTemplate[] {
  return CREATION_TEMPLATES.filter(
    (template) =>
      live.includes(template.id) &&
      ownGoals(selections.get(template.id)).includes(goal),
  );
}

/** A ring (conic) or a stripe (linear) split into equal parts, one per colour. */
export function ringGradient(
  colors: readonly string[],
  shape: "ring" | "stripe" = "ring",
): string | undefined {
  if (colors.length === 0) return undefined;
  const share = 100 / colors.length;
  const stops = colors.map(
    (color, index) =>
      `${color} ${(index * share).toFixed(2)}% ${((index + 1) * share).toFixed(2)}%`,
  );
  return shape === "ring"
    ? `conic-gradient(${stops.join(", ")})`
    : `linear-gradient(to bottom, ${stops.join(", ")})`;
}
