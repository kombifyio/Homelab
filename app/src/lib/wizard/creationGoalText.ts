import { tr, type MessageKey } from "#lib/i18n.svelte.js";
import type { CreationGoal, CreationGoalId } from "./creationGoals.js";
import { getEasyGoalQuestions } from "./standardBundle.js";

const PREVIEW_TITLE_KEYS: Partial<Record<CreationGoalId, MessageKey>> = {
  files: "wizard.preview.files.title",
  mail: "wizard.preview.mail.title",
  game: "wizard.preview.game.title",
  network: "wizard.preview.network.title",
  automation: "wizard.preview.automation.title",
};

const PREVIEW_DESCRIPTION_KEYS: Partial<Record<CreationGoalId, MessageKey>> = {
  files: "wizard.preview.files.description",
  mail: "wizard.preview.mail.description",
  game: "wizard.preview.game.description",
  dev: "wizard.preview.dev.description",
  ai: "wizard.preview.ai.description",
  network: "wizard.preview.network.description",
  automation: "wizard.preview.automation.description",
};

/** The name of a use case known only by its slug (a run answer names it). */
export function useCaseTitle(id: string): string {
  const question = getEasyGoalQuestions().find((entry) => entry.configKey === id);
  const previewKey = PREVIEW_TITLE_KEYS[id as CreationGoalId];
  if (id === "files" || id === "mail" || id === "game") {
    return tr(PREVIEW_TITLE_KEYS[id]!);
  }
  if (question) return tr(question.titleKey);
  if (previewKey) return tr(previewKey);
  return id;
}

/** The words of one use case, the same in both stages of step 1. Files, Mail
 * and Game name the use case, not the bundle question they share. */
export function goalTitle(card: CreationGoal): string {
  if (card.id === "files" || card.id === "mail" || card.id === "game") {
    return tr(PREVIEW_TITLE_KEYS[card.id]!);
  }
  if (card.question) return tr(card.question.titleKey);
  const previewKey = PREVIEW_TITLE_KEYS[card.id];
  return previewKey ? tr(previewKey) : card.id;
}

export function goalDescription(card: CreationGoal): string {
  if (
    card.id === "files" ||
    card.id === "mail" ||
    card.id === "game" ||
    card.id === "dev" ||
    card.id === "ai"
  ) {
    return tr(PREVIEW_DESCRIPTION_KEYS[card.id]!);
  }
  if (card.question) return tr(card.question.descriptionKey);
  const previewKey = PREVIEW_DESCRIPTION_KEYS[card.id];
  return previewKey ? tr(previewKey) : "";
}

/** One short line of what the use case means, never the tool behind it. */
export function goalSummary(card: CreationGoal): string {
  return tr(`wizard.goals.summary.${card.id}`);
}
