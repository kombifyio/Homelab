package edgeauth

import (
	"encoding/json"
)

// BudgetUnavailable is a value inside the existing signed budgets object. It
// never adds a header, signature element or alternate signing format. A valid
// resolved value (including a valid cache fallback) must not use this marker.
type BudgetUnavailable struct {
	Resolution string `json:"resolution"`
	Reason     string `json:"reason"`
}

func UnavailableBudget() BudgetUnavailable {
	return BudgetUnavailable{Resolution: "unavailable", Reason: "budget_resolution_unavailable"}
}

// IsUnavailableBudget recognizes only the exact reserved marker. Malformed
// values still fail the caller's normal grant validation; false grants nothing.
func IsUnavailableBudget(raw json.RawMessage) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 2 {
		return false
	}
	var resolution, reason string
	if json.Unmarshal(fields["resolution"], &resolution) != nil || json.Unmarshal(fields["reason"], &reason) != nil {
		return false
	}
	return resolution == "unavailable" && reason == "budget_resolution_unavailable"
}
