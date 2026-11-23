/**
 * The app's current AI setup (`ai-setup.ts`), kept fresh for the three places that carry it:
 * every Companion panel turn (`CompanionHost`), the Sentry scope of every error report, and the
 * support text a person copies. A failed read changes nothing but the answer: the account choice
 * reads as `unknown`, the local runtimes are left out.
 */
import * as Sentry from "@sentry/sveltekit";
import { fetchAiUse } from "#lib/ai/ai-use.js";
import {
  AI_SETUP_TAG_KEYS,
  aiSetupTags,
  buildAiSetup,
  type AiSetup,
} from "#lib/ai/ai-setup.js";
import {
  hasLocalAiBridge,
  readLocalAi,
  watchLocalAi,
  type LocalAiSnapshot,
} from "#lib/client/local-ai-bridge.js";
import {
  localAnswerOf,
  setLocalAiFlag,
  setLocalAiSnapshot,
} from "#lib/ai/local-answer.svelte.js";
import { features } from "#lib/stores/features.js";

// The account record and a switched-on runtime's probe are cheap but not free: a panel open
// refreshes at most this often; a sign-in or a change of the device file always refreshes.
const REFRESH_MS = 30_000;

// Before anything was read (and for a person who never signed in) the setup is the shell's own
// view: kombify AI answers, the account choice is unknown, the local runtimes are not known.
let current: AiSetup = buildAiSetup({ aiUse: null });
let lastRefresh = 0;
let generation = 0;
// The flag `local_ai_providers` as last read: a local answer counts only with it on.
let localFlag = false;
const subscribers = new Set<(setup: AiSetup) => void>();

export function currentAiSetup(): AiSetup {
  return current;
}

/** Calls `listener` after every change; returns the unsubscribe function. */
export function subscribeAiSetup(
  listener: (setup: AiSetup) => void,
): () => void {
  subscribers.add(listener);
  return () => subscribers.delete(listener);
}

function publish(next: AiSetup): void {
  const unchanged = JSON.stringify(next) === JSON.stringify(current);
  current = next;
  applyTags(next);
  if (!unchanged) for (const listener of subscribers) listener(next);
}

function applyTags(setup: AiSetup): void {
  const tags = aiSetupTags(setup);
  // A tag whose source became unknown is cleared, not left stale.
  Sentry.setTags(
    Object.fromEntries(AI_SETUP_TAG_KEYS.map((key) => [key, tags[key]])),
  );
  // The whole block beside the error envelope (FLOATING-PANEL-V2-CONTRACT §7).
  Sentry.setContext("ai_setup", { ...setup });
}

async function readLocal(): Promise<LocalAiSnapshot | null> {
  if (!hasLocalAiBridge()) return null;
  try {
    return await readLocalAi();
  } catch {
    return null;
  }
}

/** Reads the account choice and, in the Windows client, the local runtimes again. */
export async function refreshAiSetup(force = false): Promise<void> {
  const now = Date.now();
  if (!force && now - lastRefresh < REFRESH_MS) return;
  lastRefresh = now;
  const mine = ++generation;
  const [aiUse, local] = await Promise.all([fetchAiUse(), readLocal()]);
  if (mine !== generation) return; // a newer read answers
  setLocalAiSnapshot(local);
  publish(
    buildAiSetup({
      aiUse,
      local,
      localAnswer: localAnswerOf(local, localFlag),
    }),
  );
}

/**
 * Sets the fallback tags at once and follows the device file for as long as the layout lives;
 * returns the stop function. Reads the current state once now.
 */
export function startAiSetup(): () => void {
  applyTags(current);
  const stopWatching = watchLocalAi(() => void refreshAiSetup(true));
  const stopFlag = features.allFeatures.subscribe((all) => {
    const on =
      hasLocalAiBridge() && (all.get("local_ai_providers")?.enabled ?? false);
    setLocalAiFlag(on);
    if (on !== localFlag) {
      localFlag = on;
      void refreshAiSetup(true);
    }
  });
  void refreshAiSetup(true);
  return () => {
    stopWatching();
    stopFlag();
  };
}
