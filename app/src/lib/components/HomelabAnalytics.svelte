<script lang="ts">
  import { afterNavigate } from "$app/navigation";
  import * as Sentry from "@sentry/sveltekit";
  import { getClientBootstrap } from "#lib/client/bootstrap.js";
  import { analyticsRelease } from "#lib/config.js";
  import {
    classifyRoute,
    createPostHogClient,
    isValidEventName,
    sanitizeNavigationTarget,
    toHomelabAnalyticsUser,
    type AnalyticsUser,
  } from "#lib/analytics/posthog.js";
  import { authStore } from "#lib/stores/auth.svelte.js";

  let lastIdentityKey = $state("");
  let lastRouteKey = $state("");
  // Not reactive: the identity effect must not re-run when it changes.
  let identifiedSubject = "";

  afterNavigate(({ to, shallow }) => {
    if (shallow) return;
    if (!to?.url) return;
    captureRoute(to.url);
  });

  $effect(() => {
    const user = getAnalyticsUser();
    const identityKey = [
      user?.authSubject,
      user?.organizationId,
      user?.role,
    ].join("|");

    if (user?.authSubject && identityKey !== lastIdentityKey) {
      lastIdentityKey = identityKey;
      Sentry.setUser({ id: user.authSubject });
      Sentry.setTag("organization_id", user.organizationId ?? "unscoped");
      identify(user);
      captureRoute(new URL(window.location.href));
      return;
    }

    if (!user?.authSubject && lastIdentityKey) {
      lastIdentityKey = "";
      lastRouteKey = "";
      identifiedSubject = "";
      Sentry.setUser(null);
      Sentry.setTag("organization_id", "unscoped");
    }
  });

  // One identification per subject and document: the Cloud user fills in
  // organization and role after the handover, which is no new session. A
  // refused capture (no consent yet) is tried again on the next page.
  function identify(user: AnalyticsUser): void {
    const subject = user.authSubject;
    if (!subject || identifiedSubject === subject) return;
    identifiedSubject = subject;
    void createClient()
      .capture("auth:session_identified", {
        user,
        properties: {
          deployment_mode: authStore.deploymentMode,
        },
      })
      .then((sent) => {
        if (!sent && identifiedSubject === subject) identifiedSubject = "";
      });
  }

  function createClient() {
    const bootstrap = getClientBootstrap();
    return createPostHogClient({
      apiKey: bootstrap.telemetry.posthog.key || undefined,
      host: bootstrap.telemetry.posthog.host || undefined,
      environment: bootstrap.telemetry.posthog.environment || undefined,
      edition:
        bootstrap.kombifyEdition ||
        (authStore.deploymentMode === "saas"
          ? "saas-embedded"
          : "selfhost-oss"),
      appVersion: analyticsRelease,
      location: window.location,
    });
  }

  function getAnalyticsUser(): AnalyticsUser | null {
    return toHomelabAnalyticsUser(authStore.cloudUser);
  }

  function captureRoute(url: URL): void {
    const user = getAnalyticsUser();
    if (!user?.authSubject) {
      return;
    }

    const routeKey = `${url.pathname}${url.hash || ""}`;
    if (routeKey === lastRouteKey) {
      return;
    }

    lastRouteKey = routeKey;
    identify(user);
    const routeGroup = classifyRoute(url.pathname);
    void createClient().capture("homelab:page_viewed", {
      user,
      properties: {
        page_id: routeGroup,
      },
    });
  }

  function handleClick(event: MouseEvent): void {
    const user = getAnalyticsUser();
    if (!user?.authSubject) {
      return;
    }

    const target = event.target;
    if (!(target instanceof Element)) {
      return;
    }

    const explicitTarget = target.closest<HTMLElement>(
      "[data-posthog-event], [data-analytics-event]",
    );
    const explicitEvent =
      explicitTarget?.dataset.posthogEvent ??
      explicitTarget?.dataset.analyticsEvent;
    if (explicitTarget && explicitEvent && isValidEventName(explicitEvent)) {
      void createClient().capture(explicitEvent, {
        user,
        properties: {
          item_id: explicitTarget.dataset.analyticsItemId,
          navigation_area: explicitTarget.dataset.analyticsNavigationArea,
          deployment_mode: authStore.deploymentMode,
        },
      });
      return;
    }

    const anchor = target.closest<HTMLAnchorElement>("a[href]");
    if (!anchor) {
      return;
    }

    const href = sanitizeNavigationTarget(anchor.href, window.location.origin);
    if (!href) {
      return;
    }

    void createClient().capture("navigation:item_clicked", {
      user,
      properties: {
        item_id: classifyRoute(href),
        navigation_area: classifyRoute(window.location.pathname),
      },
    });
  }
</script>

<svelte:window onclick={handleClick}></svelte:window>
