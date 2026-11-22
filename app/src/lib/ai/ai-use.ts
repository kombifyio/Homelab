/**
 * The account's AI use choice (`aiUse` of AI Platform's account document
 * `GET /v1/ai/settings/account-design`, D-2026-11-10-05): the one record every
 * kombify app reads and none copies (workspace
 * `platform/standards/local-ai-runtimes.v1.md` rule 7). kombify Cloud owns
 * the control; Homelab Runtime only shows it.
 *
 * `null`: no Gateway configured (self-hosted), signed out, or the record could
 * not be read. A failed read never disables anything.
 */
export type AiUseChoice = "undecided" | "kombify" | "own" | "none";

const GATEWAY_API_BASE =
  (import.meta.env.VITE_GATEWAY_API_BASE as string | undefined)?.replace(
    /\/+$/,
    "",
  ) || "";
const ACCOUNT_DOCUMENT_PATH = "/v1/ai/settings/account-design";

export async function fetchAiUse(): Promise<AiUseChoice | null> {
  if (!GATEWAY_API_BASE || typeof window === "undefined") return null;
  let origin: string;
  try {
    origin = new URL(GATEWAY_API_BASE).origin;
  } catch {
    return null;
  }
  let token: string;
  try {
    const { getGatewayToken } = await import("#lib/auth/gateway-auth.js");
    token = await getGatewayToken();
  } catch {
    return null;
  }
  if (!token) return null;
  try {
    const response = await fetch(`${origin}${ACCOUNT_DOCUMENT_PATH}`, {
      headers: { accept: "application/json", authorization: `Bearer ${token}` },
    });
    if (!response.ok) return null;
    const choice = (
      (await response.json()) as { aiUse?: { choice?: unknown } } | null
    )?.aiUse?.choice;
    return choice === "kombify" || choice === "own" || choice === "none"
      ? choice
      : "undecided";
  } catch {
    return null;
  }
}
