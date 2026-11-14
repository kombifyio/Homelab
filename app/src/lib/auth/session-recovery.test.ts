import { beforeEach, describe, expect, it, vi } from "vite-plus/test";

const sentryState = vi.hoisted(() => {
  const scope = {
    setLevel: vi.fn(),
    setTag: vi.fn(),
  };
  return {
    captureMessage: vi.fn(),
    scope,
    withScope: vi.fn((callback: (scope: unknown) => void) => {
      callback(scope);
    }),
  };
});

vi.mock("@sentry/sveltekit", () => sentryState);

import {
  AUTO_RELOGIN_MARKER_KEY,
  AUTO_RELOGIN_TTL_MS,
  clearAutoReloginMarker,
  markAutoReloginAttempt,
  noteGatewayTokenSuccess,
  shouldAttemptAutoRelogin,
} from "./session-recovery";

function fakeStorage(initial: Record<string, string> = {}): Storage {
  const map = new Map(Object.entries(initial));
  return {
    get length() {
      return map.size;
    },
    clear: () => map.clear(),
    getItem: (key: string) => map.get(key) ?? null,
    key: (index: number) => [...map.keys()][index] ?? null,
    removeItem: (key: string) => {
      map.delete(key);
    },
    setItem: (key: string, value: string) => {
      map.set(key, value);
    },
  };
}

describe("auto-relogin loop guard", () => {
  it("allows the attempt when no marker exists", () => {
    expect(shouldAttemptAutoRelogin(1_000_000, fakeStorage())).toBe(true);
  });

  it("blocks while the marker is fresh and allows after the TTL", () => {
    const storage = fakeStorage();
    markAutoReloginAttempt(1_000_000, storage);
    expect(shouldAttemptAutoRelogin(1_000_000 + 5_000, storage)).toBe(false);
    expect(
      shouldAttemptAutoRelogin(1_000_000 + AUTO_RELOGIN_TTL_MS + 1, storage),
    ).toBe(true);
  });

  it("tolerates a corrupted marker value", () => {
    const storage = fakeStorage({ [AUTO_RELOGIN_MARKER_KEY]: "not-a-number" });
    expect(shouldAttemptAutoRelogin(1_000_000, storage)).toBe(true);
  });

  it("clears the marker", () => {
    const storage = fakeStorage();
    markAutoReloginAttempt(1_000_000, storage);
    clearAutoReloginMarker(storage);
    expect(storage.getItem(AUTO_RELOGIN_MARKER_KEY)).toBeNull();
  });
});

describe("noteGatewayTokenSuccess", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("emits the recovered event and clears a pending marker", () => {
    const storage = fakeStorage();
    markAutoReloginAttempt(1_000_000, storage);

    noteGatewayTokenSuccess(storage);

    expect(storage.getItem(AUTO_RELOGIN_MARKER_KEY)).toBeNull();
    expect(sentryState.captureMessage).toHaveBeenCalledWith(
      "techstack_session_auto_relogin_recovered",
    );
  });

  it("does nothing without a pending marker", () => {
    noteGatewayTokenSuccess(fakeStorage());
    expect(sentryState.captureMessage).not.toHaveBeenCalled();
  });
});
