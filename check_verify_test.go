package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/hashicorp/consul/api"
)

type mockAgentChecks struct {
	checks map[string]*api.AgentCheck
	err    error
}

func (m *mockAgentChecks) Checks() (map[string]*api.AgentCheck, error) {
	return m.checks, m.err
}

func TestLookupAgentCheck(t *testing.T) {
	agent := &mockAgentChecks{checks: map[string]*api.AgentCheck{
		"vip-http": {CheckID: "vip-http", Status: api.HealthCritical},
	}}

	st, err := lookupAgentCheck(agent, "vip-http")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !st.Found || st.Status != api.HealthCritical {
		t.Errorf("registered check: got %+v, want found and critical", st)
	}

	st, err = lookupAgentCheck(agent, "no-such-check")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if st.Found {
		t.Errorf("missing check: got %+v, want not found", st)
	}

	// A failed query must not be reported as a missing check.
	if _, err := lookupAgentCheck(&mockAgentChecks{err: errors.New("connection refused")}, "vip-http"); err == nil {
		t.Error("query failure: expected error, got nil")
	}
}

func TestCheckWarning(t *testing.T) {
	tests := []struct {
		name         string
		st           agentCheckStatus
		err          error
		wantContains []string
	}{
		{
			name: "registered and passing",
			st:   agentCheckStatus{Found: true, Status: api.HealthPassing},
		},
		{
			// Consul accepts sessions with a warning check.
			name: "registered with warning",
			st:   agentCheckStatus{Found: true, Status: api.HealthWarning},
		},
		{
			name:         "registered but critical",
			st:           agentCheckStatus{Found: true, Status: api.HealthCritical},
			wantContains: []string{"critical", "keeps retrying"},
		},
		{
			// Not registered, or hidden from a token without service:read:
			// the agent does not tell the Go client which.
			name:         "not found",
			st:           agentCheckStatus{},
			wantContains: []string{"not found", "no session can be created", "keeps retrying", "typos", "service:read"},
		},
		{
			name:         "lookup failed",
			err:          errors.New("connection refused"),
			wantContains: []string{"Could not look up", "If it is not registered"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := checkWarning(tt.st, tt.err)
			if len(tt.wantContains) == 0 {
				if msg != "" {
					t.Errorf("expected no warning, got %q", msg)
				}
				return
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(msg, want) {
					t.Errorf("warning %q does not contain %q", msg, want)
				}
			}
			// The old message promised to run without the check.
			if strings.Contains(msg, "without service check") {
				t.Errorf("warning claims to run without the check: %q", msg)
			}
		})
	}
}

func TestSessionCreateHint(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantHint bool
	}{
		{
			// Consul 1.20: agent/consul/state/session_ce.go validateSessionChecksTxn
			name:     "missing check",
			err:      errors.New("Unexpected response code: 500 (apply failed: Missing check 'no-such-check' registration)"),
			wantHint: true,
		},
		{
			name:     "critical check",
			err:      errors.New("Unexpected response code: 500 (apply failed: Check 'vip-http' is in critical state)"),
			wantHint: true,
		},
		{
			name:     "serfHealth critical",
			err:      errors.New("Unexpected response code: 500 (apply failed: Check 'serfHealth' is in critical state)"),
			wantHint: true,
		},
		{
			name:     "permission denied",
			err:      errors.New("Unexpected response code: 403 (Permission denied: token lacks permission 'session:write')"),
			wantHint: false,
		},
		{
			name:     "connection refused",
			err:      errors.New("Put \"http://127.0.0.1:8500/v1/session/create\": dial tcp 127.0.0.1:8500: connect: connection refused"),
			wantHint: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hint := sessionCreateHint(tt.err)
			if (hint != "") != tt.wantHint {
				t.Errorf("sessionCreateHint() = %q, want hint=%v", hint, tt.wantHint)
			}
		})
	}
}
