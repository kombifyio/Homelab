package specv2

import "testing"

// The intent contract is closed: a decision may only travel with the goal it
// belongs to. Which ids and values are legal is the catalog's call and is
// checked by the handler; this only holds the shape rule the contract itself
// states.
func TestIntentUseCaseSettingsRequireTheirGoal(t *testing.T) {
	intent := foundIntent("ok", "photos")
	intent.UseCaseSettings = map[string]map[string]any{
		"photos": {"machine-learning": false},
	}
	if err := intent.Validate(); err != nil {
		t.Fatalf("settings for a selected goal must validate: %v", err)
	}

	intent.UseCaseSettings = map[string]map[string]any{
		"media": {"hardware-transcoding": "on"},
	}
	if err := intent.Validate(); err == nil {
		t.Fatal("settings for a use case that is not a goal must be rejected")
	}

	intent.UseCaseSettings = map[string]map[string]any{"photos": {}}
	if err := intent.Validate(); err == nil {
		t.Fatal("an entry without settings must be rejected")
	}
}

// A stored narrowing must use the same key after goal-list normalization, or
// a later run could miss it and dispatch with broader defaults.
func TestUseCaseAccessSurvivesGoalCaseNormalization(t *testing.T) {
	intent := foundIntent("ok", "Photos")
	decision := UseCaseAccessIntent{Mode: AccessModeLocal, Audience: "owner"}
	intent.UseCaseAccess = map[string]UseCaseAccessIntent{"Photos": decision}
	if err := intent.Validate(); err == nil {
		t.Fatal("noncanonical stored decision key must be rejected")
	}
	intent.UseCaseAccess = map[string]UseCaseAccessIntent{"photos": decision}
	if err := intent.Validate(); err != nil {
		t.Fatal(err)
	}
	intent.Goals = []string{"photos"}
	if err := intent.Validate(); err != nil || len(intent.PendingUseCaseAccess()) != 0 {
		t.Fatalf("normalized supported access remained blocked: %v", err)
	}
	intent.UseCaseAccess["photos"] = UseCaseAccessIntent{Mode: "inherit"}
	if err := intent.Validate(); err != nil || len(intent.PendingUseCaseAccess()) != 0 {
		t.Fatalf("explicit withdrawal remained blocked: %v", err)
	}
}
