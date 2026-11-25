package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"os"

	"github.com/kombifyio/homelab/internal/providercontrol"
	"github.com/kombifyio/homelab/internal/routes"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/middleware"
)

func registerIONOSSharedPool(router *httpx.Router, deps routeDeps) error {
	if os.Getenv("HOMELAB_IONOS_SHARED_POOL_ENABLED") != "true" {
		return nil
	}
	if deps.startup == nil || !deps.startup.cfg.DeploymentMode.IsSaaS() || deps.v2 == nil || deps.v2.db == nil || deps.featureSvc == nil {
		return fmt.Errorf("IONOS pool requires hosted database and feature authority")
	}
	var node providercontrol.IONOSSharedPoolNode
	var apps map[string]providercontrol.IONOSPoolApplication
	if json.Unmarshal([]byte(os.Getenv("HOMELAB_IONOS_SHARED_POOL_NODE")), &node) != nil || json.Unmarshal([]byte(os.Getenv("HOMELAB_IONOS_SHARED_POOL_APPS")), &apps) != nil {
		return fmt.Errorf("IONOS pool requires admitted node and curated image configuration")
	}
	certificate, err := tls.LoadX509KeyPair(os.Getenv("HOMELAB_IONOS_SHARED_POOL_CLIENT_CERT_FILE"), os.Getenv("HOMELAB_IONOS_SHARED_POOL_CLIENT_KEY_FILE"))
	if err != nil {
		return fmt.Errorf("IONOS pool client certificate unavailable")
	}
	ca, err := os.ReadFile(os.Getenv("HOMELAB_IONOS_SHARED_POOL_CA_FILE"))
	if err != nil {
		return fmt.Errorf("IONOS pool server CA unavailable")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return fmt.Errorf("IONOS pool server CA invalid")
	}
	origin, err := url.Parse(os.Getenv("HOMELAB_IONOS_SHARED_POOL_ORIGIN"))
	if err != nil || origin.Scheme != "https" {
		return fmt.Errorf("IONOS pool requires its private WireGuard HTTPS origin")
	}
	workerIP, ipErr := netip.ParseAddr(origin.Hostname())
	if ipErr != nil || !netip.MustParsePrefix("100.64.200.0/24").Contains(workerIP) {
		return fmt.Errorf("IONOS pool requires its private WireGuard HTTPS origin")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{certificate}}, ResponseHeaderTimeout: providercontrol.IONOSSharedPoolMaxCommandDuration}
	runtime, err := providercontrol.NewIONOSSharedPoolHTTPRuntime(origin.String(), &http.Client{Transport: transport, Timeout: providercontrol.IONOSSharedPoolMaxCommandDuration})
	if err != nil {
		return err
	}
	budgetReader, err := newManagedAppBudgetReader()
	if err != nil {
		return err
	}
	admission := func(ctx context.Context, id providercontrol.IONOSPoolIdentity, req providercontrol.IONOSPoolRequest) (providercontrol.ManagedAppBudget, error) {
		grants, ok := middleware.SignedEntitlementsFromContext(ctx)
		if !ok || !grants.Has(routes.InventoryEntitlementOperate) || (req.Action == providercontrol.IONOSPoolProvision && !grants.Has(routes.InventoryEntitlementProvision)) {
			return providercontrol.ManagedAppBudget{}, &providercontrol.ManagedAppRefusal{Code: "entitlement_refused", Repair: "Obtain a current signed workload operation entitlement."}
		}
		// Cleanup still requires current signed identity/authority and an existing
		// exact reservation, but never creates capacity or calls Cloud commerce.
		if providercontrol.IsManagedAppCleanup(string(req.Action)) {
			return providercontrol.ManagedAppBudget{}, nil
		}
		enabled, err := deps.featureSvc.IsEnabled(ctx, providercontrol.IONOSSharedPoolFlag, id.SubjectID)
		if err != nil || !enabled {
			return providercontrol.ManagedAppBudget{}, &providercontrol.ManagedAppRefusal{Code: "module_disabled", Repair: "Enable the default-off IONOS Managed Apps module for this tester."}
		}
		return budgetReader.Read(ctx, id)
	}
	control, err := providercontrol.NewIONOSSharedPoolControl(deps.v2.db.DB, node, runtime, runtime, apps, admission)
	if err != nil {
		return err
	}
	routes.RegisterIONOSSharedPoolRoutes(router, control)
	return nil
}
