import { redirect } from "@sveltejs/kit";
import { browser } from "$app/env";
import type { PageLoad } from "./$types";
import {
  hostNavigationRequested,
  withHostNavigation,
} from "#lib/embedded-navigation.js";

/**
 * Root page load function.
 * Redirects authenticated users to their Homelab dashboard, unauthenticated to the
 * current SaaS or self-hosted entry.
 * This runs before the page renders, preventing flash of content.
 */
export const load: PageLoad = async ({ url }) => {
  if (browser) {
    const { initAuth, isAuthenticated, sessionCheckUnconfirmed } =
      await import("#lib/stores/auth.svelte.js");
    await initAuth();
    if (isAuthenticated()) {
      const embedded = url.searchParams.get("embedded") === "true";
      const hostNavigation = embedded && hostNavigationRequested(url);
      throw redirect(
        302,
        embedded
          ? hostNavigation
            ? withHostNavigation("/dashboard?embedded=true")
            : "/dashboard?embedded=true"
          : "/dashboard",
      );
    } else {
      // No definite answer from the session check: never send a signed-in
      // person to sign-in for a service fault (AUTHENTICATION.md §6). The
      // page keeps the shell and its own error state; the check is repeated
      // once when the page is looked at again, or on the next reload.
      if (sessionCheckUnconfirmed()) return {};
      const embedded = url.searchParams.get("embedded") === "true";
      if (embedded) {
        throw redirect(
          302,
          hostNavigationRequested(url)
            ? withHostNavigation("/login?embedded=true")
            : "/login?embedded=true",
        );
      }
      const { resolveUnauthenticatedEntry } =
        await import("#lib/auth/unauthenticated-redirect.js");
      throw redirect(302, resolveUnauthenticatedEntry());
    }
  }
  // SSR: Fall through and let client-side handle the redirect
  return {};
};
