// Homelab settings tabs on KOMBIFY-DESIGN-SYSTEM-STANDARD.md §10.2.1: shared tabs in the fixed
// order account, identity, design, companion, notifications; the tool tab (homelab)
// follows. The slug is the deep-link contract (`/settings?tab=<slug>`). The `companion` tab
// exists only in the Windows client while it can show the desktop Companion (the native side
// answers the Companion bridge); in a browser and in the hosted web app it is not a tab and
// `?tab=companion` falls back to Account. The `ai-providers` tool tab (AI providers: the local AI
// runtimes this PC shares) exists only in the Windows client with the flag `local_ai_providers` on;
// elsewhere `?tab=ai-providers` falls back to Account too.

export const SHARED_SETTINGS_TABS = [
  "account",
  "identity",
  "design",
  "companion",
  "notifications",
] as const;
export const TOOL_SETTINGS_TABS = ["ai-providers", "homelab"] as const;

export type SettingsTabSlug =
  (typeof SHARED_SETTINGS_TABS)[number] | (typeof TOOL_SETTINGS_TABS)[number];

const KNOWN = new Set<string>([...SHARED_SETTINGS_TABS, ...TOOL_SETTINGS_TABS]);

/**
 * Canonical tab for a `?tab=` value: unknown or missing falls back to Account, and so does
 * `companion` while the Companion tab is not available here.
 */
export function resolveSettingsTab(
  raw: string | null | undefined,
  companionAvailable = false,
  aiProvidersAvailable = false,
): SettingsTabSlug {
  const slug = (raw ?? "").trim().toLowerCase();
  if (slug === "companion" && !companionAvailable) return "account";
  if (slug === "ai-providers" && !aiProvidersAvailable) return "account";
  return KNOWN.has(slug) ? (slug as SettingsTabSlug) : "account";
}

export function settingsTabHref(slug: SettingsTabSlug): string {
  return slug === "account" ? "/settings" : `/settings?tab=${slug}`;
}
