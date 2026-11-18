/**
 * Desktop shortcut chords: the canonical form, the grammar and the key
 * capture behind the shortcut row (port of the Brand UI package's chord
 * module, which is not released with the vendored UI package).
 *
 * Canonical form: lower-case tokens joined by `+`, modifiers first in the
 * order ctrl, alt, shift, win, then the other keys in the order pressed
 * (`ctrl+shift+s`, `ctrl+win`, `f9`). The grammar is exactly what the
 * Companion program parses (`hotkeycombo.ParseStrict` in kombify-AI-Platform):
 * modifiers, letters, top-row digits, F1 to F24, space, enter, esc, tab,
 * backspace and the two side mouse buttons; no empty token, no key twice.
 * Anything else is refused before it is written.
 */

export const CHORD_MODIFIERS = ["ctrl", "alt", "shift", "win"] as const;
export type ChordModifier = (typeof CHORD_MODIFIERS)[number];

/** Named keys besides letters, digits and F-keys. */
export const CHORD_NAMED_KEYS = [
  "space",
  "enter",
  "esc",
  "tab",
  "backspace",
  "mouse-x1",
  "mouse-x2",
] as const;
export type ChordNamedKey = (typeof CHORD_NAMED_KEYS)[number];

/** What a person reads for each modifier and named key, in their language. */
export type ChordKeyNames = Record<ChordModifier | ChordNamedKey, string>;

export const DEFAULT_CHORD_KEY_NAMES: ChordKeyNames = {
  ctrl: "Ctrl",
  alt: "Alt",
  shift: "Shift",
  win: "Win",
  space: "Space",
  enter: "Enter",
  esc: "Esc",
  tab: "Tab",
  backspace: "Backspace",
  "mouse-x1": "Mouse 4",
  "mouse-x2": "Mouse 5",
};

/** Spellings the program accepts for the same key. */
const ALIASES: Readonly<Record<string, string>> = {
  control: "ctrl",
  windows: "win",
  cmd: "win",
  meta: "win",
  return: "enter",
  escape: "esc",
  mb4: "mouse-x1",
  mb5: "mouse-x2",
};

function canonicalToken(raw: string): string | null {
  const token = ALIASES[raw] ?? raw;
  if ((CHORD_MODIFIERS as readonly string[]).includes(token)) return token;
  if ((CHORD_NAMED_KEYS as readonly string[]).includes(token)) return token;
  if (/^[a-z0-9]$/u.test(token)) return token;
  const fkey = /^f(\d{1,2})$/u.exec(token);
  if (fkey) {
    const n = Number(fkey[1]);
    if (n >= 1 && n <= 24 && String(n) === fkey[1]) return token;
  }
  return null;
}

/** The canonical form of a chord, or `null` when the program would refuse it (`shortcut_invalid`). */
export function canonicalChord(text: string): string | null {
  const trimmed = text.trim();
  if (!trimmed) return null;
  const modifiers = new Set<string>();
  const keys: string[] = [];
  for (const part of trimmed.toLowerCase().split("+")) {
    const token = canonicalToken(part.trim());
    if (!token) return null;
    if (modifiers.has(token) || keys.includes(token)) return null;
    if ((CHORD_MODIFIERS as readonly string[]).includes(token))
      modifiers.add(token);
    else keys.push(token);
  }
  return [...CHORD_MODIFIERS.filter((m) => modifiers.has(m)), ...keys].join(
    "+",
  );
}

/** The pieces of a canonical chord as a person reads them: their names, keys in capitals. */
export function chordParts(
  chord: string,
  names: ChordKeyNames = DEFAULT_CHORD_KEY_NAMES,
): string[] {
  return chord
    .split("+")
    .map((part) => part.trim().toLowerCase())
    .filter(Boolean)
    .map((part) => names[part as keyof ChordKeyNames] ?? part.toUpperCase());
}

/** `KeyboardEvent.code` (layout independent) as a chord token; `null` for a key the grammar has no name for. */
export function chordTokenForCode(code: string): string | null {
  if (code === "ControlLeft" || code === "ControlRight") return "ctrl";
  if (code === "AltLeft" || code === "AltRight") return "alt";
  if (code === "ShiftLeft" || code === "ShiftRight") return "shift";
  if (
    code === "MetaLeft" ||
    code === "MetaRight" ||
    code === "OSLeft" ||
    code === "OSRight"
  )
    return "win";
  if (/^Key[A-Z]$/u.test(code)) return code.slice(3).toLowerCase();
  if (/^Digit[0-9]$/u.test(code)) return code.slice(5);
  if (/^F([1-9]|1[0-9]|2[0-4])$/u.test(code)) return code.toLowerCase();
  if (code === "Space") return "space";
  if (code === "Enter" || code === "NumpadEnter") return "enter";
  if (code === "Escape") return "esc";
  if (code === "Tab") return "tab";
  if (code === "Backspace") return "backspace";
  if (code === "MouseX1") return "mouse-x1";
  if (code === "MouseX2") return "mouse-x2";
  return null;
}

/** The pseudo `code` of the side mouse buttons (`PointerEvent.button` 3 and 4). */
export function chordCodeForPointerButton(button: number): string | null {
  if (button === 3) return "MouseX1";
  if (button === 4) return "MouseX2";
  return null;
}

export interface ChordCaptureState {
  recording: boolean;
  /** The canonical tokens held right now, for the live display while recording. */
  held: string[];
}

export interface ChordCapture {
  getState(): ChordCaptureState;
  subscribe(listener: (state: ChordCaptureState) => void): () => void;
  start(): void;
  cancel(): void;
  /** Returns true when the event belongs to the capture (the caller then prevents its default). */
  down(code: string): boolean;
  up(code: string): boolean;
}

/**
 * Records one chord: every key or side button pressed while recording counts,
 * and the chord is complete when the last one is released. A key outside the
 * grammar refuses the whole chord.
 */
export function createChordCapture(handlers: {
  onCommit: (chord: string) => void;
  onRefuse: () => void;
}): ChordCapture {
  let recording = false;
  const held = new Set<string>();
  const pressed: string[] = [];
  let state: ChordCaptureState = { recording: false, held: [] };
  const listeners = new Set<(state: ChordCaptureState) => void>();

  function emit(): void {
    // Held keys in canonical order while they form a valid chord; a key
    // outside the grammar shows as "?" until the chord is refused.
    const tokens = pressed
      .filter((code) => held.has(code))
      .map((code) => chordTokenForCode(code) ?? "?");
    const canonical = tokens.length ? canonicalChord(tokens.join("+")) : null;
    state = { recording, held: canonical ? canonical.split("+") : tokens };
    for (const listener of listeners) listener(state);
  }

  function reset(): void {
    recording = false;
    held.clear();
    pressed.length = 0;
  }

  function finish(): void {
    const tokens = pressed.map(chordTokenForCode);
    reset();
    emit();
    const chord = tokens.every((token): token is string => token !== null)
      ? canonicalChord(tokens.join("+"))
      : null;
    if (chord) handlers.onCommit(chord);
    else handlers.onRefuse();
  }

  return {
    getState: () => state,
    subscribe(listener) {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
    start() {
      reset();
      recording = true;
      emit();
    },
    cancel() {
      if (!recording) return;
      reset();
      emit();
    },
    down(code) {
      if (!recording) return false;
      if (!held.has(code)) {
        held.add(code);
        if (!pressed.includes(code)) pressed.push(code);
        emit();
      }
      return true;
    },
    up(code) {
      if (!recording) return false;
      if (!held.delete(code)) return true;
      if (held.size === 0 && pressed.length > 0) finish();
      else emit();
      return true;
    },
  };
}
