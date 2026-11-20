// Package cloudlogin is a thin compatibility shim around the shared
// [goCloudlogin] package in kombify-go-common.
//
// Migration history: this package was the original donor of cloudlogin
// (lifted 2026-05-03 as part of the kombify auth standardization). It now
// re-exports the upstream surface so existing Homelab import paths
// (`pkg/cloudlogin`) keep working without behavioral change.
//
// New code SHOULD import `github.com/kombifyio/homelab/internal/gocommon/cloudlogin`
// directly. This shim will be removed once all call sites have been
// migrated; track the cleanup in Beads.
package cloudlogin

import (
	goCloudlogin "github.com/kombifyio/homelab/internal/gocommon/cloudlogin"

	"github.com/kombifyio/homelab/pkg/config"
	"github.com/kombifyio/homelab/pkg/fulllocal"
)

// FeatureKey identifies the enrollment claim feature this gate consumes.
const FeatureKey = goCloudlogin.FeatureKey

// DefaultAudience preserves Homelab's historic audience constant
// ("kombify-homelab:selfhosted-cloud-login") instead of the more generic
// kombify default. Existing enrollment tokens continue to verify because
// OptionsFromEnv stamps this value into ExpectedAudience when the env
// override is missing.
const DefaultAudience = "kombify-homelab:selfhosted-cloud-login"

// Type aliases — Options and Result are upstream structs.
type (
	Options = goCloudlogin.Options
	Result  = goCloudlogin.Result
)

// Evaluate is re-exported as a function value so callers that previously
// took a function reference (e.g. for tests) continue to compile. Under the
// Full Local data protection profile cloud login is a kombify service the
// homelab may not reach, so the gate stays closed whatever the token says.
var Evaluate = func(opts Options) Result {
	if fulllocal.Active() {
		return Result{Enabled: false, Reason: fulllocal.ReasonCode}
	}
	return goCloudlogin.Evaluate(opts)
}

// OptionsFromEnv builds gate options from the documented HOMELAB_* env
// variables and overlays the deployment mode booleans expected by the
// upstream evaluator. The signature is preserved from the donor so call
// sites in `cmd/homelab` and `internal/routes/auth` need no changes.
func OptionsFromEnv(mode config.DeploymentMode) Options {
	opts := goCloudlogin.OptionsFromEnv("HOMELAB")
	opts.IsSaaS = mode.IsSaaS()
	opts.IsSelfHosted = mode.IsSelfHosted()
	if opts.ExpectedAudience == "" {
		opts.ExpectedAudience = DefaultAudience
	}
	return opts
}
