/**
 * The one shared session renewal layer of the Techstack web client
 * (workspace platform/standards/AUTHENTICATION.md §6, "Session renewal, never
 * a sign-in error").
 *
 * A server answers a request whose session needs renewal with HTTP 401,
 * header `x-kombify-session-renewal: required` and JSON
 * `error_code: "session_renewal_required"` (with `reason` and a same-origin
 * `renew_url`) before any side effect. The kombify Gateway sends the same
 * header when it refuses the user's bearer. Every request of this app goes
 * through {@link fetchWithSessionRenewal}, which recognizes the signal, runs
 * ONE renewal for all waiting requests and replays each request once.
 *
 * Renewal ladder, per kind:
 * - "session" (the Techstack browser session cookie): embedded in kombify
 *   Cloud, the host re-mints the SSO token over the postMessage bridge and
 *   portal-verify re-issues the cookie; otherwise the renewal route
 *   (`/auth/renew`) opens in a small Universal Login window that completes
 *   without a prompt on a live Auth0 session and reports back through
 *   `/auth/reauth-complete`.
 * - "gateway" (the Auth0 API token the Gateway checks): a forced silent
 *   refresh, the host's freshly minted token when embedded, then a Universal
 *   Login window that asks only for what is missing (the second factor after
 *   `mfa_required`).
 *
 * When the browser blocks the window, the product asks in place, neutrally,
 * for a confirmation whose click opens it; "Continue in this tab" is the last
 * resort full-page round trip back to the same page. A person who dismisses
 * the confirmation keeps the action pending: the request fails with
 * {@link SESSION_RENEWAL_PENDING_CODE} and neutral wording, never a sign-in
 * error.
 */
import * as Sentry from "@sentry/sveltekit";

export const SESSION_RENEWAL_HEADER = "x-kombify-session-renewal";
export const SESSION_RENEWAL_ERROR_CODE = "session_renewal_required";
/** The request waits for the person's confirmation of the renewal. */
export const SESSION_RENEWAL_PENDING_CODE = "session_renewal_pending";
/** Same-origin renewal route (served by routes/auth/renew). */
export const SESSION_RENEWAL_ROUTE = "/auth/renew";
const RENEWAL_WINDOW_NAME = "kombify-session-renewal";
const RENEWAL_WINDOW_FEATURES = "popup,width=520,height=700";

/**
 * The origin could not verify the gateway's signed decision envelope — a
 * trust-material or configuration fault between edge and origin. It arrives as
 * a 401 but says nothing about the caller's session, and no renewal repairs
 * it (a renewal loop that could not terminate went live 2026-09-12 to
 * 2026-09-18).
 */
export const EDGE_DECISION_UNVERIFIABLE_REASON_CODE =
  "edge_decision_unverifiable";

/** True when a 401 is the origin refusing the edge's decision, not the session. */
export function isEdgeDecisionUnverifiableError(err: unknown): boolean {
  if (typeof err !== "object" || err === null) return false;
  const candidate = err as {
    status?: unknown;
    code?: unknown;
    details?: unknown;
  };
  if (candidate.status !== 401) return false;
  if (candidate.code === EDGE_DECISION_UNVERIFIABLE_REASON_CODE) return true;
  const details = recordOf(candidate.details);
  return (
    details.reason_code === EDGE_DECISION_UNVERIFIABLE_REASON_CODE ||
    details.error_code === EDGE_DECISION_UNVERIFIABLE_REASON_CODE
  );
}

/** The client could not obtain the user's API token before sending. */
export const GATEWAY_TOKEN_UNAVAILABLE_CODE = "gateway_auth_unavailable";

export type SessionRenewalKind = "session" | "gateway";

export interface SessionRenewalSignal {
  kind: SessionRenewalKind;
  reason: string;
  /** Same-origin renewal route path. */
  renewUrl: string;
}

/** Thrown when a request still waits for the person's renewal confirmation. */
export class SessionRenewalPendingError extends Error {
  readonly status = 401;
  readonly code = SESSION_RENEWAL_PENDING_CODE;
  readonly retryable = true;
  constructor(
    message: string,
    readonly signal: SessionRenewalSignal,
    readonly url?: string,
    readonly method?: string,
  ) {
    super(message);
    this.name = "SessionRenewalPendingError";
  }
}

export function isSessionRenewalPending(err: unknown): boolean {
  if (typeof err !== "object" || err === null) return false;
  const { code, reason } = err as { code?: unknown; reason?: unknown };
  // API errors carry it as code, Wallet refusals as reason.
  return (
    code === SESSION_RENEWAL_PENDING_CODE ||
    reason === SESSION_RENEWAL_PENDING_CODE
  );
}

function recordOf(value: unknown): Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {};
}

function stringOf(value: unknown): string {
  return typeof value === "string" ? value.trim() : "";
}

function sameOriginPath(raw: string): string | null {
  if (!raw || typeof window === "undefined") return null;
  try {
    const url = new URL(raw, window.location.origin);
    if (url.origin !== window.location.origin) return null;
    return `${url.pathname}${url.search}`;
  } catch {
    return null;
  }
}

/** Reasons a Techstack session cookie renewal (not an API token) repairs. */
const SESSION_REASONS = new Set(["session_expired", "session_reprojection"]);

function kindFor(reason: string, viaGateway: boolean): SessionRenewalKind {
  if (!viaGateway) return "session";
  return SESSION_REASONS.has(reason) ? "session" : "gateway";
}

/**
 * Read the renewal signal off a response without consuming its body. Only a
 * 401 that carries the header or the error code is the signal; an
 * entitlement, role or ownership denial never is.
 */
export async function sessionRenewalSignalOf(
  response: Response,
  options: { viaGateway: boolean },
): Promise<SessionRenewalSignal | null> {
  if (response.status !== 401) return null;
  const flagged =
    response.headers.get(SESSION_RENEWAL_HEADER)?.trim().toLowerCase() ===
    "required";
  let body: Record<string, unknown> = {};
  try {
    body = recordOf(await response.clone().json());
  } catch {
    body = {};
  }
  const nested = recordOf(body.error);
  const details = recordOf(nested.details);
  const coded =
    body.error_code === SESSION_RENEWAL_ERROR_CODE ||
    nested.code === SESSION_RENEWAL_ERROR_CODE ||
    details.error_code === SESSION_RENEWAL_ERROR_CODE;
  if (!flagged && !coded) return null;
  const reason =
    stringOf(body.reason) ||
    stringOf(details.reason) ||
    stringOf(body.reason_code) ||
    "unspecified";
  return {
    kind: kindFor(reason, options.viaGateway),
    reason,
    renewUrl:
      sameOriginPath(stringOf(body.renew_url) || stringOf(details.renew_url)) ??
      SESSION_RENEWAL_ROUTE,
  };
}

/** The signal for a request that could not get its API token before sending. */
export function sessionRenewalSignalOfSendError(
  err: unknown,
): SessionRenewalSignal | null {
  if (recordOf(err).code !== GATEWAY_TOKEN_UNAVAILABLE_CODE) return null;
  return {
    kind: "gateway",
    reason: "gateway_token_unavailable",
    renewUrl: SESSION_RENEWAL_ROUTE,
  };
}

type WindowOutcome = "renewed" | "blocked" | "dismissed";

interface ConfirmationRequest {
  kind: SessionRenewalKind;
  /** The browser refused the window even after the confirmation click. */
  blocked: boolean;
  /** A window is open and the renewal waits for it. */
  waiting: boolean;
}

function isEmbedded(): boolean {
  return typeof window !== "undefined" && window.parent !== window;
}

function capture(
  signal: SessionRenewalSignal,
  outcome: string,
  rung: string,
): void {
  Sentry.withScope((scope) => {
    scope.setTag("component", "session_renewal");
    scope.setTag("renewal_kind", signal.kind);
    scope.setTag("renewal_reason", signal.reason);
    scope.setTag("renewal_rung", rung);
    scope.setTag("outcome", outcome);
    scope.setTag("embedded", String(isEmbedded()));
    if (typeof window !== "undefined") {
      scope.setTag("route", window.location.pathname);
    }
    scope.setLevel(outcome === "renewed" ? "info" : "warning");
    Sentry.captureMessage("techstack_session_renewal");
  });
}

class SessionRenewalStore {
  /** The in-place confirmation the layout renders, or null. */
  confirmation = $state<ConfirmationRequest | null>(null);

  private inFlight = new Map<SessionRenewalKind, Promise<boolean>>();
  private signals = new Map<SessionRenewalKind, SessionRenewalSignal>();
  private settleConfirmation:
    ((confirmed: Window | null | false) => void) | null = null;

  /**
   * Renew the session the signal names. Concurrent callers of the same kind
   * share one renewal. Resolves true once the session is renewed and the
   * caller may replay, false when the person dismissed the confirmation.
   */
  renew(signal: SessionRenewalSignal): Promise<boolean> {
    const running = this.inFlight.get(signal.kind);
    if (running) return running;
    this.signals.set(signal.kind, signal);
    const attempt = this.run(signal).finally(() => {
      this.inFlight.delete(signal.kind);
    });
    this.inFlight.set(signal.kind, attempt);
    return attempt;
  }

  private async run(signal: SessionRenewalSignal): Promise<boolean> {
    if (await this.renewQuietly(signal)) {
      capture(signal, "renewed", "quiet");
      return true;
    }
    const unprompted = await this.openAndWait(signal, undefined);
    if (unprompted === "renewed") {
      capture(signal, "renewed", "window");
      return true;
    }
    if (unprompted === "dismissed") {
      capture(signal, "pending", "window_dismissed");
      return false;
    }
    // The browser blocked a window it was not asked for by a click: ask in
    // place, neutrally, with the action still pending.
    const renewed = await this.confirmInPlace(signal);
    capture(signal, renewed ? "renewed" : "pending", "confirmation");
    return renewed;
  }

  /** Renewal paths that need no window. */
  private async renewQuietly(signal: SessionRenewalSignal): Promise<boolean> {
    if (signal.kind === "gateway") {
      const { getGatewayToken, gatewayRenewalNeedsSecondFactor } =
        await import("#lib/auth/gateway-auth.js");
      if (!gatewayRenewalNeedsSecondFactor()) {
        try {
          await getGatewayToken({ fresh: true });
          return true;
        } catch {
          // Continue with the host or a window.
        }
      }
      if (isEmbedded()) {
        try {
          const { initBridge, requestGatewayToken } =
            await import("#lib/stores/postMessageBridge.js");
          initBridge();
          await requestGatewayToken(undefined, { fresh: true });
          return true;
        } catch {
          return false;
        }
      }
      return false;
    }
    if (isEmbedded()) {
      const { refreshEmbeddedCloudSession } =
        await import("#lib/auth/embedded-session.js");
      try {
        if (await refreshEmbeddedCloudSession({ forcePortalVerify: true })) {
          return true;
        }
      } catch {
        // Continue with a window.
      }
    }
    // Another tab may already have renewed the cookie.
    if (signal.reason === "session_expired") {
      const { authStore } = await import("#lib/stores/auth.svelte.js");
      return authStore.probeRenewedSession();
    }
    return false;
  }

  /**
   * Open the renewal window (or use the one a click opened) and wait for it.
   * `preOpened` undefined: open it now, unprompted.
   */
  private async openAndWait(
    signal: SessionRenewalSignal,
    preOpened: Window | null | undefined,
  ): Promise<WindowOutcome> {
    if (signal.kind === "gateway") {
      const { renewGatewayTokenInWindow } =
        await import("#lib/auth/gateway-auth.js");
      if (preOpened === null) return "blocked";
      const outcome = await renewGatewayTokenInWindow(preOpened);
      if (outcome === "unavailable") {
        // No Universal Login client in this build: nothing a window can do.
        try {
          preOpened?.close();
        } catch {
          // Already closed.
        }
        return "dismissed";
      }
      return outcome;
    }
    const popup =
      preOpened === undefined ? openSessionRenewalWindow(signal) : preOpened;
    if (!popup) return "blocked";
    const { watchStepUpCompletion } = await import("#lib/auth/step-up.js");
    await new Promise<void>((resolve) => {
      const stop = watchStepUpCompletion(popup, () => {
        stop();
        resolve();
      });
    });
    try {
      if (!popup.closed) popup.close();
    } catch {
      // The window may already be gone.
    }
    const { authStore } = await import("#lib/stores/auth.svelte.js");
    if (await authStore.probeRenewedSession()) {
      return "renewed";
    }
    if (isEmbedded()) {
      const { refreshEmbeddedCloudSession } =
        await import("#lib/auth/embedded-session.js");
      try {
        if (await refreshEmbeddedCloudSession({ forcePortalVerify: true })) {
          return "renewed";
        }
      } catch {
        // Fall through.
      }
    }
    return "dismissed";
  }

  private async confirmInPlace(signal: SessionRenewalSignal): Promise<boolean> {
    this.confirmation = { kind: signal.kind, blocked: false, waiting: false };
    try {
      while (true) {
        const choice = await new Promise<Window | null | false>((resolve) => {
          this.settleConfirmation = resolve;
        });
        this.settleConfirmation = null;
        if (choice === false) return false;
        this.confirmation = {
          kind: signal.kind,
          blocked: false,
          waiting: true,
        };
        const outcome = await this.openAndWait(signal, choice);
        if (outcome === "renewed") return true;
        if (outcome === "dismissed") return false;
        this.confirmation = {
          kind: signal.kind,
          blocked: true,
          waiting: false,
        };
      }
    } finally {
      this.confirmation = null;
      this.settleConfirmation = null;
    }
  }

  /**
   * The confirmation's button. Call it directly from the click handler: the
   * window opens synchronously so the browser counts the click as its
   * gesture.
   */
  confirm(): void {
    const request = this.confirmation;
    if (!request || !this.settleConfirmation) return;
    const signal = this.signals.get(request.kind);
    let popup: Window | null = null;
    try {
      popup =
        request.kind === "gateway"
          ? window.open("", "auth0:authorize:popup", RENEWAL_WINDOW_FEATURES)
          : signal
            ? openSessionRenewalWindow(signal)
            : null;
    } catch {
      popup = null;
    }
    this.settleConfirmation(popup);
  }

  /** "Not now": the action stays pending, worded neutrally. */
  dismiss(): void {
    this.settleConfirmation?.(false);
  }

  /**
   * Last resort: a full-page round trip that comes back to this page. Pages
   * holding unsaved input keep it in their draft storage.
   */
  async continueInThisTab(): Promise<void> {
    const request = this.confirmation;
    if (!request) return;
    const { currentAuthReturnTo } =
      await import("#lib/auth/login-experience.js");
    const returnTo = currentAuthReturnTo();
    if (request.kind === "gateway") {
      const { startGatewayLogin } = await import("#lib/auth/gateway-auth.js");
      if (await startGatewayLogin({ returnTo })) return;
    }
    const renewal = new URL(SESSION_RENEWAL_ROUTE, window.location.origin);
    renewal.searchParams.set("return_to", returnTo);
    window.location.assign(renewal.toString());
  }

  /**
   * Renew from a control the person just pressed (the panel a page shows
   * after the confirmation was dismissed). Call it directly from the click
   * handler: the window opens synchronously, so the click is its gesture.
   * Resolves true once renewed.
   */
  renewWithGesture(kind: SessionRenewalKind = "session"): Promise<boolean> {
    const signal = this.signals.get(kind) ?? {
      kind,
      reason: "renewal_requested",
      renewUrl: SESSION_RENEWAL_ROUTE,
    };
    let popup: Window | null = null;
    try {
      popup =
        kind === "gateway"
          ? window.open("", "auth0:authorize:popup", RENEWAL_WINDOW_FEATURES)
          : openSessionRenewalWindow(signal);
    } catch {
      popup = null;
    }
    const running = this.inFlight.get(kind);
    if (running) {
      // A renewal already waits for its confirmation: this click is it.
      this.settleConfirmation?.(popup);
      return running;
    }
    this.signals.set(kind, signal);
    const attempt = (async () => {
      const outcome = await this.openAndWait(signal, popup);
      if (outcome === "renewed") return true;
      if (outcome === "dismissed") return false;
      return this.confirmInPlace(signal);
    })().finally(() => {
      this.inFlight.delete(kind);
    });
    this.inFlight.set(kind, attempt);
    return attempt;
  }

  /**
   * Ask for the renewal without a gesture (a page's retry path). Resolves
   * true once renewed.
   */
  renewFromControl(kind: SessionRenewalKind = "session"): Promise<boolean> {
    const signal = this.signals.get(kind) ?? {
      kind,
      reason: "renewal_requested",
      renewUrl: SESSION_RENEWAL_ROUTE,
    };
    return this.renew(signal);
  }
}

function renewalWindowUrl(signal: SessionRenewalSignal): string {
  return new URL(signal.renewUrl, window.location.origin).toString();
}

function openSessionRenewalWindow(signal: SessionRenewalSignal): Window | null {
  try {
    return window.open(
      renewalWindowUrl(signal),
      RENEWAL_WINDOW_NAME,
      RENEWAL_WINDOW_FEATURES,
    );
  } catch {
    return null;
  }
}

export const sessionRenewal = new SessionRenewalStore();

export interface SessionRenewalFetchOptions {
  /** The request goes to the kombify Gateway with the user's API token. */
  viaGateway: boolean;
  url?: string;
  method?: string;
  /** Neutral wording for a request that waits for confirmation. */
  pendingMessage: string;
}

/**
 * Send a request through the shared renewal layer. `send` builds and sends
 * the request; it runs again for the single replay, so it must rebuild
 * everything it needs (the bearer, a re-readable body). Resolves with the
 * response (the replay's, after a renewal). Rejects with
 * {@link SessionRenewalPendingError} when the person dismissed the renewal.
 */
export async function fetchWithSessionRenewal(
  send: () => Promise<Response>,
  options: SessionRenewalFetchOptions,
): Promise<Response> {
  let signal: SessionRenewalSignal | null = null;
  try {
    const response = await send();
    signal = await sessionRenewalSignalOf(response, options);
    if (!signal) return response;
  } catch (err) {
    signal = sessionRenewalSignalOfSendError(err);
    if (!signal) throw err;
  }
  const pending = (current: SessionRenewalSignal) =>
    new SessionRenewalPendingError(
      options.pendingMessage,
      current,
      options.url,
      options.method,
    );
  if (typeof window === "undefined" || !(await sessionRenewal.renew(signal))) {
    throw pending(signal);
  }
  // The single replay. A replay that still needs renewal is not replayed
  // again: it waits for the person, worded neutrally.
  let replay: Response;
  try {
    replay = await send();
  } catch (err) {
    const again = sessionRenewalSignalOfSendError(err);
    if (again) throw pending(again);
    throw err;
  }
  const again = await sessionRenewalSignalOf(replay, options);
  if (again) throw pending(again);
  return replay;
}
