//go:build e2e

package e2e

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// silentListener accepts connections and never answers, like a DCS that
// hangs. It returns its address.
func silentListener(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to listen: %v", err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})
	return ln.Addr().String()
}

// TestSignalDuringPreflightEndsProcess verifies that SIGTERM ends vip-elector
// at once while its pre-flight check waits for a DCS that does not answer.
// The Consul client takes no context there, so signals keep their default
// action until the election loop starts.
func TestSignalDuringPreflightEndsProcess(t *testing.T) {
	addr := silentListener(t)

	tests := []struct {
		dcsType string
		key     string
		ready   string // logged right before the pre-flight check
	}{
		{dcsType: "consul", key: "test/vip-elector/e2e/silent", ready: "Consul client"},
		{dcsType: "etcd", key: "/test/vip-elector/e2e/silent", ready: "etcd client"},
	}
	for _, tt := range tests {
		t.Run(tt.dcsType, func(t *testing.T) {
			config := filepath.Join(t.TempDir(), "vip-manager.yml")
			content := fmt.Sprintf("dcs-type: %s\ndcs-endpoints:\n  - \"http://%s\"\ntrigger-key: %q\n", tt.dcsType, addr, tt.key)
			if err := os.WriteFile(config, []byte(content), 0o644); err != nil {
				t.Fatalf("Failed to write config: %v", err)
			}

			var output lockedBuffer
			cmd := exec.Command("./"+binaryName, "--vip-manager-config="+config, "--hostname=node1")
			cmd.Stdout = io.MultiWriter(os.Stdout, &output)
			cmd.Stderr = os.Stderr
			if err := cmd.Start(); err != nil {
				t.Fatalf("Failed to start elector: %v", err)
			}
			exited := make(chan struct{})
			go func() {
				cmd.Wait()
				close(exited)
			}()
			defer func() {
				cmd.Process.Kill()
				<-exited
			}()

			deadline := time.Now().Add(10 * time.Second)
			for !strings.Contains(output.String(), tt.ready) {
				if time.Now().After(deadline) {
					t.Fatalf("Elector did not reach the pre-flight check: %s", output.String())
				}
				time.Sleep(20 * time.Millisecond)
			}
			time.Sleep(500 * time.Millisecond)

			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatalf("Failed to send SIGTERM: %v", err)
			}
			select {
			case <-exited:
			case <-time.After(2 * time.Second):
				t.Fatalf("Elector did not exit within 2s of SIGTERM during the pre-flight check")
			}
			if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
				t.Errorf("Expected the default action of SIGTERM, got %v", cmd.ProcessState)
			}
		})
	}
}
