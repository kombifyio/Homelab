/**
 * What the Companion settings page shows, derived from the controller's
 * state: groups in a fixed order, and inside each group the snapshot's values
 * in the order the program gave them. Port of the view model of the Brand UI
 * package's `companion-settings` entry (not yet released), without its preview
 * and its page list: the page is one tab of Homelab's own settings.
 *
 * - Not installed: only the switch; switching on installs (the adapter's
 *   `write("enabled", true)`).
 * - Installed: the switch and start with Windows, Kubi, the pill's buttons,
 *   dictation and microphone, assistant, screenshot with its quick actions,
 *   and the account: the device's sign-in as a label, speech consent while it
 *   can still be given, and the account's settings on the web as destination
 *   rows.
 * - A key without a label here (a function a newer program declares) is not
 *   drawn until a label names it; the same holds for an enum value it has
 *   not named, and for a row whose own value has no name.
 * - Unavailable: dimmed, with the reason for assistive technology only.
 */
import type {
  CompanionSettingsRefusal,
  CompanionSettingsSnapshot,
  CompanionSettingValue,
} from "./contract.js";
import type { CompanionSettingsState } from "./controller.js";
import type { CompanionSettingsLabels } from "./labels.js";

export type CompanionGroupId =
  | "companion"
  | "kubi"
  | "dictation"
  | "assistant"
  | "screenshot"
  | "pill"
  | "account";

/** The groups in page order. */
export const COMPANION_GROUP_ORDER: readonly CompanionGroupId[] = [
  "companion",
  "kubi",
  "pill",
  "dictation",
  "assistant",
  "screenshot",
  "account",
];

export interface CompanionOption {
  value: string;
  label: string;
}

interface RowBase {
  /** The setting key, or `session` / `link.<name>` for the account rows. */
  key: string;
  label: string;
  /** The accessible name: the label, plus the reason when the row is unavailable. */
  name: string;
  /** Dimmed and not operable; `reason` is spoken, never shown. */
  unavailable: boolean;
  reason?: string;
  pending: boolean;
  /** The refusal of the last write (or a standing shortcut conflict), shown on the row. */
  refusal?: string;
}

export type CompanionRow =
  | (RowBase & { kind: "switch"; value: boolean })
  | (RowBase & { kind: "choice"; value: string; options: CompanionOption[] })
  | (RowBase & { kind: "select"; value: string; options: CompanionOption[] })
  | (RowBase & {
      kind: "chord";
      value: string;
      clearable: boolean;
      invalid: boolean;
    })
  | (RowBase & { kind: "folder"; value: string })
  | (RowBase & { kind: "account"; value: string; signedIn: boolean })
  | (RowBase & { kind: "destination"; url: string });

export interface CompanionGroup {
  id: CompanionGroupId;
  title: string;
  rows: CompanionRow[];
}

export interface CompanionSettingsView {
  state: "loading" | "unreachable" | "not-installed" | "off" | "on";
  groups: CompanionGroup[];
}

const CHORD_GROUP: Readonly<Record<string, CompanionGroupId>> = {
  "dictation.shortcut": "dictation",
  "assistant.shortcut": "assistant",
  "screenshot.shortcut": "screenshot",
};

function groupOf(key: string): CompanionGroupId | null {
  if (key === "startWithWindows") return "companion";
  if (key === "showKubi" || key === "voiceAnimation") return "kubi";
  if (
    key === "dictation.shortcut" ||
    key === "dictation.behavior" ||
    key === "microphone"
  )
    return "dictation";
  if (key === "assistant.shortcut") return "assistant";
  if (
    key === "screenshot.shortcut" ||
    key === "screenshot.destination" ||
    key === "screenshot.folder"
  )
    return "screenshot";
  if (key.startsWith("screenshot.quick.")) return "screenshot";
  if (key.startsWith("pill.")) return "pill";
  if (key === "consent.speech") return "account";
  return null;
}

function labelOf(key: string, l: CompanionSettingsLabels): string | undefined {
  switch (key) {
    case "startWithWindows":
      return l.rows.startWithWindows;
    case "showKubi":
      return l.rows.showKubi;
    case "voiceAnimation":
      return l.rows.voiceAnimation;
    case "dictation.shortcut":
    case "assistant.shortcut":
    case "screenshot.shortcut":
      return l.rows.shortcut;
    case "dictation.behavior":
      return l.rows.dictationBehavior;
    case "microphone":
      return l.rows.microphone;
    case "screenshot.destination":
      return l.rows.screenshotDestination;
    case "screenshot.folder":
      return l.rows.screenshotFolder;
    case "consent.speech":
      return l.rows.consent;
  }
  if (key.startsWith("screenshot.quick."))
    return l.quick[key.slice("screenshot.quick.".length)];
  if (key.startsWith("pill.")) return l.pill[key.slice("pill.".length)];
  return undefined;
}

function refusalText(
  refusal: CompanionSettingsRefusal | undefined,
  l: CompanionSettingsLabels,
): string | undefined {
  if (!refusal) return undefined;
  switch (refusal.error) {
    case "unknown_key":
    case "not_available":
      return l.refusals.notAvailable;
    case "invalid_value":
      return l.refusals.invalidValue;
    case "shortcut_invalid":
      return l.refusals.shortcutInvalid;
    case "shortcut_conflict": {
      const group = refusal.with ? CHORD_GROUP[refusal.with] : undefined;
      return group
        ? l.refusals.inUseBy(l.sections[group])
        : l.refusals.alreadyInUse;
    }
    case "shortcut_taken":
      return l.refusals.alreadyInUse;
    default:
      return l.refusals.notSaved;
  }
}

/** Why the account may not use a pill action: the first missing condition of the contract. */
function pillReason(
  snapshot: CompanionSettingsSnapshot,
  l: CompanionSettingsLabels,
): string {
  if (!snapshot.session.signedIn) return l.reasons.signIn;
  if (snapshot.session.flag !== "on") return l.reasons.notEnabled;
  if (!snapshot.session.consent) return l.reasons.consentMissing;
  return l.refusals.notAvailable;
}

/**
 * The options this package can name. A value it has not named (a newer
 * program's microphone, animation or destination id) is not drawn, as a key
 * without a label is not: the raw id never reaches the person.
 */
function enumOptions(
  entry: Extract<CompanionSettingValue, { type: "enum" }>,
  snapshot: CompanionSettingsSnapshot,
  l: CompanionSettingsLabels,
): CompanionOption[] {
  const nameOf = (value: string): string | undefined => {
    switch (entry.key) {
      case "dictation.behavior":
        return value === "hold_to_talk"
          ? l.values.holdToTalk
          : value === "toggle"
            ? l.values.toggle
            : undefined;
      case "voiceAnimation":
        return l.voiceAnimations[value];
      case "microphone":
        return value === ""
          ? l.values.systemMicrophone
          : snapshot.microphones.find((mic) => mic.id === value)?.label;
      case "screenshot.destination":
        return l.destinations[value];
      default:
        return undefined;
    }
  };
  const options: CompanionOption[] = [];
  for (const value of entry.allowed) {
    const label = nameOf(value);
    if (label !== undefined) options.push({ value, label });
  }
  return options;
}

function nameWith(label: string, reason: string | undefined): string {
  return reason ? `${label}, ${reason}` : label;
}

export function companionSettingsView(
  state: CompanionSettingsState,
  l: CompanionSettingsLabels,
): CompanionSettingsView {
  const snapshot = state.snapshot;
  const enabledReason = snapshot
    ? undefined
    : state.status === "unreachable"
      ? l.reasons.unreachable
      : l.reasons.loading;
  const enabledRefusal = state.refusals.enabled;
  const enabledRow: CompanionRow = {
    kind: "switch",
    key: "enabled",
    label: l.rows.enabled,
    name: nameWith(l.rows.enabled, enabledReason),
    value: snapshot?.enabled ?? false,
    unavailable: !snapshot,
    reason: enabledReason,
    pending: state.pending.has("enabled"),
    refusal: refusalText(enabledRefusal, l),
  };
  const companion: CompanionGroup = {
    id: "companion",
    title: l.sections.companion,
    rows: [enabledRow],
  };

  if (!snapshot)
    return {
      state: state.status === "unreachable" ? "unreachable" : "loading",
      groups: [companion],
    };
  if (!snapshot.installed)
    return { state: "not-installed", groups: [companion] };

  const groups = new Map<CompanionGroupId, CompanionGroup>(
    COMPANION_GROUP_ORDER.map((id) => [
      id,
      id === "companion" ? companion : { id, title: l.sections[id], rows: [] },
    ]),
  );
  const off = !snapshot.running;
  const conflicts = new Map(
    snapshot.conflicts.map((conflict) => [conflict.key, conflict]),
  );

  for (const entry of snapshot.values) {
    if (entry.key === "enabled") continue;
    const group = groupOf(entry.key);
    const label = labelOf(entry.key, l);
    if (!group || !label) continue;

    let reason: string | undefined = off ? l.reasons.off : undefined;
    if (!reason && entry.key.startsWith("pill.")) {
      const action = snapshot.pill.find(
        (pill) => `pill.${pill.id}` === entry.key,
      );
      if (action && !action.allowed) reason = pillReason(snapshot, l);
    }
    // Given consent is withdrawn in the account, not here: nothing on this row to act on.
    if (entry.key === "consent.speech" && entry.value === true) continue;

    const standing = conflicts.get(entry.key);
    const refusal =
      refusalText(state.refusals[entry.key], l) ??
      (standing
        ? refusalText(
            {
              error:
                standing.with === "windows"
                  ? "shortcut_taken"
                  : "shortcut_conflict",
              with: standing.with,
            },
            l,
          )
        : undefined);
    const base = {
      key: entry.key,
      label,
      name: nameWith(label, reason),
      unavailable: Boolean(reason),
      reason,
      pending: state.pending.has(entry.key),
      refusal,
    };

    let row: CompanionRow;
    switch (entry.type) {
      case "bool":
        row = { ...base, kind: "switch", value: entry.value };
        break;
      case "enum": {
        const options = enumOptions(entry, snapshot, l);
        // The value the program holds has no name here: the row is not drawn rather than showing a raw id.
        if (!options.some((option) => option.value === entry.value)) continue;
        const kind =
          entry.key === "dictation.behavior" || entry.key === "voiceAnimation"
            ? "choice"
            : "select";
        row = { ...base, kind, value: entry.value, options };
        break;
      }
      case "chord":
        row = {
          ...base,
          kind: "chord",
          value: entry.value,
          clearable: entry.default === "",
          invalid: Boolean(refusal),
        };
        break;
      case "path":
        row = { ...base, kind: "folder", value: entry.value };
        break;
    }
    groups.get(group)!.rows.push(row);
  }

  const account = groups.get("account")!;
  account.rows.unshift({
    kind: "account",
    key: "session",
    label: l.rows.account,
    name: l.rows.account,
    value: snapshot.session.signedIn
      ? snapshot.session.label
      : l.values.notSignedIn,
    signedIn: snapshot.session.signedIn,
    unavailable: false,
    pending: false,
  });
  for (const [name, url] of Object.entries(snapshot.links)) {
    const label = l.links[name];
    if (!label || !url) continue;
    account.rows.push({
      kind: "destination",
      key: `link.${name}`,
      label,
      name: label,
      url,
      unavailable: false,
      pending: false,
    });
  }

  return {
    state: off ? "off" : "on",
    groups: COMPANION_GROUP_ORDER.map((id) => groups.get(id)!).filter(
      (group) => group.rows.length > 0,
    ),
  };
}
