/**
 * The local answer (workspace `platform/standards/local-ai-runtimes.v1.md` rule 9): when the person
 * chose that Homelab answers with a local runtime (the device-wide `answer` of `local-ai.json`, set
 * in Settings › AI providers or in any other kombify app), the Companion's questions run on that
 * runtime: this PC's Ollama or the Ollama of their homelab server. Windows client only (its shell
 * holds the file and talks to the runtime), behind the flag `local_ai_providers`.
 *
 * `ai-setup-state.ts` feeds every read of the file here, so this follows a change in any app at once,
 * without a reload. This is an agent design (2026-11-23), revisable.
 */
import type {
  LocalAiAnswerReason,
  LocalAiSnapshot,
} from "#lib/client/local-ai-bridge.js";

export interface LocalAnswerView {
  /** The runtime row the person chose. */
  runtime: string;
  /** The runtime's kind (`ollama`). */
  kind: string;
  /** The name a person sees: the server's name, or empty for this PC's own runtime. */
  label: string;
  remote: boolean;
  /** The model that answers, or the chosen one while the runtime cannot answer. */
  model: string;
  /** Why it cannot answer now; empty when it can, undefined before the shell probed it. */
  reason?: LocalAiAnswerReason;
}

let snapshot = $state.raw<LocalAiSnapshot | null>(null);
let flagOn = $state(false);

/** Called with every read of the device file (`null`: not readable here). */
export function setLocalAiSnapshot(next: LocalAiSnapshot | null): void {
  snapshot = next;
}

/** Called whenever the flag `local_ai_providers` is read. */
export function setLocalAiFlag(on: boolean): void {
  flagOn = on;
}

/** The local answer the person chose, or null when Homelab answers from the account's AI use. */
export function localAnswerOf(
  current: LocalAiSnapshot | null,
  flag: boolean,
): LocalAnswerView | null {
  const answer = current?.readable ? current.answer : undefined;
  if (!flag || !answer) return null;
  const row = current?.runtimes.find(
    (runtime) => runtime.id === answer.runtime,
  );
  return {
    runtime: answer.runtime,
    kind: row?.kind || "ollama",
    label: row?.label ?? "",
    remote: row?.remote ?? false,
    model: answer.resolvedModel || answer.model,
    ...(answer.reason !== undefined ? { reason: answer.reason } : {}),
  };
}

export const localAnswer = {
  /** The person chose a local answer: the local Companion stands in for the kombify one. */
  get active(): boolean {
    return localAnswerOf(snapshot, flagOn) !== null;
  },
  get view(): LocalAnswerView | null {
    return localAnswerOf(snapshot, flagOn);
  },
};
