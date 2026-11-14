import {
  CANONICAL_USE_CASE_GOALS,
  type CanonicalUseCaseGoalKey,
} from "./standardBundle.js";
import { createDefaultConfig } from "./types.js";

/** The Cloud Builder sends preferences, not deployment instructions. */
export const BUILDER_GOALS = {
  photos: "photos",
  media: "media",
  documents: "files",
  passwords: "vault",
  "smart-home": "smart-home",
  games: "game",
  mail: "mail",
  network: null,
  automation: null,
  ai: "ai",
  development: "dev",
  website: null,
} satisfies Record<string, CanonicalUseCaseGoalKey | null>;
export type BuilderIdea = keyof typeof BUILDER_GOALS;
export const BUILDER_PLACEMENTS = ["local", "cloud", "managed"] as const;
export type BuilderPlacement = (typeof BUILDER_PLACEMENTS)[number];
export interface BuilderChoice {
  idea: BuilderIdea;
  placement: BuilderPlacement;
  included: boolean;
}

export function readBuilderPlan(value: string | null): BuilderChoice[] | null {
  if (!value || value.length > 300) return null;
  const pairs = value.split(",");
  if (pairs.length > Object.keys(BUILDER_GOALS).length) return null;
  const seen = new Set<string>();
  const choices: BuilderChoice[] = [];
  for (const pair of pairs) {
    const parts = pair.split(":");
    const idea = Object.keys(BUILDER_GOALS).find(
      (key): key is BuilderIdea => key === parts[0],
    );
    const placement = BUILDER_PLACEMENTS.find((key) => key === parts[1]);
    if (parts.length !== 2 || !idea || !placement || seen.has(idea))
      return null;
    seen.add(idea);
    choices.push({ idea, placement, included: true });
  }
  return choices;
}

export function builderWizardConfig(choices: BuilderChoice[]) {
  const config = createDefaultConfig();
  if (config.goals) {
    for (const goal of CANONICAL_USE_CASE_GOALS)
      config.goals[goal] = choices.some(
        (choice) => choice.included && BUILDER_GOALS[choice.idea] === goal,
      );
    config.goals.everything = false;
  }
  return config;
}
