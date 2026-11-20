// Package trust provides HTTP routes for worker-enrollment pairing tokens.
package trust

import (
	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/httpx"
)

type RouteStores struct {
	Stacks  controlplane.StackStore
	Workers controlplane.WorkerStore
	Jobs    controlplane.JobStore
}

func RegisterRoutesWithStores(r *httpx.Router, stores RouteStores) {
	RegisterPairingRoutesWithStores(r, stores)
}
