//go:build e2e

package e2e

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuffer is a bytes.Buffer safe for concurrent writes and reads.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestUnregisteredCheckRetriesUntilRegistered verifies that vip-elector
// started with a --check-id that is not registered yet (e.g. the service is
// registered after vip-elector starts at boot) says so, does not take part
// in the election, keeps running, and becomes leader once the check exists.
func TestUnregisteredCheckRetriesUntilRegistered(t *testing.T) {
	client := setupConsulClient(t)

	const serviceID = "vip-elector-e2e-late"
	checkID := serviceID + "-check" // what registerTTLCheck registers
	client.Agent().ServiceDeregister(serviceID)

	configPath, err := filepath.Abs(testConfigPath)
	if err != nil {
		t.Fatalf("Failed to get absolute config path: %v", err)
	}

	var output lockedBuffer
	cmd := exec.Command("./"+binaryName,
		"--vip-manager-config="+configPath,
		"--hostname=node1",
		"--ttl=10s",
		"--lock-delay=100ms",
		"--check-id="+checkID,
	)
	cmd.Stdout = io.MultiWriter(os.Stdout, &output)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start elector: %v", err)
	}
	// exited is closed when the process ends, so that both the early-exit
	// check and the cleanup can wait on it.
	exited := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(exited)
	}()
	defer func() {
		cmd.Process.Signal(os.Interrupt)
		<-exited
	}()

	// It must not exit, and must not claim to run without the check.
	select {
	case <-exited:
		t.Fatalf("Elector exited with an unregistered check: %v\n%s", waitErr, output.String())
	case <-time.After(3 * time.Second):
	}

	out := output.String()
	if !strings.Contains(out, "Health check not found on the local Consul agent") {
		t.Errorf("Expected a warning about the unregistered check, got:\n%s", out)
	}
	if strings.Contains(out, "without service check") {
		t.Errorf("Elector claims to run without the service check:\n%s", out)
	}
	if !strings.Contains(out, "Missing check") || !strings.Contains(out, `"hint"`) {
		t.Errorf("Expected session creation to fail with a hint about the missing check, got:\n%s", out)
	}

	pair, _, err := client.KV().Get(triggerKey, nil)
	if err != nil {
		t.Fatalf("Failed to get key: %v", err)
	}
	if pair != nil {
		t.Fatalf("Elector became leader without its health check: %+v", pair)
	}

	t.Log("Registering the health check")
	registerTTLCheck(t, client, serviceID)

	// Session creation is retried with exponential backoff (1s, 2s, 4s, ...).
	held := waitForLeaderValue(t, client, "node1", 20*time.Second)

	session, _, err := client.Session().Info(held.Session, nil)
	if err != nil || session == nil {
		t.Fatalf("Failed to get session info: %v (session=%v)", err, session)
	}
	if len(session.ServiceChecks) != 1 || session.ServiceChecks[0].ID != checkID {
		t.Errorf("Expected ServiceChecks=[%s], got %+v", checkID, session.ServiceChecks)
	}
}
