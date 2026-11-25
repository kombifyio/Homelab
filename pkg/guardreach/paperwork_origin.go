package guardreach

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kombifyio/homelab/pkg/kombifyme"
)

const (
	paperworkService  = "paperwork"
	paperworkTarget   = "http://127.0.0.1:8080"
	paperworkProofTTL = 45 * time.Second
)

type PaperworkOriginProof struct {
	Origin     string `json:"origin"`
	ValidUntil int64  `json:"valid_until"`
}

type PaperworkRouteVerifier interface {
	VerifyPaperworkRoute(context.Context, Row, time.Time) (*PaperworkOriginProof, error)
}

var errPaperworkRouteUnverified = errors.New("guardreach: Paperwork route readback does not match the active Guard binding")

func (m *Me) VerifyPaperworkRoute(ctx context.Context, row Row, now time.Time) (*PaperworkOriginProof, error) {
	if row.Desired != DesiredActive || !row.Public.Enabled || row.Public.Service != paperworkService ||
		row.Public.TargetAddr() != paperworkTarget || !row.Me.registeredAt(row.Generation) || !strings.HasSuffix(row.Me.Ref, "."+meZoneDomain) {
		return nil, nil
	}
	address, err := kombifyme.NewManagedAddress(row.TenantID, row.OwnerSubjectID, firstNonEmpty(row.StackID, row.ServerID))
	if err != nil {
		return nil, errPaperworkRouteUnverified
	}
	name := strings.TrimSuffix(row.Me.Ref, "."+meZoneDomain)
	suffix := "-" + routeServiceName(row.TenantID, row.ServerID, paperworkService)
	if base, matched := strings.CutSuffix(name, suffix); !matched || base == "" {
		return nil, errPaperworkRouteUnverified
	}
	query := url.Values{"tenant_id": {row.TenantID}, "server_id": {row.ServerID}, "route": {name}}
	response, err := m.client.Forward(ctx, http.MethodGet, "guard/servers/paperwork-origin?"+query.Encode(), nil, "")
	if err != nil {
		return nil, errPaperworkRouteUnverified
	}
	binding, ok := response.Body.(map[string]any)
	if !ok || binding["tenant_id"] != row.TenantID || binding["server_id"] != row.ServerID ||
		binding["owner_ref"] != address.OwnerRef || binding["stack_ref"] != address.StackRef || binding["worker_id"] != row.WorkerID ||
		binding["guard_key_thumbprint"] != row.KeyThumbprint || binding["generation"] != float64(row.Generation) {
		return nil, errPaperworkRouteUnverified
	}
	origin := "https://" + row.Me.Ref
	validUntil, ok := binding["valid_until"].(float64)
	if binding["origin"] != origin || !ok || validUntil <= float64(now.Unix()) || validUntil != float64(int64(validUntil)) {
		return nil, errPaperworkRouteUnverified
	}
	return &PaperworkOriginProof{
		Origin:     origin,
		ValidUntil: min(int64(validUntil), now.Add(paperworkProofTTL).Unix()),
	}, nil
}
