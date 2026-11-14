import { tr } from "#lib/i18n.svelte.js";
import type { CreationGoal } from "./creationGoals.js";
import { getEasyGoalQuestions } from "./standardBundle.js";

/** The name of a use case known only by its slug (a run answer names it). */
export function useCaseTitle(id: string): string {
  return goalTitle({
    id,
    question: getEasyGoalQuestions().find((entry) => entry.configKey === id),
  } as CreationGoal);
}

/** The words of one use case, the same in both stages of step 1. Files, Mail
 * and Game name the use case, not the bundle question they share. */
export function goalTitle(card: CreationGoal): string {
  if (["files", "mail", "game"].includes(card.id) || !card.question)
    return tr(`wizard.preview.${card.id}.title`);
  return tr(card.question.titleKey);
}

export function goalDescription(card: CreationGoal): string {
  if (
    ["files", "mail", "game", "dev", "ai"].includes(card.id) ||
    !card.question
  )
    return tr(`wizard.preview.${card.id}.description`);
  return tr(card.question.descriptionKey);
}

/** One short line of what the use case means, never the tool behind it. */
export function goalSummary(card: CreationGoal): string {
  return tr(`wizard.goals.summary.${card.id}`);
}
