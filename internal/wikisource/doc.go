// Package wikisource projects the Homelab Runtime's own state and change
// receipts into the neutral document a Space Wiki refresh reads
// (platform-m09kf.5.11, D-2026-11-22-04).
//
// The document is state, not Wiki semantics: it names servers, apps and their
// versions, addresses, storage, backups, the network a server belongs to and
// the completed change receipts of the tenant. The AI Platform maps it to
// claims, so the evidence class is not the runtime's to choose: a document
// read from here is a receipt, never an assertion about a person.
//
// One source per server keeps change detection granular: an unchanged server
// costs no refresh, and a changed app only rewrites that server's source. One
// source per change receipt makes the receipt the cited source of the version
// it changed. Failure occurrences stay in the tenant that saw them
// (D-2026-10-26-07): they are only ever read for the authenticated owner and
// are never aggregated across installations here.
package wikisource

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// Schema is the wire schema of one document.
	Schema = "kombify.homelab-wiki/v1"
	// AddressPrefix is the source address prefix the Wiki stores.
	AddressPrefix = "homelab://"
	// AreaSpace is the area the person's own Wiki lists.
	AreaSpace = "space"

	// MaxServers bounds one document; a larger installation reports the
	// first servers in stable id order and says so in Skipped.
	MaxServers = 200
	// MaxServicesPerServer bounds the apps of one server source.
	MaxServicesPerServer = 24
	// MaxReceipts bounds the change receipts of one document, newest first.
	MaxReceipts = 100
	// MaxAddresses bounds the addresses listed for one server.
	MaxAddresses = 4
	// MaxFailures bounds the tenant-owned failure occurrences of one source.
	MaxFailures = 4
)

// Document is one owner-scoped projection of the runtime. It is the body of
// GET /api/v1/wiki/sources and the body a report pushes to the Wiki.
type Document struct {
	Schema         string `json:"schema"`
	OwnerSubjectID string `json:"ownerSubjectId"`
	// Area is the Wiki area the sources are written to ("space").
	Area        string   `json:"area"`
	GeneratedAt string   `json:"generatedAt"`
	Sources     []Source `json:"sources"`
	// Skipped names the bounds that cut the document, so a reader does not
	// mistake a truncated report for a complete one.
	Skipped []string `json:"skipped,omitempty"`
}

// Source is one stable address the Wiki refresh stores and cites. Exactly one
// of Server and Receipt is set.
type Source struct {
	Address string       `json:"address"`
	ReadAt  string       `json:"readAt"`
	Server  *ServerState `json:"server,omitempty"`
	Receipt *Receipt     `json:"receipt,omitempty"`
}

// ServerState is the state of one server and what runs on it.
type ServerState struct {
	Server   Server    `json:"server"`
	Network  *Network  `json:"network,omitempty"`
	Services []Service `json:"services"`
	Backup   *Backup   `json:"backup,omitempty"`
	Failures []Failure `json:"failures,omitempty"`
}

// Server is one secret-free server projection.
type Server struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	OperatingSystem string   `json:"operatingSystem,omitempty"`
	OSVersion       string   `json:"osVersion,omitempty"`
	Addresses       []string `json:"addresses,omitempty"`
	DiskPercent     *float64 `json:"diskPercent,omitempty"`
	Lifecycle       string   `json:"lifecycle,omitempty"`
}

// Service is one app or service projection. Version is the application
// version when the runtime verified one, else the StackKits release.
type Service struct {
	ID         string   `json:"id"`
	Key        string   `json:"key"`
	Name       string   `json:"name"`
	ServerID   string   `json:"serverId"`
	Version    string   `json:"version,omitempty"`
	ObservedAt string   `json:"observedAt,omitempty"`
	Hostnames  []string `json:"hostnames,omitempty"`
	Ports      []int    `json:"ports,omitempty"`
	Observed   string   `json:"observedState,omitempty"`
}

// Network is the private network a server is addressed on.
type Network struct {
	Name string `json:"name"`
	CIDR string `json:"cidr"`
}

// Backup is one stack's backup state for a server.
type Backup struct {
	Target       string `json:"target,omitempty"`
	State        string `json:"state,omitempty"`
	LastRunAt    string `json:"lastRunAt,omitempty"`
	LastResult   string `json:"lastResult,omitempty"`
	RestoreDrill string `json:"restoreDrill,omitempty"`
}

// Failure is one tenant-owned failure occurrence summary. It never leaves the
// owner's own Wiki.
type Failure struct {
	ServerID  string `json:"serverId,omitempty"`
	ServiceID string `json:"serviceId,omitempty"`
	Step      string `json:"step,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Count     int    `json:"count"`
	LastAt    string `json:"lastAt"`
}

// Receipt is one completed change of the runtime: who asked for it, what it
// did and which release it moved. It is the evidence of the version claim.
type Receipt struct {
	ID            string `json:"id"`
	Action        string `json:"action"`
	Actor         string `json:"actor,omitempty"`
	Result        string `json:"result"`
	ServiceID     string `json:"serviceId,omitempty"`
	ServiceKey    string `json:"serviceKey,omitempty"`
	ServiceName   string `json:"serviceName,omitempty"`
	ServerID      string `json:"serverId,omitempty"`
	PriorVersion  string `json:"priorVersion,omitempty"`
	TargetVersion string `json:"targetVersion,omitempty"`
	At            string `json:"at"`
	Summary       string `json:"summary,omitempty"`
}

// Input is the already owner-authorized read a document is built from.
type Input struct {
	OwnerSubjectID string
	GeneratedAt    time.Time
	Servers        []Server
	Services       []Service
	Backups        []BackupState
	Receipts       []Receipt
	Failures       []Failure
}

// BackupState binds one backup projection to its server.
type BackupState struct {
	ServerID string
	Backup   Backup
}

// Build projects the input into one document. It is deterministic: the same
// input yields the same bytes, order and addresses, so an unchanged runtime
// is an unchanged document and costs no refresh.
func Build(input Input) Document {
	now := input.GeneratedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	readAt := now.Format(time.RFC3339)
	document := Document{
		Schema:         Schema,
		OwnerSubjectID: strings.TrimSpace(input.OwnerSubjectID),
		Area:           AreaSpace,
		GeneratedAt:    readAt,
		Sources:        []Source{},
	}

	servers := append([]Server(nil), input.Servers...)
	sort.SliceStable(servers, func(i, j int) bool { return servers[i].ID < servers[j].ID })
	if len(servers) > MaxServers {
		document.Skipped = append(document.Skipped, "servers:"+strconv.Itoa(len(servers)-MaxServers))
		servers = servers[:MaxServers]
	}

	backupByServer := make(map[string]Backup, len(input.Backups))
	for _, entry := range input.Backups {
		backupByServer[strings.TrimSpace(entry.ServerID)] = entry.Backup
	}
	servicesByServer := make(map[string][]Service, len(servers))
	for _, service := range input.Services {
		serverID := strings.TrimSpace(service.ServerID)
		if serverID == "" {
			continue
		}
		service.ServerID = serverID
		servicesByServer[serverID] = append(servicesByServer[serverID], service)
	}
	failuresByServer := make(map[string][]Failure, len(servers))
	for _, failure := range input.Failures {
		if failure.ServerID != "" {
			if _, known := backupByServer[failure.ServerID]; known || serverKnown(servers, failure.ServerID) {
				failuresByServer[failure.ServerID] = append(failuresByServer[failure.ServerID], failure)
			}
			continue
		}
		for _, server := range servers {
			if failure.ServiceID == "" || serviceOfServer(server.ID, failure.ServiceID, servicesByServer[server.ID]) {
				failuresByServer[server.ID] = append(failuresByServer[server.ID], failure)
				break
			}
		}
	}

	for _, server := range servers {
		state := ServerState{
			Server:   normalizeServer(server),
			Network:  networkOf(server),
			Services: projectServices(servicesByServer[server.ID], server.ID),
			Failures: trimFailures(failuresByServer[server.ID]),
		}
		if backup, ok := backupByServer[server.ID]; ok {
			copied := backup
			state.Backup = &copied
		}
		document.Sources = append(document.Sources, Source{
			Address: AddressPrefix + "state/servers/" + escape(server.ID),
			ReadAt:  readAt,
			Server:  &state,
		})
	}

	receipts := append([]Receipt(nil), input.Receipts...)
	sort.SliceStable(receipts, func(i, j int) bool {
		if receipts[i].At != receipts[j].At {
			return receipts[i].At > receipts[j].At
		}
		return receipts[i].ID > receipts[j].ID
	})
	if len(receipts) > MaxReceipts {
		document.Skipped = append(document.Skipped, "receipts:"+strconv.Itoa(len(receipts)-MaxReceipts))
		receipts = receipts[:MaxReceipts]
	}
	for index := range receipts {
		receipt := normalizeReceipt(receipts[index])
		if receipt.ID == "" {
			continue
		}
		document.Sources = append(document.Sources, Source{
			Address: AddressPrefix + "receipts/" + escape(receipt.ID),
			ReadAt:  readAt,
			Receipt: &receipt,
		})
	}
	return document
}

func normalizeServer(server Server) Server {
	server.ID = strings.TrimSpace(server.ID)
	server.Name = strings.TrimSpace(server.Name)
	server.OperatingSystem = strings.TrimSpace(server.OperatingSystem)
	server.OSVersion = strings.TrimSpace(server.OSVersion)
	server.Lifecycle = strings.TrimSpace(server.Lifecycle)
	if server.Name == "" {
		server.Name = server.ID
	}
	addresses := make([]string, 0, MaxAddresses)
	seen := map[string]struct{}{}
	for _, address := range server.Addresses {
		address = strings.TrimSpace(address)
		if address == "" {
			continue
		}
		if _, duplicate := seen[address]; duplicate {
			continue
		}
		if len(addresses) >= MaxAddresses {
			break
		}
		seen[address] = struct{}{}
		addresses = append(addresses, address)
	}
	server.Addresses = addresses
	return server
}

// networkOf names the private network a server is addressed on: the /24 of
// its first private address. A server without one reports no network.
func networkOf(server Server) *Network {
	for _, address := range server.Addresses {
		cidr, ok := privateNetwork(address)
		if !ok {
			continue
		}
		return &Network{Name: "Private network " + cidr, CIDR: cidr}
	}
	return nil
}

// privateNetwork maps a private IPv4 address to its /24. Public addresses
// name no LAN, so a document never publishes a public prefix as a network.
func privateNetwork(address string) (string, bool) {
	parts := strings.Split(strings.TrimSpace(address), ".")
	if len(parts) != 4 {
		return "", false
	}
	octets := make([]int, 4)
	for index, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 || value > 255 {
			return "", false
		}
		octets[index] = value
	}
	private := octets[0] == 10 ||
		(octets[0] == 172 && octets[1] >= 16 && octets[1] <= 31) ||
		(octets[0] == 192 && octets[1] == 168)
	if !private {
		return "", false
	}
	return strconv.Itoa(octets[0]) + "." + strconv.Itoa(octets[1]) + "." + strconv.Itoa(octets[2]) + ".0/24", true
}

func projectServices(services []Service, serverID string) []Service {
	sorted := append([]Service(nil), services...)
	sort.SliceStable(sorted, func(i, j int) bool {
		left, right := sorted[i], sorted[j]
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		return left.ID < right.ID
	})
	out := make([]Service, 0, len(sorted))
	for _, service := range sorted {
		if len(out) >= MaxServicesPerServer {
			break
		}
		service.ID = strings.TrimSpace(service.ID)
		service.Key = strings.TrimSpace(service.Key)
		service.Name = strings.TrimSpace(service.Name)
		service.Version = strings.TrimSpace(service.Version)
		service.Observed = strings.TrimSpace(service.Observed)
		service.ServerID = serverID
		if service.Name == "" {
			service.Name = firstNonEmpty(service.Key, service.ID)
		}
		if service.Name == "" {
			continue
		}
		out = append(out, service)
	}
	return out
}

// LinkShape derives the hostnames and ports of one service access link. It is
// exported for the route that projects a sanitized URL into the document.
func LinkShape(rawURL string) (hostname string, port int) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" {
		return "", 0
	}
	hostname = strings.ToLower(parsed.Hostname())
	if value := parsed.Port(); value != "" {
		port, _ = strconv.Atoi(value)
	} else if parsed.Scheme == "https" {
		port = 443
	} else if parsed.Scheme == "http" {
		port = 80
	}
	return hostname, port
}

func trimFailures(failures []Failure) []Failure {
	if len(failures) == 0 {
		return nil
	}
	sorted := append([]Failure(nil), failures...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].LastAt > sorted[j].LastAt })
	if len(sorted) > MaxFailures {
		sorted = sorted[:MaxFailures]
	}
	return sorted
}

func serviceOfServer(serverID, serviceID string, services []Service) bool {
	for _, service := range services {
		if service.ID == serviceID && service.ServerID == serverID {
			return true
		}
	}
	return false
}

func serverKnown(servers []Server, serverID string) bool {
	for _, server := range servers {
		if server.ID == serverID {
			return true
		}
	}
	return false
}

func normalizeReceipt(receipt Receipt) Receipt {
	receipt.ID = strings.TrimSpace(receipt.ID)
	receipt.Action = strings.TrimSpace(receipt.Action)
	receipt.Actor = strings.TrimSpace(receipt.Actor)
	receipt.Result = strings.TrimSpace(receipt.Result)
	receipt.ServiceID = strings.TrimSpace(receipt.ServiceID)
	receipt.ServiceKey = strings.TrimSpace(receipt.ServiceKey)
	receipt.ServiceName = strings.TrimSpace(receipt.ServiceName)
	receipt.ServerID = strings.TrimSpace(receipt.ServerID)
	receipt.PriorVersion = strings.TrimSpace(receipt.PriorVersion)
	receipt.TargetVersion = strings.TrimSpace(receipt.TargetVersion)
	receipt.At = strings.TrimSpace(receipt.At)
	receipt.Summary = strings.TrimSpace(receipt.Summary)
	return receipt
}

// Escape keeps a source address one path segment.
func Escape(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	return url.PathEscape(value)
}

func escape(value string) string { return Escape(value) }

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
