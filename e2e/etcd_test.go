//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const (
	etcdEndpoint   = "http://127.0.0.1:2379"
	etcdTriggerKey = "/test/vip-elector/e2e/lock"
)

// setupEtcdClient connects to the test etcd and deletes the trigger key.
func setupEtcdClient(t *testing.T) *clientv3.Client {
	t.Helper()

	client, err := clientv3.New(clientv3.Config{Endpoints: []string{etcdEndpoint}, DialTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("Failed to create etcd client: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	deadline := time.Now().Add(10 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := client.Delete(ctx, etcdTriggerKey)
		cancel()
		if err == nil {
			return client
		}
		if time.Now().After(deadline) {
			t.Fatalf("etcd not ready: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// writeEtcdConfig writes a vip-manager.yml for the test etcd with extra
// lines appended and returns its path.
func writeEtcdConfig(t *testing.T, extra string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "vip-manager.yml")
	content := fmt.Sprintf("dcs-type: etcd\ndcs-endpoints:\n  - %q\ntrigger-key: %q\n%s", etcdEndpoint, etcdTriggerKey, extra)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}
	return path
}

// etcdElectorProcess is a running vip-elector.
type etcdElectorProcess struct {
	cmd    *exec.Cmd
	output *lockedBuffer
	exited chan struct{}
}

// startEtcdElector starts vip-elector with the given config and arguments
// (defaults: --ttl=10s --lock-delay=100ms, overridable) and stops it when
// the test ends.
func startEtcdElector(t *testing.T, configPath string, args ...string) *etcdElectorProcess {
	t.Helper()

	args = append([]string{
		"--vip-manager-config=" + configPath,
		"--ttl=10s",
		"--lock-delay=100ms",
	}, args...)

	p := &etcdElectorProcess{output: &lockedBuffer{}, exited: make(chan struct{})}
	p.cmd = exec.Command("./"+binaryName, args...)
	p.cmd.Stdout = io.MultiWriter(os.Stdout, p.output)
	p.cmd.Stderr = os.Stderr
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("Failed to start elector: %v", err)
	}
	go func() {
		p.cmd.Wait()
		close(p.exited)
	}()

	t.Cleanup(func() {
		p.cmd.Process.Signal(os.Interrupt)
		select {
		case <-p.exited:
		case <-time.After(10 * time.Second):
			p.cmd.Process.Kill()
			<-p.exited
		}
	})
	return p
}

// stop sends the signal and returns how long the process took to exit.
func (p *etcdElectorProcess) stop(t *testing.T, sig os.Signal) time.Duration {
	t.Helper()

	start := time.Now()
	if err := p.cmd.Process.Signal(sig); err != nil {
		t.Fatalf("Failed to send %v: %v", sig, err)
	}
	select {
	case <-p.exited:
		return time.Since(start)
	case <-time.After(10 * time.Second):
		t.Fatalf("Elector did not exit within 10s of %v", sig)
		return 0
	}
}

// waitForLog waits until the process has logged a line containing s.
func (p *etcdElectorProcess) waitForLog(t *testing.T, s string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for !strings.Contains(p.output.String(), s) {
		if time.Now().After(deadline) {
			t.Fatalf("Timeout waiting for log %q", s)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// grantedLeases returns the lease IDs the process logged as granted.
func (p *etcdElectorProcess) grantedLeases(t *testing.T) []clientv3.LeaseID {
	t.Helper()

	var ids []clientv3.LeaseID
	for _, line := range strings.Split(p.output.String(), "\n") {
		var entry struct {
			Msg     string `json:"msg"`
			LeaseID string `json:"lease_id"`
		}
		if json.Unmarshal([]byte(line), &entry) != nil || entry.Msg != "Lease granted" {
			continue
		}
		id, err := strconv.ParseInt(entry.LeaseID, 16, 64)
		if err != nil {
			t.Fatalf("Bad lease_id %q: %v", entry.LeaseID, err)
		}
		ids = append(ids, clientv3.LeaseID(id))
	}
	return ids
}

// runEtcdElectorToExit runs vip-elector and expects it to exit with an error.
func runEtcdElectorToExit(t *testing.T, args ...string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "./"+binaryName, args...).CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("Elector did not exit: %s", output)
	}
	if err == nil {
		t.Fatalf("Expected elector to exit with an error, output: %s", output)
	}
	return string(output)
}

// getEtcdRevision returns etcd's current revision.
func getEtcdRevision(t *testing.T, client *clientv3.Client) int64 {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := client.Get(ctx, etcdTriggerKey)
	if err != nil {
		t.Fatalf("Failed to read the revision: %v", err)
	}
	return resp.Header.Revision
}

func getEtcdKey(t *testing.T, client *clientv3.Client) *mvccpb.KeyValue {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := client.Get(ctx, etcdTriggerKey)
	if err != nil {
		t.Fatalf("Failed to read the trigger key: %v", err)
	}
	if len(resp.Kvs) == 0 {
		return nil
	}
	return resp.Kvs[0]
}

// waitForEtcdLeader waits until the trigger key carries want and a lease.
func waitForEtcdLeader(t *testing.T, client *clientv3.Client, want string, timeout time.Duration) *mvccpb.KeyValue {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var last *mvccpb.KeyValue
	for time.Now().Before(deadline) {
		last = getEtcdKey(t, client)
		if last != nil && last.Lease != 0 && string(last.Value) == want {
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
	if last == nil {
		t.Fatalf("Timeout waiting for leader %q: key does not exist", want)
	}
	t.Fatalf("Timeout waiting for leader %q: value=%q lease=%x", want, last.Value, last.Lease)
	return nil
}

func waitForEtcdKeyDeleted(t *testing.T, client *clientv3.Client, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for getEtcdKey(t, client) != nil {
		if time.Now().After(deadline) {
			t.Fatalf("Timeout waiting for key deletion: %+v", getEtcdKey(t, client))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// leaseExists reports whether etcd still knows the lease.
func leaseExists(t *testing.T, client *clientv3.Client, id clientv3.LeaseID) bool {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := client.TimeToLive(ctx, id)
	if err != nil {
		t.Fatalf("Failed to look up lease %x: %v", id, err)
	}
	return resp.TTL > 0
}

// TestEtcdLeaderElection verifies that a single node takes the key with a
// lease of the configured TTL.
func TestEtcdLeaderElection(t *testing.T) {
	client := setupEtcdClient(t)

	startEtcdElector(t, writeEtcdConfig(t, ""), "--hostname=node1", "--ttl=7s")

	kv := waitForEtcdLeader(t, client, "node1", 10*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ttl, err := client.TimeToLive(ctx, clientv3.LeaseID(kv.Lease))
	if err != nil {
		t.Fatalf("Failed to look up lease: %v", err)
	}
	if ttl.GrantedTTL != 7 {
		t.Errorf("Expected the lease to have the --ttl of 7s, got %ds", ttl.GrantedTTL)
	}
}

// TestEtcdLockValueFromTriggerValue verifies that vip-elector writes
// vip-manager's trigger-value to etcd too.
func TestEtcdLockValueFromTriggerValue(t *testing.T) {
	client := setupEtcdClient(t)

	startEtcdElector(t, writeEtcdConfig(t, "trigger-value: \"tv-node\"\n"))

	waitForEtcdLeader(t, client, "tv-node", 10*time.Second)
}

// TestEtcdConcurrentStartup verifies that of several nodes started at once
// exactly one becomes leader and stays leader.
func TestEtcdConcurrentStartup(t *testing.T) {
	client := setupEtcdClient(t)
	config := writeEtcdConfig(t, "")

	for _, name := range []string{"node1", "node2", "node3"} {
		startEtcdElector(t, config, "--hostname="+name)
	}

	deadline := time.Now().Add(10 * time.Second)
	var kv *mvccpb.KeyValue
	for kv == nil {
		if time.Now().After(deadline) {
			t.Fatal("No leader elected")
		}
		time.Sleep(50 * time.Millisecond)
		kv = getEtcdKey(t, client)
	}

	time.Sleep(2 * time.Second)
	now := getEtcdKey(t, client)
	if now == nil || now.ModRevision != kv.ModRevision {
		t.Errorf("Expected the leader %q to stay, key is now %+v", kv.Value, now)
	}
}

// TestEtcdGracefulFailoverWaitsLockDelay verifies that a graceful shutdown
// deletes the key at once, and that the standby takes it only after
// lock-delay, which etcd itself does not have.
func TestEtcdGracefulFailoverWaitsLockDelay(t *testing.T) {
	client := setupEtcdClient(t)
	config := writeEtcdConfig(t, "")

	node1 := startEtcdElector(t, config, "--hostname=node1", "--lock-delay=1s")
	waitForEtcdLeader(t, client, "node1", 10*time.Second)

	node2 := startEtcdElector(t, config, "--hostname=node2", "--lock-delay=1s")
	node2.waitForLog(t, "Lock is held by another node, waiting", 10*time.Second)

	// Record when the key is deleted and when it is written again, from
	// node1's write on, so that no event can be missed.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watch := client.Watch(ctx, etcdTriggerKey, clientv3.WithRev(getEtcdKey(t, client).ModRevision+1))

	node1.stop(t, os.Interrupt)

	var deletedAt time.Time
	timeout := time.After(15 * time.Second)
	for {
		select {
		case resp := <-watch:
			for _, ev := range resp.Events {
				switch {
				case ev.Type == mvccpb.DELETE:
					deletedAt = time.Now()
				case deletedAt.IsZero():
					t.Fatalf("Key written before it was deleted: %q", ev.Kv.Value)
				default:
					if string(ev.Kv.Value) != "node2" {
						t.Fatalf("Expected node2 to take over, got %q", ev.Kv.Value)
					}
					gap := time.Since(deletedAt)
					t.Logf("node2 took the key %v after it was deleted", gap)
					if gap < 900*time.Millisecond {
						t.Errorf("Expected node2 to wait lock-delay (1s), it waited %v", gap)
					}
					return
				}
			}
		case <-timeout:
			t.Fatalf("Timeout waiting for failover (deleted at %v)", deletedAt)
		}
	}
}

// TestEtcdLockDelayCountsFromLastRelease verifies that a node waiting
// lock-delay on a free key starts its wait over when another holder takes
// the key and lets it go meanwhile. Otherwise the delay after that release
// could be cut short to almost nothing.
func TestEtcdLockDelayCountsFromLastRelease(t *testing.T) {
	client := setupEtcdClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	watch := client.Watch(ctx, etcdTriggerKey, clientv3.WithRev(getEtcdRevision(t, client)+1))

	node := startEtcdElector(t, writeEtcdConfig(t, ""), "--hostname=node1", "--lock-delay=2s")
	node.waitForLog(t, "Trigger key is free, waiting lock-delay", 10*time.Second)

	// Another holder takes the key 1s into the wait and lets it go 0.3s later.
	time.Sleep(time.Second)
	lease, err := client.Grant(ctx, 10)
	if err != nil {
		t.Fatalf("Failed to grant lease: %v", err)
	}
	if _, err := client.Put(ctx, etcdTriggerKey, "other", clientv3.WithLease(lease.ID)); err != nil {
		t.Fatalf("Failed to write the key: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := client.Revoke(ctx, lease.ID); err != nil {
		t.Fatalf("Failed to revoke lease: %v", err)
	}

	var releasedAt time.Time
	for {
		select {
		case resp := <-watch:
			for _, ev := range resp.Events {
				switch {
				case ev.Type == mvccpb.DELETE:
					releasedAt = time.Now()
				case string(ev.Kv.Value) == "node1":
					if releasedAt.IsZero() {
						t.Fatal("node1 wrote the key before the other holder released it")
					}
					gap := time.Since(releasedAt)
					t.Logf("node1 took the key %v after the other holder released it", gap)
					if gap < 1900*time.Millisecond {
						t.Errorf("Expected node1 to wait lock-delay (2s) after the release, it waited %v", gap)
					}
					return
				}
			}
		case <-ctx.Done():
			t.Fatal("Timeout waiting for node1 to take the key")
		}
	}
}

// TestEtcdLeaderCrashFailover verifies that a standby takes over when the
// leader process dies without cleaning up. This relies on the lease TTL.
func TestEtcdLeaderCrashFailover(t *testing.T) {
	client := setupEtcdClient(t)
	config := writeEtcdConfig(t, "")

	node1 := startEtcdElector(t, config, "--hostname=node1", "--ttl=3s")
	waitForEtcdLeader(t, client, "node1", 10*time.Second)

	node2 := startEtcdElector(t, config, "--hostname=node2")
	node2.waitForLog(t, "Lock is held by another node, waiting", 10*time.Second)

	t.Log("Killing leader node1 with SIGKILL")
	node1.stop(t, syscall.SIGKILL)

	// Nothing revoked the lease, so the key stays until the TTL ends.
	if kv := getEtcdKey(t, client); kv == nil || string(kv.Value) != "node1" {
		t.Errorf("Expected the key to stay with node1 until its lease expires, got %+v", kv)
	}

	waitForEtcdLeader(t, client, "node2", 15*time.Second)
}

// TestEtcdStandbyShutdownIsPrompt verifies that a standby waiting for the
// key exits at once on SIGTERM and revokes its lease.
func TestEtcdStandbyShutdownIsPrompt(t *testing.T) {
	client := setupEtcdClient(t)
	config := writeEtcdConfig(t, "")

	startEtcdElector(t, config, "--hostname=node1")
	waitForEtcdLeader(t, client, "node1", 10*time.Second)

	node2 := startEtcdElector(t, config, "--hostname=node2")
	node2.waitForLog(t, "Lock is held by another node, waiting", 10*time.Second)

	took := node2.stop(t, syscall.SIGTERM)
	t.Logf("Standby exited after %v", took)
	if took > 2*time.Second {
		t.Errorf("Expected the standby to exit within 2s, it took %v", took)
	}

	leases := node2.grantedLeases(t)
	if len(leases) == 0 {
		t.Fatal("Standby logged no granted lease")
	}
	for _, id := range leases {
		if leaseExists(t, client, id) {
			t.Errorf("Standby lease %x still exists after shutdown", id)
		}
	}
	if kv := getEtcdKey(t, client); kv == nil || string(kv.Value) != "node1" {
		t.Errorf("Expected node1 to stay leader, got %+v", kv)
	}
}

// TestEtcdLeaseRevokedExternally verifies that the leader notices its lease
// is gone and takes part in the election again, and that revoking a lease
// etcd no longer has is not reported as a failure.
func TestEtcdLeaseRevokedExternally(t *testing.T) {
	client := setupEtcdClient(t)

	node1 := startEtcdElector(t, writeEtcdConfig(t, ""), "--hostname=node1")
	kv := waitForEtcdLeader(t, client, "node1", 10*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := client.Revoke(ctx, clientv3.LeaseID(kv.Lease)); err != nil {
		t.Fatalf("Failed to revoke lease: %v", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for {
		now := getEtcdKey(t, client)
		if now != nil && string(now.Value) == "node1" && now.Lease != kv.Lease {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Timeout waiting for node1 to take the key with a new lease, key is %+v", now)
		}
		time.Sleep(50 * time.Millisecond)
	}

	if out := node1.output.String(); strings.Contains(out, "Failed to revoke lease") {
		t.Errorf("Revoking a lease that is already gone was reported as a failure:\n%s", out)
	}
}

// TestEtcdKeyDeletedExternally verifies that the leader notices its key is
// gone, revokes its lease and takes the key again.
func TestEtcdKeyDeletedExternally(t *testing.T) {
	client := setupEtcdClient(t)

	node1 := startEtcdElector(t, writeEtcdConfig(t, ""), "--hostname=node1")
	kv := waitForEtcdLeader(t, client, "node1", 10*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := client.Delete(ctx, etcdTriggerKey); err != nil {
		t.Fatalf("Failed to delete the key: %v", err)
	}

	node1.waitForLog(t, "LOCK LOST - Re-entering election", 10*time.Second)
	now := waitForEtcdLeader(t, client, "node1", 15*time.Second)
	if now.Lease == kv.Lease {
		t.Error("Expected node1 to take the key again with a new lease")
	}
	if leaseExists(t, client, clientv3.LeaseID(kv.Lease)) {
		t.Errorf("Expected the old lease %x to be revoked", kv.Lease)
	}
}

// TestEtcdRejectsPlainKey verifies that a key without a lease, which would
// never expire, stops vip-elector at startup with a way out.
func TestEtcdRejectsPlainKey(t *testing.T) {
	client := setupEtcdClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := client.Put(ctx, etcdTriggerKey, "manual"); err != nil {
		t.Fatalf("Failed to write the key: %v", err)
	}
	t.Cleanup(func() { client.Delete(context.Background(), etcdTriggerKey) })

	output := runEtcdElectorToExit(t, "--vip-manager-config="+writeEtcdConfig(t, ""), "--hostname=node1")
	for _, want := range []string{"pre-flight", "has no lease", "etcdctl del " + etcdTriggerKey} {
		if !strings.Contains(output, want) {
			t.Errorf("Expected %q in output, got: %s", want, output)
		}
	}
}

// TestEtcdRejectsConsulOnlyFlags verifies that --check-id is refused with
// etcd instead of being ignored, and that the TTL must be whole seconds.
func TestEtcdRejectsConsulOnlyFlags(t *testing.T) {
	config := writeEtcdConfig(t, "")

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "check-id", args: []string{"--check-id=vip-http"}, want: "--check-id is not supported with dcs-type etcd"},
		{name: "token", args: []string{"--token=secret"}, want: "--token is only used with dcs-type consul"},
		{name: "fractional ttl", args: []string{"--ttl=1500ms"}, want: "whole seconds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"--vip-manager-config=" + config, "--hostname=node1"}, tt.args...)
			if output := runEtcdElectorToExit(t, args...); !strings.Contains(output, tt.want) {
				t.Errorf("Expected %q in output, got: %s", tt.want, output)
			}
		})
	}
}
