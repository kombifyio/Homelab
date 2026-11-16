import { authStore } from "#lib/stores/auth.svelte.js";

/**
 * The plan page in Cloud, where a plan-less account chooses a plan. Every app
 * feature is in every paid plan, so the step names a plan, never a feature.
 */
export function planHref(): string {
  try {
    const origin = authStore.portalUrl
      ? new URL(authStore.portalUrl).origin
      : "https://kombify.io";
    return `${origin}/dashboard/billing`;
  } catch {
    return "https://kombify.io/dashboard/billing";
  }
}
