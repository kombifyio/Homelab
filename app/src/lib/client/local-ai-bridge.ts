// Settings › AI providers and the local answer in the Windows client: the device-wide local AI
// runtimes (workspace `platform/standards/local-ai-runtimes.v1.md`) over the client's WebView2 bridge.
// The native side (`ClientWindow.LocalAi.cs`) reads and writes the shared file, probes a switched-on
// runtime and runs a conversation turn on the runtime the person chose (rule 9); the page sees only
// the result and never names the runtime a turn runs on. Every message carries the document revision
// and token the shell issued for this document, as the device-context read does.

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
const CHAT_DELTA = `${PREFIX}chat-delta.v1`;
const BINDING_WAIT_MS = 4000;
// A read probes the switched-on runtimes for at most 3 s.
const ANSWER_MS = 10000;
// A local model may need a while for its first word (loading into memory); each piece renews it.
const CHAT_ANSWER_MS = 120_000;

export interface LocalAiRuntimeStatus {
  reachable: boolean;
  version: string;
  models: string[];
  /** The models that can hold a conversation (no embedding models); absent from an older shell. */
  chatModels?: string[];
  error: string;
}

export interface LocalAiRuntime {
  id: string;
  /** The runtime it speaks (`ollama`). */
  kind: string;
  /** The name a person sees; empty for this PC's own runtime. */
  label: string;
  /** A runtime on another machine, such as the Ollama of a homelab server. */
  remote: boolean;
  enabled: boolean;
  endpoint: string;
  /** Empty for a runtime on another machine. */
  defaultEndpoint: string;
  /** Present for a switched-on runtime. */
  status?: LocalAiRuntimeStatus;
}

/** Why the answer choice cannot answer now (empty: it can). */
export type LocalAiAnswerReason =
  | ""
  | "runtime_missing"
  | "runtime_off"
  | "unreachable"
  | "no_models"
  | "model_missing";

/** The answer choice of rule 9: Homelab answers with this runtime and model. */
export interface LocalAiAnswer {
  runtime: string;
  /** Empty: the first model the runtime reports. */
  model: string;
  /** The model that answers, after the shell probed the runtime. */
  resolvedModel?: string;
  reason?: LocalAiAnswerReason;
}

export interface LocalAiSnapshot {
  /** False when the shared file has another version or is damaged: shown off, never written. */
  readable: boolean;
  revision: number;
  runtimes: LocalAiRuntime[];
  /** Absent: Homelab answers from the account's AI use. */
  answer?: LocalAiAnswer;
}

export type LocalAiRefusal =
  "invalid_endpoint" | "invalid" | "unreadable" | "failed";

type Operation = "read" | "write" | "add" | "remove" | "answer" | "chat";

type Answer = { status: string; local_ai?: unknown; error?: unknown } & Record<
  string,
  unknown
>;

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
  operation: Operation,
  fields: Record<string, unknown>,
  options: {
    onDelta?: (text: string) => void;
    signal?: AbortSignal;
    answerMs?: number;
  } = {},
): Promise<Answer> {
  const host = window as LocalAiWindow;
  const bridge = host.chrome?.webview;
  if (!bridge) return Promise.reject(new LocalAiBridgeError("unavailable"));
  const requestId = crypto.randomUUID();
  const answerMs = options.answerMs ?? ANSWER_MS;
  return new Promise((resolve, reject) => {
    let binding: DocumentBinding | undefined;
    let settled = false;
    const finish = (outcome: () => void) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      bridge.removeEventListener("message", receive);
      host.removeEventListener("kombify-homelab-device-context-ready", send);
      options.signal?.removeEventListener("abort", abort);
      outcome();
    };
    const receive = (event: MessageEvent) => {
      const data = event.data as Record<string, unknown> | null;
      if (
        !data ||
        data.request_id !== requestId ||
        data.document_revision !== binding?.document_revision
      )
        return;
      if (data.type === CHAT_DELTA && typeof data.text === "string") {
        clearTimeout(timer);
        timer = setTimeout(timeout, answerMs);
        options.onDelta?.(data.text);
        return;
      }
      if (data.type !== RESULT || data.operation !== operation) return;
      finish(() => resolve(data as Answer));
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
      timer = setTimeout(timeout, answerMs);
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
    // Stops a running turn; its result still arrives (as `cancelled`) and settles the call.
    const abort = () => {
      if (!binding) {
        finish(() => reject(new LocalAiBridgeError("failed")));
        return;
      }
      try {
        bridge.postMessage({
          type: `${PREFIX}chat-cancel.v1`,
          request_id: requestId,
          document_revision: binding.document_revision,
          document_token: binding.document_token,
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
    options.signal?.addEventListener("abort", abort);
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

async function change(
  operation: Operation,
  fields: Record<string, unknown>,
): Promise<LocalAiSnapshot | { refused: LocalAiRefusal }> {
  const answer = await call(operation, fields);
  if (answer.status === "refused") {
    const error = answer.error;
    return {
      refused:
        error === "invalid_endpoint" ||
        error === "unreadable" ||
        error === "invalid"
          ? error
          : "failed",
    };
  }
  if (answer.status !== "ok") throw new LocalAiBridgeError("failed");
  return snapshotOf(answer.local_ai);
}

/** Sets one runtime's switch or server address; the file after the change, or the refusal. */
export function writeLocalAi(
  runtime: string,
  update: { enabled?: boolean; endpoint?: string },
): Promise<LocalAiSnapshot | { refused: LocalAiRefusal }> {
  return change("write", { runtime, ...update });
}

/** Adds an Ollama on another machine (such as a homelab server), switched on. */
export function addLocalAiRuntime(
  label: string,
  endpoint: string,
): Promise<LocalAiSnapshot | { refused: LocalAiRefusal }> {
  return change("add", { label, endpoint });
}

/** Removes a runtime on another machine; an answer choice that names it goes with it. */
export function removeLocalAiRuntime(
  runtime: string,
): Promise<LocalAiSnapshot | { refused: LocalAiRefusal }> {
  return change("remove", { runtime });
}

/** Sets the answer choice (a runtime and model), or `null` to answer from the account's AI use again. */
export function setLocalAiAnswer(
  choice: { runtime: string; model?: string } | null,
): Promise<LocalAiSnapshot | { refused: LocalAiRefusal }> {
  return change(
    "answer",
    choice
      ? {
          runtime: choice.runtime,
          ...(choice.model ? { model: choice.model } : {}),
        }
      : {},
  );
}

export interface LocalChatMessage {
  role: "system" | "user" | "assistant";
  content: string;
}

/** Who answered a turn: the runtime the person chose and the model. */
export interface LocalChatSource {
  runtime: string;
  kind: string;
  label: string;
  remote: boolean;
  model: string;
}

export type LocalChatRefusal =
  Exclude<LocalAiAnswerReason, ""> | "no_answer" | "cancelled" | "failed";

export type LocalChatOutcome =
  | ({ answered: true } & LocalChatSource)
  | ({ answered: false; reason: LocalChatRefusal } & LocalChatSource);

const CHAT_REFUSALS: readonly LocalChatRefusal[] = [
  "runtime_missing",
  "runtime_off",
  "unreachable",
  "no_models",
  "model_missing",
  "no_answer",
  "cancelled",
];

/**
 * One conversation turn on the runtime the person chose; `onDelta` gets each piece of the answer.
 * A runtime that cannot answer is a reason, never a turn somewhere else.
 */
export async function chatLocalAi(
  messages: LocalChatMessage[],
  onDelta: (text: string) => void,
  signal?: AbortSignal,
): Promise<LocalChatOutcome> {
  const answer = await call(
    "chat",
    { messages },
    { onDelta, signal, answerMs: CHAT_ANSWER_MS },
  );
  const source: LocalChatSource = {
    runtime: typeof answer.runtime === "string" ? answer.runtime : "",
    kind: typeof answer.kind === "string" ? answer.kind : "",
    label: typeof answer.label === "string" ? answer.label : "",
    remote: answer.remote === true,
    model: typeof answer.model === "string" ? answer.model : "",
  };
  if (answer.status === "ok") return { answered: true, ...source };
  const reason = CHAT_REFUSALS.find((known) => known === answer.error);
  return { answered: false, reason: reason ?? "failed", ...source };
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
