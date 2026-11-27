import type { MessageKey, MessageParams } from "#lib/i18n.js";
import type { GuardFact, GuardMessage } from "./facts";

export type Translate = (
  id: MessageKey,
  params?: MessageParams,
) => string;

/**
 * Translate one fact message. A parameter named `at` carries an ISO timestamp
 * and is shown in the active locale; everything else is passed through.
 */
export function renderGuardMessage(
  message: GuardMessage,
  translate: Translate,
  formatTime: (iso: string) => string,
): string {
  const params: Record<string, string | number> = { ...message.params };
  if (typeof params.at === "string") params.at = formatTime(params.at);
  return translate(message.id, params);
}

/** The seven facts in display order with their resource for icon lookup. */
export function factTitleId(key: GuardFact["key"]): MessageKey {
  return `guard.fact.${key}.title`;
}

export function factStateId(state: GuardFact["state"]): MessageKey {
  return `guard.state.${state}`;
}
