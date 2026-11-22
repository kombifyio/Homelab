// Settings › AI providers in the Windows client: the device-wide local AI runtimes (workspace
// `platform/standards/local-ai-runtimes.v1.md`) over the client's WebView2 bridge. The native side
// (`ClientWindow.LocalAi.cs`) reads and writes the shared file and probes a switched-on runtime; the
// page sees only the result. Every message carries the document revision and token the shell issued
// for this document, as the device-context read does.

interface DocumentBinding {
  document_revision: number;
  document_token: string;
}

interface LocalAiWindow extends Window {
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

const PREFIX = "kombify.homelab.local-ai.";
const RESULT = `${PREFIX}result.v1`;
const CHANGED = `${PREFIX}changed.v1`;
const BINDING_WAIT_MS = 4000;
// A read probes each switched-on runtime for at most 3 s each.
const ANSWER_MS = 10000;

export interface LocalAiRuntimeStatus {
  reachable: boolean;
  version: string;
  models: string[];
  error: string;
}

export interface LocalAiRuntime {
  id: string;
  enabled: boolean;
  endpoint: string;
  defaultEndpoint: string;
  /** Present for a switched-on runtime. */
  status?: LocalAiRuntimeStatus;
}

export interface LocalAiSnapshot {
  /** False when the shared file has another version or is damaged: shown off, never written. */
  readable: boolean;
  revision: number;
  runtimes: LocalAiRuntime[];
}

export type LocalAiRefusal = "invalid_endpoint" | "unreadable" | "failed";

export class LocalAiBridgeError extends Error {
  constructor(readonly code: "unavailable" | "failed") {
    super(code);
  }
}

/** True in the Windows client's WebView; false in a browser and in the hosted web app. */
export function hasLocalAiBridge(): boolean {
  return (
    typeof window !== "undefined" &&
    Boolean((window as LocalAiWindow).chrome?.webview)
  );
}

function call(
  operation: "read" | "write",
  fields: Record<string, unknown>,
): Promise<{ status: string; local_ai?: unknown; error?: unknown }> {
  const host = window as LocalAiWindow;
  const bridge = host.chrome?.webview;
  if (!bridge) return Promise.reject(new LocalAiBridgeError("unavailable"));
  const requestId = crypto.randomUUID();
  return new Promise((resolve, reject) => {
    let binding: DocumentBinding | undefined;
    let settled = false;
    const finish = (outcome: () => void) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      bridge.removeEventListener("message", receive);
      host.removeEventListener("kombify-homelab-device-context-ready", send);
      outcome();
    };
    const receive = (event: MessageEvent) => {
      const data = event.data as Record<string, unknown> | null;
      if (
        !data ||
        data.type !== RESULT ||
        data.request_id !== requestId ||
        data.document_revision !== binding?.document_revision ||
        data.operation !== operation
      )
        return;
      finish(() =>
        resolve(
          data as { status: string; local_ai?: unknown; error?: unknown },
        ),
      );
    };
    const send = () => {
      const current = host.__kombifyHomelabDocument;
      if (
        binding ||
        !current ||
        !Number.isSafeInteger(current.document_revision)
      )
        return;
      binding = { ...current };
      clearTimeout(timer);
      timer = setTimeout(timeout, ANSWER_MS);
      try {
        bridge.postMessage({
          type: `${PREFIX}${operation}.v1`,
          request_id: requestId,
          document_revision: binding.document_revision,
          document_token: binding.document_token,
          ...fields,
        });
      } catch {
        finish(() => reject(new LocalAiBridgeError("failed")));
      }
    };
    const timeout = () =>
      finish(() => reject(new LocalAiBridgeError("failed")));
    let timer = setTimeout(timeout, BINDING_WAIT_MS);
    bridge.addEventListener("message", receive);
    host.addEventListener("kombify-homelab-device-context-ready", send);
    send();
  });
}

function snapshotOf(value: unknown): LocalAiSnapshot {
  const snapshot = value as Partial<LocalAiSnapshot> | null;
  if (
    !snapshot ||
    typeof snapshot.readable !== "boolean" ||
    !Number.isSafeInteger(snapshot.revision) ||
    !Array.isArray(snapshot.runtimes)
  )
    throw new LocalAiBridgeError("failed");
  return snapshot as LocalAiSnapshot;
}

export async function readLocalAi(): Promise<LocalAiSnapshot> {
  const answer = await call("read", {});
  if (answer.status !== "ok") throw new LocalAiBridgeError("failed");
  return snapshotOf(answer.local_ai);
}

/** Sets one runtime's switch or server address; the file after the change, or the refusal. */
export async function writeLocalAi(
  runtime: string,
  change: { enabled?: boolean; endpoint?: string },
): Promise<LocalAiSnapshot | { refused: LocalAiRefusal }> {
  const answer = await call("write", { runtime, ...change });
  if (answer.status === "refused") {
    const error = answer.error;
    return {
      refused:
        error === "invalid_endpoint" || error === "unreadable"
          ? error
          : "failed",
    };
  }
  if (answer.status !== "ok") throw new LocalAiBridgeError("failed");
  return snapshotOf(answer.local_ai);
}

/** Calls `onChange` whenever any kombify app on this PC changed the shared file. */
export function watchLocalAi(onChange: () => void): () => void {
  const bridge = (window as LocalAiWindow).chrome?.webview;
  if (!bridge) return () => {};
  const receive = (event: MessageEvent) => {
    const data = event.data as { type?: unknown } | null;
    if (data?.type === CHANGED) onChange();
  };
  bridge.addEventListener("message", receive);
  return () => bridge.removeEventListener("message", receive);
}
