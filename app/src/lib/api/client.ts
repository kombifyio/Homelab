/**
 * kombify-Homelab API Client - Core Utilities
 *
 * Low-level fetch wrapper and helper functions for API communication.
 * Handles communication with the Go backend.
 */

import { get as readStore } from "svelte/store";
import { getClientBootstrap, loadClientBootstrap } from "#lib/client/bootstrap.js";
import { deploymentMode } from "#lib/stores/deploymentMode.js";
import { getGatewayToken } from "#lib/auth/gateway-auth.js";
import {
  isGatewayLoginRedirecting,
  noteGatewayTokenSuccess,
} from "#lib/auth/session-recovery.js";
import {
  fetchWithSessionRenewal,
  GATEWAY_TOKEN_UNAVAILABLE_CODE,
} from "#lib/auth/session-renewal.svelte.js";

import { tr } from "#lib/i18n.svelte.js";
import {
  promptForWalletDenial,
  walletDenialMessage,
  walletDenialOf,
} from "#lib/kombify-wallet/denials.svelte.js";
// IMPORTANT:
// - In the browser we prefer relative URLs so the deployment can decide routing
//   (reverse proxy, same-origin, etc.) without hardcoding any host/port.
// - For SSR, provide an absolute base URL via env (e.g. VITE_API_URL at build
//   time or HOMELAB_API_URL at runtime).
const ENV_API_BASE =
  import.meta.env.VITE_API_URL ||
  (typeof process !== "undefined" && process.env
    ? process.env.HOMELAB_API_URL || ""
    : "");

function isIpHost(hostname: string): boolean {
  // IPv4
  if (/^\d{1,3}(?:\.\d{1,3}){3}$/.test(hostname)) return true;
  // IPv6 (very loose check, but good enough for routing decisions)
  if (hostname.includes(":")) return true;
  return false;
}

function stripTrailingSlash(url: string): string {
  return url ? url.replace(/\/+$/, "") : "";
}

/**
 * API base URL resolution.
 *
 * We keep same-origin by default (API_BASE="") to support reverse proxies.
 * For local Vite where the UI runs on a different port than the backend,
 * rewrite that current origin to the backend port, or set `VITE_API_URL`.
 */
export const API_BASE = (() => {
  if (ENV_API_BASE) {
    // In the browser, ignore internal Docker hostnames like `http://homelab:5260`
    // because the browser can't resolve them. Still allow explicit same-origin,
    // localhost, IPs, or real domains.
    if (typeof window !== "undefined") {
      const envOrigin = safeOrigin(ENV_API_BASE);
      if (envOrigin) {
        try {
          const envUrl = new URL(envOrigin);
          const hostname = envUrl.hostname;
          const isSameOrigin = envOrigin === window.location.origin;
          const isLocalhost = isLocalhostOrigin(envOrigin);
          const isRealDomain = hostname.includes(".");
          const isIp = isIpHost(hostname);

          if (isSameOrigin || isLocalhost || isRealDomain || isIp) {
            return stripTrailingSlash(ENV_API_BASE);
          }
          // fall through to normal browser heuristics
        } catch {
          // fall through
        }
      } else {
        // Non-URL value; keep previous behavior.
        return stripTrailingSlash(ENV_API_BASE);
      }
    } else {
      return stripTrailingSlash(ENV_API_BASE);
    }
  }

  // Browser:
  // - In production / embedded mode, prefer same-origin so reverse proxies and
  //   cookie policies work naturally.
  // - In Vite dev, the UI runs on a different port (e.g. 5173/5271) and needs
  //   an explicit backend origin.
  if (typeof window !== "undefined") {
    const uiOrigin = window.location.origin;
    const uiPort = (() => {
      try {
        return new URL(uiOrigin).port;
      } catch {
        return "";
      }
    })();

    const isViteDevPort = uiPort === "5173" || uiPort === "5271";

    // In Vite dev, prefer a stable default (localhost:5261) unless explicitly
    // configured via env. Do NOT default to the PocketBase origin (often :5260),
    // because core API routes live on the Go backend.
    if (isViteDevPort) {
      try {
        const u = new URL(uiOrigin);
        u.port = "5261";
        return u.origin;
      } catch {
        // fall through
      }
    }

    return ""; // same-origin
  }

  // SSR: only an explicit env origin is an absolute API base. Otherwise stay
  // same-origin so a missing HOMELAB_API_URL cannot mint localhost.
  const env = typeof process !== "undefined" && process.env ? process.env : {};
  const runtimeBase = env.HOMELAB_API_URL || "";
  if (runtimeBase) return stripTrailingSlash(runtimeBase);
  return "";
})();

// Cloudflare gateway base for the embedded data plane, e.g.
// https://api.kombify.io/v1/homelab. Set at build time (VITE_GATEWAY_API_BASE).
const GATEWAY_API_BASE =
  (import.meta.env.VITE_GATEWAY_API_BASE as string | undefined)?.replace(
    /\/+$/,
    "",
  ) || "";
// Runtime edition (formerly PUBLIC_KOMBIFY_EDITION via $env/dynamic/public).
// Read per call from the client bootstrap snapshot so the static bundle stays
// deployment-agnostic; before the bootstrap fetch resolves this is "" and
// gateway routing falls back to the deployment-mode store, matching the
// pre-existing async-config behavior.
function runtimeKombifyEdition(): string {
  return getClientBootstrap().kombifyEdition;
}

/**
 * In SaaS data-plane mode, route authenticated /api/v1/* calls through the
 * Cloudflare gateway (api.kombify.io/v1/homelab/*) so the edge validates the
 * user's Auth0 token and signs the entitlement-flag envelope. The gateway strips
 * /v1/homelab back to /api/v1 at the origin, so /api/v1/x maps to
 * `${gatewayBase}/x`.
 *
 * Pure for testability. Returns null when the call must stay same-origin:
 * a candidate preview, self-hosted data plane, no gateway base configured, an
 * absolute URL, or a bootstrap/auth endpoint that must establish the browser
 * session locally.
 */
export function resolveGatewayUrl(
  endpoint: string,
  opts: {
    mode: string;
    gatewayBase: string;
    dataPlane?: string;
    kombifyEdition?: string;
    method?: string;
  },
): string | null {
  if (!opts.gatewayBase) return null;
  const edition = opts.kombifyEdition?.trim().toLowerCase();
  // Render PR previews must exercise the UI and API from the same candidate
  // revision. Routing them through the production gateway would validate the
  // preview UI against whichever Homelab revision happens to be live.
  if (edition === "preview") return null;
  const shouldUseGateway =
    opts.dataPlane === "gateway" ||
    opts.mode === "saas-embedded" ||
    edition === "cloud" ||
    edition === "saas-standalone";
  if (!shouldUseGateway) return null;
  if (endpoint.startsWith("http://") || endpoint.startsWith("https://"))
    return null;
  if (endpoint !== "/api/v1" && !endpoint.startsWith("/api/v1/")) return null;
  if (mustStaySameOrigin(endpoint, opts.method)) return null;
  return `${opts.gatewayBase}${endpoint.slice("/api/v1".length)}`;
}

function gatewayUrlFor(endpoint: string, method?: string): string | null {
  if (typeof window === "undefined") return null;
  const config = readStore(deploymentMode);
  return resolveGatewayUrl(endpoint, {
    mode: config.mode,
    dataPlane: config.dataPlane,
    gatewayBase: GATEWAY_API_BASE,
    kombifyEdition: runtimeKombifyEdition(),
    method,
  });
}

async function getParentBridgeGatewayToken(): Promise<string> {
  if (typeof window === "undefined" || window.parent === window) {
    throw new Error("embedded_parent_bridge_unavailable");
  }

  const { initBridge, requestGatewayToken } =
    await import("#lib/stores/postMessageBridge.js");
  initBridge();
  const token = await requestGatewayToken();
  if (!token) {
    throw new Error("embedded_parent_bridge_token_empty");
  }
  return token;
}

/**
 * The signed-in user's Auth0 access token for the kombify API
 * (`https://api.kombify.io`): the SPA's own token first, then the parent
 * portal's when this app runs embedded. Used for every gateway call, including
 * the Wallet owner API. Fails closed: it never returns an empty or anonymous
 * token, and a login redirect already under way is passed through.
 */
export async function resolveGatewayBearer(
  url: string,
  method = "GET",
): Promise<string> {
  try {
    const token = await getGatewayToken();
    noteGatewayTokenSuccess();
    return token;
  } catch (err) {
    if (isGatewayLoginRedirecting(err)) {
      throw err;
    }
    try {
      return await getParentBridgeGatewayToken();
    } catch (bridgeErr) {
      // The shared session renewal layer renews on this code and replays
      // the request; the person only ever sees a neutral confirmation.
      throw new ApiRequestError(tr("sessionRenewal.pending"), {
        status: 401,
        code: GATEWAY_TOKEN_UNAVAILABLE_CODE,
        url,
        method,
        details: {
          auth0: err instanceof Error ? err.message : "gateway token failed",
          parentBridge:
            bridgeErr instanceof Error
              ? bridgeErr.message
              : "parent gateway token failed",
        },
      });
    }
  }
}

function mustStaySameOrigin(endpoint: string, method?: string): boolean {
  const path = endpoint.split(/[?#]/, 1)[0] || endpoint;
  // These check the session's own re-authentication, which only a request
  // carrying the session cookie reaches the control plane with.
  const reauthBoundMutation =
    (method || "GET").toUpperCase() === "POST" &&
    (/^\/api\/v1\/servers\/[^/]+\/terminal-sessions$/.test(path) ||
      /^\/api\/v1\/servers\/[^/]+\/access\/authorized-key$/.test(path) ||
      /^\/api\/v1\/wallet\/[^/]+\/reauth-proof$/.test(path));
  // The Guard binary pins are a public read the control plane serves to
  // installers. The edge containment rule (owner_20261025_stop_render_artifact_egress)
  // blocks every Homelab path containing "/install" on api.kombify.io and
  // admits only this read on the Homelab origin itself; through the Gateway
  // the request is refused, no pin is known and no install command is shown.
  const guardPinsRead =
    (method || "GET").toUpperCase() === "GET" &&
    path === "/api/v1/workers/install-pins";
  return (
    reauthBoundMutation ||
    guardPinsRead ||
    path === "/api/v1/csrf" ||
    path === "/api/v1/info" ||
    path === "/api/v1/health" ||
    path === "/api/v1/instance" ||
    path === "/api/v1/auth" ||
    path.startsWith("/api/v1/auth/")
  );
}

export function safeOrigin(raw: string): string {
  try {
    return new URL(raw).origin;
  } catch {
    return "";
  }
}

export function isLocalhostOrigin(origin: string): boolean {
  try {
    const u = new URL(origin);
    return (
      u.hostname === "localhost" ||
      u.hostname === "127.0.0.1" ||
      u.hostname === "::1"
    );
  } catch {
    return false;
  }
}

// Best-effort URL selection for commands that must run on a different machine.
// Priority: current browser origin (if not localhost) -> configured API base
// (if not localhost) -> browser origin.
export function getBestPublicServerUrl(): string {
  const browserOrigin =
    typeof window !== "undefined" ? window.location.origin : "";
  const apiOrigin = safeOrigin(API_BASE);

  const candidates = [browserOrigin, apiOrigin, browserOrigin]
    .map((s) => s.replace(/\/$/, ""))
    .filter(Boolean);

  const preferred = candidates.find((c) => !isLocalhostOrigin(c));
  return (preferred || candidates[0] || "").replace(/\/$/, "");
}

export interface ApiResponse<T> {
  data: T;
  meta?: {
    total?: number;
    page?: number;
    per_page?: number;
  };
}

export interface ApiError {
  error: {
    code: string;
    message: string;
    details?: unknown;
  };
}

export class ApiRequestError extends Error {
  status?: number;
  code?: string;
  url?: string;
  method?: string;
  contentType?: string;
  requestId?: string;
  responseBodySnippet?: string;
  details?: unknown;
  retryable?: boolean;

  constructor(message: string, init?: Partial<ApiRequestError>) {
    super(message);
    this.name = "ApiRequestError";
    Object.assign(this, init);
  }
}

const CSRF_HEADER_NAME = "X-CSRF-Token";
/**
 * The server's machine-readable refusal of a failed double-submit check
 * (pkg/middleware/csrf.go). The token cookie lives 12 h, so a long-lived tab
 * caches a token that no longer matches; the cached token is dropped, a fresh
 * one fetched and the request replayed once.
 */
const CSRF_INVALID_TOKEN_CODE = "csrf_token_invalid";
let csrfToken: string | null = null;
let csrfTokenInFlight: Promise<string> | null = null;

function normalizeMethod(method: string | undefined): string {
  return (method || "GET").toUpperCase();
}

function needsCSRF(method: string): boolean {
  return !["GET", "HEAD", "OPTIONS", "TRACE"].includes(method);
}

function csrfEndpointFor(url: string): string {
  if (url.startsWith("http://") || url.startsWith("https://")) {
    return new URL("/api/v1/csrf", url).toString();
  }
  return "/api/v1/csrf";
}

async function getCSRFToken(url: string): Promise<string> {
  if (csrfToken) return csrfToken;
  if (csrfTokenInFlight) return csrfTokenInFlight;

  csrfTokenInFlight = fetch(csrfEndpointFor(url), {
    method: "GET",
    credentials: "include",
  })
    .then(async (response) => {
      if (!response.ok) {
        throw await parseApiErrorResponse(response, {
          action: "CSRF token request failed",
          method: "GET",
        });
      }
      const token = response.headers.get(CSRF_HEADER_NAME);
      if (token) return token;
      const payload = (await response.json()) as { token?: string };
      if (!payload.token)
        throw new Error(tr("ui.client.csrfTokenResponseMissingToken"));
      return payload.token;
    })
    .then((token) => {
      csrfToken = token;
      return token;
    })
    .finally(() => {
      csrfTokenInFlight = null;
    });

  return csrfTokenInFlight;
}

function extractRequestId(headers: Headers): string {
  return (
    headers.get("x-request-id") ||
    headers.get("x-correlation-id") ||
    headers.get("x-trace-id") ||
    ""
  );
}

/** True for the server's CSRF refusal, read from the code and never a text. */
async function isCSRFRefusal(response: Response): Promise<boolean> {
  if (response.status !== 403) return false;
  try {
    const body = (await response.clone().json()) as { error?: unknown };
    const error = body?.error;
    return (
      typeof error === "object" &&
      error !== null &&
      (error as { code?: unknown }).code === CSRF_INVALID_TOKEN_CODE
    );
  } catch {
    return false;
  }
}

function looksLikeHtml(body: string): boolean {
  const t = body.trimStart().toLowerCase();
  return t.startsWith("<!doctype") || t.startsWith("<html");
}

function safeTruncate(text: string, max: number): string {
  if (!text) return "";
  return text.length > max ? `${text.slice(0, max)}\n…(truncated)` : text;
}

export async function parseApiErrorResponse(
  response: Response,
  context?: { action?: string; method?: string },
): Promise<ApiRequestError> {
  const contentType = response.headers.get("content-type") || "";
  const requestId = extractRequestId(response.headers);

  let bodyText = "";
  try {
    bodyText = await response.text();
  } catch {
    // ignore
  }

  const basePrefix = context?.action ? `${context.action}: ` : "";
  let message = `${basePrefix}${response.status} ${response.statusText}`.trim();
  let details: unknown = undefined;
  let code: string | undefined;

  // Prefer structured API errors when available.
  const maybeJson = bodyText.trimStart();
  const mayBeJson =
    contentType.includes("application/json") ||
    maybeJson.startsWith("{") ||
    maybeJson.startsWith("[");

  if (mayBeJson) {
    try {
      const data = JSON.parse(bodyText);
      const apiErr = data as Partial<ApiError> & {
        message?: string;
        error?: any;
      };
      code = typeof apiErr?.error?.code === "string" ? apiErr.error.code : code;
      message =
        apiErr?.error?.message ||
        (typeof apiErr?.message === "string" ? apiErr.message : "") ||
        message;
      details = apiErr?.error?.details ?? apiErr?.error ?? data;
    } catch {
      // fall back to HTML/plaintext detection
    }
  }

  if (looksLikeHtml(bodyText) || contentType.includes("text/html")) {
    // This usually means a reverse proxy, auth redirect, or misrouted request.
    message = tr("ui.client.serverReturnedHTMLInsteadOf", {
      basePrefix,
      status: response.status,
    });
    if ([502, 503, 504].includes(response.status)) {
      code = "upstream_unavailable";
    }
  } else if (!mayBeJson && bodyText.trim()) {
    // Plain-text error; surface a short snippet.
    message = `${basePrefix}${safeTruncate(bodyText.trim(), 180)}`;
  }

  // A refusal about the kombify Wallet reads the same wherever it surfaces, and
  // one the owner can resolve with a single grant is offered right here.
  const walletDenial = walletDenialOf({
    status: response.status,
    code,
    details,
  });
  if (walletDenial) {
    code ??= walletDenial.reason;
    message = walletDenialMessage(walletDenial);
    promptForWalletDenial(walletDenial);
  }

  return new ApiRequestError(message, {
    status: response.status,
    code,
    url: response.url,
    method: context?.method,
    contentType,
    requestId,
    responseBodySnippet: safeTruncate(bodyText, 2000),
    details,
    retryable:
      response.status === 429 || [502, 503, 504].includes(response.status),
  });
}

export interface ApiRequestOptions extends RequestInit {
  timeoutMs?: number;
  /** Internal: set on the single automatic replay of an idempotent 429. */
  rateLimitReplay?: boolean;
}

const DEFAULT_READ_RETRY_AFTER_MS = 1_000;
const MAX_READ_RETRY_AFTER_MS = 60_000;
const readRateLimitUntilByOrigin = new Map<string, number>();

function rateLimitOrigin(url: string): string {
  try {
    return new URL(url, "http://localhost").origin;
  } catch {
    return url;
  }
}

function retryAfterDelayMs(value: string | null): number {
  const seconds = Number(value);
  if (value !== null && Number.isFinite(seconds) && seconds >= 0) {
    return Math.min(seconds * 1_000, MAX_READ_RETRY_AFTER_MS);
  }
  if (value) {
    const at = Date.parse(value);
    if (Number.isFinite(at)) {
      return Math.min(Math.max(0, at - Date.now()), MAX_READ_RETRY_AFTER_MS);
    }
  }
  return DEFAULT_READ_RETRY_AFTER_MS;
}

async function waitForReadRateLimitCooldown(url: string): Promise<void> {
  const origin = rateLimitOrigin(url);
  const waitMs = (readRateLimitUntilByOrigin.get(origin) ?? 0) - Date.now();
  if (waitMs <= 0) {
    readRateLimitUntilByOrigin.delete(origin);
    return;
  }
  await new Promise((resolve) => setTimeout(resolve, waitMs));
}

async function waitForReadRateLimit(
  url: string,
  response: Response,
): Promise<void> {
  const waitMs = retryAfterDelayMs(response.headers.get("retry-after"));
  const origin = rateLimitOrigin(url);
  readRateLimitUntilByOrigin.set(
    origin,
    Math.max(readRateLimitUntilByOrigin.get(origin) ?? 0, Date.now() + waitMs),
  );
  await waitForReadRateLimitCooldown(url);
}

export async function fetchApi<T>(
  endpoint: string,
  options: ApiRequestOptions = {},
): Promise<ApiResponse<T>> {
  const {
    timeoutMs = 10_000,
    rateLimitReplay = false,
    ...fetchOptions
  } = options;

  const sameOriginUrl =
    endpoint.startsWith("http://") || endpoint.startsWith("https://")
      ? endpoint
      : API_BASE
        ? `${API_BASE}${endpoint}`
        : endpoint;
  const method = normalizeMethod(fetchOptions.method);

  // SaaS data-plane calls must go through the Cloudflare gateway with a real
  // Auth0 API token so the edge signs identity and entitlement flags.
  let gatewayUrl = gatewayUrlFor(endpoint, method);
  if (gatewayUrl) {
    // The first request must read the service's channel answer before it can
    // reach the shared Gateway. An empty or unknown bootstrap answer must never
    // select Stable.
    const bootstrap = await loadClientBootstrap();
    gatewayUrl = gatewayUrlFor(endpoint, method);
    if (gatewayUrl && bootstrap.releaseChannel !== "stable") {
      throw new ApiRequestError(tr("ui.client.networkError"), {
        status: 503, code: "gateway_channel_unavailable", url: gatewayUrl, method,
      });
    }
  }
  const url = gatewayUrl ? gatewayUrl : sameOriginUrl;

  // One send, rebuilt in full for the single replay after a session renewal
  // (fresh bearer, same re-readable body) and for the single replay after a
  // refused CSRF token.
  const sendOnce = async (): Promise<Response> => {
    const gatewayToken = gatewayUrl
      ? await resolveGatewayBearer(gatewayUrl, method)
      : "";
    const authHeader: Record<string, string> = gatewayUrl
      ? { Authorization: `Bearer ${gatewayToken}` }
      : {};

    const headers = new Headers(fetchOptions.headers);
    headers.set(
      "Content-Type",
      headers.get("Content-Type") || "application/json",
    );
    for (const [key, value] of Object.entries(authHeader)) {
      if (!headers.has(key)) headers.set(key, value);
    }
    const authorizationHeader = headers.get("Authorization") ?? "";
    const hasBearerAuth = authorizationHeader.startsWith("Bearer ");
    if (
      typeof window !== "undefined" &&
      needsCSRF(method) &&
      !headers.has(CSRF_HEADER_NAME) &&
      !hasBearerAuth
    ) {
      headers.set(CSRF_HEADER_NAME, await getCSRFToken(url));
    }

    if (method === "GET" || method === "HEAD") {
      await waitForReadRateLimitCooldown(url);
    }

    // Avoid requests hanging forever (e.g. network/DNS issues). Keep this
    // short so the UI can surface an actionable error instead of showing
    // endless spinners.
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), timeoutMs);
    try {
      return await fetch(url, {
        ...fetchOptions,
        credentials: fetchOptions.credentials ?? "include",
        signal: fetchOptions.signal ?? controller.signal,
        headers,
      });
    } catch (err) {
      if (err instanceof Error && err.name === "AbortError") {
        throw new Error(
          tr("ui.client.requestTimedOutAfterS", {
            v1: Math.round(timeoutMs / 1000),
            url,
          }),
          { cause: err },
        );
      }
      // Network error (CORS, offline, etc.)
      throw new Error(
        API_BASE
          ? tr("ui.client.networkErrorAt", { base: API_BASE })
          : tr("ui.client.networkError"),
        { cause: err },
      );
    } finally {
      clearTimeout(timeout);
    }
  };

  // The server marks a refused CSRF token with a code. The cached token is
  // stale (its 12 h cookie expired in a long-lived tab or the cookie changed):
  // drop it, fetch a fresh one inside the replay, and let the person's save
  // succeed instead of asking them to reload the page.
  const send = async (): Promise<Response> => {
    const response = await sendOnce();
    if (!(await isCSRFRefusal(response))) return response;
    csrfToken = null;
    return sendOnce();
  };

  // The shared session renewal layer: a request whose session needs renewal
  // renews once for all waiting requests and is replayed once; pages never
  // interpret the signal themselves.
  const response = await fetchWithSessionRenewal(send, {
    viaGateway: Boolean(gatewayUrl),
    url,
    method,
    pendingMessage: tr("sessionRenewal.pending"),
  });

  if (!response.ok) {
    const error = await parseApiErrorResponse(response, {
      action: "Request failed",
      method,
    });
    if (
      response.status === 429 &&
      !rateLimitReplay &&
      (method === "GET" || method === "HEAD")
    ) {
      await waitForReadRateLimit(url, response);
      return fetchApi<T>(endpoint, { ...options, rateLimitReplay: true });
    }
    throw error;
  }

  const json = await response.json();
  // Back-compat: some endpoints return raw JSON instead of { data: ... }.
  if (json && typeof json === "object" && "data" in json) {
    return json as ApiResponse<T>;
  }
  return { data: json as T };
}

export async function get<T>(endpoint: string): Promise<T> {
  const result = await fetchApi<T>(endpoint, { method: "GET" });
  return result.data;
}

export async function post<T>(endpoint: string, body?: unknown): Promise<T> {
  const result = await fetchApi<T>(endpoint, {
    method: "POST",
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  return result.data;
}

export async function put<T>(endpoint: string, body?: unknown): Promise<T> {
  const result = await fetchApi<T>(endpoint, {
    method: "PUT",
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  return result.data;
}

export async function del<T>(endpoint: string): Promise<T> {
  const result = await fetchApi<T>(endpoint, {
    method: "DELETE",
  });
  return result.data;
}
