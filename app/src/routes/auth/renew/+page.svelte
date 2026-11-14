<script lang="ts">
  /**
   * Session renewal route (`renew_url` of the session renewal signal,
   * workspace AUTHENTICATION.md §6). The shared renewal layer opens it in a
   * small window; it hands the window to the product sign-in, which on a
   * live Auth0 session completes without a prompt and returns to
   * /auth/reauth-complete, where the window tells the waiting page and
   * closes. With `return_to` (the last-resort full-page round trip) it comes
   * back to that page instead.
   */
  import { onMount } from "svelte";
  import { getAuthMode, getV2AuthProviders } from "#lib/api/auth.js";
  import {
    buildCloudAuthRedirectURL,
    sanitizeAuthReturnTo,
  } from "#lib/auth/login-experience.js";
  import { STEP_UP_COMPLETE_PATH } from "#lib/auth/step-up.js";
  import { tr } from "#lib/i18n.svelte.js";

  const V2_LOGIN_PATH = "/api/v2/auth/login";

  async function loginTarget(): Promise<string> {
    try {
      if ((await getV2AuthProviders()).length > 0) return V2_LOGIN_PATH;
    } catch {
      // Fall back to the mode's own sign-in entry.
    }
    try {
      const mode = await getAuthMode();
      if (mode.cloud_auth_url) return mode.cloud_auth_url;
    } catch {
      // Use the local entry below.
    }
    return "/login";
  }

  onMount(() => {
    void (async () => {
      const requested = new URLSearchParams(window.location.search).get(
        "return_to",
      );
      const returnTo = requested
        ? sanitizeAuthReturnTo(requested)
        : STEP_UP_COMPLETE_PATH;
      window.location.replace(
        buildCloudAuthRedirectURL(await loginTarget(), { returnTo }),
      );
    })();
  });
</script>

<svelte:head>
  <title>{tr("sessionRenewal.windowTitle")}</title>
</svelte:head>

<div class="min-h-screen bg-background flex items-center justify-center">
  <div
    class="max-w-md w-full px-6 text-center"
    data-testid="session-renewal-window"
  >
    <h1 class="text-xl font-semibold text-foreground mb-2">
      {tr("sessionRenewal.windowTitle")}
    </h1>
    <p class="text-muted-foreground">{tr("sessionRenewal.windowBody")}</p>
  </div>
</div>
