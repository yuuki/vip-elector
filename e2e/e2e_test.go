//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/consul/api"
)

const (
	consulAddr     = "127.0.0.1:8500"
	triggerKey     = "test/vip-elector/e2e/lock"
	testConfigPath = "testdata/vip-manager.yml"
	binaryName     = "vip-elector-test"
	testTimeout    = 60 * time.Second
)

// TestMain builds the binary before running tests
func TestMain(m *testing.M) {
	// Build the binary
	buildCmd := exec.Command("go", "build", "-o", binaryName, "../.")
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to build binary: %v\n", err)
		os.Exit(1)
	}

	// Run tests
	code := m.Run()

	// Cleanup
	os.Remove(binaryName)
	os.Exit(code)
}

// setupConsulClient creates a Consul client and cleans up test keys
func setupConsulClient(t *testing.T) *api.Client {
	t.Helper()

	config := api.DefaultConfig()
	config.Address = consulAddr

	client, err := api.NewClient(config)
	if err != nil {
		t.Fatalf("Failed to create Consul client: %v", err)
	}

	// Wait for Consul to be ready
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			t.Fatal("Consul not ready within timeout")
		default:
		}

		_, err := client.Status().Leader()
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Clean up any existing test keys
	// First try to get the key to check if it exists
	pair, _, err := client.KV().Get(triggerKey, nil)
	if err != nil {
		t.Logf("Warning: Failed to check test key: %v", err)
	}

	const lockFlagValue uint64 = 0x2ddccbc058a50c18
	isLockKey := false

	if pair != nil {
		t.Logf("Setup: Found existing key - Flags=%d, Session=%s, ModifyIndex=%d", pair.Flags, pair.Session, pair.ModifyIndex)
		isLockKey = (pair.Flags == lockFlagValue)

		// If key exists, try to delete it
		_, err = client.KV().Delete(triggerKey, nil)
		if err != nil {
			t.Logf("Warning: Failed to delete test key: %v", err)
		} else {
			t.Logf("Setup: Deleted existing key")

			// If we deleted a lock key, wait for lock-delay to expire
			if isLockKey {
				t.Logf("Setup: Waiting for lock-delay to expire after deleting lock key")
				time.Sleep(1 * time.Second)  // Wait for 100ms lock-delay + large buffer
			}
		}
	} else {
		t.Logf("Setup: No existing key found")
	}

	// Extra wait to ensure any lock-delays from previous tests have fully expired
	// Even if no key exists, Consul may still enforce lock-delay on the key path
	time.Sleep(500 * time.Millisecond)

	return client
}

// startElector starts a vip-elector process with the given hostname
func startElector(t *testing.T, hostname string, checkID string) (*exec.Cmd, context.CancelFunc) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())

	configPath, err := filepath.Abs(testConfigPath)
	if err != nil {
		cancel()
		t.Fatalf("Failed to get absolute config path: %v", err)
	}

	args := []string{
		"--vip-manager-config=" + configPath,
		"--hostname=" + hostname,
		"--ttl=10s",
		"--lock-delay=100ms",
	}

	if checkID != "" {
		args = append(args, "--check-id="+checkID)
	}

	cmd := exec.CommandContext(ctx, "./"+binaryName, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("Failed to start elector: %v", err)
	}

	// Cleanup function
	cleanup := func() {
		// Send SIGINT for graceful shutdown
		if cmd.Process != nil {
			cmd.Process.Signal(os.Interrupt)
		}
		// Wait for process to exit
		cmd.Wait()
		cancel()
	}

	return cmd, cleanup
}

// waitForLeader waits until a leader is elected and returns the hostname
func waitForLeader(t *testing.T, client *api.Client, timeout time.Duration) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			t.Fatal("Timeout waiting for leader election")
		case <-ticker.C:
			pair, _, err := client.KV().Get(triggerKey, nil)
			if err != nil {
				t.Logf("Error getting key: %v", err)
				continue
			}

			if pair != nil && len(pair.Value) > 0 {
				return string(pair.Value)
			}
		}
	}
}

// waitForNoLeader waits until the leader key is deleted or session is released
func waitForNoLeader(t *testing.T, client *api.Client, timeout time.Duration) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			pair, _, err := client.KV().Get(triggerKey, nil)
			if err != nil {
				t.Logf("Error getting key on timeout: %v", err)
			} else {
				t.Logf("Final key state: pair=%+v, Session=%s", pair, pair.Session)
			}
			t.Fatal("Timeout waiting for leader to step down")
		case <-ticker.C:
			pair, _, err := client.KV().Get(triggerKey, nil)
			if err != nil {
				t.Logf("Error getting key: %v", err)
				continue
			}

			// Key deleted or session released (no longer holding lock)
			if pair == nil || pair.Session == "" {
				t.Logf("Leader stepped down: key=%v, session=%s", pair != nil, pair.Session)
				return
			}
		}
	}
}

// TestSingleNodeLeaderElection tests basic leader election with a single node
func TestSingleNodeLeaderElection(t *testing.T) {
	client := setupConsulClient(t)

	// Start a single elector
	_, cleanup := startElector(t, "node1", "")
	defer cleanup()

	// Wait for leader election
	leader := waitForLeader(t, client, 10*time.Second)
	if leader != "node1" {
		t.Errorf("Expected leader to be 'node1', got '%s'", leader)
	}

	// Verify the key has the correct lock flag
	pair, _, err := client.KV().Get(triggerKey, nil)
	if err != nil {
		t.Fatalf("Failed to get key: %v", err)
	}

	const expectedFlags uint64 = 0x2ddccbc058a50c18 // LockFlagValue
	if pair.Flags != expectedFlags {
		t.Errorf("Expected Flags=%d, got %d", expectedFlags, pair.Flags)
	}

	if pair.Session == "" {
		t.Error("Expected session ID to be set")
	}
}

// TestMultiNodeLeaderElection tests leader election with multiple nodes
func TestMultiNodeLeaderElection(t *testing.T) {
	client := setupConsulClient(t)

	// Start three electors
	_, cleanup1 := startElector(t, "node1", "")
	defer cleanup1()

	_, cleanup2 := startElector(t, "node2", "")
	defer cleanup2()

	_, cleanup3 := startElector(t, "node3", "")
	defer cleanup3()

	// Wait for leader election
	leader := waitForLeader(t, client, 10*time.Second)

	// Verify one of the nodes became leader
	validLeaders := map[string]bool{"node1": true, "node2": true, "node3": true}
	if !validLeaders[leader] {
		t.Errorf("Expected leader to be one of node1/node2/node3, got '%s'", leader)
	}

	t.Logf("Leader elected: %s", leader)

	// Verify the key is locked
	pair, _, err := client.KV().Get(triggerKey, nil)
	if err != nil {
		t.Fatalf("Failed to get key: %v", err)
	}

	if pair.Session == "" {
		t.Error("Expected session ID to be set")
	}
}

// TestLeaderFailover tests failover when leader exits
func TestLeaderFailover(t *testing.T) {
	client := setupConsulClient(t)

	// Start first elector (will become leader)
	_, cleanup1 := startElector(t, "node1", "")
	defer cleanup1()

	// Wait for leader election
	leader := waitForLeader(t, client, 10*time.Second)
	if leader != "node1" {
		t.Errorf("Expected initial leader to be 'node1', got '%s'", leader)
	}

	// Start second elector
	_, cleanup2 := startElector(t, "node2", "")
	defer cleanup2()

	// Give node2 time to attempt lock acquisition (should fail)
	time.Sleep(2 * time.Second)

	// Verify node1 is still leader
	pair, _, err := client.KV().Get(triggerKey, nil)
	if err != nil {
		t.Fatalf("Failed to get key: %v", err)
	}
	if string(pair.Value) != "node1" {
		t.Errorf("Expected leader to still be 'node1', got '%s'", string(pair.Value))
	}

	// Stop the first elector (leader)
	t.Log("Stopping leader node1")
	cleanup1()

	// Wait for failover - node2 should acquire lock
	t.Log("Waiting for new leader election after node1 shutdown")

	// Poll until we see node2 as leader
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	var newLeader string
	for {
		select {
		case <-ctx.Done():
			t.Fatal("Timeout waiting for node2 to become leader")
		case <-ticker.C:
			pair, _, err := client.KV().Get(triggerKey, nil)
			if err != nil {
				continue
			}
			if pair != nil && string(pair.Value) == "node2" {
				newLeader = "node2"
				goto done
			}
		}
	}
done:

	t.Logf("Failover successful: node1 -> %s", newLeader)
}

// TestSessionExpiration tests lock release when session expires
func TestSessionExpiration(t *testing.T) {
	client := setupConsulClient(t)

	// Start elector with short TTL
	configPath, err := filepath.Abs(testConfigPath)
	if err != nil {
		t.Fatalf("Failed to get absolute config path: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, "./"+binaryName,
		"--vip-manager-config="+configPath,
		"--hostname=node1",
		"--ttl=10s",
		"--lock-delay=100ms",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start elector: %v", err)
	}

	// Wait for leader election
	leader := waitForLeader(t, client, 10*time.Second)
	if leader != "node1" {
		t.Errorf("Expected leader to be 'node1', got '%s'", leader)
	}

	// Get session ID
	pair, _, err := client.KV().Get(triggerKey, nil)
	if err != nil {
		t.Fatalf("Failed to get key: %v", err)
	}
	sessionID := pair.Session

	// Manually destroy the session to simulate expiration
	t.Logf("Destroying session: %s", sessionID)
	_, err = client.Session().Destroy(sessionID, nil)
	if err != nil {
		t.Fatalf("Failed to destroy session: %v", err)
	}

	// Stop the process immediately to prevent re-election
	cancel()

	// Wait for process to exit
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case <-done:
		t.Log("Process exited")
	case <-time.After(5 * time.Second):
		t.Fatal("Process did not exit within timeout")
	}

	// Wait for key to be deleted (behavior: delete)
	waitForNoLeader(t, client, 10*time.Second)

	// Explicitly delete the key to ensure clean state for next test
	_, err = client.KV().Delete(triggerKey, nil)
	if err != nil {
		t.Logf("Warning: Failed to delete key after test: %v", err)
	}

	// Extra wait to ensure any re-election attempts have settled and ALL lock-delays have expired
	// The process may create multiple sessions during re-election, so we need to wait for all of them
	time.Sleep(1 * time.Second)

	// Clean up any remaining sessions to prevent lock-delay from affecting subsequent tests
	sessions, _, err := client.Session().List(nil)
	if err == nil {
		for _, sess := range sessions {
			client.Session().Destroy(sess.ID, nil)
			t.Logf("Cleaned up remaining session: %s", sess.ID)
		}
	}

	t.Log("Session expiration handled correctly")
}

// TestTriggerKeyPreflightCheck tests the pre-flight check for regular KV entries
func TestTriggerKeyPreflightCheck(t *testing.T) {
	client := setupConsulClient(t)

	// Create a regular KV entry (not a lock)
	pair := &api.KVPair{
		Key:   triggerKey,
		Value: []byte("test-value"),
		Flags: 0, // Regular KV, not lock flag
	}

	_, err := client.KV().Put(pair, nil)
	if err != nil {
		t.Fatalf("Failed to create regular KV entry: %v", err)
	}

	// Try to start elector - should fail pre-flight check
	configPath, err := filepath.Abs(testConfigPath)
	if err != nil {
		t.Fatalf("Failed to get absolute config path: %v", err)
	}

	cmd := exec.Command("./"+binaryName,
		"--vip-manager-config="+configPath,
		"--hostname=node1",
	)

	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("Expected elector to fail, but it succeeded")
	}

	// Verify error message
	outputStr := string(output)
	if !strings.Contains(outputStr, "trigger-key pre-flight check failed") {
		t.Errorf("Expected pre-flight check error, got: %s", outputStr)
	}

	if !strings.Contains(outputStr, "regular KV entry") {
		t.Errorf("Expected 'regular KV entry' in error message, got: %s", outputStr)
	}

	t.Log("Pre-flight check correctly detected regular KV entry")

	// Clean up the test key and verify deletion
	_, err = client.KV().Delete(triggerKey, nil)
	if err != nil {
		t.Logf("Warning: Failed to clean up test key: %v", err)
	}

	// Wait and verify the key is deleted
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			t.Log("Warning: Key deletion verification timed out")
			// Force delete again
			client.KV().Delete(triggerKey, nil)
			return
		case <-ticker.C:
			pair, _, err := client.KV().Get(triggerKey, nil)
			if err != nil {
				t.Logf("Warning: Failed to verify key deletion: %v", err)
				return
			}
			if pair == nil {
				t.Log("Key successfully deleted and verified")
				// Extra wait to ensure lock-delay expires
				time.Sleep(1 * time.Second)
				return
			}
		}
	}
}

// TestGracefulShutdown tests that elector properly releases lock on shutdown
func TestGracefulShutdown(t *testing.T) {
	client := setupConsulClient(t)

	// Log all existing sessions before starting the test
	sessions, _, err := client.Session().List(nil)
	if err == nil {
		t.Logf("Existing sessions before test: %d", len(sessions))
		for _, sess := range sessions {
			t.Logf("  Session: %s, TTL: %s, LockDelay: %v", sess.ID, sess.TTL, sess.LockDelay)
		}
	}

	// Start elector
	cmd, cleanup := startElector(t, "node1", "")
	defer cleanup()

	// Wait for leader election with extended timeout
	leader := waitForLeader(t, client, 30*time.Second)
	if leader != "node1" {
		t.Errorf("Expected leader to be 'node1', got '%s'", leader)
	}

	// Send SIGTERM for graceful shutdown
	t.Log("Sending SIGTERM for graceful shutdown")
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("Failed to send SIGTERM: %v", err)
	}

	// Wait for process to exit
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case <-done:
		t.Log("Process exited")
	case <-time.After(10 * time.Second):
		t.Fatal("Process did not exit within timeout")
	}

	// Wait for key to be deleted with extended timeout
	waitForNoLeader(t, client, 30*time.Second)

	t.Log("Graceful shutdown completed successfully")
}

// waitForLockDelayExpiration helps verify that lock-delay has expired and the key is no longer present
func waitForLockDelayExpiration(t *testing.T, client *api.Client, timeout time.Duration) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Best effort, don't fail the test
			t.Log("Warning: Lock-delay verification timed out")
			return
		case <-ticker.C:
			pair, _, err := client.KV().Get(triggerKey, nil)
			if err != nil {
				t.Logf("Warning: Failed to get key during lock-delay verification: %v", err)
				continue
			}

			// If no key exists or session is empty, lock-delay has likely expired
			if pair == nil || pair.Session == "" {
				// Add extra buffer to ensure lock-delay is truly expired (100ms lock-delay + 100ms buffer)
				time.Sleep(200 * time.Millisecond)
				return
			}
		}
	}
}

// TestConcurrentStartup tests multiple nodes starting at the exact same time
func TestConcurrentStartup(t *testing.T) {
	client := setupConsulClient(t)

	// Start 5 electors concurrently
	nodeCount := 5
	cleanups := make([]context.CancelFunc, nodeCount)
	nodeNames := make([]string, nodeCount)

	for i := 0; i < nodeCount; i++ {
		hostname := fmt.Sprintf("node%d", i+1)
		nodeNames[i] = hostname
		_, cleanup := startElector(t, hostname, "")
		cleanups[i] = cleanup
	}

	// Cleanup all at the end
	defer func() {
		for _, cleanup := range cleanups {
			cleanup()
		}
	}()

	// Wait for leader election
	leader := waitForLeader(t, client, 30*time.Second)

	t.Logf("Leader elected from %d concurrent nodes: %s", nodeCount, leader)

	// Verify the leader is one of the expected nodes
	validLeaders := make(map[string]bool)
	for _, nodeName := range nodeNames {
		validLeaders[nodeName] = true
	}

	if !validLeaders[leader] {
		t.Errorf("Expected leader to be one of the started nodes, got '%s'", leader)
	}

	// Verify exactly one leader
	pair, _, err := client.KV().Get(triggerKey, nil)
	if err != nil {
		t.Fatalf("Failed to get key: %v", err)
	}

	if pair == nil {
		t.Fatal("Expected lock to be held")
	}

	const expectedFlags uint64 = 0x2ddccbc058a50c18
	if pair.Flags != expectedFlags {
		t.Errorf("Expected Flags=%d, got %d", expectedFlags, pair.Flags)
	}
}
