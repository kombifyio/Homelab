// The Companion settings section of the Windows client (SETTINGS-CONTRACT 2.2, AppLink v2): the
// adapter of the Companion settings page (`#lib/settings/companion`), over the client's WebView2 bridge. The
// native side (`ClientWindow.Companion.cs`) speaks AppLink; no token, pipe name, registry path or
// path of another program reaches this page. Every message is fixed and typed, and carries the
// document revision and token the shell issued for this document, as the device-context read does.
import type {
  CompanionSettingsAdapter,
  CompanionSettingsRefusal,
  CompanionSettingsSnapshot,
  CompanionSettingWrite,
} from "#lib/settings/companion/contract.js";

interface DocumentBinding {
  document_revision: number;
  document_token: string;
}

interface CompanionWindow extends Window {
  __kombifyHomelabDocument?: DocumentBinding;
  chrome?: {
    webview?: {
      postMessage(message: unknown): void;
      addEventListener(
        type: "message",
        listener: (event: MessageEvent) => void,
      ): void;
      removeEventListener(
        type: "message",
        listener: (event: MessageEvent) => void,
      ): void;
    };
  };
}

const PREFIX = "kombify.homelab.companion.";
const RESULT = `${PREFIX}result.v1`;

/** The five calls of the adapter (and `watch-cancel`, sent when a watch is aborted): the whole page-to-shell vocabulary. */
type Operation = "read" | "write" | "watch" | "pick-folder" | "open-link";

/** What the shell answers: `unavailable` means this installation cannot show the Companion at all. */
type Answer =
  | { status: "ok"; settings?: unknown; revision?: unknown; path?: unknown }
  | { status: "refused"; error: string; with?: string }
  | { status: "unavailable" }
  | { status: "failed" };

export class CompanionBridgeError extends Error {
  constructor(readonly code: "unavailable" | "failed" | "aborted") {
    super(code);
  }
}

/** True in the Windows client's WebView; false in a browser and in the hosted web app. */
export function hasCompanionBridge(): boolean {
  return (
    typeof window !== "undefined" &&
    Boolean((window as CompanionWindow).chrome?.webview)
  );
}

// A read waits for the shell's document binding (set once the navigation completed).
const BINDING_WAIT_MS = 4000;
// Reads answer within the interface's 2 s deadline plus the shell's own checks.
const READ_MS = 8000;
// A watch is held by the program for at most 25 s.
const WATCH_MS = 40000;
// Switching on may install the carried Companion first (the shell waits at most ten minutes).
const WRITE_MS = 11 * 60 * 1000;
// The folder picker waits for the person; the page leaving cancels it.
const PICK_MS = 30 * 60 * 1000;

const TIMEOUT: Record<Operation, number> = {
  read: READ_MS,
  write: WRITE_MS,
  watch: WATCH_MS,
  "pick-folder": PICK_MS,
  "open-link": READ_MS,
};

function call(
  operation: Operation,
  fields: Record<string, unknown>,
  signal?: AbortSignal,
): Promise<Answer> {
  const host = window as CompanionWindow;
  const bridge = host.chrome?.webview;
  if (!bridge) return Promise.reject(new CompanionBridgeError("unavailable"));
  if (signal?.aborted)
    return Promise.reject(new CompanionBridgeError("aborted"));
  const location = host.location.href;
  const requestId = crypto.randomUUID();
  return new Promise<Answer>((resolve, reject) => {
    let binding: DocumentBinding | undefined;
    let settled = false;
    const finish = (outcome: () => void) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      bridge.removeEventListener("message", receive);
      host.removeEventListener("kombify-homelab-device-context-ready", send);
      host.removeEventListener("pagehide", leave);
      signal?.removeEventListener("abort", abort);
      outcome();
    };
    const post = (type: string, extra: Record<string, unknown>) => {
      if (!binding) return;
      bridge.postMessage({
        type: `${PREFIX}${type}.v1`,
        request_id: requestId,
        document_revision: binding.document_revision,
        document_token: binding.document_token,
        ...extra,
      });
    };
    const leave = () => {
      try {
        if (operation === "watch") post("watch-cancel", {});
      } catch {
        /* The document may already be gone. */
      }
      finish(() => reject(new CompanionBridgeError("aborted")));
    };
    const abort = leave;
    const receive = (event: MessageEvent) => {
      if (host.location.href !== location) return leave();
      const data = event.data as {
        type?: unknown;
        request_id?: unknown;
        document_revision?: unknown;
        operation?: unknown;
        status?: unknown;
      } | null;
      if (
        !data ||
        data.type !== RESULT ||
        data.request_id !== requestId ||
        data.document_revision !== binding?.document_revision ||
        data.operation !== operation
      )
        return;
      const answer = parseAnswer(data);
      finish(() =>
        answer ? resolve(answer) : reject(new CompanionBridgeError("failed")),
      );
    };
    const send = () => {
      const current = host.__kombifyHomelabDocument;
      if (
        binding ||
        !current ||
        !Number.isSafeInteger(current.document_revision) ||
        typeof current.document_token !== "string"
      )
        return;
      binding = { ...current };
      clearTimeout(timer);
      timer = setTimeout(timeout, TIMEOUT[operation]);
      try {
        post(operation, fields);
      } catch {
        finish(() => reject(new CompanionBridgeError("failed")));
      }
    };
    const timeout = () =>
      finish(() => reject(new CompanionBridgeError("failed")));
    let timer = setTimeout(timeout, BINDING_WAIT_MS);
    bridge.addEventListener("message", receive);
    host.addEventListener("kombify-homelab-device-context-ready", send);
    host.addEventListener("pagehide", leave);
    signal?.addEventListener("abort", abort, { once: true });
    send();
  });
}

function parseAnswer(data: Record<string, unknown>): Answer | null {
  switch (data.status) {
    case "ok":
      return {
        status: "ok",
        settings: data.settings,
        revision: data.revision,
        path: data.path,
      };
    case "refused":
      return typeof data.error === "string"
        ? {
            status: "refused",
            error: data.error,
            ...(typeof data.with === "string" ? { with: data.with } : {}),
          }
        : null;
    case "unavailable":
      return { status: "unavailable" };
    case "failed":
      return { status: "failed" };
    default:
      return null;
  }
}

function snapshotOf(answer: Answer): CompanionSettingsSnapshot {
  if (answer.status === "unavailable")
    throw new CompanionBridgeError("unavailable");
  if (answer.status !== "ok" || !isSnapshot(answer.settings))
    throw new CompanionBridgeError("failed");
  return answer.settings;
}

function isSnapshot(value: unknown): value is CompanionSettingsSnapshot {
  const snapshot = value as Partial<CompanionSettingsSnapshot> | null;
  return (
    typeof snapshot === "object" &&
    snapshot !== null &&
    Number.isSafeInteger(snapshot.revision) &&
    typeof snapshot.installed === "boolean" &&
    typeof snapshot.running === "boolean" &&
    typeof snapshot.enabled === "boolean" &&
    Array.isArray(snapshot.values) &&
    typeof snapshot.session === "object" &&
    snapshot.session !== null &&
    typeof snapshot.links === "object" &&
    snapshot.links !== null
  );
}

/**
 * Asks whether this installation can show the Companion: the Windows client answers yes while a
 * Companion is installed or the installer carries one. The Companion tab exists only then. `null`: the
 * probe was aborted and has not answered; ask again.
 */
export async function probeCompanion(
  signal?: AbortSignal,
): Promise<boolean | null> {
  if (!hasCompanionBridge()) return false;
  try {
    return (await call("read", {}, signal)).status !== "unavailable";
  } catch (error) {
    // The probe was cut short (the person left the document or the caller aborted it): that says nothing
    // about the installation, so the caller asks again. Never answer "unavailable" for an abort.
    if (error instanceof CompanionBridgeError && error.code === "aborted")
      return null;
    // An unanswered probe (no shell handler, no document binding) shows no tab.
    return false;
  }
}

/** The adapter of the Companion settings controller, over the WebView2 bridge of this client. */
export function createCompanionAdapter(): CompanionSettingsAdapter {
  return {
    async read() {
      return snapshotOf(await call("read", {}));
    },
    async write(
      key: string,
      value: CompanionSettingWrite,
    ): Promise<CompanionSettingsSnapshot | CompanionSettingsRefusal> {
      const answer = await call("write", { key, value });
      if (answer.status === "refused")
        return {
          error: answer.error,
          ...(answer.with ? { with: answer.with } : {}),
        };
      return snapshotOf(answer);
    },
    async watch(after: number, signal: AbortSignal) {
      const answer = await call("watch", { after }, signal);
      if (answer.status !== "ok" || !Number.isSafeInteger(answer.revision))
        throw new CompanionBridgeError("failed");
      return answer.revision as number;
    },
    async pickFolder(current: string) {
      const answer = await call("pick-folder", { current });
      if (answer.status !== "ok") throw new CompanionBridgeError("failed");
      return typeof answer.path === "string" && answer.path
        ? answer.path
        : null;
    },
    async openLink(url: string) {
      await call("open-link", { url });
    },
  };
}
