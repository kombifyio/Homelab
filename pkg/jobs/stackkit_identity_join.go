package jobs

import (
	"context"
	"crypto/ecdh"
	"crypto/hpke"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kombifyio/homelab/internal/stackkitrelease"
	"github.com/kombifyio/homelab/pkg/stackkitcommand"
)

// JobTypeHomelabIdentityJoin joins a further server of a homelab to the
// homelab's Authority (homelab identity target §4, P2; ADR-036 authority
// pointer). Homelab orchestrates across the servers and carries public or
// sealed documents through the pinned StackKits CLI on each server:
//
//  1. zone-site on the joining server: its own domain and LAN address;
//  2. join-request on the joining server, pinned to the Authority root the
//     Owner read at the Authority;
//  3. join-approve at the Authority: the CLI waits for the Owner's passkey
//     step-up in the Authority's Hub, which Homelab cannot answer, and the
//     Authority signs the trust bundle and the intermediate;
//  4. join-install on the joining server;
//  5. client-register at the Authority (a second Owner step-up): the
//     Authority's Pocket ID registers the joined server's TinyAuth client and
//     seals its secret to a key Homelab generated for this job only;
//  6. Homelab opens the sealed secret in memory only to hand it to the
//     homelab's Wallet, and the Wallet delivers it sealed to the joined
//     server's StackKits-generated recipient key (host delivery);
//  7. client-bind on the joined server;
//  8. the homelab DNS zone: zone-site on every server, assembled here and
//     installed on every server, each admitting only what its trust vouches
//     for.
//
// Homelab signs nothing: every signature is the Authority's or a server's
// own, and the Owner's approvals are passkey step-ups at the Authority.
const JobTypeHomelabIdentityJoin JobType = "homelab_identity_join"

// HomelabIdentityJoinResultField holds the secret-free outcome of a join.
const HomelabIdentityJoinResultField = "homelab_identity_join"

const (
	homelabIdentityJoinPayloadField = "homelab_identity_join"
	// identityJoinStepUpTimeout covers the five minutes the CLI waits for the
	// Owner's passkey step-up and the signing after it.
	identityJoinStepUpTimeout = 8 * time.Minute
	identityJoinClientCapture = "stackkit.homelab-oidc-client-capture/v1\x00"
	identityJoinClientSchema  = "stackkit.homelab-oidc-client/v1"
)

// HomelabIdentityJoinNode is one server of the join, bound at enqueue from
// the stack's rollout and its admitted agent.
type HomelabIdentityJoinNode struct {
	StackID            string `json:"stack_id"`
	StackKitInstanceID string `json:"stackkit_instance_id"`
	AgentID            string `json:"agent_id"`
	NodeID             string `json:"node_id"`
	NodeRef            string `json:"node_ref"`
	StackKit           string `json:"stackkit"`
	StackName          string `json:"stack_name"`
	WorkingDirectory   string `json:"working_directory"`
	SpecPath           string `json:"spec_path"`
}

// HomelabIdentityJoinRequest is the closed input of a join. AuthorityRoot is
// the root fingerprint the Owner read at the Authority (its console or Hub);
// Homelab never takes it from the Authority's own answer.
type HomelabIdentityJoinRequest struct {
	TenantID      string                    `json:"tenant_id"`
	OwnerID       string                    `json:"owner_id"`
	HomelabID     string                    `json:"homelab_id"`
	Authority     HomelabIdentityJoinNode   `json:"authority"`
	Joining       HomelabIdentityJoinNode   `json:"joining"`
	Others        []HomelabIdentityJoinNode `json:"others,omitempty"`
	AuthorityRoot string                    `json:"authority_root"`
	Hostnames     []string                  `json:"hostnames,omitempty"`
	OwnerApproved bool                      `json:"owner_approved"`
}

// HomelabIdentityJoinWallet is the homelab's Wallet as the join sees it: it
// stores the joined server's client secret as an item of that server's
// deployment and seals it to the server (WalletHostDelivery).
type HomelabIdentityJoinWallet interface {
	StoreIdentityClientSecret(ctx context.Context, owner, stackID, clientID string, secret []byte) (itemID string, versioned bool, err error)
}

// NormalizeHomelabIdentityJoinRequest admits a join: three distinct roles,
// explicit Owner approval and a root fingerprint.
func NormalizeHomelabIdentityJoinRequest(req HomelabIdentityJoinRequest) (HomelabIdentityJoinRequest, error) {
	req.TenantID, req.OwnerID, req.HomelabID = strings.TrimSpace(req.TenantID), strings.TrimSpace(req.OwnerID), strings.TrimSpace(req.HomelabID)
	req.AuthorityRoot = strings.ToLower(strings.TrimSpace(req.AuthorityRoot))
	if !req.OwnerApproved {
		return req, fmt.Errorf("joining a server to the homelab's Authority requires explicit Owner approval")
	}
	if req.TenantID == "" || req.OwnerID == "" || req.HomelabID == "" {
		return req, fmt.Errorf("tenant, Owner and homelab are required")
	}
	if _, err := stackkitcommand.ParseIdentityJoinRequestInput(identityJoinInput(req.AuthorityRoot, normalizedJoinHostnames(req.Hostnames, "a.invalid"))); err != nil {
		return req, err
	}
	seen := map[string]bool{}
	for _, node := range append([]*HomelabIdentityJoinNode{&req.Authority, &req.Joining}, nodePointers(req.Others)...) {
		node.StackID, node.AgentID = strings.TrimSpace(node.StackID), strings.TrimSpace(node.AgentID)
		if node.StackID == "" || node.AgentID == "" || seen[node.AgentID] || seen["stack:"+node.StackID] {
			return req, fmt.Errorf("the Authority, the joining server and every other server need their own stack and agent")
		}
		seen[node.AgentID], seen["stack:"+node.StackID] = true, true
		if node.WorkingDirectory == "" {
			node.WorkingDirectory = defaultStackKitNodeWorkspace
		}
	}
	return req, nil
}

func nodePointers(nodes []HomelabIdentityJoinNode) []*HomelabIdentityJoinNode {
	result := make([]*HomelabIdentityJoinNode, 0, len(nodes))
	for index := range nodes {
		result = append(result, &nodes[index])
	}
	return result
}

// normalizedJoinHostnames adds the joining server's own domain (its joined
// core serves ca. and auth. there) and sorts the names.
func normalizedJoinHostnames(hostnames []string, domain string) []string {
	result := []string{}
	for _, host := range append(append([]string(nil), hostnames...), domain) {
		host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
		if host != "" && !slices.Contains(result, host) {
			result = append(result, host)
		}
	}
	sort.Strings(result)
	return result
}

func identityJoinInput(root string, hostnames []string) []byte {
	raw, _ := json.Marshal(stackkitcommand.IdentityJoinRequestInput{Schema: stackkitcommand.IdentityJoinRequestInputSchema, AuthorityRoot: root, Hostnames: hostnames})
	return raw
}

// HomelabIdentityJoinPayload is the persisted job payload of a join.
func HomelabIdentityJoinPayload(req HomelabIdentityJoinRequest) (map[string]interface{}, error) {
	encoded, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(encoded, &payload); err != nil {
		return nil, err
	}
	return map[string]interface{}{homelabIdentityJoinPayloadField: payload}, nil
}

func homelabIdentityJoinRequestFromJob(job *Job) (HomelabIdentityJoinRequest, error) {
	var req HomelabIdentityJoinRequest
	encoded, err := json.Marshal(job.Snapshot().Payload[homelabIdentityJoinPayloadField])
	if err != nil {
		return req, err
	}
	if err := json.Unmarshal(encoded, &req); err != nil {
		return req, fmt.Errorf("decode homelab identity join payload: %w", err)
	}
	return req, nil
}

// RegisterHomelabIdentityJoinHandler registers the join with the lifecycle
// configuration: its sender, the nodes' releases and Wallet host delivery.
func RegisterHomelabIdentityJoinHandler(q *Queue, cfg StackKitLifecycleConfig) {
	q.RegisterHandler(JobTypeHomelabIdentityJoin, HomelabIdentityJoinHandler(cfg))
}

// HomelabIdentityJoinHandler runs a join. It is never retried: an Owner
// approval is used once, and a partial join is resolved by running the join
// again after `stackkit identity join leave` on the joining server.
func HomelabIdentityJoinHandler(cfg StackKitLifecycleConfig) JobHandler {
	return func(ctx context.Context, job *Job, q *Queue) error {
		if cfg.Sender == nil {
			return NewConfigError(fmt.Errorf("typed StackKits dispatcher is not configured"))
		}
		req, err := homelabIdentityJoinRequestFromJob(job)
		if err == nil {
			req, err = NormalizeHomelabIdentityJoinRequest(req)
		}
		if err != nil {
			return NewPermanentError(err)
		}
		outcome := map[string]interface{}{
			"homelab_id": req.HomelabID, "authority_stack_id": req.Authority.StackID, "joining_stack_id": req.Joining.StackID,
			"authority_root": req.AuthorityRoot,
		}
		summary := map[string]interface{}{"schema_version": "homelab.homelab-identity-join-result/v1", HomelabIdentityJoinResultField: outcome,
			// The enqueue replay check reads the intent digest back.
			"identity_join_request_sha256": job.Snapshot().Result["identity_join_request_sha256"]}
		err = runHomelabIdentityJoin(ctx, cfg, job, q, req, outcome)
		summary["success"] = err == nil
		job.replaceResult(summary)
		if err != nil {
			var config *aiProviderConfigError
			if errors.As(err, &config) {
				return NewConfigError(config.err)
			}
			return NewPermanentError(err)
		}
		q.UpdateProgress(job.ID, 100, "The server joined the homelab's Authority")
		return nil
	}
}

type identityJoinRun struct {
	ctx     context.Context
	cfg     StackKitLifecycleConfig
	job     *Job
	req     HomelabIdentityJoinRequest
	release map[string]stackkitrelease.Release
}

func (r *identityJoinRun) node(node HomelabIdentityJoinNode, step string, document []byte) StackKitLifecycleRequest {
	return StackKitLifecycleRequest{
		StackID: node.StackID, StackKitInstanceID: node.StackKitInstanceID, TenantID: r.req.TenantID, OwnerID: r.req.OwnerID,
		AgentID: node.AgentID, NodeID: node.NodeID, NodeRef: node.NodeRef, StackKit: node.StackKit, StackName: node.StackName,
		WorkingDirectory: node.WorkingDirectory, SpecPath: node.SpecPath, Operation: StackKitLifecycleFederationHandoff,
		OwnerApproved: true, HandoffStep: step, HandoffDocument: document,
	}
}

func (r *identityJoinRun) step(role string, node HomelabIdentityJoinNode, step string, document []byte) ([]byte, error) {
	release, ok := r.release[node.StackID]
	if !ok {
		return nil, fmt.Errorf("no admitted StackKits release for the %s server", role)
	}
	result, err := dispatchModernHandoff(r.ctx, r.cfg.Sender, r.job.ID+"-"+role+"-"+step, r.node(node, step, document), release)
	if err != nil {
		return nil, err
	}
	return result.Document, nil
}

func (r *identityJoinRun) resolveReleases() error {
	r.release = map[string]stackkitrelease.Release{}
	for _, node := range append([]HomelabIdentityJoinNode{r.req.Authority, r.req.Joining}, r.req.Others...) {
		var release *stackkitrelease.Release
		var err error
		if r.cfg.releaseResolver != nil {
			release, err = r.cfg.releaseResolver()
		} else {
			release, err = TargetStackKitReleaseForStack(r.ctx, r.req.TenantID, node.StackID)
		}
		if err != nil || release == nil {
			return &aiProviderConfigError{fmt.Errorf("pinned StackKits release is unavailable for stack %s", node.StackID)}
		}
		if version := release.Receipt().Version; !stackkitcommand.ReleaseAtLeast(version, stackkitcommand.MinIdentityJoinRelease) {
			return &aiProviderConfigError{fmt.Errorf("joining servers needs StackKits %s or newer on every server; stack %s pins %s", stackkitcommand.MinIdentityJoinRelease, node.StackID, version)}
		}
		r.release[node.StackID] = *release
	}
	return nil
}

type identityJoinSite struct {
	Role      string   `json:"role"`
	Domain    string   `json:"domain"`
	Address   string   `json:"address"`
	ServerRef string   `json:"serverRef,omitempty"`
	Hostnames []string `json:"hostnames,omitempty"`
}

func (r *identityJoinRun) site(role string, node HomelabIdentityJoinNode) (identityJoinSite, error) {
	raw, err := r.step(role, node, stackkitcommand.HandoffIdentityZoneSite, nil)
	if err != nil {
		return identityJoinSite{}, err
	}
	var site identityJoinSite
	if json.Unmarshal(raw, &site) != nil || site.Domain == "" || site.Address == "" {
		return identityJoinSite{}, fmt.Errorf("the %s server reported no zone site", role)
	}
	return site, nil
}

func runHomelabIdentityJoin(ctx context.Context, cfg StackKitLifecycleConfig, job *Job, q *Queue, req HomelabIdentityJoinRequest, outcome map[string]interface{}) error {
	run := &identityJoinRun{ctx: ctx, cfg: cfg, job: job, req: req}
	if err := run.resolveReleases(); err != nil {
		return err
	}
	if cfg.WalletHostDelivery == nil || cfg.IdentityJoinWallet == nil {
		// The client secret reaches the server only through the Wallet; check
		// that before the Owner approves anything.
		return &aiProviderConfigError{fmt.Errorf("the homelab's Wallet host delivery is not configured")}
	}

	job.setStep("identity_join_site")
	q.UpdateProgress(job.ID, 5, "Reading the joining server's domain and LAN address")
	joining, err := run.site("joining", req.Joining)
	if err != nil {
		return err
	}
	if joining.Role != "authority" {
		return fmt.Errorf("the joining server already joined an Authority; run 'stackkit identity join leave' there first")
	}
	hostnames := normalizedJoinHostnames(req.Hostnames, joining.Domain)
	outcome["hostnames"] = hostnames

	job.setStep("identity_join_request")
	q.UpdateProgress(job.ID, 10, "Preparing the join request on the joining server")
	request, err := run.step("joining", req.Joining, stackkitcommand.HandoffIdentityJoinRequest, identityJoinInput(req.AuthorityRoot, hostnames))
	if err != nil {
		return err
	}
	var requested struct {
		Server struct {
			OwnerRef string `json:"ownerRef"`
		} `json:"server"`
	}
	if json.Unmarshal(request, &requested) != nil || requested.Server.OwnerRef == "" {
		return errors.New("the joining server returned no join request")
	}
	outcome["server_ref"] = requested.Server.OwnerRef

	job.setStep("identity_join_approve")
	q.UpdateProgress(job.ID, 20, "Waiting for the Owner's passkey approval in the Authority's Hub")
	bundle, err := run.step("authority", req.Authority, stackkitcommand.HandoffIdentityJoinApprove, request)
	if err != nil {
		return err
	}
	var trust struct {
		PocketID struct {
			Issuer string `json:"issuer"`
		} `json:"pocketId"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	if json.Unmarshal(bundle, &trust) != nil || trust.PocketID.Issuer == "" {
		return errors.New("the Authority returned no trust bundle")
	}
	outcome["bundle_expires_at"] = trust.ExpiresAt

	job.setStep("identity_join_install")
	q.UpdateProgress(job.ID, 40, "Installing the Authority's trust on the joining server")
	installed, err := run.step("joining", req.Joining, stackkitcommand.HandoffIdentityJoinInstall, bundle)
	if err != nil {
		return err
	}
	var installation struct {
		RootFingerprint   string `json:"rootFingerprint"`
		VerificationState string `json:"verificationState"`
	}
	if json.Unmarshal(installed, &installation) != nil || installation.RootFingerprint != req.AuthorityRoot {
		return errors.New("the joining server did not install the trust under the root the Owner entered")
	}
	outcome["verification_state"] = installation.VerificationState

	job.setStep("identity_join_client")
	q.UpdateProgress(job.ID, 55, "Registering the joined server's sign-in client at the Authority")
	itemID, clientID, err := run.client(requested.Server.OwnerRef, "auth."+joining.Domain, trust.PocketID.Issuer, q, outcome)
	if err != nil {
		return err
	}
	outcome["client_id"], outcome["wallet_item_id"] = clientID, itemID

	job.setStep("identity_join_client_bind")
	q.UpdateProgress(job.ID, 75, "Binding the joined server's TinyAuth to the Authority's Pocket ID")
	binding, _ := json.Marshal(map[string]string{
		"schema": "stackkit.homelab-oidc-client-binding/v1", "clientId": clientID, "issuer": trust.PocketID.Issuer,
		"tenantRef": stackkitcommand.DeliveryTenantRef(req.TenantID), "itemRef": itemID,
	})
	if _, err := run.step("joining", req.Joining, stackkitcommand.HandoffIdentityClientBind, binding); err != nil {
		return err
	}

	job.setStep("identity_join_zone")
	q.UpdateProgress(job.ID, 85, "Assembling the homelab DNS zone across the servers")
	zone, err := run.zone(requested.Server.OwnerRef)
	if err != nil {
		return err
	}
	outcome["zone"] = zone
	return nil
}

// client has the Authority register the joined server's client with its
// secret sealed to a carrier key that exists only in this call, opens it, and
// hands it to the Wallet, which delivers it to the joined server.
func (r *identityJoinRun) client(serverRef, tinyAuthHost, issuer string, q *Queue, outcome map[string]interface{}) (string, string, error) {
	carrier, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	request, _ := json.Marshal(map[string]string{
		"schema": "stackkit.homelab-oidc-client-request/v1", "serverRef": serverRef, "tinyAuthHost": tinyAuthHost,
		"carrierKey": base64.RawURLEncoding.EncodeToString(carrier.PublicKey().Bytes()),
	})
	raw, err := r.step("authority", r.req.Authority, stackkitcommand.HandoffIdentityClientRegister, request)
	if err != nil {
		return "", "", err
	}
	secret, client, err := openIdentityJoinClient(raw, carrier, serverRef, issuer)
	if err != nil {
		return "", "", err
	}
	defer clear(secret)
	itemID, versioned, err := r.cfg.IdentityJoinWallet.StoreIdentityClientSecret(r.ctx, r.req.OwnerID, r.req.Joining.StackID, client.ClientID, secret)
	if err != nil {
		return "", "", fmt.Errorf("the homelab's Wallet did not store the joined server's client secret: %w", err)
	}
	base := r.node(r.req.Joining, "", nil)
	base.Operation, base.HandoffStep, base.HandoffDocument = "", "", nil
	base.WalletItemID, base.WalletItemRotate = itemID, versioned
	delivered := map[string]interface{}{}
	target, version, err := deliverWalletItem(r.ctx, r.cfg, r.job, q, base, r.release[r.req.Joining.StackID], delivered)
	outcome["wallet_delivery"] = delivered
	if err != nil {
		return "", "", fmt.Errorf("the client secret did not reach the joined server through the Wallet: %w", err)
	}
	delivered["host_target"], delivered["item_version"] = hostTargetMap(target), version
	return itemID, client.ClientID, nil
}

type identityJoinClient struct {
	Schema           string    `json:"schema"`
	AuthoritySiteRef string    `json:"authoritySiteRef"`
	ServerRef        string    `json:"serverRef"`
	ClientID         string    `json:"clientId"`
	Issuer           string    `json:"issuer"`
	CallbackURL      string    `json:"callbackUrl"`
	CarrierKeySHA256 string    `json:"carrierKeySha256"`
	SealedSecret     string    `json:"sealedSecret"`
	IssuedAt         time.Time `json:"issuedAt"`
}

// openIdentityJoinClient opens the sealed secret under the exact context the
// Authority bound it to: this server, this issuer and this carrier key.
func openIdentityJoinClient(raw []byte, carrier *ecdh.PrivateKey, serverRef, issuer string) ([]byte, identityJoinClient, error) {
	var client identityJoinClient
	carrierSum := sha256.Sum256(carrier.PublicKey().Bytes())
	if json.Unmarshal(raw, &client) != nil || client.Schema != identityJoinClientSchema || client.ServerRef != serverRef || client.Issuer != issuer ||
		client.CarrierKeySHA256 != hex.EncodeToString(carrierSum[:]) || !strings.HasPrefix(client.ClientID, "stackkit-tinyauth-") {
		return nil, client, errors.New("the Authority's client answer does not name this server, issuer and carrier key")
	}
	sealed, err := base64.RawURLEncoding.Strict().DecodeString(client.SealedSecret)
	if err != nil {
		return nil, client, errors.New("the Authority's client secret is not sealed")
	}
	key, err := hpke.NewDHKEMPrivateKey(carrier)
	if err != nil {
		return nil, client, err
	}
	info := []byte(identityJoinClientCapture + strings.Join([]string{
		client.AuthoritySiteRef, client.ServerRef, client.ClientID, client.Issuer, client.CallbackURL,
		client.CarrierKeySHA256, client.IssuedAt.UTC().Format(time.RFC3339),
	}, "\n"))
	secret, err := hpke.Open(key, hpke.HKDFSHA256(), hpke.ChaCha20Poly1305(), info, sealed)
	if err != nil || len(secret) < 32 {
		clear(secret)
		return nil, client, errors.New("the Authority's client secret does not open for this job's carrier key")
	}
	return secret, client, nil
}

type identityJoinZoneServer struct {
	ServerRef string   `json:"serverRef"`
	Address   string   `json:"address"`
	Hostnames []string `json:"hostnames"`
}

// zone assembles the homelab DNS zone from every server's own site and
// installs it on each; each server admits only what its trust vouches for
// (names the Owner approved, at the LAN address approved with them).
func (r *identityJoinRun) zone(joiningRef string) (map[string]interface{}, error) {
	authority, err := r.site("authority", r.req.Authority)
	if err != nil {
		return nil, err
	}
	if authority.Role != "authority" {
		return nil, errors.New("the homelab's Authority pointer names a server that is joined to another Authority")
	}
	members := append([]HomelabIdentityJoinNode{r.req.Joining}, r.req.Others...)
	servers := []identityJoinZoneServer{}
	unreported := []string{}
	installOn := []HomelabIdentityJoinNode{r.req.Authority}
	for index, node := range members {
		site, err := r.site(fmt.Sprintf("member%d", index), node)
		if err != nil {
			if index == 0 {
				return nil, err
			}
			unreported = append(unreported, node.StackID)
			continue
		}
		if site.Role != "joined" {
			if index == 0 {
				return nil, errors.New("the joining server does not report the joined role after install")
			}
			continue
		}
		if index == 0 && site.ServerRef != joiningRef {
			return nil, errors.New("the joining server reports another server reference than its join request")
		}
		if slices.ContainsFunc(servers, func(server identityJoinZoneServer) bool { return server.ServerRef == site.ServerRef }) {
			return nil, fmt.Errorf("two servers of the homelab report the joined server %s", site.ServerRef)
		}
		servers = append(servers, identityJoinZoneServer{ServerRef: site.ServerRef, Address: site.Address, Hostnames: site.Hostnames})
		installOn = append(installOn, node)
	}
	sort.Slice(servers, func(i, j int) bool { return servers[i].ServerRef < servers[j].ServerRef })
	document, _ := json.Marshal(map[string]interface{}{
		"schema": "stackkit.homelab-dns-zone/v1", "domain": authority.Domain, "authority": authority.Address, "servers": servers,
	})
	installed := map[string]interface{}{}
	for index, node := range installOn {
		if _, err := r.step(fmt.Sprintf("zone%d", index), node, stackkitcommand.HandoffIdentityZoneInstall, document); err != nil {
			return nil, fmt.Errorf("the homelab zone did not install on stack %s: %w", node.StackID, err)
		}
		installed[node.StackID] = true
	}
	digest := sha256.Sum256(document)
	return map[string]interface{}{
		"domain": authority.Domain, "servers": len(servers), "installed_on": installed, "unreported": unreported,
		"zone_sha256": "sha256:" + hex.EncodeToString(digest[:]),
	}, nil
}
