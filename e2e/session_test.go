//go:build e2e

package e2e

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/hashicorp/consul/api"
)

// waitForLeaderValue waits until the trigger key is held by a live session
// and carries the expected hostname.
func waitForLeaderValue(t *testing.T, client *api.Client, want string, timeout time.Duration) *api.KVPair {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	var last *api.KVPair
	for {
		select {
		case <-ctx.Done():
			if last == nil {
				t.Fatalf("Timeout waiting for leader %q: key does not exist", want)
			}
			t.Fatalf("Timeout waiting for leader %q: value=%q session=%q", want, string(last.Value), last.Session)
		case <-ticker.C:
			pair, _, err := client.KV().Get(triggerKey, nil)
			if err != nil {
				continue
			}
			last = pair
			if pair != nil && pair.Session != "" && string(pair.Value) == want {
				return pair
			}
		}
	}
}

// waitForKeyDeleted waits until the trigger key no longer exists.
func waitForKeyDeleted(t *testing.T, client *api.Client, timeout time.Duration) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	var last *api.KVPair
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("Timeout waiting for key deletion: value=%q session=%q", string(last.Value), last.Session)
		case <-ticker.C:
			pair, _, err := client.KV().Get(triggerKey, nil)
			if err != nil {
				continue
			}
			if pair == nil {
				return
			}
			last = pair
		}
	}
}

// registerTTLCheck registers a service with a TTL check in passing state and
// returns the check ID. The service is deregistered when the test ends.
func registerTTLCheck(t *testing.T, client *api.Client, id string) string {
	t.Helper()

	checkID := id + "-check"
	err := client.Agent().ServiceRegister(&api.AgentServiceRegistration{
		ID:   id,
		Name: id,
		Check: &api.AgentServiceCheck{
			CheckID: checkID,
			TTL:     "10m",
			Status:  api.HealthPassing,
		},
	})
	if err != nil {
		t.Fatalf("Failed to register service %s: %v", id, err)
	}
	t.Cleanup(func() {
		client.Agent().ServiceDeregister(id)
	})

	return checkID
}

// TestLockHeldByConfiguredSession verifies that the lock is held by the
// session vip-elector configures, not by a default session created implicitly
// by the Consul Lock API. Only the configured session carries the TTL,
// Behavior=delete, lock-delay and the service check.
func TestLockHeldByConfiguredSession(t *testing.T) {
	client := setupConsulClient(t)
	checkID := registerTTLCheck(t, client, "vip-elector-e2e-session")

	_, cleanup := startElector(t, "node1", checkID)
	defer cleanup()

	pair := waitForLeaderValue(t, client, "node1", 10*time.Second)

	session, _, err := client.Session().Info(pair.Session, nil)
	if err != nil {
		t.Fatalf("Failed to get session info: %v", err)
	}
	if session == nil {
		t.Fatalf("Session %s holding the lock does not exist", pair.Session)
	}

	if session.Name != "vip-elector-node1" {
		t.Errorf("Expected session name 'vip-elector-node1', got %q", session.Name)
	}
	if session.Behavior != api.SessionBehaviorDelete {
		t.Errorf("Expected Behavior=%q, got %q", api.SessionBehaviorDelete, session.Behavior)
	}
	if session.TTL != "10s" {
		t.Errorf("Expected TTL=10s, got %q", session.TTL)
	}
	if session.LockDelay != 100*time.Millisecond {
		t.Errorf("Expected LockDelay=100ms, got %v", session.LockDelay)
	}
	if len(session.ServiceChecks) != 1 || session.ServiceChecks[0].ID != checkID {
		t.Errorf("Expected ServiceChecks=[%s], got %+v", checkID, session.ServiceChecks)
	}
}

// TestSessionRenewedBeyondTTL verifies that the leader keeps its session
// alive. Consul expires an unrenewed session after at most twice its TTL.
func TestSessionRenewedBeyondTTL(t *testing.T) {
	client := setupConsulClient(t)

	_, cleanup := startElector(t, "node1", "")
	defer cleanup()

	before := waitForLeaderValue(t, client, "node1", 10*time.Second)

	// TTL is 10s; wait past 2x TTL plus margin.
	time.Sleep(25 * time.Second)

	after, _, err := client.KV().Get(triggerKey, nil)
	if err != nil {
		t.Fatalf("Failed to get key: %v", err)
	}
	if after == nil {
		t.Fatal("Lock key was deleted: session was not renewed")
	}
	if after.Session != before.Session {
		t.Errorf("Lock session changed from %s to %s: session was not renewed", before.Session, after.Session)
	}
}

// TestHealthCheckFailover verifies that a critical health check on the
// leader removes the key and lets a standby take over.
func TestHealthCheckFailover(t *testing.T) {
	client := setupConsulClient(t)
	checkID := registerTTLCheck(t, client, "vip-elector-e2e-health")

	// node1 is tied to the health check, node2 is not.
	_, cleanup1 := startElector(t, "node1", checkID)
	defer cleanup1()

	waitForLeaderValue(t, client, "node1", 10*time.Second)

	_, cleanup2 := startElector(t, "node2", "")
	defer cleanup2()

	time.Sleep(2 * time.Second)

	t.Log("Marking health check as critical")
	if err := client.Agent().UpdateTTL(checkID, "e2e failure", api.HealthCritical); err != nil {
		t.Fatalf("Failed to update check: %v", err)
	}

	// The key must be deleted (not just released), so that vip-manager on
	// the old leader stops seeing its own hostname before node2 takes over.
	waitForKeyDeleted(t, client, 5*time.Second)

	waitForLeaderValue(t, client, "node2", 15*time.Second)
}

// TestLeaderCrashFailover verifies that a standby takes over when the leader
// process dies without cleaning up. This relies on the session TTL.
func TestLeaderCrashFailover(t *testing.T) {
	client := setupConsulClient(t)

	cmd1, cleanup1 := startElector(t, "node1", "")
	defer cleanup1()

	waitForLeaderValue(t, client, "node1", 10*time.Second)

	_, cleanup2 := startElector(t, "node2", "")
	defer cleanup2()

	time.Sleep(2 * time.Second)

	t.Log("Killing leader node1 with SIGKILL")
	if err := cmd1.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("Failed to kill node1: %v", err)
	}

	// TTL 10s: Consul expires the session after at most 2x TTL, then
	// lock-delay and the lock retry interval (5s) apply.
	waitForLeaderValue(t, client, "node2", 40*time.Second)
}

// TestGracefulShutdownDeletesKey verifies that a graceful shutdown removes
// the key instead of leaving the old hostname behind without a session.
func TestGracefulShutdownDeletesKey(t *testing.T) {
	client := setupConsulClient(t)

	cmd, cleanup := startElector(t, "node1", "")
	defer cleanup()

	waitForLeaderValue(t, client, "node1", 10*time.Second)

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("Failed to send SIGINT: %v", err)
	}

	waitForKeyDeleted(t, client, 10*time.Second)
}
