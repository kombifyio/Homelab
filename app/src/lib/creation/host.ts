/**
 * The creation module's side of the host contract: events go out through the
 * authenticated bridge, and the frame a person works in may hold back a close.
 * Outside a host page nothing is sent.
 */
import { getContext, setContext } from "svelte";
import {
  isEmbeddedFrame,
  onCreationCommand,
  sendToPortal,
} from "#lib/stores/postMessageBridge.js";
import {
  CREATION_EVENT_SCHEMA,
  type CreationEntry,
  type CreationEvent,
} from "./core/index";

// Set by the module's route and kept across navigation inside the frame: a run
// moves on to the Creation Screen, and the host still wants to hear how it ends.
// Techstack framed as a whole tool stays silent.
let hosted = false;

export function markCreationHosted(): void {
  if (hosted) return;
  hosted = true;
  onCreationCommand((command) => {
    if (command.command === "close-request") void requestCreationClose();
  });
}

export function emitCreationEvent(event: CreationEvent): void {
  if (!hosted || !isEmbeddedFrame()) return;
  sendToPortal({
    type: "creation-event",
    schema: CREATION_EVENT_SCHEMA,
    ...event,
  });
}

const ENTRY_KEY = Symbol("creation-entry");

/** The host's entry, for frames mounted inside the embedded module. */
export function provideCreationEntry(entry: () => CreationEntry | null): void {
  setContext(ENTRY_KEY, entry);
}

export function useCreationEntry(): CreationEntry | null {
  return (
    getContext<(() => CreationEntry | null) | undefined>(ENTRY_KEY)?.() ?? null
  );
}

let keepDraft: (() => Promise<void>) | null = null;

/**
 * While set, a move to Techstack's own page first writes the frame's draft, so
 * the page opens on the same step with the same answers.
 */
export function keepCreationDraftBeforeLeaving(
  keep: () => Promise<void>,
): () => void {
  keepDraft = keep;
  return () => {
    if (keepDraft === keep) keepDraft = null;
  };
}

/** Continues on Techstack's own page at `path` once the draft is written. */
export async function openCreationFull(path: string): Promise<void> {
  await keepDraft?.().catch(() => undefined);
  emitCreationEvent({ event: "open-full", path });
}

let closeGuard: (() => Promise<boolean>) | null = null;

/** While set, a close the host asks for waits for this answer. */
export function guardCreationClose(guard: () => Promise<boolean>): () => void {
  closeGuard = guard;
  return () => {
    if (closeGuard === guard) closeGuard = null;
  };
}

export async function requestCreationClose(): Promise<void> {
  if (closeGuard && !(await closeGuard())) return;
  emitCreationEvent({ event: "cancelled" });
}
