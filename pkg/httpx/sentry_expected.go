//go:build !kombify_full_local

package httpx

import "net/http"

// expectedUnavailableStatus reports statuses that mean "retry later" by
// contract (503, 504). The response, the warning log and the dependency
// failure record already tell the caller and the operator; they are not
// error events.
func expectedUnavailableStatus(status int) bool {
	return status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}
