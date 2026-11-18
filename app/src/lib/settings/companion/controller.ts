/**
 * The Companion settings controller: one snapshot, one write per key, the
 * typed refusal of the last write kept on its key, and the watch loop of
 * SETTINGS-CONTRACT §4. Framework-neutral.
 *
 * Port of `createCompanionSettingsController` from the Brand UI package
 * (`@kombiverselabs/ui/companion-settings`, unreleased: the vendored UI package
 * has no such entry), kept beside the Companion page until the released
 * package carries it. Only the state logic is ported, never its renderer.
 *
 * Watch loop: `watch(revision)` answers when the revision rose (then
 * `read()`), or after 25 s with the same revision (then watch again). A
 * rejected watch (transport loss, program stopped) backs off 1 s, 2 s, 5 s,
 * 10 s, 20 s, then every 30 s, reading the snapshot after each pause; a
 * snapshot that shows the program running again ends the pause at once.
 */
import { canonicalChord } from "./chord.js";
import {
  isCompanionRefusal,
  type CompanionSettingWrite,
  type CompanionSettingsAdapter,
  type CompanionSettingsRefusal,
  type CompanionSettingsSnapshot,
} from "./contract.js";

export const COMPANION_WATCH_BACKOFF_MS = [
  1000, 2000, 5000, 10000, 20000, 30000,
] as const;

/** A watch that answers faster than this without a change is treated as a pause, never a busy loop. */
const MIN_IDLE_WATCH_MS = 1000;

export interface CompanionSettingsState {
  snapshot: CompanionSettingsSnapshot | null;
  /** `unreachable` only while no snapshot was ever read. */
  status: "loading" | "ready" | "unreachable";
  /** Keys with a write in flight. */
  pending: ReadonlySet<string>;
  /** The refusal of the last write per key, until that key is written again or its value changes. */
  refusals: Readonly<Record<string, CompanionSettingsRefusal>>;
}

export interface CompanionSettingsController {
  getState(): CompanionSettingsState;
  subscribe(listener: (state: CompanionSettingsState) => void): () => void;
  /** Reads the snapshot and watches while the section is shown. */
  start(): void;
  stop(): void;
  /**
   * Holds the controller running for one mounted page: the first holder
   * starts it, the last release stops it, so pages mounted together (one
   * under another, or a page and the shell's list) read and watch once.
   */
  retain(): () => void;
  write(key: string, value: CompanionSettingWrite): Promise<void>;
  /** A chord from the capture field: refused here as the program would, else written. */
  writeChord(key: string, chord: string): Promise<void>;
  /** Shows a refusal on a row without a write (a chord the field refused). */
  refuse(key: string, refusal: CompanionSettingsRefusal): void;
  chooseFolder(key: string): Promise<void>;
  openLink(url: string): void;
}

function valueOf(
  snapshot: CompanionSettingsSnapshot | null,
  key: string,
): CompanionSettingWrite | undefined {
  return snapshot?.values.find((entry) => entry.key === key)?.value;
}

export function createCompanionSettingsController(
  adapter: CompanionSettingsAdapter,
): CompanionSettingsController {
  let state: CompanionSettingsState = {
    snapshot: null,
    status: "loading",
    pending: new Set(),
    refusals: {},
  };
  const listeners = new Set<(state: CompanionSettingsState) => void>();
  const queued = new Map<string, CompanionSettingWrite>();
  let run: AbortController | null = null;
  let wake: (() => void) | null = null;
  let holders = 0;

  function set(next: Partial<CompanionSettingsState>): void {
    state = { ...state, ...next };
    for (const listener of listeners) listener(state);
  }

  /** Takes a snapshot. A read always wins; a write's answer only when it is not older than what is shown. */
  function accept(
    snapshot: CompanionSettingsSnapshot,
    fromRead: boolean,
  ): void {
    const current = state.snapshot;
    if (
      !fromRead &&
      current &&
      snapshot.revision < current.revision &&
      snapshot.running === current.running &&
      snapshot.installed === current.installed
    ) {
      return;
    }
    const refusals: Record<string, CompanionSettingsRefusal> = {};
    for (const [key, refusal] of Object.entries(state.refusals)) {
      if (valueOf(current, key) === valueOf(snapshot, key))
        refusals[key] = refusal;
    }
    const resumed = Boolean(current && !current.running && snapshot.running);
    set({ snapshot, status: "ready", refusals });
    if (resumed) wake?.();
  }

  function pause(ms: number, signal: AbortSignal): Promise<void> {
    return new Promise((resolve) => {
      const done = () => {
        clearTimeout(timer);
        signal.removeEventListener("abort", done);
        if (wake === done) wake = null;
        resolve();
      };
      const timer = setTimeout(done, ms);
      signal.addEventListener("abort", done, { once: true });
      wake = done;
    });
  }

  async function read(): Promise<boolean> {
    try {
      accept(await adapter.read(), true);
      return true;
    } catch {
      if (!state.snapshot) set({ status: "unreachable" });
      return false;
    }
  }

  async function loop(signal: AbortSignal): Promise<void> {
    let failures = 0;
    let quick = 0;
    const backOff = (): number =>
      COMPANION_WATCH_BACKOFF_MS[
        Math.min(failures++, COMPANION_WATCH_BACKOFF_MS.length - 1)
      ] ?? 30000;
    while (!signal.aborted) {
      const snapshot = state.snapshot;
      if (!snapshot) {
        if (!(await read()) && !signal.aborted) await pause(backOff(), signal);
        continue;
      }
      const asked = Date.now();
      try {
        const revision = await adapter.watch(snapshot.revision, signal);
        if (signal.aborted) return;
        // The back-off resets only after a watch the program held (an answer after at least one second),
        // as the shell's CompanionSettingsWatcher does. A read that succeeds proves nothing about the watch:
        // a watch refused again and again would otherwise loop at 1 Hz.
        const held = Date.now() - asked >= MIN_IDLE_WATCH_MS;
        if (held) {
          failures = 0;
          quick = 0;
        }
        // A different revision is read, a lower one too (a restarted program counts from its own start, and a
        // read always wins over what is shown).
        if (revision !== snapshot.revision) await read();
        if (!held) {
          // Quick answers are real changes in a burst; a program that keeps answering at once is paused.
          quick += 1;
          if (revision === snapshot.revision || quick > 2)
            await pause(backOff(), signal);
        }
      } catch {
        if (signal.aborted) return;
        await pause(backOff(), signal);
        if (!signal.aborted) await read();
      }
    }
  }

  async function write(
    key: string,
    value: CompanionSettingWrite,
  ): Promise<void> {
    if (state.pending.has(key)) {
      queued.set(key, value);
      return;
    }
    const refusals = { ...state.refusals };
    delete refusals[key];
    set({ pending: new Set([...state.pending, key]), refusals });
    try {
      const answer = await adapter.write(key, value);
      if (isCompanionRefusal(answer))
        set({ refusals: { ...state.refusals, [key]: answer } });
      else accept(answer, false);
    } catch {
      set({ refusals: { ...state.refusals, [key]: { error: "not_saved" } } });
    } finally {
      const pending = new Set(state.pending);
      pending.delete(key);
      set({ pending });
    }
    if (queued.has(key)) {
      const next = queued.get(key)!;
      queued.delete(key);
      if (next !== valueOf(state.snapshot, key)) await write(key, next);
    }
  }

  function start(): void {
    if (run) return;
    run = new AbortController();
    void loop(run.signal);
  }
  function stop(): void {
    run?.abort();
    run = null;
  }

  return {
    getState: () => state,
    subscribe(listener) {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
    start,
    stop,
    retain() {
      holders += 1;
      start();
      let released = false;
      return () => {
        if (released) return;
        released = true;
        holders -= 1;
        if (holders === 0) stop();
      };
    },
    write,
    async writeChord(key, chord) {
      const canonical = chord === "" ? "" : canonicalChord(chord);
      if (canonical === null) {
        set({
          refusals: { ...state.refusals, [key]: { error: "shortcut_invalid" } },
        });
        return;
      }
      if (canonical === valueOf(state.snapshot, key)) return;
      const other = canonical
        ? state.snapshot?.values.find(
            (entry) =>
              entry.type === "chord" &&
              entry.key !== key &&
              entry.value === canonical,
          )
        : undefined;
      if (other) {
        set({
          refusals: {
            ...state.refusals,
            [key]: { error: "shortcut_conflict", with: other.key },
          },
        });
        return;
      }
      await write(key, canonical);
    },
    refuse(key, refusal) {
      set({ refusals: { ...state.refusals, [key]: refusal } });
    },
    async chooseFolder(key) {
      const current = valueOf(state.snapshot, key);
      let folder: string | null;
      try {
        folder = await adapter.pickFolder(
          typeof current === "string" ? current : "",
        );
      } catch {
        set({ refusals: { ...state.refusals, [key]: { error: "not_saved" } } });
        return;
      }
      if (folder && folder !== current) await write(key, folder);
    },
    openLink(url) {
      // A host whose browser launch fails must not raise an unhandled rejection in the client.
      Promise.resolve(adapter.openLink(url)).catch(() => {});
    },
  };
}
