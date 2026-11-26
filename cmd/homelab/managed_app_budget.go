package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/kombifyio/homelab/internal/gocommon/servicecall"
	"github.com/kombifyio/homelab/internal/providercontrol"
	"github.com/kombifyio/homelab/pkg/serviceauth"
)

const managedAppBudgetPath = "/api/v1/internal/entitlements/budgets"

type managedAppBudgetReader struct {
	origin string
	client *http.Client
}

func newManagedAppBudgetReader() (*managedAppBudgetReader, error) {
	config, err := serviceauth.Apply(servicecall.Config{})
	if err != nil || config.SigningKey == nil {
		return nil, fmt.Errorf("managed App budgets require Homelab's Ed25519 service identity")
	}
	origin := strings.TrimRight(os.Getenv("HOMELAB_CLOUD_URL"), "/")
	if origin == "" {
		origin = "https://cloud.kombify.io"
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" {
		return nil, fmt.Errorf("managed App budgets require the Cloud HTTPS origin")
	}
	return &managedAppBudgetReader{origin: origin, client: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (r *managedAppBudgetReader) Read(ctx context.Context, id providercontrol.ManagedAppIdentity) (providercontrol.ManagedAppBudget, error) {
	var budget providercontrol.ManagedAppBudget
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return budget, err
	}
	requestID := hex.EncodeToString(nonce)
	binding, err := servicecall.NewRequestBinding(http.MethodGet, managedAppBudgetPath, nil)
	if err != nil {
		return budget, err
	}
	now := time.Now()
	// The common machine-only IssueBoundToken deliberately omits OBO. This
	// budget contract requires both; sign the canonical typed service claims.
	payload, err := json.Marshal(servicecall.Claims{Iss: "kombify-homelab", Aud: "kombify-cloud", Svc: "homelab", Scope: "entitlements.managed-app-slots:read", Iat: now.Unix(), Exp: now.Add(60 * time.Second).Unix(), RequestID: requestID, RequestBinding: &binding, OnBehalfOf: &servicecall.OnBehalfOf{Sub: id.SubjectID, OrgID: id.TenantID}})
	if err != nil {
		return budget, err
	}
	token, ok, err := serviceauth.SignEdDSA(payload)
	if err != nil || !ok {
		return budget, fmt.Errorf("managed App budget signing unavailable")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.origin+managedAppBudgetPath, nil)
	if err != nil {
		return budget, err
	}
	request.Header.Set(servicecall.HeaderServiceAuth, token)
	request.Header.Set("X-Request-ID", requestID)
	response, err := r.client.Do(request)
	if err != nil {
		return budget, fmt.Errorf("cloud Managed App budget unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return budget, &providercontrol.ManagedAppRefusal{Code: "budget_refused", Repair: "Restore the account's authoritative Cloud Managed App allowance."}
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&budget) != nil || decoder.Decode(new(any)) != io.EOF || budget.AccountID == "" || budget.Subject != id.SubjectID || budget.RequestedTenantID != id.TenantID || budget.RequestID != requestID || budget.Limit < 0 || budget.Limit > 100000 || !budget.ValidUntil.After(time.Now()) || budget.ValidUntil.After(time.Now().Add(30*time.Second)) {
		return providercontrol.ManagedAppBudget{}, fmt.Errorf("cloud Managed App budget binding or freshness invalid")
	}
	return budget, nil
}
