package guardreach

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"time"
)

const gatewayRegistryPullScope = "connect:homelab-nodes:registry-pull"

var wikiRegistryToken = regexp.MustCompile(`^kpr1\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]{43}$`)
var wikiRegistryRepository = regexp.MustCompile(`^[a-z0-9-]{1,40}/kombify-knowledge-node$`)

type customerRegistryGateway interface {
	issueCustomerRegistryGrant(context.Context, Row, string) ([]byte, error)
}

func (g *Gateway) issueCustomerRegistryGrant(ctx context.Context, row Row, product string) ([]byte, error) {
	var repository, selector string
	switch product {
	case "wiki":
	case "paperwork":
		return nil, errors.New("Paperwork customer registry is unavailable in this build")
	default:
		return nil, errors.New("customer registry product is not admitted")
	}
	body, err := json.Marshal(struct {
		ServerID   string `json:"server_id"`
		WorkerID   string `json:"worker_id"`
		Generation int64  `json:"binding_generation"`
		Product    string `json:"product,omitempty"`
	}{row.ServerID, row.WorkerID, row.Generation, selector})
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if _, err := g.do(ctx, gatewayRegistryPullScope, http.MethodPost, gatewayNodesPath+"/registry-pull", body, g.obo(row.OwnerSubjectID, row.ConnectTenantID), &raw); err != nil {
		return nil, err
	}
	var grant struct {
		Repository    string    `json:"repository"`
		RegistryToken string    `json:"registryToken"`
		ExpiresAt     time.Time `json:"expiresAt"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	now := time.Now()
	if len(raw) > 2048 || decoder.Decode(&grant) != nil || decoder.Decode(new(any)) != io.EOF ||
		!registryRepositoryMatches(grant.Repository, repository) || len(grant.RegistryToken) > 1024 ||
		!wikiRegistryToken.MatchString(grant.RegistryToken) || !grant.ExpiresAt.After(now.Add(2*time.Minute)) || grant.ExpiresAt.After(now.Add(11*time.Minute)) {
		clear(raw)
		return nil, errors.New("the Gateway answered with an invalid customer registry grant")
	}
	return raw, nil
}

func registryRepositoryMatches(actual, expected string) bool {
	if expected == "" {
		return wikiRegistryRepository.MatchString(actual)
	}
	return actual == expected
}

func (s *Service) IssueCustomerRegistryGrant(ctx context.Context, tenantID, ownerID, stackID, workerID, product string) ([]byte, error) {
	if s == nil {
		return nil, errors.New("customer registry requires a connected server")
	}
	issuer, ok := s.cfg.Connect.(customerRegistryGateway)
	if !ok {
		return nil, errors.New("customer registry grant issuer is unavailable")
	}
	row, err := s.cfg.Store.Get(ctx, tenantID, workerID)
	if err != nil {
		return nil, err
	}
	if row == nil || row.TenantID != tenantID || row.OwnerSubjectID != ownerID || row.StackID != stackID || row.WorkerID != workerID ||
		row.Desired != DesiredActive || row.Placement == "local" || row.ConnectTenantID == "" || !row.Connect.registeredAt(row.Generation) {
		return nil, errors.New("customer registry requires the owner's active Connect server binding")
	}
	identity, err := s.cfg.Identities.Resolve(ctx, tenantID, workerID, row.ServerID)
	if err != nil {
		return nil, err
	}
	if !identity.Found || !identity.WorkerActive || identity.Thumbprint != row.KeyThumbprint || identity.CredentialEpoch != row.CredentialEpoch {
		return nil, errors.New("customer registry requires the server's current Guard identity")
	}
	return issuer.issueCustomerRegistryGrant(ctx, *row, product)
}
