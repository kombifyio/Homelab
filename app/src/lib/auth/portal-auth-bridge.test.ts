// @vitest-environment jsdom

import {
  afterEach,
  beforeEach,
  describe,
  expect,
  it,
  vi,
} from "vite-plus/test";

vi.mock("$app/env", () => ({ browser: true }));
vi.mock("#lib/i18n.svelte.js", () => ({ tr: (key: string) => key }));
vi.mock("#lib/stores/theme", () => ({ theme: {} }));
vi.mock("#lib/stores/stackIdentity", () => ({ setStackIdentity: vi.fn() }));

const parent = { postMessage: vi.fn() };
const origin = "https://app.kombify.io";

function receive(data: object): void {
  window.dispatchEvent(
    new MessageEvent("message", { source: window.parent, origin, data }),
  );
}

describe("Cloud auth bridge recovery", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.resetModules();
    vi.clearAllMocks();
    Object.defineProperty(window, "parent", {
      value: parent,
      configurable: true,
    });
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
  });

  afterEach(async () => {
    const { destroyBridge } = await import("#lib/stores/postMessageBridge.js");
    destroyBridge();
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  it("accepts a renewed token after a retryable error and more than ten seconds", async () => {
    const { initBridge, requestAuthToken } =
      await import("#lib/stores/postMessageBridge.js");
    initBridge(origin);
    const token = requestAuthToken();
    receive({
      type: "auth-error",
      error: "renewing",
      code: 503,
      retryable: true,
    });
    await vi.advanceTimersByTimeAsync(20_000);
    receive({ type: "auth-token", token: "renewed-token" });
    await expect(token).resolves.toBe("renewed-token");
    expect(vi.getTimerCount()).toBe(0);
  });

  it("rejects terminal errors immediately with their structured metadata", async () => {
    const { initBridge, requestAuthToken, PortalAuthError } =
      await import("#lib/stores/postMessageBridge.js");
    initBridge(origin);
    const token = requestAuthToken();
    const rejected = expect(token).rejects.toMatchObject({
      message: "sign in again",
      code: 401,
      retryable: false,
    });
    receive({
      type: "auth-error",
      message: "sign in again",
      code: 401,
      retryable: false,
    });
    await rejected;
    await expect(token).rejects.toBeInstanceOf(PortalAuthError);
    expect(vi.getTimerCount()).toBe(0);
  });

  it("preserves a transient error when the renewal deadline expires", async () => {
    const { initBridge, requestAuthToken } =
      await import("#lib/stores/postMessageBridge.js");
    initBridge(origin);
    const token = requestAuthToken();
    const rejected = expect(token).rejects.toMatchObject({
      message: "temporarily unavailable",
      code: "session_renewal_failed",
      retryable: true,
    });
    receive({
      type: "auth-error",
      error: "temporarily unavailable",
      code: "session_renewal_failed",
      retryable: true,
    });
    await vi.advanceTimersByTimeAsync(30_000);
    await rejected;
    expect(vi.getTimerCount()).toBe(0);
  });

  it("sends a new auth request for a fresh token instead of using the cache", async () => {
    const { initBridge, requestAuthToken } =
      await import("#lib/stores/postMessageBridge.js");
    initBridge(origin);
    receive({ type: "auth-token", token: "cached-token" });
    const token = requestAuthToken({ fresh: true });
    expect(parent.postMessage).toHaveBeenLastCalledWith(
      { type: "auth-request", tool: "kombifystack", fresh: true },
      origin,
    );
    receive({ type: "auth-token", token: "new-token" });
    await expect(token).resolves.toBe("new-token");
  });
});
