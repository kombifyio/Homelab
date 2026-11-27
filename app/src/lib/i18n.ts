/**
 * kombify-Homelab Internationalization (i18n)
 *
 * Thin runtime adapter over Paraglide JS compiled catalogs (`messages/*.json`).
 * Locale resolution: explicit choice stored on this device, then the browser
 * language list, then English.
 */

import { m } from "#lib/paraglide/messages.js";
import { defineCustomClientStrategy } from "#lib/paraglide/runtime.js";

export type Locale = "en" | "de" | "es" | "zh-Hans" | "hi" | "ar";

export const defaultLocale: Locale = "en";

export interface LocaleInfo {
  code: Locale;
  /** Endonym: the language's name in that language. */
  name: string;
  dir: "ltr" | "rtl";
}

const localeInfos: LocaleInfo[] = [
  { code: "en", name: "English", dir: "ltr" },
  { code: "de", name: "Deutsch", dir: "ltr" },
  { code: "es", name: "Español", dir: "ltr" },
  { code: "zh-Hans", name: "简体中文", dir: "ltr" },
  { code: "hi", name: "हिन्दी", dir: "ltr" },
  { code: "ar", name: "العربية", dir: "rtl" },
];

const LOCALE_STORAGE_KEY = "homelab-locale";

export function isLocale(value: unknown): value is Locale {
  return localeInfos.some((info) => info.code === value);
}

/** Map one BCP 47 tag from the browser to a supported locale, if any. */
export function matchBrowserLocale(tag: string): Locale | undefined {
  const [language] = tag.toLowerCase().split("-");
  switch (language) {
    case "en":
    case "de":
    case "es":
    case "hi":
    case "ar":
      return language;
    case "zh":
      return "zh-Hans";
    default:
      return undefined;
  }
}

/**
 * Resolve the locale: an explicit choice stored on this device wins, then the
 * first supported language of the browser, then English.
 */
export function resolveLocale(
  stored: string | null | undefined,
  browserLanguages: readonly string[],
): Locale {
  if (isLocale(stored)) return stored;
  for (const tag of browserLanguages) {
    const match = matchBrowserLocale(tag);
    if (match) return match;
  }
  return defaultLocale;
}

// Resolve the locale for this device
export function getStoredLocale(): Locale {
  if (typeof window === "undefined") return defaultLocale;
  let stored: string | null = null;
  try {
    stored = localStorage.getItem(LOCALE_STORAGE_KEY);
  } catch {
    // Storage can be blocked; fall back to the browser language.
  }
  const languages =
    typeof navigator === "undefined"
      ? []
      : navigator.languages?.length
        ? navigator.languages
        : [navigator.language];
  return resolveLocale(stored, languages);
}

// Store the explicit locale choice in localStorage
export function setStoredLocale(locale: Locale): void {
  if (typeof window === "undefined") return;
  try {
    localStorage.setItem(LOCALE_STORAGE_KEY, locale);
  } catch {
    // The choice still applies for this session.
  }
}

/** Reading direction of a locale. */
export function localeDirection(locale: Locale): "ltr" | "rtl" {
  return localeInfos.find((info) => info.code === locale)?.dir ?? "ltr";
}

// Keep the Paraglide strategy strictly read-only so Paraglide's own lazy
// getLocale() initialization never persists a browser-detected locale into
// localStorage; explicit user changes call setStoredLocale() via setLocale().
defineCustomClientStrategy("custom-homelab", {
  getLocale: () => getStoredLocale(),
  setLocale: () => {},
});

// Translation function (static, for use in non-reactive contexts)
export type MessageParams = Record<string, string | number>;

type MessageFn = (
  inputs?: Record<string, string | number>,
  options?: { locale?: Locale },
) => string;

const messageCatalog = m as unknown as Record<string, MessageFn | undefined>;

// Preserve literal `{name}` tokens (including `{slot}` used by `trParts`) when
// a caller omits a placeholder value.
const EMPTY_PLACEHOLDER_INPUTS = new Proxy<Record<string, string | number>>(
  {},
  {
    has: () => true,
    get: (_target, prop) =>
      typeof prop === "string" ? `{${prop}}` : undefined,
  },
);

function buildMessageInputs(
  params?: MessageParams,
): Record<string, string | number> {
  if (!params) return EMPTY_PLACEHOLDER_INPUTS;
  return new Proxy<Record<string, string | number>>(params, {
    has: () => true,
    get: (target, prop) => {
      if (typeof prop !== "string") return undefined;
      return Object.hasOwn(target, prop) ? target[prop] : `{${prop}}`;
    },
  });
}

export function t(
  key: string,
  locale?: Locale,
  params?: MessageParams,
): string {
  const currentLocale = locale || getStoredLocale();
  const resolve = Object.hasOwn(messageCatalog, key)
    ? messageCatalog[key]
    : undefined;
  if (typeof resolve !== "function") return key;
  return String(resolve(buildMessageInputs(params), { locale: currentLocale }));
}

// Get all available locales
export function getAvailableLocales(): { code: Locale; name: string }[] {
  return localeInfos.map(({ code, name }) => ({ code, name }));
}
