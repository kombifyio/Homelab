/**
 * How a local runtime and the reason it cannot answer read for a person: shared by Settings › AI
 * providers and the local Companion, so both say the same.
 */
import { tr } from "#lib/i18n.svelte.js";
import type { LocalAiAnswerReason } from "#lib/client/local-ai-bridge.js";

const KIND_NAMES: Record<string, string> = { ollama: "Ollama" };

/** "Ollama (this PC)" for the device's runtime, the label for one on another machine. */
export function runtimeName(
  runtime:
    { kind?: string; label?: string; remote?: boolean; id?: string } | string,
): string {
  if (typeof runtime === "string") return runtime;
  if (runtime.remote && runtime.label) return runtime.label;
  return tr("settings.aiProviders.thisPc", {
    name: KIND_NAMES[runtime.kind ?? ""] ?? runtime.kind ?? runtime.id ?? "",
  });
}

/** The reason line of a runtime that cannot answer. */
export function localAiReasonText(
  reason:
    Exclude<LocalAiAnswerReason, ""> | "no_answer" | "failed" | "cancelled",
  name: string,
  model: string,
): string {
  switch (reason) {
    case "runtime_missing":
      return tr("settings.aiProviders.reason.runtimeMissing", { name });
    case "runtime_off":
      return tr("settings.aiProviders.reason.runtimeOff", { name });
    case "unreachable":
      return tr("settings.aiProviders.reason.unreachable", { name });
    case "no_models":
      return tr("settings.aiProviders.reason.noModels", { name });
    case "model_missing":
      return tr("settings.aiProviders.reason.modelMissing", { name, model });
    case "no_answer":
      return tr("settings.aiProviders.reason.noAnswer");
    case "cancelled":
      return tr("localCompanion.stopped");
    default:
      return tr("settings.aiProviders.reason.failed", { name });
  }
}
