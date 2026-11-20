// Package routes provides REST API routes for kombifyHomelab.
// Trust routes have been refactored into the trust/ subpackage.
package routes

import (
	"github.com/kombifyio/homelab/internal/routes/trust"
	"github.com/kombifyio/homelab/pkg/httpx"
)

type TrustRouteStores = trust.RouteStores

func RegisterTrustRoutesWithStores(r *httpx.Router, stores TrustRouteStores) {
	trust.RegisterRoutesWithStores(r, stores)
}
