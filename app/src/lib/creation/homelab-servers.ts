/**
 * The servers a person already has, as choices on the Server step. There is
 * one homelab per person: everything they run is part of it. New use cases go
 * onto a single server they pick here, or onto a new one. A server that runs a
 * StackKit takes them through that StackKit; a server without one gets its
 * first StackKit.
 */
import {
  isCurrentCanonicalServer,
  type CanonicalServer,
} from "#lib/api/registry.js";
import type { KitDeployment } from "#lib/api/stacks.js";
import type { StackKitInfo } from "#lib/api/unifier.js";

export type HomelabServerState =
  /** Can take new use cases now. */
  | "ready"
  /** A server without a StackKit that must reconnect first. */
  | "disconnected"
  /** A managed server that must keep its current provider binding. */
  | "leased"
  /** Changes on this server are not allowed. */
  | "restricted";

export interface HomelabServerChoice {
  id: string;
  name: string;
  /** The StackKit deployment the server belongs to, if any. */
  deploymentId?: string;
  /** The StackKit it runs, as the catalog names it. */
  stackkit?: string;
  state: HomelabServerState;
}

/** A deployment a connected server without a StackKit can join as a node. */
export interface HomelabJoinTarget {
  id: string;
  name: string;
}

/** What the Server step needs to offer the person's existing servers. */
export interface HomelabServerSelection {
  readonly servers: readonly HomelabServerChoice[];
  readonly loading: boolean;
  readonly failed: boolean;
  /** Why the server the person started from is not the one chosen. */
  readonly notice?: string;
  /** The chosen server's id; empty while a new server is chosen. */
  readonly selected: string;
  select(id: string): void;
  retry(): void;
  /** Deployments the chosen server without a StackKit can join instead. */
  readonly joinTargets: readonly HomelabJoinTarget[];
  /** The deployment it joins; empty while it gets its own StackKit. */
  readonly joinTarget: string;
  selectJoin(deploymentId: string): void;
}

/** The catalog's kit name for a deployment's `stackkit_id` (the backend's aliases). */
const KIT_ALIASES: Record<string, string> = {
  basement: "basement-kit",
  basementkit: "basement-kit",
  "homelab-starter": "basement-kit",
  "homelab-basic": "basement-kit",
  "base-homelab": "basement-kit",
  "minimal-arm": "basement-kit",
  cloud: "cloud-kit",
  cloudkit: "cloud-kit",
  "kombify-cloud-kit": "cloud-kit",
  "modern-kit": "modern-homelab",
  "homelab-advanced": "modern-homelab",
};

function kitName(id: string): string {
  const name = id.trim().toLowerCase();
  return KIT_ALIASES[name] ?? name;
}

/**
 * The running deployments a bare server can join: the pinned release must
 * declare multi-node join (feature `multiNode`) for the deployment's kit.
 * The run checks the same gate again; this only keeps the choice honest.
 */
export function homelabJoinTargets(
  deployments: readonly KitDeployment[],
  kits: readonly StackKitInfo[],
): HomelabJoinTarget[] {
  const joinable = new Set(
    kits
      .filter((kit) => kit.features?.includes("multiNode"))
      .map((kit) => kit.name),
  );
  return deployments
    .filter(
      (deployment) =>
        (deployment.status ?? deployment.state) === "running" &&
        Boolean(deployment.stackkit_id) &&
        joinable.has(kitName(deployment.stackkit_id ?? "")),
    )
    .map((deployment) => ({ id: deployment.id, name: deployment.name }))
    .sort((a, b) => a.name.localeCompare(b.name));
}

function stateOf(server: CanonicalServer): HomelabServerState {
  if (!server.mutations_allowed) return "restricted";
  if (server.kit_deployment_id) return "ready";
  if (server.provider.lease_id) return "leased";
  return server.connection.state === "connected" ? "ready" : "disconnected";
}

/**
 * The servers that can be offered: current, enrolled, never the hypervisor
 * host that only carries guests. Ready servers come first.
 */
export function homelabServerChoices(
  servers: readonly CanonicalServer[],
  stackkitOf: (deploymentId: string) => string | undefined = () => undefined,
): HomelabServerChoice[] {
  return servers
    .filter(
      (server) =>
        isCurrentCanonicalServer(server) &&
        server.node_role !== "substrate" &&
        (Boolean(server.kit_deployment_id) || Boolean(server.worker_id)),
    )
    .map((server) => ({
      id: server.id,
      name: server.name,
      deploymentId: server.kit_deployment_id || undefined,
      stackkit: server.kit_deployment_id
        ? stackkitOf(server.kit_deployment_id)
        : undefined,
      state: stateOf(server),
    }))
    .sort(
      (a, b) =>
        Number(b.state === "ready") - Number(a.state === "ready") ||
        a.name.localeCompare(b.name),
    );
}
