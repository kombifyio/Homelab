import { createHash } from "node:crypto";
import { mkdirSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";

import { expect, test, type Browser } from "@playwright/test";

import {
  authenticateRuntimeUser,
  captureGatewayApiSession,
  type RuntimeFixturePrefix,
} from "./runtime-auth";
import {
  classifyPublicAnswer,
  denialCode,
  hostOf,
  parseJSON,
  relayBaseName,
  safeToken,
} from "./guard-tenant-isolation-rules";

// Live Guard tenant isolation (kombify-StackKits-4evy; the two live-product
// steps of harness scenario k-cross-tenant). Two shared test customers, each
// owning one registered Guard, sign in to the live product. Each one reads its
// own Guard through the Gateway Connect registry and the kombify.me relay
// facts, then asks both for the other customer's Guard. Nothing is written:
// the two registry calls send an empty body with a customer bearer and must be
// refused before they act.
//
// Opt-in only (HOMELAB_GUARD_TENANT_ISOLATION=1, set by guard-window.yml job
// g13-live-tenant-isolation): runtime-e2e-gate.mjs runs this Playwright config
// without a spec filter in lanes that do not hold the second customer.
//
// The evidence names identifiers only as short SHA-256 digests. Guard-window
// proof (scripts/test-window/guard-window-proof.mjs live) turns it into the
// suite receipt of homelab-guard-live-tenant-isolation.

const FIXTURES = ["TEST_RIL_PRO_USER", "TEST_PRO_USER"] as const satisfies readonly RuntimeFixturePrefix[];
type Fixture = (typeof FIXTURES)[number];

const PRODUCT_BASE = liveURL(
  process.env.HOMELAB_RUNTIME_E2E_PRODUCT_URL ||
    process.env.HOMELAB_E2E_PRODUCT_URL ||
    "https://homelab.kombify.io",
);
const GATEWAY_ORIGIN = liveURL(process.env.HOMELAB_E2E_GATEWAY_ORIGIN || "https://api.kombify.io");
const EVIDENCE = resolve(
  process.env.RUNTIME_E2E_ARTIFACTS_DIR ?? "../artifacts/runtime-e2e",
  "guard-tenant-isolation-receipt.json",
);
const SCHEMA = "homelab.guard-tenant-isolation/v1";

// Relay facts of a server the product publishes at kombify.me
// (pkg/guardreach View): enabled by its owner, registered at kombify.me and
// connected by its Guard.
const RELAY_READY = { kombifyMe: "registered", relay: "connected" } as const;
interface GuardNode {
  node_id: string;
  server_id: string;
  worker_id: string;
  stack_id: string;
  display_name: string;
}

interface Actor {
  fixture: Fixture;
  token: string;
  homelabBase: string;
  subject: string;
  tenant: string;
  node?: GuardNode;
  address?: string;
  reach?: Record<string, unknown>;
}

interface Check {
  id: string;
  case: "connect" | "relay";
  actor: Fixture;
  target: "own" | "foreign";
  expected: string;
  observed: string;
  status: "passed" | "failed" | "blocked";
}

test("a Guard is listed and relayed only inside its owner's tenant", async ({ browser }) => {
  test.skip(process.env.HOMELAB_GUARD_TENANT_ISOLATION !== "1", "the live Guard tenant isolation lane is not selected");
  test.setTimeout(600_000);
  const actors: Actor[] = [];
  for (const fixture of FIXTURES) actors.push(await signIn(browser, fixture));
  const [a, b] = actors as [Actor, Actor];

  const checks: Check[] = [];
  const leaks: string[] = [];
  const record = (check: Check) => {
    checks.push(check);
    return check.status === "passed";
  };

  // Identities first: two distinct customers in two distinct tenants.
  const distinct = a.subject !== b.subject && a.tenant !== b.tenant;
  for (const actor of actors) {
    record({
      id: "distinct-customers", case: "connect", actor: actor.fixture, target: "own",
      expected: "the two fixtures are different subjects in different tenants",
      observed: distinct ? "distinct subject and tenant" : "the fixtures share a subject or a tenant",
      status: distinct ? "passed" : "failed",
    });
  }

  // Each customer's own Guard, as the Connect registry lists it.
  for (const actor of actors) {
    const listing = await gatewayJSON(actor, "GET", "/v1/connect/devices?include=homelab-nodes");
    const nodes = guardNodes(listing.body);
    const ok = listing.status === 200 && listing.body?.scope === "subject" && nodes.length === 1;
    if (ok) actor.node = nodes[0];
    record({
      id: "connect-own-listing", case: "connect", actor: actor.fixture, target: "own",
      expected: "200, subject scope, exactly one active Guard node",
      observed: `${listing.status}, scope ${String(listing.body?.scope)}, ${nodes.length} active Guard node(s)`,
      status: ok ? "passed" : "blocked",
    });
  }

  if (a.node && b.node) {
    // The relay facts each owner reads for its own Guard.
    for (const actor of actors) {
      const own = await gatewayJSON(actor, "GET", `${actor.homelabBase}/servers/${encodeURIComponent(actor.node!.server_id)}`);
      actor.reach = (own.body?.data?.reach ?? {}) as Record<string, unknown>;
      actor.address = relayAddress(actor.reach);
    }
    if (relayReady(a.reach) && relayReady(b.reach)) {
      const distinctRelay = hostOf(a.address!) !== hostOf(b.address!);
      for (const actor of actors) {
        record({
          id: "relay-distinct-addresses", case: "relay", actor: actor.fixture, target: "own",
          expected: "each customer's Guard has its own kombify.me address",
          observed: distinctRelay ? "distinct addresses" : "both Guards report the same address",
          status: distinctRelay ? "passed" : "failed",
        });
      }
    }
    for (const [actor, victim] of [[a, b], [b, a]] as const) {
      const foreign = victimIdentifiers(actor, victim).map((value) => value.toLowerCase());
      const leakIn = (label: string, text: string) => {
        const haystack = text.toLowerCase();
        const found = foreign.filter((value) => haystack.includes(value)).length;
        if (found) leaks.push(`${actor.fixture} ${label}: ${found} identifier(s) of ${victim.fixture}`);
        return found;
      };
      await runConnectChecks(actor, victim, leakIn, record);
      await runRelayChecks(actor, victim, leakIn, record);
    }
  }

  const caseStatus = (name: Check["case"]) => {
    const rows = checks.filter((check) => check.case === name);
    if (rows.some((check) => check.status === "failed")) return "failed";
    if (rows.length === 0 || rows.some((check) => check.status === "blocked")) return "blocked";
    return "passed";
  };
  const cases = {
    connect: { status: leaks.length ? "failed" : caseStatus("connect") },
    relay: { status: leaks.length ? "failed" : caseStatus("relay"), ...relayPrecondition(actors) },
  };
  const evidence = {
    schemaVersion: SCHEMA,
    status: cases.connect.status === "passed" && cases.relay.status === "passed" && leaks.length === 0 ? "passed" : "failed",
    testSourceCommit: process.env.HOMELAB_TEST_SOURCE_COMMIT ?? "",
    observedAt: new Date().toISOString(),
    product: PRODUCT_BASE,
    gateway: GATEWAY_ORIGIN,
    writes: "none",
    actors: actors.map((actor) => ({
      fixture: actor.fixture,
      subject: digest(actor.subject),
      tenant: digest(actor.tenant),
      node: actor.node ? digest(actor.node.node_id) : null,
      server: actor.node ? digest(actor.node.server_id) : null,
      relay_address: actor.address ? digest(hostOf(actor.address)) : null,
    })),
    cases,
    checks,
    leaks,
    limitations: [
      "Guard-plane relay admission with a foreign binding (Guard A's host proof naming Guard B's tenant and server) needs Guard A's private key, which never leaves its host; this journey reads the relay from the owner, Gateway and public side.",
    ],
  };
  mkdirSync(dirname(EVIDENCE), { recursive: true });
  writeFileSync(EVIDENCE, `${JSON.stringify(evidence, null, 2)}\n`);
  console.log(`[guard-tenant-isolation] ${JSON.stringify({ status: evidence.status, cases, leaks })}`);
  for (const check of checks) console.log(`[guard-tenant-isolation] ${check.status} ${check.actor} ${check.id}: ${check.observed}`);

  expect(leaks, "no response of one customer carries the other customer's Guard").toEqual([]);
  expect(cases).toEqual({ connect: { status: "passed" }, relay: expect.objectContaining({ status: "passed" }) });
});

type LeakIn = (label: string, text: string) => number;
type RecordCheck = (check: Check) => boolean;

async function runConnectChecks(actor: Actor, victim: Actor, leakIn: LeakIn, record: RecordCheck) {
  const own = actor.node!;
  const target = victim.node!;
  const base = { case: "connect" as const, actor: actor.fixture, target: "foreign" as const };

  // A foreign id in any parameter the listing might read never widens it.
  const query = new URLSearchParams({
    include: "homelab-nodes",
    node_id: target.node_id,
    server_id: target.server_id,
    worker_id: target.worker_id,
    owner_subject_id: victim.subject,
    tenant_id: victim.tenant,
    org_id: victim.tenant,
  });
  const listed = await gatewayJSON(actor, "GET", `/v1/connect/devices?${query}`);
  const nodes = guardNodes(listed.body);
  const leaked = leakIn("Connect listing with foreign parameters", listed.text);
  record({
    ...base, id: "connect-foreign-parameters",
    expected: "200; exactly the actor's own Guard; no foreign identifier",
    observed: `${listed.status}; ${nodes.length} node(s), own ${nodes.length === 1 && nodes[0]!.node_id === own.node_id}; ${leaked} foreign identifier(s)`,
    status: listed.status === 200 && nodes.length === 1 && nodes[0]!.node_id === own.node_id && leaked === 0 ? "passed" : "failed",
  });

  const plain = await gatewayJSON(actor, "GET", "/v1/connect/devices?include=homelab-nodes");
  const plainLeaked = leakIn("Connect listing", plain.text);
  record({
    ...base, id: "connect-listing-excludes-foreign",
    expected: "200; the listing carries no identifier of the other customer's Guard",
    observed: `${plain.status}; ${plainLeaked} foreign identifier(s)`,
    status: plain.status === 200 && plainLeaked === 0 ? "passed" : "failed",
  });

  const server = await gatewayJSON(actor, "GET", `${actor.homelabBase}/servers/${encodeURIComponent(target.server_id)}`);
  const serverLeaked = leakIn("foreign server read", server.text);
  record({
    ...base, id: "server-foreign-read",
    expected: "404 NOT_FOUND; no foreign identifier",
    observed: `${server.status} ${String(server.body?.error?.code ?? "")}; ${serverLeaked} foreign identifier(s)`,
    status: server.status === 404 && server.body?.error?.code === "NOT_FOUND" && serverLeaked === 0 ? "passed" : "failed",
  });

  const servers = await gatewayJSON(actor, "GET", `${actor.homelabBase}/servers`);
  const serversLeaked = leakIn("server list", servers.text);
  record({
    ...base, id: "server-list-excludes-foreign",
    expected: "200; no foreign identifier",
    observed: `${servers.status}; ${serversLeaked} foreign identifier(s)`,
    status: servers.status === 200 && serversLeaked === 0 ? "passed" : "failed",
  });

  if (target.stack_id) {
    const stack = await gatewayJSON(actor, "GET", `${actor.homelabBase}/stacks/${encodeURIComponent(target.stack_id)}`);
    const stackLeaked = leakIn("foreign stack read", stack.text);
    record({
      ...base, id: "stack-foreign-read",
      expected: "404 NOT_FOUND; no foreign identifier",
      observed: `${stack.status} ${String(stack.body?.error?.code ?? "")}; ${stackLeaked} foreign identifier(s)`,
      status: stack.status === 404 && stack.body?.error?.code === "NOT_FOUND" && stackLeaked === 0 ? "passed" : "failed",
    });
  }

  // The registry accepts registrations only with Homelab's service identity.
  // The body is empty, so even a broken boundary has nothing to register.
  const registry = "/v1/internal/connect/homelab-nodes";
  const put = await gatewayJSON(actor, "PUT", registry, "{}");
  record({
    ...base, id: "registry-put-refused",
    expected: "401: a customer bearer is no service identity",
    observed: `${put.status} ${denialCode(put.body)}`,
    status: put.status === 401 ? "passed" : "failed",
  });
  // The revoke route answers POST, so its refusal is the service-auth check
  // itself, not a method check that runs first.
  const revoke = await gatewayJSON(actor, "POST", `${registry}/revoke`, "{}");
  record({
    ...base, id: "registry-revoke-refused",
    expected: "401: a customer bearer is no service identity",
    observed: `${revoke.status} ${denialCode(revoke.body)}`,
    status: revoke.status === 401 ? "passed" : "failed",
  });
  // The revocation log names every tenant's servers to Homelab; a customer reads none of it.
  const revocations = await gatewayJSON(actor, "GET", `${registry}/revocations`);
  const revocationsLeaked = leakIn("registry revocation log", revocations.text);
  record({
    ...base, id: "registry-revocations-refused",
    expected: "401; no foreign identifier",
    observed: `${revocations.status} ${denialCode(revocations.body)}; ${revocationsLeaked} foreign identifier(s)`,
    status: revocations.status === 401 && revocationsLeaked === 0 ? "passed" : "failed",
  });
}

async function runRelayChecks(actor: Actor, victim: Actor, leakIn: LeakIn, record: RecordCheck) {
  const base = { case: "relay" as const, actor: actor.fixture };
  const ready = relayReady(actor.reach);
  record({
    ...base, target: "own", id: "relay-own-reach",
    expected: `public reach enabled, kombify.me ${RELAY_READY.kombifyMe}, relay ${RELAY_READY.relay}, an address`,
    observed: reachSummary(actor.reach),
    status: ready ? "passed" : "blocked",
  });
  if (!ready || !relayReady(victim.reach)) return;

  // The owner's own address answers through its own Guard's relay.
  const own = await publicFetch(actor.address!);
  record({
    ...base, target: "own", id: "relay-own-round-trip",
    expected: "an HTTPS answer from the Guard's upstream, not a relay or routing error",
    observed: own.summary,
    status: own.relayed ? "passed" : "failed",
  });

  // The other customer's reach base is no base of this customer. kombify.me
  // answers another owner's base exactly like an unknown name
  // (services/me/worker/src/reach/api.ts: 404 base_not_found). Its earlier
  // refusals (deployment not configured, caller not a verified Me account) run
  // before the ownership comparison, so they prove nothing and block the case.
  const victimBase = relayBaseName(victim.address!, victim.reach);
  if (!victimBase) {
    record({
      ...base, target: "foreign", id: "relay-foreign-reach-read",
      expected: "the other Guard's reach base is derivable from its address and published service",
      observed: "the address does not carry a base of the published service",
      status: "blocked",
    });
  } else {
    const reach = await gatewayJSON(actor, "GET", `/v1/subdomains/reach/${encodeURIComponent(victimBase)}/tunnel`);
    const reachLeaked = leakIn("foreign reach read", reach.text);
    const code = denialCode(reach.body);
    const unobservable = (reach.status === 503 && code === "reach_unconfigured")
      || (reach.status === 403 && code === "account_not_verified");
    record({
      ...base, target: "foreign", id: "relay-foreign-reach-read",
      expected: "404 base_not_found (another owner's base answers like an unknown one); no foreign identifier",
      observed: `${reach.status} ${code}; ${reachLeaked} foreign identifier(s)`,
      status: reach.status === 404 && code === "base_not_found" && reachLeaked === 0 ? "passed"
        : unobservable && reachLeaked === 0 ? "blocked" : "failed",
    });
  }

  // The actor's own kombify.me routes name nothing of the other customer.
  const subdomains = await gatewayJSON(actor, "GET", "/v1/subdomains");
  const subdomainsLeaked = leakIn("kombify.me route list", subdomains.text);
  record({
    ...base, target: "foreign", id: "relay-subdomain-list-excludes-foreign",
    expected: "200 list; no route, base or address of the other customer's Guard",
    observed: `${subdomains.status}; ${Array.isArray(subdomains.body) ? subdomains.body.length : "no"} route(s); ${subdomainsLeaked} foreign identifier(s)`,
    status: subdomains.status === 200 && Array.isArray(subdomains.body) && subdomainsLeaked === 0 ? "passed" : "failed",
  });

  // Nothing the actor reads about its own Guard names the other relay.
  const ownServer = await gatewayJSON(actor, "GET", `${actor.homelabBase}/servers/${encodeURIComponent(actor.node!.server_id)}`);
  const ownLeaked = leakIn("own server read", ownServer.text);
  record({
    ...base, target: "foreign", id: "relay-own-facts-exclude-foreign",
    expected: "200; the own Guard's reach facts carry no identifier or address of the other Guard",
    observed: `${ownServer.status}; ${ownLeaked} foreign identifier(s)`,
    status: ownServer.status === 200 && ownLeaked === 0 ? "passed" : "failed",
  });
}

async function signIn(browser: Browser, fixture: Fixture): Promise<Actor> {
  const context = await browser.newContext({ baseURL: PRODUCT_BASE });
  try {
    const page = await context.newPage();
    await authenticateRuntimeUser("cloud", page, {
      productBase: PRODUCT_BASE,
      apiBase: PRODUCT_BASE,
      sessionCookieName: process.env.HOMELAB_E2E_SESSION_COOKIE_NAME ?? "homelab_session",
      fixturePrefix: fixture,
    });
    const session = await captureGatewayApiSession(page);
    const origin = new URL(session.apiBase).origin;
    if (origin !== GATEWAY_ORIGIN) {
      throw new Error(`${fixture} signed in against ${origin}, not the live Gateway ${GATEWAY_ORIGIN}`);
    }
    const claims = jwtClaims(session.token);
    const subject = typeof claims.sub === "string" ? claims.sub : "";
    if (!subject) throw new Error(`${fixture}'s Gateway bearer names no subject`);
    const org = ["https://kombify.io/org_id", "org_id", "tenant_id", "organization_id"]
      .map((name) => claims[name])
      .find((value): value is string => typeof value === "string" && value !== "");
    return { fixture, token: session.token, homelabBase: new URL(session.apiBase).pathname, subject, tenant: org ?? `usr:${subject}` };
  } finally {
    await context.close();
  }
}

async function gatewayJSON(actor: Actor, method: string, path: string, body?: string) {
  const response = await fetch(`${GATEWAY_ORIGIN}${path}`, {
    method,
    headers: {
      Accept: "application/json",
      Authorization: `Bearer ${actor.token}`,
      ...(body === undefined ? {} : { "Content-Type": "application/json" }),
    },
    body,
    redirect: "manual",
    signal: AbortSignal.timeout(30_000),
  });
  const text = await response.text();
  return { status: response.status, text, body: parseJSON(text) };
}

// A public HTTPS request to a kombify.me address, read by classifyPublicAnswer.
async function publicFetch(address: string) {
  const host = hostOf(address);
  try {
    const response = await fetch(`https://${host}/`, { redirect: "manual", signal: AbortSignal.timeout(30_000) });
    return classifyPublicAnswer(host, response.status, response.headers.get("location"), await response.text());
  } catch (error) {
    return { relayed: false, summary: `no answer: ${safeToken((error as Error).name)}` };
  }
}

function guardNodes(body: any): GuardNode[] {
  const resources: any[] = Array.isArray(body?.resources) ? body.resources : [];
  return resources
    .filter((row) => row?.kind === "homelab-node" && row?.status === "active" && typeof row?.node_id === "string"
      && row.node_id !== "" && typeof row?.server_id === "string" && row.server_id !== "")
    .map((row) => ({
      node_id: String(row.node_id),
      server_id: String(row.server_id ?? ""),
      worker_id: String(row.worker_id ?? ""),
      stack_id: String(row.stack_id ?? ""),
      display_name: String(row.display_name ?? ""),
    }));
}

// What would identify the victim's Guard in a response to the actor. A display
// name counts only when it is distinctive and differs from the actor's own.
function victimIdentifiers(actor: Actor, victim: Actor): string[] {
  const node = victim.node!;
  const values = [node.node_id, node.server_id, node.worker_id, node.stack_id, victim.subject];
  if (!victim.tenant.startsWith("usr:")) values.push(victim.tenant);
  if (node.display_name.length >= 8 && node.display_name !== actor.node?.display_name) values.push(node.display_name);
  if (victim.address) {
    const host = hostOf(victim.address);
    values.push(host, host.split(".")[0] ?? "");
    const base = relayBaseName(victim.address, victim.reach);
    if (base) values.push(base);
  }
  return [...new Set(values.filter((value) => value.length >= 6))];
}

function relayAddress(reach: Record<string, any> | undefined): string | undefined {
  const address = reach?.kombify_me?.address;
  return typeof address === "string" && address.trim() ? address.trim() : undefined;
}

function relayReady(reach: Record<string, any> | undefined): boolean {
  return reach?.public?.enabled === true && reach?.kombify_me?.state === RELAY_READY.kombifyMe
    && reach?.relay?.state === RELAY_READY.relay && Boolean(relayAddress(reach));
}

function reachSummary(reach: Record<string, any> | undefined): string {
  return [
    `public.enabled=${String(reach?.public?.enabled)}`,
    `kombify_me.state=${String(reach?.kombify_me?.state)}`,
    reach?.kombify_me?.reason_code ? `reason_code=${String(reach.kombify_me.reason_code)}` : "",
    `relay.state=${String(reach?.relay?.state)}`,
    `address=${relayAddress(reach) ? "present" : "absent"}`,
  ].filter(Boolean).join(", ");
}

function relayPrecondition(actors: Actor[]) {
  const missing = actors.filter((actor) => !relayReady(actor.reach));
  if (!missing.length) return {};
  return {
    reason: "relay_fixture_not_ready",
    detail: missing.map((actor) => `${actor.fixture}: ${reachSummary(actor.reach)}`).join("; "),
  };
}

function jwtClaims(token: string): Record<string, unknown> {
  try {
    return JSON.parse(Buffer.from(token.split(".")[1] ?? "", "base64url").toString("utf8"));
  } catch {
    return {};
  }
}

function digest(value: string): string {
  return createHash("sha256").update(value).digest("hex").slice(0, 16);
}

function liveURL(value: string): string {
  const parsed = new URL(value.trim());
  const host = parsed.hostname.toLowerCase();
  if (parsed.protocol !== "https:" || ["localhost", "127.0.0.1", "::1"].includes(host)) {
    throw new Error(`Live Guard tenant isolation must target a live origin over HTTPS, got ${value}`);
  }
  return parsed.origin;
}
