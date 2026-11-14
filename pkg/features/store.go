package features

import "context"

// Store holds consent only. Administration owns per-user flag decisions.
type Store interface {
	GetUserConsentsMap(ctx context.Context, userID string, featureKeys []string) (map[string]bool, error)
	HasUserConsent(ctx context.Context, userID, featureKey string) (bool, error)
}
