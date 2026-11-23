/**
 * Which AI the person had set in this app: the content-free `aiSetup` block of
 * SurfaceContextV2 (workspace FLOATING-PANEL-V2-CONTRACT §7, AppLink v2
 * `context.report`). When a person reports a fault or turns to support, kombify
 * can then tell whether kombify AI, another provider or a local runtime was in
 * play. It carries no endpoint URL, prompt or key: `provider` and `model` are
 * short codes (driver id, model id) and are left out unless really known.
 *
 * Sources: the account choice `aiUse` (AI Platform `account-design`, read by
 * `ai-use.ts`) and the device-wide local runtimes (`local-ai-runtimes.v1.md`),
 * which only the Windows client can read.
 *
 * Homelab Runtime's AI is the Companion. It answers on kombify AI through the
 * panel AI Platform serves ("other" when the account chose its own provider),
 * or, when the person chose a local answer (`local-ai-runtimes.v1.md` rule 9,
 * `local-answer.svelte.ts`), on that local runtime: then it is "local" with
 * the runtime's kind as `provider` and the model that answers.
 */
import type { AiUseChoice } from "#lib/ai/ai-use.js";
import type { LocalAiSnapshot } from "#lib/client/local-ai-bridge.js";
import type { LocalAnswerView } from "#lib/ai/local-answer.svelte.js";

export type AccountAiUse = "kombify" | "own" | "none" | "unknown";
export type ActiveAiSource = "kombify" | "other" | "local";

/** Per runtime row of the device file (`ollama`, `ollama-nas`): its switch and, after a probe, whether it answered. */
export type AiSetupLocalRuntimes = Record<
  string,
  { enabled: boolean; reachable?: boolean }
>;

// The schema's bounds (`$defs/aiSetup`): at most eight runtimes, short codes only.
const MAX_LOCAL_RUNTIMES = 8;
const CODE = /^[A-Za-z0-9._:@/+-]{1,160}$/;
const PROVIDER = /^[A-Za-z][A-Za-z0-9_.-]{0,63}$/;

export interface AiSetup {
  accountAiUse: AccountAiUse;
  activeSource: ActiveAiSource;
  provider?: string;
  model?: string;
  localRuntimes?: AiSetupLocalRuntimes;
}

/**
 * An undecided account runs on kombify AI and is `kombify` (FLOATING-PANEL-V2-CONTRACT §7);
 * no record or an unreadable one is `unknown`.
 */
export function accountAiUseOf(choice: AiUseChoice | null): AccountAiUse {
  if (choice === "undecided") return "kombify";
  return choice === "kombify" || choice === "own" || choice === "none"
    ? choice
    : "unknown";
}

/** Each runtime's switch and, after a probe, its reachability; undefined when the device store is not readable here. */
export function localRuntimesOf(
  snapshot: LocalAiSnapshot | null,
): AiSetupLocalRuntimes | undefined {
  if (!snapshot) return undefined;
  const runtimes: AiSetupLocalRuntimes = {};
  const rows = snapshot.readable
    ? snapshot.runtimes
    : snapshot.runtimes.filter((runtime) => runtime.id === "ollama");
  for (const runtime of rows.slice(0, MAX_LOCAL_RUNTIMES)) {
    const enabled = snapshot.readable && runtime.enabled;
    const reachable = enabled ? runtime.status?.reachable : undefined;
    runtimes[runtime.id] = {
      enabled,
      ...(typeof reachable === "boolean" ? { reachable } : {}),
    };
  }
  return runtimes;
}

export function buildAiSetup(input: {
  aiUse: AiUseChoice | null;
  local?: LocalAiSnapshot | null;
  /** The local answer the person chose (rule 9); it answers in this app instead of kombify AI. */
  localAnswer?: LocalAnswerView | null;
}): AiSetup {
  const accountAiUse = accountAiUseOf(input.aiUse);
  const localRuntimes = localRuntimesOf(input.local ?? null);
  const answer = input.localAnswer;
  return {
    accountAiUse,
    activeSource: answer
      ? "local"
      : accountAiUse === "own"
        ? "other"
        : "kombify",
    ...(answer && PROVIDER.test(answer.kind) ? { provider: answer.kind } : {}),
    ...(answer && CODE.test(answer.model) ? { model: answer.model } : {}),
    ...(localRuntimes ? { localRuntimes } : {}),
  };
}

/** The tag keys, so a stale value is cleared when its source becomes unknown. */
export const AI_SETUP_TAG_KEYS = [
  "ai.source",
  "ai.account_use",
  "ai.provider",
  "ai.local.ollama",
] as const;

/**
 * The error-report tags (the same names the Windows shell's `CompanionAiSetup.Tags()` writes):
 * `ai.local.ollama` is `off`, `on` (enabled, reachability unknown), `reachable` or `unreachable`
 * and is absent while the local runtimes are unknown (a browser).
 */
export function aiSetupTags(
  setup: AiSetup,
): Partial<Record<(typeof AI_SETUP_TAG_KEYS)[number], string>> {
  const ollama = setup.localRuntimes?.ollama;
  return {
    "ai.source": setup.activeSource,
    "ai.account_use": setup.accountAiUse,
    ...(setup.provider ? { "ai.provider": setup.provider } : {}),
    ...(ollama
      ? {
          "ai.local.ollama": !ollama.enabled
            ? "off"
            : ollama.reachable === true
              ? "reachable"
              : ollama.reachable === false
                ? "unreachable"
                : "on",
        }
      : {}),
  };
}
