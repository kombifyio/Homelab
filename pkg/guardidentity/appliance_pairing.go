package guardidentity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kombifyio/techstack/pkg/pairingtoken"
)

// AppliancePairing uses the same tenant-authorized mint as the own-server wizard.
// A device login authorizes this request only; it is never the Guard credential.
func AppliancePairing(ctx context.Context, client *http.Client, endpoint, accessToken, hostname string) (string, error) {
	body, err := json.Marshal(map[string]any{"name": hostname, "expiry_minutes": 15, "server_provisioning_mode": "install-command"})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	// Never forward the user's access token to a redirect target.
	bounded := *client
	bounded.Timeout = 30 * time.Second
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := bounded.Do(req)
	if err != nil {
		return "", errors.New("the account pairing endpoint could not be reached")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("account pairing refused (HTTP %d)", response.StatusCode)
	}
	var result struct {
		Data struct {
			Token     string    `json:"token"`
			ExpiresAt time.Time `json:"expires_at"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 16<<10)).Decode(&result); err != nil {
		return "", errors.New("the account pairing response is invalid")
	}
	parsed, err := pairingtoken.Parse(strings.TrimSpace(result.Data.Token))
	if err != nil || parsed.Legacy || !result.Data.ExpiresAt.After(time.Now()) {
		return "", errors.New("the account pairing response has no current enrollment capability")
	}
	return result.Data.Token, nil
}
