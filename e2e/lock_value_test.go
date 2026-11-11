//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// writeVipManagerConfig writes a vip-manager.yml for the test Consul with
// extra lines appended and returns its path.
func writeVipManagerConfig(t *testing.T, extra string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "vip-manager.yml")
	content := fmt.Sprintf("dcs-type: consul\ndcs-endpoints:\n  - \"http://%s\"\ntrigger-key: %q\n%s", consulAddr, triggerKey, extra)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}
	return path
}

// startElectorWithConfig starts vip-elector with the given config and extra
// arguments, without --hostname, and stops it when the test ends.
func startElectorWithConfig(t *testing.T, configPath string, args ...string) {
	t.Helper()

	args = append([]string{
		"--vip-manager-config=" + configPath,
		"--ttl=10s",
		"--lock-delay=100ms",
	}, args...)

	cmd := exec.Command("./"+binaryName, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start elector: %v", err)
	}
	t.Cleanup(func() {
		cmd.Process.Signal(os.Interrupt)
		cmd.Wait()
	})
}

// TestLockValueFromTriggerValue verifies that vip-elector writes vip-manager's
// trigger-value, so that vip-manager on the leader sees a match.
func TestLockValueFromTriggerValue(t *testing.T) {
	client := setupConsulClient(t)

	startElectorWithConfig(t, writeVipManagerConfig(t, "trigger-value: \"tv-node\"\n"))

	waitForLeaderValue(t, client, "tv-node", 10*time.Second)
}

// TestLockValueDefaultsToOSHostname verifies that without --hostname and
// trigger-value the lock value is os.Hostname() unmodified, which is what
// vip-manager compares against by default.
func TestLockValueDefaultsToOSHostname(t *testing.T) {
	client := setupConsulClient(t)

	hostname, err := os.Hostname()
	if err != nil {
		t.Fatalf("os.Hostname: %v", err)
	}

	startElectorWithConfig(t, writeVipManagerConfig(t, ""))

	waitForLeaderValue(t, client, hostname, 10*time.Second)
}

// TestHostnameFlagOverridesTriggerValue verifies that --hostname still takes
// precedence over trigger-value.
func TestHostnameFlagOverridesTriggerValue(t *testing.T) {
	client := setupConsulClient(t)

	startElectorWithConfig(t, writeVipManagerConfig(t, "trigger-value: \"tv-node\"\n"), "--hostname=flag-node")

	waitForLeaderValue(t, client, "flag-node", 10*time.Second)
}
