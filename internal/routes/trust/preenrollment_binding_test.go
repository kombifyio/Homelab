package trust

// The pre-enrollment SSH binding (platform-x21l30) that the queued enrollment of
// this package relies on.

import (
	"context"
	"testing"

	"github.com/kombifyio/homelab/internal/preenrollment"
	"github.com/kombifyio/homelab/pkg/middleware"
)

func TestPreEnrollmentAgentCallMarksMCPAndSignedNonPersonPrincipals(t *testing.T) {
	person := context.Background()
	if preenrollment.AgentCall(person) {
		t.Fatal("an unmarked request without a signed principal is the person's own")
	}
	if !preenrollment.AgentCall(preenrollment.MarkAgentCall(person)) {
		t.Fatal("a call through the MCP surface is an agent call")
	}
	for principal, agent := range map[string]bool{
		middleware.PrincipalTypeUser:          false,
		middleware.PrincipalTypeAgent:         true,
		middleware.PrincipalTypeDelegatedUser: true,
		"machine":                             true,
		"":                                    true,
	} {
		ctx := middleware.WithVerifiedStepUp(person, middleware.StepUpClaims{PrincipalType: principal})
		if preenrollment.AgentCall(ctx) != agent {
			t.Fatalf("principal %q: agent call = %v, want %v", principal, !agent, agent)
		}
	}
}

// Only the person's own request records a target as entered by them; an agent
// keeps that record only while it writes the same target, and a changed host,
// port or login drops it.
func TestPreEnrollmentRecordKeepsThePersonEnteredTargetOnlyWhileItStaysTheSame(t *testing.T) {
	person := context.Background()
	agent := preenrollment.MarkAgentCall(person)
	target := preenrollment.Target{Host: "Server.Example.Test", User: "ubuntu"}
	config := map[string]any{"server_remote_host": "server.example.test", "server_remote_user": "ubuntu"}

	preenrollment.Record(agent, config, target, true)
	if preenrollment.PersonEntered(config) {
		t.Fatal("an agent's stored target was recorded as the person's")
	}
	preenrollment.Record(person, config, target, false)
	if preenrollment.PersonEntered(config) {
		t.Fatal("a target copied from storage was recorded as entered by the person")
	}
	if preenrollment.AdmittedTarget(agent, config, target) != "" {
		t.Fatal("an agent was admitted to a target the person did not enter")
	}
	preenrollment.Record(person, config, target, true)
	if !preenrollment.PersonEntered(config) {
		t.Fatal("the person's own target was not recorded")
	}
	if preenrollment.AdmittedTarget(agent, config, target) != "server.example.test|22|ubuntu" ||
		preenrollment.AdmittedTarget(agent, config, preenrollment.Target{Host: "attacker.example.test", User: "ubuntu"}) != "" {
		t.Fatal("an agent's admission does not follow the person-entered target")
	}
	preenrollment.Record(agent, config, preenrollment.Target{Host: "server.example.test", Port: 22, User: "ubuntu"}, true)
	if !preenrollment.PersonEntered(config) {
		t.Fatal("an agent writing the same target dropped the person's record")
	}
	config["server_remote_host"] = "attacker.example.test"
	if preenrollment.PersonEntered(config) {
		t.Fatal("a changed stored host still counts as the person's target")
	}
	preenrollment.Record(agent, config, preenrollment.StoredTarget(config), false)
	if _, kept := config[preenrollment.StackConfigKey]; kept {
		t.Fatal("an agent's different target kept the person's record")
	}
}
