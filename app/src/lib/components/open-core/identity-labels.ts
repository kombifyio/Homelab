/**
 * Localized labels for the shared Stack Identity components
 * (`@kombiverselabs/ui/identity`).
 *
 * The shared components ship English defaults and take overrides, so a host
 * that carries the platform's release locales translates every label itself.
 * Without them the settings identity editor stays English in a German, Spanish,
 * Chinese, Hindi or Arabic product ("Rocket · Not saved yet", "Customize",
 * "Save"), which is the defect `platform-v2at1o` reports. The catalog lookup
 * falls back to the English name the component passes, so a character, icon
 * style or glow preset this dictionary does not know yet still has a label.
 */
import type {
  StackIdentityCatalogKind,
  StackIdentityEditorLabels,
  StackIdentityLabelFor,
} from "@kombiverselabs/ui/identity";
import { isMessageKey, tr } from "#lib/i18n.svelte.js";

/**
 * Chrome of the identity editor: name field, section heads, buttons and the
 * saved date. Read during render, so a language change repaints it.
 */
export function stackIdentityEditorLabels(): StackIdentityEditorLabels {
  return {
    customize: tr("ui.identity.editor.customize"),
    collapse: tr("ui.identity.editor.collapse"),
    save: tr("ui.identity.editor.save"),
    saving: tr("ui.identity.editor.saving"),
    saved: tr("ui.identity.editor.saved"),
    notSavedYet: tr("ui.identity.editor.notSavedYet"),
    savedAt: (date: string) => tr("ui.identity.editor.savedAt", { date }),
    playful: tr("ui.identity.editor.playful"),
    futuristic: tr("ui.identity.editor.futuristic"),
    iconStyle: tr("ui.identity.editor.iconStyle"),
    glowColor: tr("ui.identity.editor.glowColor"),
    animations: tr("ui.identity.editor.animations"),
    selectItem: (label: string) => tr("ui.identity.editor.select", { label }),
    selectGlow: (label: string) =>
      tr("ui.identity.editor.selectGlow", { label }),
    toggleAnimations: tr("ui.identity.editor.toggleAnimations"),
    stackNamePlaceholder: tr("ui.identity.editor.stackNamePlaceholder"),
    resetToDefault: tr("ui.identity.editor.resetToDefault"),
    unknownCharacter: tr("ui.identity.editor.unknown"),
  };
}

/** The dictionary key of one catalog name, by the kind the component asks for. */
function catalogKey(kind: StackIdentityCatalogKind, id: string): string {
  if (kind === "character") return `ui.identity.${id}`;
  if (kind === "iconStyle") return `ui.identity.iconStyle.${id}`;
  return `ui.identity.glow.${id}`;
}

/**
 * Localized character, icon style and glow preset names. Unknown ids keep the
 * English catalog label the component hands over.
 */
export function stackIdentityLabelFor(
  kind: StackIdentityCatalogKind,
  id: string,
  fallback: string,
): string {
  const key = catalogKey(kind, id);
  return isMessageKey(key) ? tr(key) : fallback;
}
