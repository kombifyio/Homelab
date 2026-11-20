/**
 * Creation drafts (KOMBIFY-DESIGN-SYSTEM-STANDARD.md §9): where the signed-in
 * owner stands in one creation process, kept by Homelab so every frame and
 * device continues it. The server refuses any field that carries a secret.
 */
import { fetchApi } from "./client";

export type CreationDraftStep =
  "goals" | "server" | "access" | "users" | "login";

export interface CreationDraftRecord {
  key: string;
  process: string;
  step: CreationDraftStep;
  furthest: CreationDraftStep;
  answers: Record<string, unknown>;
  expires_at: string;
  updated_at: string;
}

export interface CreationDraftWrite {
  process: string;
  step: CreationDraftStep;
  furthest: CreationDraftStep;
  answers: Record<string, unknown>;
}

const draftPath = (key: string) =>
  `/api/v1/creation/drafts/${encodeURIComponent(key)}`;

export async function getCreationDraft(
  key: string,
): Promise<CreationDraftRecord | null> {
  const res = await fetchApi<{ draft: CreationDraftRecord | null }>(
    draftPath(key),
  );
  return res.data?.draft ?? null;
}

export async function putCreationDraft(
  key: string,
  draft: CreationDraftWrite,
): Promise<CreationDraftRecord | null> {
  const res = await fetchApi<{ draft: CreationDraftRecord | null }>(
    draftPath(key),
    { method: "PUT", body: JSON.stringify(draft) },
  );
  return res.data?.draft ?? null;
}

export async function discardCreationDraft(key: string): Promise<void> {
  await fetchApi<{ draft: null }>(draftPath(key), { method: "DELETE" });
}
