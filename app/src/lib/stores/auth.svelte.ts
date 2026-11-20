/**
 * Unified Auth Store for kombify-TechStack
 *
 * Manages authentication state across both self-hosted and
 * cloud-hosted (kombify Cloud SSO) deployment modes. This store is the single
 * source of truth for authentication state in the frontend.
 *
 * Features:
 * - Automatic auth mode detection on init
 * - Local Go session integration (self-hosted)
 * - Hosted cloud SSO integration (cloud mode)
 * - User role detection (admin vs homelab user)
 * - Session persistence and refresh
 */

import { goto } from "$app/navigation";
import { browser } from "$app/env";
import * as Sentry from "@sentry/sveltekit";
import {
  getAuthMode,
  getV2AuthProviders,
  getV2WhoAmI,
  isV2WhoAmINoSession,
  loginWithLocalSession,
  logoutLocalSession,
  verifyPortalToken,
  type AuthMode,
  type V2WhoAmIResponse,
  type DeploymentMode,
  type CloudUser,
  type WalletSource,
} from "#lib/api/auth.js";
import {
  buildCloudAuthRedirectURL,
  buildV2ProviderLogoutPath,
  currentAuthReturnTo,
  getPostLogoutRedirectPath,
  resolveLoginExperience,
} from "#lib/auth/login-experience.js";
import {
  hasWindowsLocalClientContext,
  windowsLocalClientReturnUrl,
} from "#lib/client/windows-onboarding.js";
import { setStackIdentity } from "#lib/stores/stackIdentity.js";
import {
  clearGatewayAuth,
  completeGatewayRedirectIfPresent,
  getGatewayToken,
  startGatewayLogin,
} from "#lib/auth/gateway-auth.js";
import {
  isSilentAuthFailure,
  markAutoReloginAttempt,
  noteGatewayTokenSuccess,
  shouldAttemptAutoRelogin,
} from "#lib/auth/session-recovery.js";
import { clearTechstackSecuritySessionState } from "#lib/logout-cleanup.js";

import { tr } from "#lib/i18n.svelte.js";
const CLOUD_PORTAL_SESSION_PROVIDER_ID = "cloud";

/**
 * How long the first session check waits before it repeats, when a failure
 * said nothing definite about the session (workspace AUTHENTICATION.md §6:
 * "a timeout, a 5xx, a 429, a network fault and an unreadable answer are no
 * definite answer"). Three repeats keep a fault from reaching a person before
 * the check has had a fair chance to answer.
 */
const V2_SESSION_CHECK_BACKOFF_MS = [500, 1_500, 4_000] as const;

function wait(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/**
 * One structured event per check that ended in doubt: which read failed, the
 * cause, the route and the client, through the telemetry the surrounding code
 * uses.
 */
function captureSessionCheckInDoubt(
  cause: unknown,
  check: "whoami" | "providers",
): void {
  Sentry.withScope((scope) => {
    scope.setTag("component", "auth_session_check");
    scope.setTag("check", check);
    scope.setTag("outcome", "unconfirmed");
    scope.setTag("client", "web");
    scope.setExtra(
      "cause",
      cause instanceof Error ? cause.message : String(cause),
    );
    if (typeof window !== "undefined") {
      scope.setTag("route", window.location.pathname);
    }
    scope.setLevel("warning");
    Sentry.captureMessage("techstack_session_check_in_doubt");
  });
}

// ============================================================================
// Types
// ============================================================================

export interface AuthState {
  /** Current authentication mode */
  mode: AuthMode;
  /** Whether auth mode has been detected */
  modeDetected: boolean;
  /** Cloud auth URL for hosted cloud login */
  cloudAuthUrl: string | null;
  /** Portal URL for cloud mode */
  portalUrl: string | null;
  /** Whether local login is allowed in cloud mode */
  allowLocalLogin: boolean;
  /** Current cloud user claims (if any) */
  cloudUser: CloudUser | null;
  /** Whether user is authenticated */
  isAuthenticated: boolean;
  /** A platform read the guard needs got no definite answer (fault, not signed out). */
  sessionCheckUnconfirmed: boolean;
  /** Loading state */
  loading: boolean;
  /** Error message */
  error: string | null;
}

// ============================================================================
// Auth Store (Svelte 5 Runes)
// ============================================================================

class AuthStore {
  private initInFlight: Promise<void> | null = null;
  private initialized = false;
  private subscriptionsBound = false;
  private hostedProviderIDs = new Set<string>();
  /**
   * Whether `hostedProviderIDs` is the server's answer. A failed read leaves it
   * unloaded: an empty set then means "unknown", never "no hosted provider",
   * so no session is ended on it.
   */
  private hostedProviderIDsLoaded = false;
  /** One re-check on the next visible/focus event, never a timer. */
  private returnRecheckBound = false;

  // Core state using Svelte 5 runes
  mode = $state<AuthMode>("local");
  deploymentMode = $state<DeploymentMode>("self-hosted");
  isFirstRun = $state(false);
  modeDetected = $state(false);
  cloudAuthUrl = $state<string | null>(null);
  v2LoginUrl = $state<string | null>(null);
  portalUrl = $state<string | null>(null);
  allowLocalLogin = $state(true);
  /** Which Wallet backs the app; `legacy` until the backend says otherwise. */
  walletSource = $state<WalletSource>("legacy");
  cloudUser = $state<CloudUser | null>(null);
  v2SessionActive = $state(false);
  /**
   * A platform read that the guard needs got no definite answer: the session
   * check itself when no confirmed state exists, or the hosted-provider list
   * that decides whether a valid session's provider is hosted here. Route
   * guards must not send a person to sign-in for this: it names a service
   * fault, not a session that ended.
   */
  v2SessionUnconfirmed = $state(false);
  loading = $state(false);
  error = $state<string | null>(null);

  // Derived state
  get isAuthenticated(): boolean {
    return this.v2SessionActive && this.cloudUser !== null;
  }

  get currentUser(): CloudUser | null {
    return this.cloudUser;
  }

  get userEmail(): string | null {
    return this.cloudUser?.email ?? null;
  }

  get userName(): string | null {
    return this.cloudUser?.name || this.cloudUser?.email || null;
  }

  // ============================================================================
  // Initialization
  // ============================================================================

  /**
   * Initialize auth state - call this on app mount
   */
  async init(options: { embedded?: boolean } = {}): Promise<void> {
    if (!browser) return;

    // Derive iframe context at the authority boundary so alternate boot paths
    // (including initAuth()) cannot accidentally issue a pre-handoff whoami.
    const embedded = options.embedded ?? window.parent !== window;

    if (this.initialized) return;

    if (this.initInFlight) {
      await this.initInFlight;
      return;
    }

    this.initInFlight = (async () => {
      this.loading = true;
      this.error = null;

      try {
        // 1. Detect auth mode from backend
        await this.detectAuthMode();

        // 1.5 Detect whether the V2 auth endpoints are available.
        await this.detectV2Auth();

        // SPA gateway login returns to the app origin with code+state. Claim
        // that transaction before the v2 OIDC handler can steal it.
        const gatewayReturn = await completeGatewayRedirectIfPresent();
        if (gatewayReturn) {
          if (!embedded) {
            await this.syncV2Session();
          }
          await goto(gatewayReturn);
          return;
        }

        const pendingOidc = this.hasOidcCallbackParams();

        // 2. Check for an existing V2 cookie-backed session.
        // Embedded identity is established by the parent portal exchange. A
        // speculative same-origin whoami before that handoff only produces a
        // misleading 401 and cannot authenticate the iframe.
        if (!embedded) {
          await this.syncV2Session();
        }

        // 3. Check for hosted OIDC callback params.
        await this.checkPortalRedirect();
        if (pendingOidc) return;

        // 4. Cookie session is not the gateway API token. Mint it once via
        // the SPA client so standalone demo/Universal Login users can call
        // api.kombify.io instead of sitting on a dead error banner.
        if (!embedded) {
          const redirected = await this.ensureStandaloneGatewayToken();
          if (redirected) return;
        }

        if (!this.subscriptionsBound) {
          this.subscriptionsBound = true;
        }
      } catch (err) {
        console.error("[AuthStore] Init error:", err);
        this.error =
          err instanceof Error ? err.message : tr("ui.auth.authInitFailed");
      } finally {
        this.loading = false;
        this.initialized = true;
      }
    })();

    try {
      await this.initInFlight;
    } finally {
      this.initInFlight = null;
    }
  }

  /**
   * Re-check the authoritative cookie-backed browser session.
   *
   * Unlike init(), this method does not treat the first page boot as the last
   * word: callers use it after a 401 so a renewed login is adopted without a
   * reload and an expired session is not reported as refreshed.
   */
  async refreshSession(options: { embedded?: boolean } = {}): Promise<boolean> {
    if (!browser) return false;

    if (!this.initialized) {
      await this.init(options);
      return this.v2SessionActive;
    }

    return this.syncV2Session();
  }

  /**
   * Session renewal probe (#lib/auth/session-renewal.svelte.ts): adopt a
   * session that is valid again, and leave the signed-in state untouched
   * when it is not yet, so the page and its unsaved input stay mounted while
   * the renewal is still under way.
   */
  async probeRenewedSession(): Promise<boolean> {
    if (!browser || !this.initialized) return false;
    try {
      await getV2WhoAmI();
    } catch {
      return false;
    }
    return this.syncV2Session();
  }

  /**
   * Detect auth mode from backend
   */
  private async detectAuthMode(): Promise<void> {
    try {
      const authModeResponse = await getAuthMode();
      this.mode = authModeResponse.mode;
      this.deploymentMode = authModeResponse.deployment_mode || "self-hosted";
      this.isFirstRun = authModeResponse.is_first_run || false;
      this.cloudAuthUrl = authModeResponse.cloud_auth_url;
      this.portalUrl = authModeResponse.portal_url;
      this.allowLocalLogin = authModeResponse.allow_local_login;
      this.walletSource =
        authModeResponse.wallet_source === "kombify" ? "kombify" : "legacy";
      this.modeDetected = true;
    } catch {
      // Default to local mode if backend is unreachable
      console.warn(
        "[AuthStore] Could not detect auth mode, defaulting to local",
      );
      this.mode = "local";
      this.deploymentMode = "self-hosted";
      this.isFirstRun = false;
      this.walletSource = "legacy";
      this.modeDetected = true;
    }
  }

  /**
   * Read the hosted-provider list. A fault said nothing about it: the read
   * repeats with the session check's ladder, and if it still fails the list
   * stays unloaded (so no session is ended on it) and the doubt is counted.
   */
  private async detectV2Auth(): Promise<void> {
    for (let attempt = 0; ; attempt++) {
      try {
        const providers = await getV2AuthProviders();
        const hostedProviderIDs = providers
          .map((provider) => provider.id.trim())
          .filter((providerID) => providerID.length > 0);
        if (this.deploymentMode === "saas") {
          // The embedded portal exchange issues the same authoritative V2
          // browser session with provider=cloud. Keep it on the existing hosted
          // session allowlist so syncV2Session does not immediately clear the
          // freshly verified cookie as if it were a local compatibility session.
          hostedProviderIDs.push(CLOUD_PORTAL_SESSION_PROVIDER_ID);
        }
        this.hostedProviderIDs = new Set(hostedProviderIDs);
        this.hostedProviderIDsLoaded = true;
        if (providers.length > 0) {
          this.v2LoginUrl = "/api/v2/auth/login";
          if (!this.cloudAuthUrl) {
            this.cloudAuthUrl = this.v2LoginUrl;
          }
        } else {
          this.v2LoginUrl = null;
        }
        return;
      } catch (err) {
        const backoffMs = V2_SESSION_CHECK_BACKOFF_MS[attempt];
        if (backoffMs === undefined) {
          this.hostedProviderIDs.clear();
          this.hostedProviderIDsLoaded = false;
          this.v2LoginUrl = null;
          captureSessionCheckInDoubt(err, "providers");
          return;
        }
        await wait(backoffMs);
      }
    }
  }

  /**
   * Re-check the authoritative cookie-backed browser session.
   *
   * Only the server's 401 is a session that ended. A network fault, a
   * timeout, a 5xx or a 429 is no answer: the last confirmed state stays, and
   * with none yet the check repeats before anything is decided (workspace
   * AUTHENTICATION.md §6, "Authentication holds; doubt admits").
   */
  private async syncV2Session(): Promise<boolean> {
    const confirmed = this.cloudUser !== null && this.v2SessionActive;
    for (let attempt = 0; ; attempt++) {
      const check = await this.checkV2Session();
      if (check.kind === "session") {
        const whoami = check.whoami;
        const providerGovernsSession =
          this.deploymentMode === "saas" &&
          !this.allowLocalLogin &&
          Boolean(whoami.provider);
        // A session from a provider this deployment does not host is ended
        // only when the list is the server's answer. When the list could not
        // be read, the session stays and the guard is in doubt (the fault is
        // counted where the read failed).
        if (
          providerGovernsSession &&
          this.hostedProviderIDsLoaded &&
          !this.hostedProviderIDs.has(whoami.provider ?? "")
        ) {
          await logoutLocalSession();
          this.cloudUser = null;
          this.v2SessionActive = false;
          this.v2SessionUnconfirmed = false;
          return false;
        }
        this.cloudUser = v2WhoAmIToCloudUser(whoami);
        this.v2SessionActive = true;
        this.v2SessionUnconfirmed =
          providerGovernsSession && !this.hostedProviderIDsLoaded;
        if (this.v2SessionUnconfirmed) {
          this.recheckWhenVisibleAgain();
        }
        return true;
      }
      if (check.kind === "none") {
        this.cloudUser = null;
        this.v2SessionActive = false;
        this.v2SessionUnconfirmed = false;
        return false;
      }
      // No definite answer. A session already confirmed stays as it is; a
      // first load repeats the check instead of deciding.
      if (confirmed) {
        this.v2SessionUnconfirmed = false;
        captureSessionCheckInDoubt(check.cause, "whoami");
        return true;
      }
      const backoffMs = V2_SESSION_CHECK_BACKOFF_MS[attempt];
      if (backoffMs === undefined) {
        this.v2SessionUnconfirmed = true;
        captureSessionCheckInDoubt(check.cause, "whoami");
        this.recheckWhenVisibleAgain();
        return false;
      }
      await wait(backoffMs);
    }
  }

  /**
   * One re-check of an unconfirmed state when the person looks at the page
   * again (it becomes visible or the window regains focus). One listener, no
   * timer; it is removed after that single re-check, which repeats the
   * provider read when it never loaded and then the session check. Guards keep
   * the page in the meantime, so the page gets a fresh answer without a reload.
   */
  private recheckWhenVisibleAgain(): void {
    if (
      !browser ||
      this.returnRecheckBound ||
      typeof document === "undefined"
    ) {
      return;
    }
    this.returnRecheckBound = true;
    const recheck = () => {
      document.removeEventListener("visibilitychange", onVisibilityChange);
      window.removeEventListener("focus", recheck);
      // A later doubt may bind its own single re-check.
      this.returnRecheckBound = false;
      if (!this.v2SessionUnconfirmed) return;
      void (async () => {
        // Both reads the doubt can rest on are repeated once: the provider
        // list when it never loaded, then the session.
        if (!this.hostedProviderIDsLoaded) await this.detectV2Auth();
        await this.syncV2Session();
      })();
    };
    const onVisibilityChange = () => {
      if (document.visibilityState === "visible") recheck();
    };
    document.addEventListener("visibilitychange", onVisibilityChange);
    window.addEventListener("focus", recheck);
  }

  private async checkV2Session(): Promise<
    | { kind: "session"; whoami: V2WhoAmIResponse }
    | { kind: "none" }
    | { kind: "fault"; cause: unknown }
  > {
    try {
      return { kind: "session", whoami: await getV2WhoAmI() };
    } catch (err) {
      return isV2WhoAmINoSession(err)
        ? { kind: "none" }
        : { kind: "fault", cause: err };
    }
  }

  /** Check for hosted OIDC callback params. */
  private hasOidcCallbackParams(): boolean {
    if (!browser) return false;
    const params = new URLSearchParams(window.location.search);
    return Boolean(params.get("code") && params.get("state"));
  }

  /**
   * Acquire the API-audience token for standalone SaaS. Returns true when a
   * full-page SPA login navigation started.
   */
  private async ensureStandaloneGatewayToken(): Promise<boolean> {
    if (this.deploymentMode !== "saas" || !this.v2SessionActive) {
      return false;
    }
    try {
      await getGatewayToken();
      noteGatewayTokenSuccess();
      return false;
    } catch (err) {
      if (!isSilentAuthFailure(err) || !shouldAttemptAutoRelogin()) {
        return false;
      }
      markAutoReloginAttempt();
      return startGatewayLogin({ returnTo: currentAuthReturnTo() });
    }
  }

  private async checkPortalRedirect(): Promise<void> {
    if (!browser) return;

    const params = new URLSearchParams(window.location.search);
    const code = params.get("code");
    const state = params.get("state");

    // Handle OIDC callback (code + state)
    if (code && state) {
      await this.handleOIDCCallback(code, state);
      // Clean URL
      const url = new URL(window.location.href);
      url.searchParams.delete("code");
      url.searchParams.delete("state");
      window.history.replaceState({}, "", url.toString());
    }
  }

  // ============================================================================
  // Login Methods
  // ============================================================================

  /**
   * Login with email and password via the Go local session endpoint.
   */
  async loginWithPassword(email: string, password: string): Promise<boolean> {
    this.loading = true;
    this.error = null;

    try {
      await loginWithLocalSession(email, password);
      await this.syncV2Session();
      return this.v2SessionActive;
    } catch (err) {
      console.error("[AuthStore] Login error:", err);
      this.error = tr("ui.auth.invalidEmailOrPassword");
      return false;
    } finally {
      this.loading = false;
    }
  }

  /**
   * Initiate cloud login (redirect to the hosted identity provider)
   */
  initiateCloudLogin(options?: {
    returnTo?: string | null;
    redirect?: (url: string) => void;
  }): string | null {
    const target = this.v2LoginUrl || this.cloudAuthUrl;
    if (target) {
      const redirectURL = buildCloudAuthRedirectURL(target, {
        returnTo: options?.returnTo ?? currentAuthReturnTo(),
      });
      (options?.redirect ?? window.location.assign.bind(window.location))(
        redirectURL,
      );
      return redirectURL;
    } else {
      this.error = tr("ui.auth.cloudAuthenticationNotConfigured");
      return null;
    }
  }

  /**
   * Complete portal SSO login and sync the shared auth state.
   */
  async completePortalLogin(token: string): Promise<void> {
    const data = await verifyPortalToken(token);
    const v2SessionConfirmed = await this.syncV2Session();
    if (!v2SessionConfirmed || !this.v2SessionActive || !this.cloudUser) {
      throw new Error(tr("ui.auth.portalSignInDidNot"));
    }
    this.cloudUser = data.cloud_user
      ? portalCloudUserToCloudUser(data.cloud_user)
      : this.cloudUser;
    setStackIdentity(data.stack_identity ?? null);
  }

  /**
   * Handle OIDC callback (after hosted cloud redirect)
   */
  private async handleOIDCCallback(code: string, state: string): Promise<void> {
    const callbackUrl = new URL(
      "/api/v2/auth/callback",
      window.location.origin,
    );
    callbackUrl.searchParams.set("code", code);
    callbackUrl.searchParams.set("state", state);
    window.location.replace(callbackUrl.toString());
  }

  // ============================================================================
  // Logout
  // ============================================================================

  /**
   * Clear the local auth/session state without navigation.
   */
  async clearSession(): Promise<void> {
    await logoutLocalSession();
    await clearGatewayAuth();
    clearTechstackSecuritySessionState();
    this.cloudUser = null;
    this.v2SessionActive = false;
    this.v2SessionUnconfirmed = false;
  }

  /**
   * Logout from all auth providers
   */
  async logout(options?: {
    manualLogin?: boolean;
    redirect?: (url: string) => void;
  }): Promise<void> {
    this.loading = true;

    try {
      const shouldUseV2ProviderLogout = this.deploymentMode !== "self-hosted";

      await this.clearSession();

      const nextPath = options?.manualLogin
        ? getManualLogoutRedirectPath({
            deploymentMode: this.deploymentMode,
            embedded: false,
          })
        : getPostLogoutRedirectPath({
            deploymentMode: this.deploymentMode,
            embedded: false,
          });

      if (browser && shouldUseV2ProviderLogout) {
        (options?.redirect ?? window.location.assign.bind(window.location))(
          buildV2ProviderLogoutPath({
            deploymentMode: this.deploymentMode,
            nextPath,
          }),
        );
        return;
      }

      if (
        browser &&
        resolveLoginExperience({
          deploymentMode: this.deploymentMode,
          embedded: false,
        }) === "saas-auth0"
      ) {
        window.location.assign("/api/v1/auth/logout");
        return;
      }

      // Future: implement back-channel logout with the hosted identity provider.
      await goto(nextPath);
    } finally {
      this.loading = false;
    }
  }

  // ============================================================================
  // Helpers
  // ============================================================================

  /**
   * Reset auth store state
   */
  reset(): void {
    this.initialized = false;
    this.mode = "local";
    this.modeDetected = false;
    this.cloudAuthUrl = null;
    this.v2LoginUrl = null;
    this.hostedProviderIDs.clear();
    this.hostedProviderIDsLoaded = false;
    this.returnRecheckBound = false;
    this.portalUrl = null;
    this.allowLocalLogin = true;
    this.cloudUser = null;
    this.v2SessionActive = false;
    this.v2SessionUnconfirmed = false;
    clearTechstackSecuritySessionState();
    this.loading = false;
    this.error = null;
  }

  /**
   * Get full auth state (for debugging)
   */
  getState(): AuthState {
    return {
      mode: this.mode,
      modeDetected: this.modeDetected,
      cloudAuthUrl: this.cloudAuthUrl,
      portalUrl: this.portalUrl,
      allowLocalLogin: this.allowLocalLogin,
      cloudUser: this.cloudUser,
      isAuthenticated: this.isAuthenticated,
      sessionCheckUnconfirmed: this.v2SessionUnconfirmed,
      loading: this.loading,
      error: this.error,
    };
  }
}

// Export singleton instance
export const authStore = new AuthStore();

// ============================================================================
// Convenience Exports
// ============================================================================

/**
 * Initialize auth - call on app mount
 */
export async function initAuth(): Promise<void> {
  return authStore.init();
}

/**
 * Check if user is authenticated
 */
export function isAuthenticated(): boolean {
  return authStore.isAuthenticated;
}

/**
 * True when a platform read the guard needs got no definite answer (the
 * session check itself, or the hosted-provider list): a service fault, never a
 * session that ended.
 */
export function sessionCheckUnconfirmed(): boolean {
  return authStore.v2SessionUnconfirmed;
}

function v2WhoAmIToCloudUser(user: V2WhoAmIResponse): CloudUser {
  const roles = user.role ? [user.role] : [];
  return {
    sub: user.subject,
    orgId: user.orgId,
    tenantId: user.tenantId,
    email: user.email ?? "",
    email_verified: true,
    name: user.email || user.subject,
    provider: user.provider,
    role: user.role,
    roles,
  };
}

function portalCloudUserToCloudUser(user: {
  sub: string;
  email: string;
  name: string;
}): CloudUser {
  return {
    sub: user.sub,
    email: user.email,
    email_verified: true,
    name: user.name || user.email || user.sub,
    roles: [],
  };
}

function getManualLogoutRedirectPath(options: {
  deploymentMode: DeploymentMode;
  embedded: boolean;
}): string {
  if (browser) {
    try {
      if (hasWindowsLocalClientContext(window.localStorage)) {
        return windowsLocalClientReturnUrl;
      }
    } catch {
      // Storage can be unavailable in restricted browser contexts.
    }
  }

  return getPostLogoutRedirectPath(options);
}
