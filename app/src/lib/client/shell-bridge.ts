// What the Windows client's shell offers this page (`ClientWindow.Shell.cs`): the update state with its
// "Restart to update", and kombify's own Homelab sign-in in the system browser for "With my kombify
// account" on the first start. Every message is fixed and typed and carries the document revision and
// token the shell issued for this document, as the device-context read does. A browser and the hosted
// web app have no bridge: nothing is shown there and the calls report `false`.

interface DocumentBinding {
  document_revision: number;
  document_token: string;
}

interface ShellWindow extends Window {
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

const PREFIX = "kombify.homelab.shell.";
const RESULT = `${PREFIX}result.v1`;
const READY_EVENT = "kombify-homelab-device-context-ready";

/** "ready": a verified update waits for a restart. "updated": this start applied one (said once). */
export type ClientUpdateState =
  | { state: "none" }
  | { state: "ready" | "updated"; version: string };

function shellWindow(): ShellWindow | null {
  if (typeof window === "undefined") return null;
  const host = window as ShellWindow;
  return host.chrome?.webview ? host : null;
}

function currentBinding(host: ShellWindow): DocumentBinding | null {
  const current = host.__kombifyHomelabDocument;
  if (
    !current ||
    !Number.isSafeInteger(current.document_revision) ||
    typeof current.document_token !== "string"
  )
    return null;
  return {
    document_revision: current.document_revision,
    document_token: current.document_token,
  };
}

function post(
  host: ShellWindow,
  action: string,
  binding: DocumentBinding,
  requestId: string,
): boolean {
  try {
    host.chrome!.webview!.postMessage({
      type: `${PREFIX}${action}.v1`,
      request_id: requestId,
      document_revision: binding.document_revision,
      document_token: binding.document_token,
    });
    return true;
  } catch {
    return false;
  }
}

/**
 * Asks the shell once per document for the update state; the shell answers at once and again under
 * the same request id when an update becomes ready. Returns the stop function.
 */
export function watchClientUpdate(
  changed: (update: ClientUpdateState) => void,
): () => void {
  const host = shellWindow();
  if (!host) return () => {};
  const bridge = host.chrome!.webview!;
  const requestId = crypto.randomUUID();
  let binding: DocumentBinding | null = null;
  const receive = (event: MessageEvent) => {
    const data = event.data as {
      type?: unknown;
      request_id?: unknown;
      document_revision?: unknown;
      operation?: unknown;
      status?: unknown;
      state?: unknown;
      version?: unknown;
    } | null;
    if (
      !data ||
      data.type !== RESULT ||
      data.request_id !== requestId ||
      data.document_revision !== binding?.document_revision ||
      data.operation !== "update" ||
      data.status !== "ok" ||
      typeof data.version !== "string"
    )
      return;
    if (data.state === "none") changed({ state: "none" });
    else if (data.state === "ready" || data.state === "updated")
      changed({ state: data.state, version: data.version });
  };
  const send = () => {
    if (binding) return;
    const current = currentBinding(host);
    if (!current) return;
    binding = current;
    post(host, "update", binding, requestId);
  };
  bridge.addEventListener("message", receive);
  host.addEventListener(READY_EVENT, send);
  send();
  return () => {
    bridge.removeEventListener("message", receive);
    host.removeEventListener(READY_EVENT, send);
  };
}

/** Closes the client and starts it again with the staged update. */
export function restartToUpdate(): boolean {
  const host = shellWindow();
  const binding = host && currentBinding(host);
  return host && binding
    ? post(host, "restart", binding, crypto.randomUUID())
    : false;
}

/** Opens kombify's own Homelab sign-in in the system browser; false where there is no shell. */
export function openAccountSignIn(): boolean {
  const host = shellWindow();
  const binding = host && currentBinding(host);
  return host && binding
    ? post(host, "open-account", binding, crypto.randomUUID())
    : false;
}
