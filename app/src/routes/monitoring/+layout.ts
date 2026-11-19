import { redirect } from "@sveltejs/kit";
import { browser } from "$app/env";
import type { LayoutLoad } from "./$types";

/**
 * Protected route load function.
 * Checks authentication status and redirects to login if not authenticated.
 */
export const load: LayoutLoad = async () => {
  if (browser) {
    const { initAuth, isAuthenticated, sessionCheckUnconfirmed } =
      await import("#lib/stores/auth.svelte.js");
    await initAuth();
    if (!isAuthenticated()) {
      // No definite answer from the session check: the guard keeps the person
      // on the page instead of sending them to sign-in (AUTHENTICATION.md §6).
      if (sessionCheckUnconfirmed()) return {};
      const { resolveUnauthenticatedEntry } =
        await import("#lib/auth/unauthenticated-redirect.js");
      throw redirect(302, resolveUnauthenticatedEntry());
    }
  }
  return {};
};
