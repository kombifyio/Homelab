import { createHmac } from "node:crypto";
import { fillSecret } from "../helpers/secret-input";
import { type Page } from "@playwright/test";

import {
  requireHomelabTestUser,
  type HomelabTestUserRole,
} from "../../src/lib/testing/homelab-test-users";
import {
  AUTH0_FORM_ERROR_PATTERN,
  compactAuth0Error,
} from "../../src/lib/testing/auth0-errors";

export type RuntimeAuthRole = Extract<
  HomelabTestUserRole,
  "oneliner" | "remote" | "cloud"
>;

export interface RuntimeAuthSession {
  token: string;
}

// RuntimeGatewaySession is intentionally bearer-only. Callers must never log,
// attach, or serialize its token; it exists only to drive a single Gateway
// request inside a short-lived E2E browser context.
export interface RuntimeGatewaySession {
  token: string;
  apiBase: string;
}

export interface RuntimeAuthOptions {
  productBase: string;
  apiBase: string;
  sessionCookieName?: string;
  entryPath?: string;
  expectedAuthIssuer?: string;
  /**
   * Existing canonical shared identities, selected explicitly for A/B
   * isolation, or a shared staff fixture of the test fleet (TEST_ADMIN,
   * TEST_GLOBAL_ADMIN) for staff-only governed operations. Staff fixtures
   * answer Auth0's authenticator challenge with `<prefix>_TOTP_SECRET`.
   */
  fixturePrefix?: RuntimeFixturePrefix;
}

export type RuntimeFixturePrefix =
  | "TEST_RIL_PRO_USER"
  | "TEST_PRO_USER"
  | "TEST_PRO_DOCUMENTS_MEDIA_USER"
  | "TEST_ALL_YOU_NEED_USER"
  | "TEST_FREE_USER"
  | RuntimeStaffFixturePrefix;

export type RuntimeStaffFixturePrefix = "TEST_ADMIN" | "TEST_GLOBAL_ADMIN";

export async function authenticateRuntimeUser(
  role: RuntimeAuthRole,
  page: Page,
  options: RuntimeAuthOptions,
): Promise<RuntimeAuthSession> {
  const user = options.fixturePrefix
    ? canonicalRuntimeFixture(options.fixturePrefix)
    : requireHomelabTestUser(role);

  await page.goto(options.entryPath ?? "/login", {
    waitUntil: "domcontentloaded",
  });
  const cloudLoginButton = page
    .getByRole("button", {
      name: /Sign in with|Continue with|Create owner with/i,
    })
    .first();
  if (await cloudLoginButton.isVisible({ timeout: 5_000 }).catch(() => false)) {
    await cloudLoginButton.click();
  }
  await fillAuth0UniversalLogin(
    page,
    user.email,
    user.password,
    options.expectedAuthIssuer ??
      process.env.HOMELAB_AUTH_CLOUD_ISSUER ??
      "https://login.kombify.io",
  );
  const totpSecret =
    "totpSecret" in user && typeof user.totpSecret === "string"
      ? user.totpSecret
      : undefined;
  await answerAuth0OtpChallenge(page, totpSecret);
  await continuePastAuth0PostLoginPrompts(page);
  await waitForRuntimeDashboard(page);
  const token = await browserSessionToken(page, options);
  if (!token) {
    throw new Error(
      "Auth0 browser login did not produce a Homelab Runtime API session token",
    );
  }
  return { token };
}

function canonicalRuntimeFixture(prefix: RuntimeFixturePrefix) {
  const email = process.env[`${prefix}_EMAIL`]?.trim();
  const password = process.env[`${prefix}_PASSWORD`];
  if (!email || !password) throw new Error(`Canonical shared fixture ${prefix} is not configured`);
  const totpSecret = process.env[`${prefix}_TOTP_SECRET`]?.trim() || undefined;
  if (prefix === "TEST_PRO_DOCUMENTS_MEDIA_USER" && !totpSecret) {
    throw new Error("The isolated Documents & media Pro fixture requires its enrolled authenticator");
  }
  return { email, password, totpSecret };
}

const OTP_CHALLENGE_PATH = "/u/mfa-otp-challenge";

// Staff fixtures carry an enrolled authenticator (kombify-require-staff-mfa).
// One code per challenge: a rejected code fails the login instead of spending
// Auth0's MFA attempt budget. The code is never logged.
async function answerAuth0OtpChallenge(page: Page, totpSecret: string | undefined) {
  const challenged = await page
    .waitForURL((url) => url.pathname.startsWith(OTP_CHALLENGE_PATH), { timeout: 10_000 })
    .then(() => true, () => false);
  if (!challenged) return;
  if (!totpSecret) {
    throw new Error("Auth0 requires an authenticator code, but the fixture has no <PREFIX>_TOTP_SECRET");
  }
  const codeInput = visibleLocator(page, 'input[name="code"], input[autocomplete="one-time-code"]');
  await codeInput.first().waitFor({ state: "visible", timeout: 15_000 });
  await codeInput.first().fill(runtimeTotp(totpSecret));
  const continueButton = page.getByRole("button", { name: /^continue$/i }).first();
  if (await continueButton.isVisible().catch(() => false)) {
    await continueButton.click({ noWaitAfter: true });
  } else {
    await codeInput.first().press("Enter");
  }
  const accepted = await page
    .waitForURL((url) => !url.pathname.startsWith(OTP_CHALLENGE_PATH), { timeout: 15_000 })
    .then(() => true, () => false);
  if (!accepted) throw new Error("Auth0 did not accept the fixture's authenticator code");
}

// RFC 6238 TOTP (SHA-1, 30 s, 6 digits) over a base32 seed.
function runtimeTotp(seed: string, timeMs = Date.now()): string {
  const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567";
  let bits = "";
  for (const char of seed.replace(/[\s=-]/g, "").toUpperCase()) {
    const value = alphabet.indexOf(char);
    if (value < 0) throw new Error("TOTP secret is not base32");
    bits += value.toString(2).padStart(5, "0");
  }
  const key = Buffer.from((bits.match(/.{8}/g) ?? []).map((byte) => parseInt(byte, 2)));
  const counter = Buffer.alloc(8);
  counter.writeBigUInt64BE(BigInt(Math.floor(timeMs / 30_000)));
  const digest = createHmac("sha1", key).update(counter).digest();
  const lastByte = digest[digest.length - 1];
  if (lastByte === undefined) throw new Error("TOTP digest is empty");
  const offset = lastByte & 0x0f;
  return String((digest.readUInt32BE(offset) & 0x7fffffff) % 1_000_000).padStart(6, "0");
}

export async function fetchBrowserWhoAmI(page: Page): Promise<{
  status: number;
  body: unknown;
}> {
  return await page.evaluate(async () => {
    const response = await fetch("/api/v2/whoami", {
      credentials: "include",
    });
    const text = await response.text();
    let body: unknown = text;
    try {
      body = text ? JSON.parse(text) : null;
    } catch {
      body = text;
    }
    return { status: response.status, body };
  });
}

// captureGatewayApiSession observes an authenticated SPA data-plane request,
// so its bearer is minted for the public /v1/homelab surface rather than a
// private Homelab session. Do not couple this to a particular dashboard read:
// the owner-scoped UI may load stacks, homelab, inventory, or another aggregate.
// The bearer is never written to a log or artifact.
export async function captureGatewayApiSession(
  page: Page,
): Promise<RuntimeGatewaySession> {
  const requestPromise = page.waitForRequest(
    (request) => {
      const url = new URL(request.url());
      const authorization = request.headers()["authorization"] ?? "";
      return (
        request.method() === "GET" &&
        url.pathname.startsWith("/v1/homelab/") &&
        /^Bearer\s+\S+/i.test(authorization)
      );
    },
    { timeout: 30_000 },
  );
  await page.goto(`/dashboard?gateway_probe=${Date.now()}`, {
    waitUntil: "domcontentloaded",
  });
  const request = await requestPromise;
  const authorization = request.headers()["authorization"] ?? "";
  const token = authorization.replace(/^Bearer\s+/i, "").trim();
  if (!token) {
    throw new Error("Homelab Runtime dashboard omitted its Auth0 Gateway token");
  }
  const url = new URL(request.url());
  return { token, apiBase: `${url.origin}/v1/homelab` };
}

// A Gateway bearer for owner API calls: either a fixed token (short scenarios)
// or a keeper that is read at request time (long managed lanes).
export type RuntimeBearer = string | GatewaySessionKeeper;

export interface GatewaySessionKeeper {
  readonly apiBase: string;
  /** The current bearer; refreshed first when it is about to expire. */
  token(): Promise<string>;
  /** Re-capture the bearer now, for example after a 401. */
  refresh(): Promise<string>;
  /** Replace the held session with one observed elsewhere (Wizard create). */
  adopt(session: RuntimeGatewaySession): void;
}

const KEEPER_MAX_AGE_MS = 5 * 60_000;
// Below the Auth0 SPA SDK's 60s cache leeway, so a refresh then mints a new
// token instead of returning the cached one.
const KEEPER_EXPIRY_MARGIN_MS = 45_000;
const KEEPER_MIN_INTERVAL_MS = 30_000;

// createGatewaySessionKeeper holds the managed lane's Gateway session and
// re-captures it from the signed-in page. The bearer lives about 15 minutes,
// while a managed provision, Day-2 drill and destroy take far longer.
//
// A refresh navigates `page` to /dashboard, so it must never overlap a page
// interaction of the test. There is deliberately no timer: a refresh runs
// only inside token()/refresh(), which the API helpers await from the test's
// single flow of control, so the page is idle whenever one runs. Concurrent
// callers share one in-flight refresh. A failed refresh keeps the current
// session, and the next 401 retries it.
export function createGatewaySessionKeeper(
  page: Page,
  initial: RuntimeGatewaySession,
): GatewaySessionKeeper {
  let session = initial;
  let capturedAt = Date.now();
  let lastAttemptAt = 0;
  let inflight: Promise<string> | undefined;

  const refreshDue = () => {
    const now = Date.now();
    if (now - lastAttemptAt < KEEPER_MIN_INTERVAL_MS) return false;
    const expiresAt = bearerExpiryMs(session.token);
    return (
      now - capturedAt >= KEEPER_MAX_AGE_MS ||
      (expiresAt !== undefined && expiresAt - now < KEEPER_EXPIRY_MARGIN_MS)
    );
  };

  const refresh = () => {
    inflight ??= (async () => {
      lastAttemptAt = Date.now();
      try {
        const next = await captureGatewayApiSession(page);
        session = next;
        capturedAt = Date.now();
      } catch {
        // Keep the current session; the caller's request reports any 401.
      } finally {
        inflight = undefined;
      }
      return session.token;
    })();
    return inflight;
  };

  return {
    get apiBase() {
      return session.apiBase;
    },
    async token() {
      if (inflight) return inflight;
      return refreshDue() ? refresh() : session.token;
    },
    refresh,
    adopt(next) {
      session = next;
      capturedAt = Date.now();
    },
  };
}

export async function bearerToken(bearer: RuntimeBearer): Promise<string> {
  return typeof bearer === "string" ? bearer : bearer.token();
}

// authorizedFetch sends one owner API request with the bearer read at call
// time. With a keeper, a 401 (for example token_expired) re-captures the
// session and retries once; an unauthorized request performed nothing, so the
// retry cannot duplicate an action.
export async function authorizedFetch(
  bearer: RuntimeBearer,
  url: string,
  init: RequestInit = {},
): Promise<Response> {
  const send = (token: string) => {
    const headers = new Headers(init.headers);
    headers.set("Authorization", `Bearer ${token}`);
    return fetch(url, { ...init, headers });
  };
  const response = await send(await bearerToken(bearer));
  if (response.status !== 401 || typeof bearer === "string") return response;
  await response.body?.cancel().catch(() => undefined);
  return send(await bearer.refresh());
}

function bearerExpiryMs(token: string): number | undefined {
  const payload = token.split(".")[1];
  if (!payload) return undefined;
  try {
    const exp = Number(
      JSON.parse(Buffer.from(payload, "base64url").toString("utf8")).exp,
    );
    return Number.isFinite(exp) && exp > 0 ? exp * 1_000 : undefined;
  } catch {
    return undefined;
  }
}

async function browserSessionToken(
  page: Page,
  options: RuntimeAuthOptions,
): Promise<string> {
  const localStorageSession = await page.evaluate(() => {
    const raw = window.localStorage.getItem("pocketbase_auth");
    if (!raw) return null;
    const parsed = JSON.parse(raw) as {
      token?: string;
    };
    return { token: parsed.token ?? "" };
  });
  if (localStorageSession?.token) {
    return localStorageSession.token;
  }

  const cookies = await page
    .context()
    .cookies([options.productBase, options.apiBase]);
  return (
    cookies.find(
      (cookie) =>
        cookie.name === (options.sessionCookieName ?? "homelab_session"),
    )?.value ?? ""
  );
}

async function fillAuth0UniversalLogin(
  page: Page,
  email: string,
  password: string,
  expectedIssuer: string,
) {
  const expectedOrigin = requireExpectedIssuerOrigin(expectedIssuer);
  await page.waitForURL((url) => url.origin.toLowerCase() === expectedOrigin, {
    timeout: 60_000,
    waitUntil: "domcontentloaded",
  });
  assertExpectedIssuer(page, expectedOrigin);
  await page.waitForLoadState("domcontentloaded");
  const emailInput = visibleLocator(
    page,
    'input[type="email"], input[name="email"], input[name="username"], #username',
  );
  await waitForAuth0Field(page, emailInput, "email");
  const passwordInput = visibleLocator(
    page,
    'input[type="password"]:not([aria-hidden="true"]):not(.hide), input[name="password"]:not([aria-hidden="true"]):not(.hide), #password:not([aria-hidden="true"]):not(.hide)',
  );
  if (
    !(await passwordInput
      .first()
      .isVisible()
      .catch(() => false))
  ) {
    // Auth0's automatic passkey POST can replace this document. Fill and
    // submit the same identifier form in one browser task so navigation
    // cannot discard the username between separate Playwright operations.
    // A document replaced during that task (by the submit itself or by that
    // POST) destroys its context; the page that follows decides: its
    // password field continues below, a fresh identifier form is filled again.
    for (let attempt = 1; ; attempt += 1) {
      try {
        await submitAuth0Identifier(emailInput, email, expectedOrigin);
        break;
      } catch (error) {
        if (
          attempt >= 3 ||
          !/Execution context was destroyed/i.test(String(error))
        )
          throw error;
      }
      const next = await nextAuth0Field(page, passwordInput, emailInput);
      assertExpectedIssuer(page, expectedOrigin);
      if (next !== "identifier") break;
    }
  } else {
    await fillSecret(emailInput.first(), email);
  }
  await waitForAuth0Field(page, passwordInput, "password");
  assertExpectedIssuer(page, expectedOrigin);
  await fillSecret(passwordInput.first(), password);
  await page
    .getByRole("button", { name: /^(log in|sign in|continue)$/i })
    .last()
    .click();
}

// Which Auth0 form follows a replaced document: the password field, a new
// identifier form, or (after 20 s) neither, which the password wait reports.
async function nextAuth0Field(
  page: Page,
  passwordInput: ReturnType<typeof visibleLocator>,
  emailInput: ReturnType<typeof visibleLocator>,
): Promise<"password" | "identifier" | "none"> {
  const deadline = Date.now() + 20_000;
  while (Date.now() < deadline) {
    await page.waitForLoadState("domcontentloaded").catch(() => undefined);
    if (await passwordInput.first().isVisible().catch(() => false))
      return "password";
    if (await emailInput.first().isVisible().catch(() => false))
      return "identifier";
    await page.waitForTimeout(250);
  }
  return "none";
}

async function submitAuth0Identifier(
  emailInput: ReturnType<typeof visibleLocator>,
  email: string,
  expectedOrigin: string,
) {
  await emailInput.first().evaluate((input, { identifier, expectedOrigin }) => {
    if (location.origin.toLowerCase() !== expectedOrigin) {
      throw new Error("Refusing to enter Auth0 identifier on unexpected issuer");
    }
    if (
      !(input instanceof HTMLInputElement) ||
      !input.form ||
      input.disabled ||
      input.readOnly
    ) {
      throw new Error("Auth0 identifier field is not editable in a form");
    }
    const form = input.form;
    const submitter = form.querySelector(
      'button[type="submit"][name="action"]',
    );
    if (!(submitter instanceof HTMLButtonElement)) {
      throw new Error("Auth0 identifier form has no Continue submitter");
    }
    input.value = identifier;
    input.dispatchEvent(new Event("input", { bubbles: true }));
    input.dispatchEvent(new Event("change", { bubbles: true }));
    form.requestSubmit(submitter);
  }, { identifier: email, expectedOrigin });
}

function requireExpectedIssuerOrigin(value: string) {
  let parsed: URL;
  try {
    parsed = new URL(value);
  } catch {
    throw new Error("Expected Auth0 issuer must be a valid HTTPS URL");
  }
  if (parsed.protocol !== "https:") {
    throw new Error("Expected Auth0 issuer must use HTTPS");
  }
  return parsed.origin.toLowerCase();
}

function assertExpectedIssuer(page: Page, expectedOrigin: string) {
  let actualOrigin = "";
  try {
    actualOrigin = new URL(page.url()).origin.toLowerCase();
  } catch {
    // Preserve the fail-closed mismatch below without including credentials.
  }
  if (actualOrigin !== expectedOrigin) {
    throw new Error(
      `Refusing to enter Auth0 credentials on unexpected issuer origin ${actualOrigin || "invalid"}`,
    );
  }
}

function visibleLocator(page: Page, selector: string) {
  return page.locator(
    selector
      .split(",")
      .map((part) => `${part.trim()}:visible`)
      .join(", "),
  );
}

async function continuePastAuth0PostLoginPrompts(page: Page) {
  for (let attempt = 0; attempt < 3; attempt += 1) {
    await page.waitForLoadState("domcontentloaded").catch(() => undefined);
    const passkeySkip = page.getByText(/continue without passkeys/i).first();
    const visible = await passkeySkip
      .isVisible({ timeout: 8_000 })
      .catch(() => false);
    if (!visible) return;

    const dontShowAgain = page.getByLabel(/don'?t show me this again/i).first();
    if (await dontShowAgain.isVisible({ timeout: 1_000 }).catch(() => false)) {
      await dontShowAgain
        .check({ force: true, timeout: 5_000 })
        .catch(() => undefined);
    }
    await passkeySkip
      .click({ force: true, timeout: 10_000 })
      .catch(async (err) => {
        const clicked = await clickAuth0Text(
          page,
          /continue without passkeys/i,
        );
        if (!clicked) throw err;
      });
  }
}

async function clickAuth0Text(page: Page, pattern: RegExp) {
  return await page.evaluate((source) => {
    const regex = new RegExp(source, "i");
    const elements = Array.from(
      document.querySelectorAll("button, a, [role='button'], input"),
    );
    const target = elements.find((element) =>
      regex.test(
        [
          element.textContent ?? "",
          element.getAttribute("aria-label") ?? "",
          element.getAttribute("value") ?? "",
        ].join(" "),
      ),
    );
    if (!(target instanceof HTMLElement)) return false;
    target.click();
    return true;
  }, pattern.source);
}

async function waitForRuntimeDashboard(page: Page) {
  const deadline = Date.now() + 120_000;
  let lastWaitError = "";
  while (Date.now() < deadline) {
    const formError = await auth0FormErrorMessage(page, 500);
    if (formError) {
      throw new Error(
        `Runtime Auth0 login failed before Homelab Runtime callback: ${formError}. Current location: ${safeLocation(page.url())}`,
      );
    }
    if (/\/dashboard(?:[/?#]|$)/.test(page.url())) return;

    try {
      await page.waitForURL(/\/dashboard/, {
        timeout: Math.max(1, Math.min(5_000, deadline - Date.now())),
      });
      return;
    } catch (err) {
      lastWaitError =
        err instanceof Error ? err.name : "navigation wait failed";
    }
  }
  throw new Error(
    `Runtime Auth0 login did not reach /dashboard. Current location: ${safeLocation(page.url())}. ${lastWaitError}`,
  );
}

async function auth0FormErrorMessage(page: Page, timeoutMs = 1_000) {
  const visibleTextError = page.getByText(AUTH0_FORM_ERROR_PATTERN).first();
  if (
    await visibleTextError.isVisible({ timeout: timeoutMs }).catch(() => false)
  ) {
    return compactAuth0Error(
      await visibleTextError.innerText({ timeout: 1_000 }).catch(() => ""),
    );
  }

  const alertError = page
    .locator('[role="alert"], [aria-live="assertive"], [aria-live="polite"]')
    .filter({ hasText: AUTH0_FORM_ERROR_PATTERN })
    .first();
  if (await alertError.isVisible({ timeout: 250 }).catch(() => false)) {
    return compactAuth0Error(
      await alertError.innerText({ timeout: 1_000 }).catch(() => ""),
    );
  }

  return "";
}

async function waitForAuth0Field(
  page: Page,
  locator: ReturnType<typeof visibleLocator>,
  label: string,
) {
  const deadline = Date.now() + 20_000;
  while (Date.now() < deadline) {
    const formError = await auth0FormErrorMessage(page, 250);
    if (formError) {
      throw new Error(
        `Runtime Auth0 login stopped before the ${label} field: ${formError}. Current location: ${safeLocation(page.url())}`,
      );
    }
    if (
      await locator
        .first()
        .isVisible()
        .catch(() => false)
    )
      return;
    await page.waitForTimeout(250);
  }
  throw new Error(
    `Runtime Auth0 login did not render the ${label} field. Current location: ${safeLocation(page.url())}`,
  );
}

function safeLocation(value: string) {
  try {
    const parsed = new URL(value);
    return `${parsed.origin}${parsed.pathname}`;
  } catch {
    return "invalid";
  }
}
