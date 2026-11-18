/**
 * The Companion settings snapshot and the host adapter, as SETTINGS-CONTRACT
 * 2.2 (AppLink v2) defines them. Port of the contract types of the Brand UI
 * package's `companion-settings` entry (not yet released). The Windows client
 * implements the adapter over its native side (`companion-bridge.ts`); the
 * page never sees a pipe, a token or a path of another program.
 */

/** One changeable setting. `allowed` is present for `enum` only. */
export type CompanionSettingValue =
  | { key: string; type: "bool"; value: boolean; default: boolean }
  | {
      key: string;
      type: "enum";
      value: string;
      default: string;
      allowed: readonly string[];
    }
  | { key: string; type: "chord"; value: string; default: string }
  | { key: string; type: "path"; value: string; default: string };

export type CompanionSettingType = CompanionSettingValue["type"];

export interface CompanionMicrophone {
  id: string;
  label: string;
  isDefault: boolean;
}

/** A chord that cannot be bound now: `with` is another key, or `windows`. */
export interface CompanionShortcutConflict {
  key: string;
  with: string;
}

/** A declared pill action; `allowed` is false when the account may not use it. */
export interface CompanionPillAction {
  id: string;
  allowed: boolean;
}

export interface CompanionVoiceAnimation {
  id: string;
  default: boolean;
}

/** The device's one kombify sign-in (contract §5). */
export interface CompanionSession {
  signedIn: boolean;
  /** The account's display label; `""` when not signed in. */
  label: string;
  flag: "on" | "off" | "unknown";
  consent: boolean;
}

/** `settings.get`: every field is always present; arrays may be empty. */
export interface CompanionSettingsSnapshot {
  revision: number;
  installed: boolean;
  running: boolean;
  enabled: boolean;
  version: string;
  values: readonly CompanionSettingValue[];
  microphones: readonly CompanionMicrophone[];
  conflicts: readonly CompanionShortcutConflict[];
  pill: readonly CompanionPillAction[];
  voiceAnimations: readonly CompanionVoiceAnimation[];
  screenshotDestinations: readonly string[];
  session: CompanionSession;
  /** Account settings that live on the web, opened in the system browser. */
  links: Readonly<Record<string, string>>;
}

/** The typed refusals of `settings.set` (contract §3); anything else is a v1 wire error. */
export const COMPANION_REFUSAL_CODES = [
  "unknown_key",
  "invalid_value",
  "shortcut_invalid",
  "shortcut_conflict",
  "shortcut_taken",
  "not_available",
  "not_saved",
] as const;

export type CompanionRefusalCode = (typeof COMPANION_REFUSAL_CODES)[number];

/** A refused write: nothing changed. `with` names the other key, or `windows`. */
export interface CompanionSettingsRefusal {
  error: CompanionRefusalCode | (string & {});
  with?: string;
}

export type CompanionSettingWrite = boolean | string;

/**
 * The only thing a client implements for the section (decision 2 of the
 * program). `write("enabled", true)` while the Companion is not installed
 * installs the carried Companion first, then switches it on.
 */
export interface CompanionSettingsAdapter {
  read(): Promise<CompanionSettingsSnapshot>;
  write(
    key: string,
    value: CompanionSettingWrite,
  ): Promise<CompanionSettingsSnapshot | CompanionSettingsRefusal>;
  /** Resolves with a revision; equal to `after` after 25 s without a change. Rejects on transport loss. */
  watch(after: number, signal: AbortSignal): Promise<number>;
  /** The client's native folder picker; `null` when the person cancelled. */
  pickFolder(current: string): Promise<string | null>;
  /** Opens a URL in the system browser. */
  openLink(url: string): void | Promise<void>;
}

export function isCompanionRefusal(
  answer: CompanionSettingsSnapshot | CompanionSettingsRefusal,
): answer is CompanionSettingsRefusal {
  return typeof (answer as CompanionSettingsRefusal).error === "string";
}
