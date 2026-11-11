package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/consul/api"
)

func TestValidateConfig(t *testing.T) {
	tests := []struct {
		name      string
		ttl       time.Duration
		lockDelay time.Duration
		wantErr   string
	}{
		{name: "defaults", ttl: 10 * time.Second, lockDelay: time.Second},
		{name: "zero lock delay is allowed", ttl: 10 * time.Second, lockDelay: 0},
		// Consul treats a zero TTL as "no TTL", so the session never expires.
		{name: "zero ttl", ttl: 0, lockDelay: time.Second, wantErr: "--ttl"},
		{name: "negative ttl", ttl: -time.Second, lockDelay: time.Second, wantErr: "--ttl"},
		{name: "negative lock delay", ttl: 10 * time.Second, lockDelay: -time.Second, wantErr: "--lock-delay"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateConfig(&Config{TTL: tt.ttl, LockDelay: tt.lockDelay})
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("expected no error, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("expected error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestBuildSessionEntry(t *testing.T) {
	entry := buildSessionEntry(&Config{
		TTL:       10 * time.Second,
		LockDelay: 2 * time.Second,
		CheckID:   "vip-http",
	}, "node-a")

	if entry.Name != "vip-elector-node-a" {
		t.Errorf("expected name=vip-elector-node-a, got %s", entry.Name)
	}
	if entry.Behavior != api.SessionBehaviorDelete {
		t.Errorf("expected behavior=delete, got %s", entry.Behavior)
	}
	if entry.TTL != "10s" {
		t.Errorf("expected TTL=10s, got %s", entry.TTL)
	}
	if entry.LockDelay != 2*time.Second {
		t.Errorf("expected LockDelay=2s, got %v", entry.LockDelay)
	}
	if len(entry.Checks) != 1 || entry.Checks[0] != "serfHealth" {
		t.Errorf("expected Checks=[serfHealth], got %v", entry.Checks)
	}
	if len(entry.ServiceChecks) != 1 || entry.ServiceChecks[0].ID != "vip-http" {
		t.Errorf("expected ServiceChecks=[{ID:vip-http}], got %v", entry.ServiceChecks)
	}
}

func TestBuildSessionEntry_ZeroLockDelay(t *testing.T) {
	entry := buildSessionEntry(&Config{TTL: 10 * time.Second, LockDelay: 0}, "node-a")

	// A zero LockDelay is dropped from the request by the Consul client and
	// the server then applies its 15s default.
	if entry.LockDelay != time.Millisecond {
		t.Errorf("expected LockDelay=1ms for --lock-delay=0s, got %v", entry.LockDelay)
	}
}

func TestDestroySessions_ReturnsFailedOnes(t *testing.T) {
	var destroyed []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/v1/session/destroy/")
		if r.Method != http.MethodPut || id == r.URL.Path {
			http.NotFound(w, r)
			return
		}
		if id == "unreachable" {
			http.Error(w, "rpc error making call: i/o deadline reached", http.StatusInternalServerError)
			return
		}
		destroyed = append(destroyed, id)
		w.Write([]byte("true"))
	}))
	defer srv.Close()

	client, err := api.NewClient(&api.Config{Address: srv.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	remaining := destroySessions(client, []string{"s1", "unreachable", "s2"})

	if len(remaining) != 1 || remaining[0] != "unreachable" {
		t.Errorf("expected remaining=[unreachable], got %v", remaining)
	}
	if len(destroyed) != 2 || destroyed[0] != "s1" || destroyed[1] != "s2" {
		t.Errorf("expected s1 and s2 to be destroyed, got %v", destroyed)
	}

	if got := destroySessions(client, nil); len(got) != 0 {
		t.Errorf("expected no remaining sessions for empty input, got %v", got)
	}
}

func TestDefaultLockDelayExceedsVipManagerInterval(t *testing.T) {
	// vip-manager polls the trigger key every 1000ms by default. The lock
	// delay must be longer, or another node can acquire the lock before the
	// old leader's vip-manager has removed the VIP.
	const vipManagerDefaultInterval = time.Second

	lockDelay, err := time.ParseDuration(defaultLockDelay)
	if err != nil {
		t.Fatalf("defaultLockDelay %q is not a duration: %v", defaultLockDelay, err)
	}
	if lockDelay <= vipManagerDefaultInterval {
		t.Errorf("defaultLockDelay=%v must exceed vip-manager's default interval %v", lockDelay, vipManagerDefaultInterval)
	}
}
