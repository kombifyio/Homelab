package featureflags

import (
	"encoding/json"
	"strconv"
)

// TokenBudgetPolicy is a token allowance for one account. It comes from the
// account's plan (a Gateway-signed plan budget), never from a platform role:
// two accounts on the same plan carry the same budget, whatever roles they hold.
// Tokens are the universal metering unit. Different engines may burn tokens at
// different rates (e.g. Docker=1/min, DigitalOcean=10/min).
type TokenBudgetPolicy struct {
	Enabled bool `json:"enabled"`
	// Source is the origin of the value as the caller names it, for example
	// "gateway" for a plan budget signed by the Gateway.
	Source string `json:"source,omitempty"`
	Tokens int    `json:"tokens,omitempty"`
	// Unlimited is the plan's own fact (a self-hosted installation sets it), never
	// a role's exemption.
	Unlimited bool `json:"unlimited,omitempty"`
}

func readBool(source map[string]any, keys ...string) (bool, bool) {
	for _, key := range keys {
		value, ok := source[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case bool:
			return typed, true
		case string:
			if typed == "true" {
				return true, true
			}
			if typed == "false" {
				return false, true
			}
		}
	}
	return false, false
}

func readInt(source map[string]any, keys ...string) (int, bool) {
	for _, key := range keys {
		value, ok := source[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case int:
			return typed, true
		case int64:
			return int(typed), true
		case float64:
			return int(typed), true
		case json.Number:
			i, err := typed.Int64()
			if err == nil {
				return int(i), true
			}
		case string:
			i, err := strconv.Atoi(typed)
			if err == nil {
				return i, true
			}
		}
	}
	return 0, false
}
