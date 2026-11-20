// Package openapi embeds the kombifyHomelab OpenAPI specification.
package openapi

import _ "embed"

//go:embed homelab-v1.yaml
var Spec []byte

// InternalSpec is deliberately separate from Spec so public API discovery and
// generated clients never expose servicecall-only RuntimeLease operations.
//
//go:embed homelab-internal-v1.yaml
var InternalSpec []byte
