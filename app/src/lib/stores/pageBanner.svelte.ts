/**
 * Page title and page action of the phone top bar.
 *
 * Below 48rem the layout's top bar (menu button, product mark) is the only
 * chrome above a page. A page that wants its title and its one action up
 * there, instead of in a title block of its own, registers them with
 * `usePageBanner`; the layout renders them and reports through `hosted`
 * whether a top bar exists at all (embedded shells have none), so the page
 * keeps its own header where nothing else would show it.
 */
import type { Component } from "svelte";

export interface PageBannerAction {
  /** Accessible name; the button itself shows only the icon. */
  label: string;
  icon: Component<{ class?: string }>;
  onSelect: () => void;
  disabled?: boolean;
  testId?: string;
  /** Coach-mark anchor (KOMBIFY-DESIGN-SYSTEM-STANDARD.md §12.6). */
  anchor?: string;
}

export interface PageBanner {
  /** The page's title; it becomes the page heading at phone width. */
  title?: string;
  action?: PageBannerAction;
}

let entry = $state<{ owner: symbol; value: PageBanner } | null>(null);
let hosted = $state(false);

export const pageBanner = {
  /** What the current page asked the top bar to show. */
  get current(): PageBanner | null {
    return entry?.value ?? null;
  },
  /** True while the layout renders a phone top bar that shows the banner. */
  get hosted(): boolean {
    return hosted;
  },
};

/** Called by the layout: whether its phone top bar exists in this shell. */
export function setPageBannerHosted(value: boolean): void {
  hosted = value;
}

/**
 * Register the page's banner for as long as the calling component lives.
 * `read` is tracked, so the banner follows the page's state.
 */
export function usePageBanner(read: () => PageBanner | null): void {
  const owner = Symbol("page-banner");
  // Updates replace the entry in place; only the page's end clears it, so
  // the top bar never sees an empty banner between two updates.
  $effect(() => {
    const value = read();
    entry = value ? { owner, value } : null;
  });
  $effect(() => () => {
    if (entry?.owner === owner) entry = null;
  });
}
