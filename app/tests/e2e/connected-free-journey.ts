/**
 * Connected free journey (kombify-StackKits-qqec): the four runtime steps of
 * Journey C (Connected free) of the workspace public-beta journey contract.
 *
 *   existing-runtime-connect  a StackKits node that already runs local-only
 *                             (the public Basement one-line installer, Standard
 *                             Mode, no kombify account) connects as the shared
 *                             Free test user through the install command, with
 *                             no reinstall and its workload unchanged
 *   cloud-enrollment          its Guard enrolls and the server reads back
 *                             connected in Cloud
 *   monitoring-read           Cloud reads that server's health and Guard
 *                             telemetry through the product API: a fresh and
 *                             advancing heartbeat and the monitor cockpit row
 *   disconnect-rollback       the owner detaches the server: Cloud reads it back
 *                             as removed and no revoked heartbeat revives it,
 *                             the Cloud stack record is removed, and the node
 *                             keeps its StackKits install and workload, local-only
 *
 * The heartbeat windows are the product contract (pkg/runtimehealth): fresh up
 * to 90 s after the last accepted heartbeat, stale up to 300 s, offline beyond.
 * Each bound counts on the control plane's own clock (connection
 * staleness_seconds and last_heartbeat_at), never the runner's, plus one poll
 * interval and the whole-second staleness as resolution, as the Guard
 * disconnect journey (c4kv) measures them.
 *
 * guard-window.yml (job c5-connected-free) validates the receipt with
 * scripts/test-window/guard-window-proof.mjs; the Journey C lane of Workbench
 * Desktop reads its checks[] as step evidence observed as identity "free".
 */
import { expect } from "@playwright/test";

type Json = Record<string, any>;

export const CONNECTED_FREE_SCHEMA = "homelab.connected-free/v1";
export const CONNECTED_FREE_STEPS = [
  "existing-runtime-connect",
  "cloud-enrollment",
  "monitoring-read",
  "disconnect-rollback",
] as const;

/** pkg/runtimehealth FreshHeartbeatWindow and StaleHeartbeatWindow. */
export const FRESH_HEARTBEAT_SECONDS = 90;
export const OFFLINE_HEARTBEAT_SECONDS = 300;
const POLL_MS = 2_000;
export const RESOLUTION_SECONDS = POLL_MS / 1000 + 3;

/** The workspace the public installer and the Guard's access manifest share. */
const HOMELAB_DIR = "/root/my-homelab";
const INSTALL_UNIT = "connected-free-local-install";
const INSTALL_BUDGET_MS = 45 * 60_000;
const NODE_PATH =
  "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/root/.local/bin";

export interface ConnectedFreeDeps {
  /** Runs a command on the server as root and returns its stdout. */
  root: (command: string, timeout?: number) => Promise<string>;
  /** The Free owner's email: the node's local Owner, never written to evidence. */
  ownerEmail: string;
  /** Creates the install-command run, enrolls the server with it and waits for Connected. */
  connect: () => Promise<{
    stackId: string;
    serverId: string;
    workerId: string;
  }>;
  /** GET /api/v1/servers/{id}; null when the product answers 404. */
  readServer: (serverId: string) => Promise<Json | null>;
  /** GET /api/v1/servers, every server of the owner. */
  listServers: () => Promise<Json[]>;
  /** GET on the owner API, returning its data. */
  get: (apiPath: string) => Promise<Json>;
  /** POST /api/v1/servers/{id}/detach; returns its receipt. */
  detach: (serverId: string) => Promise<Json>;
  /** Removes the Cloud stack record and waits until the owner no longer lists it. */
  destroyStack: (stackId: string) => Promise<void>;
  shellQuote: (value: string) => string;
  delay: (ms: number) => Promise<unknown>;
}

interface NodeFacts {
  stackkitPath: string;
  stackkitSha256: string;
  stackkitVersion: string;
  machineIdSha256: string;
  bootId: string;
}

const seconds = (value: unknown) => Date.parse(String(value ?? "")) / 1000;

async function nodeFacts(deps: ConnectedFreeDeps): Promise<NodeFacts> {
  // One key=value line per fact, so a fact that prints nothing shifts no other.
  const out = await deps.root(
    `export ${NODE_PATH}; bin="$(command -v stackkit)"; echo "path=$bin"; ` +
      `echo "sha256=$(sha256sum "$bin" | cut -d' ' -f1)"; echo "version=$(stackkit version 2>&1 | head -n 1)"; ` +
      `echo "machine=$(sha256sum /etc/machine-id | cut -d' ' -f1)"; echo "boot=$(cat /proc/sys/kernel/random/boot_id)"`,
  );
  const fact = (key: string) =>
    (out.split("\n").find((line) => line.startsWith(`${key}=`)) ?? "")
      .slice(key.length + 1)
      .trim();
  const facts = {
    stackkitPath: fact("path"),
    stackkitSha256: fact("sha256"),
    stackkitVersion: fact("version"),
    machineIdSha256: fact("machine"),
    bootId: fact("boot"),
  };
  expect(
    facts.stackkitPath,
    "the node has the StackKits CLI installed",
  ).not.toBe("");
  expect(facts.stackkitSha256).toMatch(/^[a-f0-9]{64}$/);
  return facts;
}

/** Every StackKits Compose container: name, id, start time and restart count. */
async function workload(deps: ConnectedFreeDeps): Promise<Map<string, string>> {
  const out = await deps.root(
    "docker ps -q --filter label=com.docker.compose.project | xargs -r docker inspect " +
      "-f '{{.Name}} {{.Id}} {{.State.StartedAt}} {{.RestartCount}}'",
  );
  return new Map(
    out
      .split("\n")
      .filter(Boolean)
      .map((line) => [line.split(" ")[0] ?? "", line] as [string, string]),
  );
}

function expectWorkloadUnchanged(
  before: Map<string, string>,
  after: Map<string, string>,
  when: string,
) {
  for (const [name, line] of before) {
    expect(
      after.get(name),
      `${when}: container ${name} keeps its id, start time and restart count`,
    ).toBe(line);
  }
}

async function localVerify(deps: ConnectedFreeDeps, when: string) {
  await deps
    .root(
      `export ${NODE_PATH}; cd ${HOMELAB_DIR} && stackkit verify --json >/dev/null`,
      300_000,
    )
    .catch((error) => {
      throw new Error(
        `${when}: the node's own stackkit verify failed: ${String(error).slice(0, 1200)}`,
      );
    });
}

async function guardService(deps: ConnectedFreeDeps) {
  return (
    await deps.root("systemctl is-active techstack-agent.service || true")
  ).trim();
}

/**
 * The node runs the public Basement one-line installer (Standard Mode, core
 * only) detached from the SSH session: its Apply reloads sshd and the firewall.
 */
async function installLocalOnlyNode(deps: ConnectedFreeDeps) {
  const env = [
    "HOME=/root",
    "STACKKIT_INSTALL_MODE=auto",
    `HOMELAB_DIR=${HOMELAB_DIR}`,
    `STACKKIT_ADMIN_EMAIL=${deps.ownerEmail}`,
    "STACKKIT_USE_CASES=",
  ].map((pair) => `--setenv=${deps.shellQuote(pair)}`);
  // --no-block: a oneshot start job otherwise lasts the whole install. The
  // owner email is on this command line, so a failure names it redacted.
  await deps
    .root(
      [
        "systemd-run",
        "--no-block",
        `--unit=${INSTALL_UNIT}`,
        "--property=Type=oneshot",
        "--property=RemainAfterExit=yes",
        "--working-directory=/root",
        ...env,
        "sh -c 'curl -fsSL https://base.stackkit.cc | sh'",
      ].join(" "),
    )
    .catch((error) => {
      throw new Error(
        `The Basement installer unit did not start: ${String(error).split(deps.ownerEmail).join("<owner>").slice(0, 1200)}`,
      );
    });
  const deadline = Date.now() + INSTALL_BUDGET_MS;
  let state = "";
  while (Date.now() < deadline) {
    // A connection refused while sshd reloads is retried like any poll.
    state = await deps
      .root(`systemctl show -p ActiveState --value ${INSTALL_UNIT}`, 30_000)
      .then((out) => out.trim())
      .catch(() => "unreachable");
    if (state === "active" || state === "failed") break;
    await deps.delay(20_000);
  }
  if (state !== "active") {
    // The installer prints the initial admin access; only lines without it are named.
    const tail = await deps
      .root(
        `journalctl -u ${INSTALL_UNIT} -n 60 --no-pager -o cat | grep -viE 'pass|secret|token|credential|key|@' || true`,
      )
      .catch(() => "");
    throw new Error(
      `The Basement one-line installer did not complete (unit ${state || "unknown"}): ${tail.slice(-2000)}`,
    );
  }
}

/** Polls the server until accept holds, bounded by its heartbeat silence on the control plane. */
async function waitForServer(
  deps: ConnectedFreeDeps,
  serverId: string,
  accept: (server: Json) => boolean,
  what: string,
  limitSeconds: number,
): Promise<Json> {
  const runnerCap = Date.now() + (limitSeconds + 120) * 1000;
  let last: Json | null = null;
  while (Date.now() < runnerCap) {
    last = await deps.readServer(serverId);
    if (last && accept(last)) return last;
    await deps.delay(POLL_MS);
  }
  throw new Error(
    `The server did not show ${what} within ${limitSeconds}s; last=${JSON.stringify(last?.connection ?? null)}`,
  );
}

export async function runConnectedFreeJourney(
  deps: ConnectedFreeDeps,
): Promise<Json> {
  const checks: Json[] = [];

  // Precondition: a local-only StackKits node with a running workload and no Guard.
  await installLocalOnlyNode(deps);
  const factsBefore = await nodeFacts(deps);
  const workloadBefore = await workload(deps);
  expect(
    workloadBefore.size,
    "the local-only node runs its StackKits workload",
  ).toBeGreaterThan(0);
  await localVerify(deps, "before connect");
  expect(
    (
      await deps.root(
        "test -e /etc/techstack/agent-enrollment.json && echo enrolled || echo none",
      )
    ).trim(),
    "the node is local-only before it connects",
  ).toBe("none");

  // existing-runtime-connect
  const { stackId, serverId, workerId } = await deps.connect();
  expect(workerId, "the connected server names its Guard").not.toBe("");
  const factsConnected = await nodeFacts(deps);
  expect(
    factsConnected,
    "connecting neither reinstalled StackKits nor rebooted the node",
  ).toEqual(factsBefore);
  expectWorkloadUnchanged(
    workloadBefore,
    await workload(deps),
    "after connect",
  );
  await localVerify(deps, "after connect");
  checks.push({
    id: "existing-runtime-connect",
    status: "passed",
    stack_id: stackId,
    server_id: serverId,
    worker_id: workerId,
    stackkit_version: factsBefore.stackkitVersion,
    stackkit_sha256: factsBefore.stackkitSha256,
    workload_containers: workloadBefore.size,
    workload_unchanged: true,
    reinstalled: false,
    rebooted: false,
  });

  // cloud-enrollment
  const enrolled = await waitForServer(
    deps,
    serverId,
    (server) => server.connection?.state === "connected",
    "connected",
    FRESH_HEARTBEAT_SECONDS,
  );
  expect(
    String(enrolled.worker_id ?? ""),
    "the server is bound to the enrolled Guard",
  ).toBe(workerId);
  const listed = (await deps.listServers()).filter(
    (server) => server.kit_deployment_id === stackId,
  );
  expect(
    listed.map((server) => String(server.id)),
    "Cloud lists exactly this server for the stack",
  ).toEqual([serverId]);
  const capabilities = enrolled.capabilities ?? {};
  expect(
    typeof capabilities.identity_binding,
    "the Guard's identity binding is persisted",
  ).toBe("string");
  expect(
    Number(capabilities.credential_generation),
    "the Guard's enrollment generation is persisted",
  ).toBeGreaterThanOrEqual(0);
  const guardConnected = await guardService(deps);
  expect(guardConnected, "the Guard service runs on the node").toBe("active");
  checks.push({
    id: "cloud-enrollment",
    status: "passed",
    server_listed: true,
    connection: "connected",
    lifecycle: String(enrolled.lifecycle?.state ?? ""),
    identity_binding: String(capabilities.identity_binding),
    generation: Number(capabilities.credential_generation),
    guard_service: guardConnected,
  });

  // monitoring-read: the heartbeat is fresh and advances within the fresh window.
  const fresh = (server: Json) =>
    server.connection?.state === "connected" &&
    Number.isFinite(Number(server.connection?.staleness_seconds)) &&
    Number(server.connection.staleness_seconds) <= FRESH_HEARTBEAT_SECONDS;
  const first = await waitForServer(
    deps,
    serverId,
    fresh,
    "a fresh heartbeat",
    FRESH_HEARTBEAT_SECONDS,
  );
  const firstBeat = seconds(first.connection.last_heartbeat_at);
  expect(Number.isFinite(firstBeat), "Cloud reports the last heartbeat").toBe(
    true,
  );
  const advanced = await waitForServer(
    deps,
    serverId,
    (server) =>
      fresh(server) && seconds(server.connection.last_heartbeat_at) > firstBeat,
    "a newer heartbeat",
    FRESH_HEARTBEAT_SECONDS + RESOLUTION_SECONDS,
  );
  const heartbeatAdvanceSeconds =
    seconds(advanced.connection.last_heartbeat_at) - firstBeat;
  expect(
    heartbeatAdvanceSeconds,
    "the next heartbeat lands inside the fresh window",
  ).toBeLessThanOrEqual(FRESH_HEARTBEAT_SECONDS + RESOLUTION_SECONDS);
  const stalenessSeconds = Number(advanced.connection.staleness_seconds);
  expect(
    String(advanced.health?.state ?? ""),
    "Cloud reports the server's health",
  ).not.toBe("");
  expect(
    Number.isFinite(seconds(advanced.health?.observed_at)),
    "the health is an observation",
  ).toBe(true);
  const cockpit = await deps.get(
    `/api/v1/monitor/cockpit?kit_deployment_id=${encodeURIComponent(stackId)}`,
  );
  const row = (Array.isArray(cockpit.servers) ? cockpit.servers : []).find(
    (server: Json) =>
      String(server.server_id || server.id) === serverId ||
      String(server.agent_id) === workerId,
  );
  expect(row, "the monitor cockpit lists the connected server").toBeTruthy();
  expect(
    String(row.health?.state ?? ""),
    "the cockpit reports its health",
  ).not.toBe("");
  expect(
    String(row.last_seen ?? ""),
    "the cockpit reports its last Guard report",
  ).not.toBe("");
  checks.push({
    id: "monitoring-read",
    status: "passed",
    connection: "connected",
    staleness_seconds: stalenessSeconds,
    heartbeat_advance_seconds: heartbeatAdvanceSeconds,
    health: String(advanced.health.state),
    inventory_revision: Number(advanced.inventory_revision ?? 0),
    cockpit_listed: true,
    cockpit_health: String(row.health.state),
    cockpit_health_source: String(row.health.source ?? ""),
    cpu_status: String(row.health.cpu_percent?.status ?? "absent"),
    memory_status: String(row.health.memory_percent?.status ?? "absent"),
    disk_status: String(row.health.disk_percent?.status ?? "absent"),
    guard_posture_reported: Boolean(capabilities.guard_posture),
  });

  // disconnect-rollback
  const detached = await deps.detach(serverId);
  expect(String(detached.server_id ?? ""), "the detach names the server").toBe(
    serverId,
  );
  expect(
    String(detached.agent_id ?? ""),
    "the detach revokes the enrolled Guard",
  ).toBe(workerId);
  const removed = (server: Json | null) =>
    server === null ||
    (server.lifecycle?.state === "decommissioned" &&
      server.lifecycle?.desired_state === "absent" &&
      server.connection?.state !== "connected");
  const afterDetach = await deps.readServer(serverId);
  expect(
    removed(afterDetach),
    `Cloud reads the server back as removed: ${JSON.stringify(afterDetach?.lifecycle ?? null)}`,
  ).toBe(true);
  expect(
    (await deps.listServers()).some((server) => String(server.id) === serverId),
    "Cloud no longer lists the server",
  ).toBe(false);
  // A revoked Guard that still reached Cloud would refresh its heartbeat or its
  // state within the fresh window; one whole window later neither has moved.
  const quietFrom = Date.now();
  await deps.delay((FRESH_HEARTBEAT_SECONDS + RESOLUTION_SECONDS) * 1000);
  const quiet = await deps.readServer(serverId);
  expect(
    removed(quiet),
    "the server stays removed one heartbeat window after the detach",
  ).toBe(true);
  expect(
    quiet?.connection?.last_heartbeat_at ?? null,
    "no heartbeat of the revoked Guard was accepted",
  ).toBe(afterDetach?.connection?.last_heartbeat_at ?? null);
  const revokedQuietSeconds = Math.round((Date.now() - quietFrom) / 1000);
  await deps.destroyStack(stackId);
  const factsAfter = await nodeFacts(deps);
  expect(
    factsAfter,
    "the node keeps its StackKits install, unrebooted",
  ).toEqual(factsBefore);
  expectWorkloadUnchanged(
    workloadBefore,
    await workload(deps),
    "after disconnect",
  );
  await localVerify(deps, "after disconnect");
  checks.push({
    id: "disconnect-rollback",
    status: "passed",
    lifecycle: String(quiet?.lifecycle?.state ?? "absent"),
    desired_state: String(quiet?.lifecycle?.desired_state ?? "absent"),
    connection: String(quiet?.connection?.state ?? "absent"),
    server_listed: false,
    revoked_quiet_seconds: revokedQuietSeconds,
    stack_removed: true,
    workload_unchanged: true,
    local_verify: "passed",
    guard_service: await guardService(deps),
  });

  return {
    schemaVersion: CONNECTED_FREE_SCHEMA,
    identity: "free",
    // The shared Free test identity, by its custody names (TEST_FREE_USER_*).
    identityCustody: "TEST_FREE_USER",
    stackId,
    serverId,
    workerId,
    node: {
      stackkitVersion: factsBefore.stackkitVersion,
      workloadContainers: workloadBefore.size,
      local: "basement one-line installer, Standard Mode, core only",
    },
    bounds: {
      freshHeartbeatSeconds: FRESH_HEARTBEAT_SECONDS,
      offlineHeartbeatSeconds: OFFLINE_HEARTBEAT_SECONDS,
      resolutionSeconds: RESOLUTION_SECONDS,
    },
    measured: {
      stalenessSeconds,
      heartbeatAdvanceSeconds,
      revokedQuietSeconds,
    },
    workloadUnchanged: true,
    reinstalled: false,
    checks,
    status: "passed",
  };
}
