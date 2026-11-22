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
 * Homelab Runtime's own AI is the Companion panel AI Platform serves, which runs
 * on kombify AI; Ollama is only a device setting here. So what answers is
 * "other" when the account chose its own provider and "kombify" otherwise, and
 * never "local".
 */
import type { AiUseChoice } from "#lib/ai/ai-use.js";
import type { LocalAiSnapshot } from "#lib/client/local-ai-bridge.js";

export type AccountAiUse = "kombify" | "own" | "none" | "unknown";
export type ActiveAiSource = "kombify" | "other" | "local";

export interface AiSetupLocalRuntimes {
  ollama?: { enabled: boolean; reachable?: boolean };
}

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

/** The Ollama switch and, after a probe, its reachability; undefined when the device store is not readable here. */
export function localRuntimesOf(
  snapshot: LocalAiSnapshot | null,
): AiSetupLocalRuntimes | undefined {
  if (!snapshot) return undefined;
  const ollama = snapshot.runtimes.find((runtime) => runtime.id === "ollama");
  const enabled = snapshot.readable && ollama?.enabled === true;
  const reachable = enabled ? ollama?.status?.reachable : undefined;
  return {
    ollama: {
      enabled,
      ...(typeof reachable === "boolean" ? { reachable } : {}),
    },
  };
}

export function buildAiSetup(input: {
  aiUse: AiUseChoice | null;
  local?: LocalAiSnapshot | null;
}): AiSetup {
  const accountAiUse = accountAiUseOf(input.aiUse);
  const localRuntimes = localRuntimesOf(input.local ?? null);
  return {
    accountAiUse,
    activeSource: accountAiUse === "own" ? "other" : "kombify",
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
