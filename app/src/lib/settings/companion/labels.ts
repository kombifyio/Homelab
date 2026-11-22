/**
 * Every word the Companion settings page shows or speaks, read from Homelab's
 * own catalogs (`companion.settings.*` in all seven web catalogs; the words the
 * Companion program already shows are taken from its catalogs, so Kubi's menu
 * and these settings name things alike). Labels only: no explanation, hint or
 * reason text is shown; the `reasons` are read by assistive technology only.
 */
import { tr } from "#lib/i18n.svelte.js";
import type { ChordKeyNames } from "./chord.js";

export interface CompanionSettingsLabels {
  sections: {
    companion: string;
    kubi: string;
    dictation: string;
    assistant: string;
    screenshot: string;
    pill: string;
    account: string;
  };
  rows: {
    enabled: string;
    startWithWindows: string;
    showKubi: string;
    appsShowCompanion: string;
    voiceAnimation: string;
    shortcut: string;
    dictationBehavior: string;
    microphone: string;
    screenshotDestination: string;
    screenshotFolder: string;
    account: string;
    consent: string;
  };
  values: {
    on: string;
    off: string;
    holdToTalk: string;
    toggle: string;
    systemMicrophone: string;
    notSignedIn: string;
    chordNone: string;
  };
  /** Read by assistive technology only: a write in flight wears this word. */
  status: { saving: string; recording: string };
  voiceAnimations: Record<string, string>;
  destinations: Record<string, string>;
  quick: Record<string, string>;
  pill: Record<string, string>;
  /** Destination rows: each label names where the row leads. */
  links: Record<string, string>;
  actions: { choose: string; open: string; removeShortcut: string };
  refusals: {
    notAvailable: string;
    invalidValue: string;
    shortcutInvalid: string;
    inUseBy: (name: string) => string;
    alreadyInUse: string;
    notSaved: string;
  };
  /** Read by assistive technology only, beside a dimmed control. */
  reasons: {
    loading: string;
    unreachable: string;
    off: string;
    signIn: string;
    notEnabled: string;
    consentMissing: string;
  };
  keys: ChordKeyNames;
}

/** The labels in the active language (reactive: read inside `$derived`). */
export function companionSettingsLabels(): CompanionSettingsLabels {
  return {
    sections: {
      companion: tr("companion.settings.sections.companion"),
      kubi: tr("companion.settings.sections.kubi"),
      dictation: tr("companion.settings.sections.dictation"),
      assistant: tr("companion.settings.sections.assistant"),
      screenshot: tr("companion.settings.sections.screenshot"),
      pill: tr("companion.settings.sections.pill"),
      account: tr("companion.settings.sections.account"),
    },
    rows: {
      enabled: tr("companion.settings.rows.enabled"),
      startWithWindows: tr("companion.settings.rows.startWithWindows"),
      showKubi: tr("companion.settings.rows.showKubi"),
      appsShowCompanion: tr("companion.settings.rows.appsShowCompanion"),
      voiceAnimation: tr("companion.settings.rows.voiceAnimation"),
      shortcut: tr("companion.settings.rows.shortcut"),
      dictationBehavior: tr("companion.settings.rows.dictationBehavior"),
      microphone: tr("companion.settings.rows.microphone"),
      screenshotDestination: tr(
        "companion.settings.rows.screenshotDestination",
      ),
      screenshotFolder: tr("companion.settings.rows.screenshotFolder"),
      account: tr("companion.settings.rows.account"),
      consent: tr("companion.settings.rows.consent"),
    },
    values: {
      on: tr("companion.settings.values.on"),
      off: tr("companion.settings.values.off"),
      holdToTalk: tr("companion.settings.values.holdToTalk"),
      toggle: tr("companion.settings.values.toggle"),
      systemMicrophone: tr("companion.settings.values.systemMicrophone"),
      notSignedIn: tr("companion.settings.values.notSignedIn"),
      chordNone: tr("companion.settings.values.chordNone"),
    },
    status: {
      saving: tr("companion.settings.status.saving"),
      recording: tr("companion.settings.status.recording"),
    },
    voiceAnimations: {
      kombify: tr("companion.settings.voiceAnimations.kombify"),
      neutral: tr("companion.settings.voiceAnimations.neutral"),
    },
    destinations: { device: tr("companion.settings.destinations.device") },
    quick: {
      open: tr("companion.settings.quick.open"),
      folder: tr("companion.settings.quick.folder"),
      delete: tr("companion.settings.quick.delete"),
    },
    pill: {
      chat: tr("companion.settings.pill.chat"),
      microphone: tr("companion.settings.pill.microphone"),
      dictate: tr("companion.settings.pill.dictate"),
      assistant: tr("companion.settings.pill.assistant"),
      feed: tr("companion.settings.pill.feed"),
      screenshot: tr("companion.settings.pill.screenshot"),
    },
    links: {
      appearance: tr("companion.settings.links.appearance"),
      companion: tr("companion.settings.links.companion"),
    },
    actions: {
      choose: tr("companion.settings.actions.choose"),
      open: tr("companion.settings.actions.open"),
      removeShortcut: tr("companion.settings.actions.removeShortcut"),
    },
    refusals: {
      notAvailable: tr("companion.settings.refusals.notAvailable"),
      invalidValue: tr("companion.settings.refusals.invalidValue"),
      shortcutInvalid: tr("companion.settings.refusals.shortcutInvalid"),
      inUseBy: (name) => tr("companion.settings.refusals.inUseBy", { name }),
      alreadyInUse: tr("companion.settings.refusals.alreadyInUse"),
      notSaved: tr("companion.settings.refusals.notSaved"),
    },
    reasons: {
      loading: tr("companion.settings.reasons.loading"),
      unreachable: tr("companion.settings.reasons.unreachable"),
      off: tr("companion.settings.reasons.off"),
      signIn: tr("companion.settings.reasons.signIn"),
      notEnabled: tr("companion.settings.reasons.notEnabled"),
      consentMissing: tr("companion.settings.reasons.consentMissing"),
    },
    keys: {
      ctrl: tr("companion.settings.keys.ctrl"),
      alt: tr("companion.settings.keys.alt"),
      shift: tr("companion.settings.keys.shift"),
      win: tr("companion.settings.keys.win"),
      space: tr("companion.settings.keys.space"),
      enter: tr("companion.settings.keys.enter"),
      esc: tr("companion.settings.keys.esc"),
      tab: tr("companion.settings.keys.tab"),
      backspace: tr("companion.settings.keys.backspace"),
      "mouse-x1": tr("companion.settings.keys.mouse-x1"),
      "mouse-x2": tr("companion.settings.keys.mouse-x2"),
    },
  };
}
