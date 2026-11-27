package main

import (
	"context"
	"errors"

	commonauthlocal "github.com/kombifyio/homelab/internal/gocommon/authlocal"
	"github.com/kombifyio/homelab/internal/gocommon/authsession"
	"github.com/kombifyio/homelab/internal/gocommon/identity"
	"github.com/kombifyio/homelab/internal/routes"
	"github.com/kombifyio/homelab/pkg/config"
)

// Local reveal reuses the current login authority and never issues another
// session, accepts a caller-selected identity, or changes hosted proof policy.
func newWalletLocalReauth(mode config.DeploymentMode, service *commonauthlocal.Service) routes.WalletLocalReauth {
	if !mode.IsSelfHosted() || service == nil {
		return nil
	}
	return func(ctx context.Context, ownerID, password string) error {
		current, err := authsession.ClaimsFrom(ctx)
		effective := identity.FromContext(ctx)
		if err != nil || current.Subject != ownerID || current.TenantID == "" ||
			current.Provider != commonauthlocal.DefaultProviderID || password == "" ||
			effective == nil || effective.UserID != ownerID || effective.OrgID != current.TenantID {
			return errors.New("local owner re-authentication required")
		}
		verified, err := service.Authenticate(ctx, current.Email, password)
		if err != nil {
			return err
		}
		if verified.Subject != current.Subject || verified.TenantID != current.TenantID ||
			verified.Provider != current.Provider {
			return errors.New("local re-authentication does not match the current owner session")
		}
		return nil
	}
}
