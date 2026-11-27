// Pure rules of the live Guard tenant isolation journey
// (guard-tenant-isolation.spec.ts): how an answer of kombify.me is read. Kept
// apart from the spec so they can be read and exercised without a browser.

export const ME_ZONE = ".kombify.me";

// kombify.me's own refusals on the public plane (services/me/worker/src
// index.ts, tunnel-relay.ts, abuse/takedown.ts, service-access/browser.ts):
// such an answer never came through a Guard.
export const ME_EDGE_REFUSALS = new Set([
  "subdomain not found", "custom domain not found", "subdomain publication expired",
  "service subdomain not exposed", "subdomain inactive", "relay route not found",
  "invalid relay identity", "session authority required", "relay binding generation is no longer current",
  "tunnel session projection unavailable", "websocket upgrade required", "verified active account required",
  "subdomain disabled by the kombify.me operator", "subdomain_taken_down", "tunnel_mode_host_mismatch",
  "reach_base_unavailable", "reach_authority_unavailable", "relay_dispatch_unavailable",
  "service_not_found", "temporary_service_access_expired", "named_service_target_transport_unsupported",
  "service_host_mismatch", "service_origin_mismatch", "browser_challenge_invalid",
  "browser_ticket_invalid_or_revoked", "service_access_authority_unavailable", "internal_server_error",
]);

// The keys of a kombify.me error body; an upstream 4xx with only these keys is
// read as a refusal, so an ambiguous answer fails closed.
export const ME_ERROR_KEYS = new Set(["error", "error_code", "reason_code", "retryable"]);

export function hostOf(address: string): string {
  return address.trim().toLowerCase().replace(/^https?:\/\//, "").replace(/[/?#].*$/, "").replace(/\.$/, "");
}

// The reach base of a Guard's kombify.me route (pkg/guardreach me.go
// EnsureRoute, state.go routeServiceName): a flat route is
// `<base>-<service>-<4 hex>.kombify.me`, an install-zone route
// `<service>.<base>.kombify.me`. Anything else names no derivable base.
export function relayBaseName(address: string, reach: Record<string, any> | undefined): string | undefined {
  const host = hostOf(address);
  if (!host.endsWith(ME_ZONE)) return undefined;
  const labels = host.slice(0, -ME_ZONE.length).split(".");
  const service = typeof reach?.public?.service === "string" ? reach.public.service.trim().toLowerCase() : "";
  let base: string | undefined;
  if (labels.length === 1 && service) {
    const suffix = new RegExp(`^(.+)-${service.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}-[0-9a-f]{4}$`);
    base = suffix.exec(labels[0]!)?.[1];
  } else if (labels.length === 2) {
    base = labels[1];
  }
  return base && /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(base) ? base : undefined;
}

// A denial code from an upstream body, reduced to one short safe token: it
// lands in the receipt and in workflow annotations.
export function denialCode(body: any): string {
  const value = body?.reason_code ?? body?.error_code ?? body?.error?.code ?? body?.error;
  return typeof value === "string" ? safeToken(value) : "";
}

export function safeToken(value: string): string {
  return value.replace(/[^A-Za-z0-9 _.-]/g, "?").replace(/\s+/g, " ").trim().slice(0, 64);
}

export function parseJSON(text: string): any {
  try {
    return text ? JSON.parse(text) : null;
  } catch {
    return null;
  }
}

// One public answer from a kombify.me address. It came through the Guard only
// when it is below 500, is no kombify.me refusal (a known edge message or code,
// or a 4xx body made only of error keys) and is no redirect off the address's
// own host (the service Access Gate sends a browser to app.kombify.io).
export function classifyPublicAnswer(host: string, status: number, location: string | null, text: string) {
  const body = parseJSON(text);
  const messages = body && typeof body === "object" && !Array.isArray(body)
    ? [body.error, body.error_code, body.reason_code].filter((value): value is string => typeof value === "string")
    : [];
  const edgeRefusal = messages.some((value) => ME_EDGE_REFUSALS.has(value.trim().toLowerCase()))
    || (status >= 400 && messages.length > 0 && Object.keys(body).every((key) => ME_ERROR_KEYS.has(key)));
  let offHost = false;
  if (status >= 300 && status < 400) {
    try {
      offHost = new URL(location ?? "", `https://${host}/`).hostname.toLowerCase() !== host;
    } catch {
      offHost = true;
    }
  }
  const code = denialCode(body);
  return {
    relayed: status < 500 && !edgeRefusal && !offHost,
    summary: `${status}${code ? ` ${code}` : ""}${offHost ? " redirect off the address" : ""}`,
  };
}
