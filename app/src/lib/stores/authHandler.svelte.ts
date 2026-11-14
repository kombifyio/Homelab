/**
 * Page-level hook into the shared session renewal layer
 * (#lib/auth/session-renewal.svelte.ts, workspace AUTHENTICATION.md §6).
 *
 * The API client already renews a session and replays the request once, so a
 * 401 that reaches a page is either a renewal the person has not confirmed yet
 * ("reauth_required": the page shows the neutral SessionRenewalPanel and keeps
 * its input) or a 401 without the renewal signal, which this handler renews
 * the same way before running the page's retry once. A signed-in person is
 * never told to sign in again.
 */

import {
  isEdgeDecisionUnverifiableError,
  isSessionRenewalPending,
  sessionRenewal,
} from "#lib/auth/session-renewal.svelte.js";
import type { AuthRecoveryOutcome } from "#lib/auth/session-recovery.js";

export type { AuthRecoveryOutcome } from "#lib/auth/session-recovery.js";

export interface HandleUnauthorizedOptions {
  /**
   * Retained for call sites that hold unsaved in-memory state: renewal
   * never navigates away unless the person picks "Continue in this tab".
   */
  allowAutoRedirect?: boolean;
}

class AuthHandlerStore {
  /**
   * Handle a 401 that reached a page.
   *
   * @param retryFn the page's action, run once after a renewal
   * @param _customMessage unused; renewal wording is neutral and shared
   * @param cause the error the page caught
   */
  async handleUnauthorized(
    retryFn?: () => Promise<void>,
    _customMessage?: string,
    cause?: unknown,
    _options?: HandleUnauthorizedOptions,
  ): Promise<AuthRecoveryOutcome> {
    // An origin that cannot verify the gateway's signed decision is not a
    // session problem: no renewal repairs it, so the call site keeps the
    // error it already holds.
    if (isEdgeDecisionUnverifiableError(cause)) return "origin_unverifiable";
    // The person dismissed the renewal confirmation: the action waits.
    if (isSessionRenewalPending(cause)) return "reauth_required";

    const renewed = await sessionRenewal.renewFromControl(kindOf(cause));
    if (!renewed) return "reauth_required";
    if (retryFn) {
      try {
        await retryFn();
      } catch (err) {
        if (this.isAuthError(err)) return "reauth_required";
        console.error("[AuthHandler] Retry after renewal failed:", err);
      }
    }
    return "recovered";
  }

  /** True when an error is a 401. */
  isAuthError(err: unknown): boolean {
    if (typeof err === "object" && err !== null && "status" in err) {
      return (err as { status: number }).status === 401;
    }
    return false;
  }
}

/** A 401 from another origin (the kombify Gateway) renews the API token. */
function kindOf(cause: unknown): "session" | "gateway" {
  const { code, url } =
    typeof cause === "object" && cause !== null
      ? (cause as { code?: unknown; url?: unknown })
      : {};
  if (code === "gateway_auth_unavailable") return "gateway";
  if (typeof url === "string" && url && typeof window !== "undefined") {
    try {
      if (
        new URL(url, window.location.origin).origin !== window.location.origin
      ) {
        return "gateway";
      }
    } catch {
      // Not a URL; treat as same origin.
    }
  }
  return "session";
}

export const authHandler = new AuthHandlerStore();
