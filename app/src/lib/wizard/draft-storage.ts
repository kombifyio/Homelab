/**
 * Wizard draft: the answers a person has entered and where they stand, kept by
 * Techstack per owner and creation process, so a creation started in one frame
 * (the Cloud popup, a sheet, one step alone) continues on the Techstack page
 * or on another device, on the same step with the same answers.
 *
 * The draft never holds a secret: passwords and values derived from a typed
 * secret are blanked before anything is written, the recovery passphrase
 * itself is not part of the config at all, and the server refuses any field
 * that still carries one. It ends when the creation is accepted, the person
 * chooses to leave, or it expires.
 */
import {
  discardCreationDraft,
  getCreationDraft,
  putCreationDraft,
} from "#lib/api/creationDrafts.js";
import {
  CREATION_STEP_KEYS,
  type CreationStepKey,
} from "#lib/creation/core/steps.js";
import type { StackConfig } from "./types";

export interface WizardDraft {
  step: CreationStepKey;
  /** The furthest step reached; every step up to it opens directly. */
  furthest: CreationStepKey;
  config: StackConfig;
}

// The names Techstack refuses to keep with a value (pkg/controlplane
// creation_draft.go): the SSH password, the recovery passphrase hash and any
// use-case setting named like a secret.
const SECRET_NAME =
  /(password|passwd|passphrase|passphrasehash|secret|token|privatekey|apikey|authkey|preauthkey|credentials?|recoverymaterial|recoverycodes?|totp)$/;

function blankSecrets(value: unknown): void {
  if (Array.isArray(value)) {
    value.forEach(blankSecrets);
    return;
  }
  if (!isRecord(value)) return;
  for (const [name, child] of Object.entries(value)) {
    if (!SECRET_NAME.test(name.toLowerCase().replace(/[_.-]/g, ""))) {
      blankSecrets(child);
      continue;
    }
    // Flags and numbers carry no secret (requirePassword); anything else is
    // blanked the way Techstack would otherwise refuse it.
    if (typeof child === "string") value[name] = "";
    else if (typeof child === "object" && child !== null) value[name] = null;
  }
}

/** A deep copy of the config without anything that is or derives from a secret. */
export function withoutSecrets(config: StackConfig): StackConfig {
  const copy = JSON.parse(JSON.stringify(config)) as StackConfig;
  blankSecrets(copy);
  return copy;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/**
 * Lay a stored object over the current defaults so a field added after the
 * draft was written still has its default. Arrays and scalars of the draft win.
 */
export function mergeOverDefaults<T>(defaults: T, stored: unknown): T {
  if (!isRecord(defaults) || !isRecord(stored))
    return (stored ?? defaults) as T;
  const merged: Record<string, unknown> = { ...defaults };
  for (const [key, value] of Object.entries(stored)) {
    merged[key] =
      key in defaults ? mergeOverDefaults(defaults[key], value) : value;
  }
  return merged as T;
}

const stepOf = (value: unknown): CreationStepKey | undefined =>
  CREATION_STEP_KEYS.find((key) => key === value);
// The order is the process's own; a process may put another step first.
const position = (
  key: CreationStepKey,
  order: readonly CreationStepKey[] = CREATION_STEP_KEYS,
) => order.indexOf(key);

// One queue per draft: a write never overtakes an earlier one, and a discard
// waits for the writes before it, so nothing written earlier brings it back.
const queues = new Map<string, Promise<unknown>>();
function enqueue(key: string, work: () => Promise<unknown>): Promise<void> {
  const next = (queues.get(key) ?? Promise.resolve())
    .catch(() => undefined)
    .then(work);
  queues.set(key, next);
  return next.then(
    () => {
      if (queues.get(key) === next) queues.delete(key);
    },
    () => {
      if (queues.get(key) === next) queues.delete(key);
    },
  );
}

/**
 * Keeps the draft. A failed write is not shown: the wizard keeps working
 * without a draft, and the next change writes it again.
 */
export function saveWizardDraft(
  key: string,
  process: string,
  draft: WizardDraft,
  order?: readonly CreationStepKey[],
): Promise<void> {
  const furthest =
    position(draft.furthest, order) > position(draft.step, order)
      ? draft.furthest
      : draft.step;
  const answers = withoutSecrets(draft.config) as unknown as Record<
    string,
    unknown
  >;
  return enqueue(key, () =>
    putCreationDraft(key, { process, step: draft.step, furthest, answers }),
  );
}

/** The kept draft, or null when there is none or it cannot be read. */
export async function loadWizardDraft(
  key: string,
  order?: readonly CreationStepKey[],
): Promise<WizardDraft | null> {
  try {
    await wizardDraftWritten(key);
    const stored = await getCreationDraft(key);
    if (!stored || !isRecord(stored.answers)) return null;
    const step = stepOf(stored.step) ?? "goals";
    const furthest = stepOf(stored.furthest);
    return {
      step,
      furthest:
        furthest && position(furthest, order) >= position(step, order)
          ? furthest
          : step,
      config: stored.answers as unknown as StackConfig,
    };
  } catch {
    return null;
  }
}

/** Resolves once every write already started for this draft is done. */
export async function wizardDraftWritten(key: string): Promise<void> {
  await queues.get(key)?.catch(() => undefined);
}

/** Ends the draft once the writes before it are done. */
export function clearWizardDraft(key: string): Promise<void> {
  return enqueue(key, () => discardCreationDraft(key)).catch(() => undefined);
}
