// Package cloudlogin holds Homelab's self-hosted cloud-login gate. The
// enrollment-token evaluation lives in the shared
// [goCloudlogin] package in kombify-go-common; this package keeps only the
// Homelab-specific defaults and wrappers that differ from the shared defaults.
//
// Migration history: this package was the original donor of cloudlogin
// (lifted 2026-05-03 as part of the kombify auth standardization). It used to
// re-export the upstream surface so old import paths kept working; those
// pass-through re-exports are gone, and callers that need only upstream
// symbols import `github.com/kombifyio/homelab/internal/gocommon/cloudlogin`
// directly.
package cloudlogin

import (
	goCloudlogin "github.com/kombifyio/homelab/internal/gocommon/cloudlogin"

	"github.com/kombifyio/homelab/pkg/config"
	"github.com/kombifyio/homelab/pkg/fulllocal"
)

// DefaultAudience preserves Homelab's historic audience constant
// ("kombify-homelab:selfhosted-cloud-login") instead of the more generic
// kombify default. Existing enrollment tokens continue to verify because
// OptionsFromEnv stamps this value into ExpectedAudience when the env
// override is missing.
const DefaultAudience = "kombify-homelab:selfhosted-cloud-login"

// Evaluate applies the Full Local data protection profile gate, then defers
// to the shared evaluator. Under Full Local cloud login is a kombify service
// the homelab may not reach, so the gate stays closed whatever the token
// says.
var Evaluate = func(opts goCloudlogin.Options) goCloudlogin.Result {
	if fulllocal.Active() {
		return goCloudlogin.Result{Enabled: false, Reason: fulllocal.ReasonCode}
	}
	return goCloudlogin.Evaluate(opts)
}

// OptionsFromEnv builds gate options from the documented HOMELAB_* env
// variables and overlays the deployment mode booleans expected by the
// upstream evaluator. The signature is preserved from the donor so call
// sites in `cmd/homelab` and `internal/routes/auth` need no changes.
func OptionsFromEnv(mode config.DeploymentMode) goCloudlogin.Options {
	opts := goCloudlogin.OptionsFromEnv("HOMELAB")
	opts.IsSaaS = mode.IsSaaS()
	opts.IsSelfHosted = mode.IsSelfHosted()
	if opts.ExpectedAudience == "" {
		opts.ExpectedAudience = DefaultAudience
	}
	return opts
}
