package providercontrol

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

const (
	// IONOSSharedPoolAgentPath is the single route of the node-agent protocol; the
	// HTTPS client in ionos_shared_pool_http.go and the handler in
	// ionos_shared_pool_agent_handler.go both use it.
	IONOSSharedPoolAgentPath = "/v1/ionos-shared-pool/execute"
	// IONOSSharedWorkerAgentPort is the TLS port of the node agent on the worker.
	IONOSSharedWorkerAgentPort = 8443
	// IONOSSharedWorkerAgentInterface is the private control-plane WireGuard
	// device the node agent binds and the firewall admits.
	IONOSSharedWorkerAgentInterface = "wg-kombify"
	// IONOSSharedWorkerControlPlaneCIDR is the sole source prefix permitted to
	// reach IONOSSharedWorkerAgentPort on IONOSSharedWorkerAgentInterface.
	IONOSSharedWorkerControlPlaneCIDR = "100.64.200.0/24"

	ionosSharedWorkerWorkloadCIDR = "10.200.0.0/16"
	ionosSharedWorkerConfigDir    = "/etc/kombify/ionos-shared-worker"
)

// IONOSSharedWorkerAgentListener is the node agent's listener contract: one
// route, TLS always, and a verified client certificate. The listener binds only
// the resolved IP of Interface (never a wildcard) and the firewall admits only
// AllowedSourceCIDR on Interface.
type IONOSSharedWorkerAgentListener struct {
	Path               string `json:"path"`
	Port               int    `json:"port"`
	Interface          string `json:"interface"`
	AllowedSourceCIDR  string `json:"allowed_source_cidr"`
	TLSRequired        bool   `json:"tls_required"`
	ClientCertRequired bool   `json:"client_cert_required"`
}

// IONOSSharedWorkerProfile is the secret-free host specification of one admitted
// IONOS de/fra/2 shared worker. Building it buys nothing and calls no provider
// API; CloudInit is the provisioning script an admitted purchase would pass as
// user data. Credentials (agent TLS identity, R2 backup credential, per-app
// database credentials) are node-held and never appear here.
type IONOSSharedWorkerProfile struct {
	NodeID            string                         `json:"node_id"`
	Location          string                         `json:"location"`
	MemoryMB          int                            `json:"memory_mb"`
	ReservedMemoryMB  int                            `json:"reserved_memory_mb"`
	Slots             int                            `json:"slots"`
	IsolationRuntimes []string                       `json:"isolation_runtimes"`
	MaskedUnits       []string                       `json:"masked_units"`
	DeniedEgressCIDRs []string                       `json:"denied_egress_cidrs"`
	MetadataEndpoints []string                       `json:"metadata_endpoints"`
	BackupStore       string                         `json:"backup_store"`
	Agent             IONOSSharedWorkerAgentListener `json:"agent"`
	CloudInit         string                         `json:"cloud_init"`
	CloudInitSHA256   string                         `json:"cloud_init_sha256"`
}

// BuildIONOSSharedWorkerProfile renders the deterministic host profile for an
// admitted worker. It applies the same admission as the adapter (de/fra/2,
// 32–64 GB, 30–40 slots, no orchestrator) before rendering anything.
func BuildIONOSSharedWorkerProfile(node IONOSSharedPoolNode) (IONOSSharedWorkerProfile, error) {
	if err := node.validate(); err != nil {
		return IONOSSharedWorkerProfile{}, err
	}
	profile := IONOSSharedWorkerProfile{
		NodeID:            node.ID,
		Location:          node.Location,
		MemoryMB:          node.MemoryMB,
		ReservedMemoryMB:  node.ReservedMemoryMB,
		Slots:             node.Slots,
		IsolationRuntimes: []string{"gvisor"},
		MaskedUnits:       []string{"docker.service", "docker.socket"},
		DeniedEgressCIDRs: []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "100.64.0.0/10"},
		MetadataEndpoints: []string{"169.254.169.254"},
		BackupStore:       "r2",
		Agent: IONOSSharedWorkerAgentListener{
			Path:               IONOSSharedPoolAgentPath,
			Port:               IONOSSharedWorkerAgentPort,
			Interface:          IONOSSharedWorkerAgentInterface,
			AllowedSourceCIDR:  IONOSSharedWorkerControlPlaneCIDR,
			TLSRequired:        true,
			ClientCertRequired: true,
		},
	}
	profile.CloudInit = renderIONOSSharedWorkerCloudInit(profile)
	digest := sha256.Sum256([]byte(profile.CloudInit))
	profile.CloudInitSHA256 = hex.EncodeToString(digest[:])
	return profile, nil
}

func renderIONOSSharedWorkerCloudInit(p IONOSSharedWorkerProfile) string {
	return strings.NewReplacer(
		"@@NODE_ID@@", p.NodeID,
		"@@LOCATION@@", p.Location,
		"@@SLOTS@@", strconv.Itoa(p.Slots),
		"@@MEMORY_MB@@", strconv.Itoa(p.MemoryMB),
		"@@RESERVED_MB@@", strconv.Itoa(p.ReservedMemoryMB),
		"@@DENIED_V4@@", strings.Join(p.DeniedEgressCIDRs, ", "),
		"@@METADATA@@", strings.Join(p.MetadataEndpoints, ", "),
		"@@AGENT_PATH@@", p.Agent.Path,
		"@@AGENT_PORT@@", strconv.Itoa(p.Agent.Port),
		"@@AGENT_IF@@", p.Agent.Interface,
		"@@CONTROL_CIDR@@", p.Agent.AllowedSourceCIDR,
		"@@WORKLOAD_CIDR@@", ionosSharedWorkerWorkloadCIDR,
		"@@CONF@@", ionosSharedWorkerConfigDir,
	).Replace(ionosSharedWorkerCloudInitTemplate)
}

// ionosSharedWorkerCloudInitTemplate is a plain shell script (cloud-init runs a
// leading #! user-data file as is). Every heredoc is quoted, so nothing expands
// at write time, and no value here is a credential.
const ionosSharedWorkerCloudInitTemplate = `#!/bin/bash
# kombify IONOS shared worker @@NODE_ID@@ (@@LOCATION@@): standalone gVisor host.
# Slots @@SLOTS@@, RAM @@MEMORY_MB@@ MB of which @@RESERVED_MB@@ MB reserved for the host.
# The agent TLS identity and R2 backup credential are installed outside this script.
set -euo pipefail
umask 022
export DEBIAN_FRONTEND=noninteractive

fail() { echo "kombify-shared-worker: $*" >&2; exit 1; }

install -d -m 0755 @@CONF@@
install -d -m 0750 @@CONF@@/agent-tls
install -d -m 0750 @@CONF@@/nft.d

# 1. No bare shared Docker: pin the packages away, then disable and mask the units.
install -d -m 0755 /etc/apt/preferences.d
cat >/etc/apt/preferences.d/kombify-no-docker <<'KOMBIFY_EOF'
Package: docker.io docker-ce docker-ce-cli docker-compose-plugin containerd.io podman-docker
Pin: release *
Pin-Priority: -1
KOMBIFY_EOF
systemctl disable --now docker.service docker.socket 2>/dev/null || true
systemctl mask docker.service docker.socket

# 2. Daemonless OCI image management with gVisor from its signed repository.
apt-get update
apt-get install -y --no-install-recommends ca-certificates curl gnupg nftables iproute2 podman wireguard-tools xfsprogs
curl -fsSL https://gvisor.dev/archive.key | gpg --dearmor -o /usr/share/keyrings/gvisor-archive-keyring.gpg
echo "deb [arch=amd64 signed-by=/usr/share/keyrings/gvisor-archive-keyring.gpg] https://storage.googleapis.com/gvisor/releases release main" >/etc/apt/sources.list.d/gvisor.list
apt-get update
apt-get install -y --no-install-recommends runsc
apt-mark hold runsc podman
{ runsc --version; podman --version; } >@@CONF@@/runtime-pins.txt

# 3. Fail-closed host and bridge network isolation. Workloads get no route to private,
# link-local or CGNAT ranges, to the metadata endpoint, to each other (routed or bridged),
# or to host services. The agent port is reachable only on @@AGENT_IF@@ from @@CONTROL_CIDR@@.
mkdir -p /etc/modules-load.d
printf 'br_netfilter\n' >/etc/modules-load.d/kombify-shared-worker.conf
modprobe br_netfilter || true
cat >/etc/sysctl.d/90-kombify-shared-worker.conf <<'KOMBIFY_EOF'
net.ipv4.ip_forward = 1
net.ipv4.conf.all.rp_filter = 1
net.ipv4.conf.default.rp_filter = 1
net.ipv4.conf.all.accept_redirects = 0
net.ipv4.conf.all.send_redirects = 0
net.ipv6.conf.all.accept_redirects = 0
net.bridge.bridge-nf-call-iptables = 1
net.bridge.bridge-nf-call-ip6tables = 1
KOMBIFY_EOF
sysctl --system >/dev/null

cat >@@CONF@@/nft.d/00-base.nft <<'KOMBIFY_EOF'
# Reserved for per-workload host rules managed atomically by the node agent.
KOMBIFY_EOF

cat >/etc/nftables.conf <<'KOMBIFY_EOF'
#!/usr/sbin/nft -f
flush ruleset

table inet kombify_shared_worker {
	chain input {
		type filter hook input priority 0; policy drop;
		iifname "lo" accept
		iifname "kw*" drop
		ct state established,related accept
		ct state invalid drop
		ip protocol icmp accept
		ip6 nexthdr icmpv6 accept
		tcp dport 22 accept
		iifname "@@AGENT_IF@@" ip saddr @@CONTROL_CIDR@@ tcp dport @@AGENT_PORT@@ accept
	}
	chain forward {
		type filter hook forward priority 0; policy drop;
		iifname "kw*" oifname "kw*" drop
		iifname "kw*" ip daddr @@METADATA@@ drop
		iifname "kw*" ip daddr { @@DENIED_V4@@ } drop
		iifname "kw*" meta nfproto ipv6 drop
		ct state established,related accept
		ct state invalid drop
		iifname "kw*" ip saddr @@WORKLOAD_CIDR@@ accept
	}
}

table bridge kombify_shared_worker_bridge {
	chain forward {
		type filter hook forward priority 0; policy drop;
		iifname "kw*" oifname "kw*" drop
	}
}

table ip kombify_shared_worker_nat {
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		ip saddr @@WORKLOAD_CIDR@@ oifname != "kw*" masquerade
	}
}

include "@@CONF@@/nft.d/*.nft"
KOMBIFY_EOF
chmod 0644 /etc/nftables.conf
nft -c -f /etc/nftables.conf
systemctl enable nftables
systemctl restart nftables

# Per-workload netns ruleset: the node agent executes
# "ip netns exec net-<sha256(tenant NUL handle)> nft -f -" on Provision/Start.
cat >@@CONF@@/workload-net.nft <<'KOMBIFY_EOF'
table inet kombify_workload {
	chain input {
		type filter hook input priority 0; policy drop;
		iifname "lo" accept
		ct state established,related accept
	}
	chain output {
		type filter hook output priority 0; policy drop;
		oifname "lo" accept
		ip daddr @@METADATA@@ drop
		ip daddr { @@DENIED_V4@@ } drop
		ip6 daddr { fc00::/7, fe80::/10 } drop
		ct state established,related accept
		ct state new accept
	}
}
KOMBIFY_EOF

# 4. R2 backup custody: expectations only. The agent reads its R2 credential from a
# systemd encrypted credential that is installed on the node, never from user data.
cat >@@CONF@@/backup-custody.env <<'KOMBIFY_EOF'
KOMBIFY_BACKUP_STORE=r2
KOMBIFY_BACKUP_KEY_PREFIX=tenants/<sha256(tenant)>/workloads/<handle>/backups/
KOMBIFY_BACKUP_VERIFY=version,size,sha256
KOMBIFY_BACKUP_CREDENTIAL=r2-backup
KOMBIFY_EOF

# 5. Node agent: mTLS only, verified client certificate, bound to @@AGENT_IF@@ address.
cat >@@CONF@@/agent.env <<'KOMBIFY_EOF'
KOMBIFY_AGENT_NODE_ID=@@NODE_ID@@
KOMBIFY_AGENT_MEMORY_MB=@@MEMORY_MB@@
KOMBIFY_AGENT_RESERVED_MB=@@RESERVED_MB@@
KOMBIFY_AGENT_SLOTS=@@SLOTS@@
KOMBIFY_AGENT_PATH=@@AGENT_PATH@@
KOMBIFY_AGENT_INTERFACE=@@AGENT_IF@@
KOMBIFY_AGENT_ALLOWED_CIDR=@@CONTROL_CIDR@@
KOMBIFY_AGENT_PORT=@@AGENT_PORT@@
KOMBIFY_AGENT_TLS=required
KOMBIFY_AGENT_CLIENT_AUTH=require-and-verify
KOMBIFY_AGENT_TLS_CERT=@@CONF@@/agent-tls/server.crt
KOMBIFY_AGENT_TLS_KEY=@@CONF@@/agent-tls/server.key
KOMBIFY_AGENT_CLIENT_CA=@@CONF@@/agent-tls/client-ca.crt
KOMBIFY_AGENT_MAX_BODY_BYTES=1048576
KOMBIFY_EOF
cat >@@CONF@@/start-agent.sh <<'KOMBIFY_EOF'
#!/bin/bash
set -euo pipefail
bind_ip="$(ip -4 -o addr show dev "${KOMBIFY_AGENT_INTERFACE}" scope global | awk '{print $4}' | cut -d/ -f1 | head -n1)"
if [ -z "${bind_ip}" ] || [ "${bind_ip}" = "0.0.0.0" ]; then
	echo "kombify-shared-worker: missing private IPv4 on ${KOMBIFY_AGENT_INTERFACE}" >&2
	exit 1
fi
export KOMBIFY_AGENT_BIND="${bind_ip}"
exec /usr/local/bin/kombify-ionos-shared-worker-agent
KOMBIFY_EOF
chmod 0750 @@CONF@@/start-agent.sh
cat >/etc/systemd/system/kombify-ionos-shared-worker-agent.service <<'KOMBIFY_EOF'
[Unit]
Description=kombify IONOS shared worker node agent
After=network-online.target nftables.service wg-quick@@@AGENT_IF@@.service
Wants=network-online.target
Requires=nftables.service
ConditionPathExists=/usr/local/bin/kombify-ionos-shared-worker-agent
ConditionPathExists=/sys/class/net/@@AGENT_IF@@
ConditionPathExists=@@CONF@@/agent-tls/server.crt
ConditionPathExists=@@CONF@@/agent-tls/server.key
ConditionPathExists=@@CONF@@/agent-tls/client-ca.crt
ConditionPathExists=@@CONF@@/apps.json
ConditionPathIsMountPoint=/var/lib/kombify/ionos-shared-worker

[Service]
Environment=LC_ALL=C
EnvironmentFile=@@CONF@@/agent.env
EnvironmentFile=@@CONF@@/backup-custody.env
LoadCredentialEncrypted=r2-backup:/etc/credstore.encrypted/kombify-r2-backup
ExecStart=@@CONF@@/start-agent.sh
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
KOMBIFY_EOF
systemctl daemon-reload
systemctl enable kombify-ionos-shared-worker-agent.service

# 6. Verify the isolation prerequisites without claiming enrollment.
for binary in runsc podman nft wg; do
	command -v "$binary" >/dev/null 2>&1 || fail "$binary is missing"
done
if command -v dockerd >/dev/null 2>&1; then fail "bare Docker is installed"; fi
[ "$(systemctl is-enabled docker.service docker.socket 2>/dev/null | sort -u)" = "masked" ] || fail "docker units are not masked"
nft list ruleset | grep -q kombify_shared_worker || fail "host isolation ruleset is not loaded"
echo "kombify-shared-worker: @@NODE_ID@@ host profile installed; enrollment pending"
`
