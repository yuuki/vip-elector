package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/consul/api"
)

func TestClassifyLockAcquireError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want lockAcquireErrorType
	}{
		{
			name: "lock conflict",
			err:  fmt.Errorf("wrapped: %w", api.ErrLockConflict),
			want: lockAcquireErrorConflict,
		},
		{
			name: "retryable consul 500",
			err:  errors.New("failed to read lock: Unexpected response code: 500 (rpc error making call: i/o deadline reached)"),
			want: lockAcquireErrorConsulRetryable,
		},
		{
			name: "unknown error",
			err:  errors.New("unexpected failure"),
			want: lockAcquireErrorUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyLockAcquireError(tt.err)
			if got != tt.want {
				t.Errorf("classifyLockAcquireError() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildLockOptions(t *testing.T) {
	opts := buildLockOptions("test/vip/lock", "node-a", "session-123")

	if opts.Key != "test/vip/lock" {
		t.Errorf("expected key=test/vip/lock, got %s", opts.Key)
	}
	if string(opts.Value) != "node-a" {
		t.Errorf("expected value=node-a, got %s", string(opts.Value))
	}
	// The lock must reuse the session we created. If Session is empty the
	// Lock API creates its own default session (no TTL, no service check,
	// Behavior=release) and SessionOpts.ID is ignored.
	if opts.Session != "session-123" {
		t.Errorf("expected Session=session-123, got %q", opts.Session)
	}
	if opts.SessionOpts != nil {
		t.Errorf("expected SessionOpts to be nil, got %+v", opts.SessionOpts)
	}

	if opts.MonitorRetries != defaultMonitorRetries {
		t.Errorf("expected MonitorRetries=%d, got %d", defaultMonitorRetries, opts.MonitorRetries)
	}
	if opts.MonitorRetryTime != defaultMonitorRetryTime {
		t.Errorf("expected MonitorRetryTime=%v, got %v", defaultMonitorRetryTime, opts.MonitorRetryTime)
	}

	// A pending Lock() only notices its stop channel between blocking
	// queries, so the query wait bounds how long a standby takes to shut
	// down or to react to a lost session.
	if opts.LockWaitTime != defaultLockWaitTime {
		t.Errorf("expected LockWaitTime=%v, got %v", defaultLockWaitTime, opts.LockWaitTime)
	}
	if opts.LockWaitTime <= 0 || opts.LockWaitTime > 5*time.Second {
		t.Errorf("expected LockWaitTime between 0 and 5s, got %v", opts.LockWaitTime)
	}
	if opts.LockTryOnce {
		t.Error("expected LockTryOnce=false: a standby must keep waiting for the lock")
	}

	if opts.MonitorRetries != 3 {
		t.Errorf("expected MonitorRetries=3, got %d", opts.MonitorRetries)
	}
	if opts.MonitorRetryTime != 10*time.Second {
		t.Errorf("expected MonitorRetryTime=10s, got %v", opts.MonitorRetryTime)
	}
}

// TestLockWaitTimeOnTheWire checks how the Consul Lock API uses
// LockWaitTime: a standby's blocking query must carry wait=2s, so that a
// pending Lock() notices its stop channel within about 2s.
func TestLockWaitTimeOnTheWire(t *testing.T) {
	clearConsulEnv(t)

	var (
		mu    sync.Mutex
		waits []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/kv/test/vip/lock" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		waits = append(waits, r.URL.Query().Get("wait"))
		mu.Unlock()

		// The key is held by another node's session.
		w.Header().Set("X-Consul-Index", "10")
		w.Header().Set("X-Consul-LastContact", "0")
		w.Header().Set("X-Consul-KnownLeader", "true")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[{"Key":"test/vip/lock","Flags":%d,"Value":"bm9kZS1i","Session":"other","CreateIndex":1,"ModifyIndex":10,"LockIndex":1}]`, LockFlagValue)
	}))
	defer srv.Close()

	client, err := createConsulClient(&Config{}, &VipManagerConfig{DCSEndpoints: []string{srv.URL}})
	if err != nil {
		t.Fatalf("createConsulClient: %v", err)
	}
	lock, err := client.LockOpts(buildLockOptions("test/vip/lock", "node-a", "session-123"))
	if err != nil {
		t.Fatalf("LockOpts: %v", err)
	}

	stopCh := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		lockCh, err := lock.Lock(stopCh)
		if err == nil && lockCh != nil {
			err = errors.New("acquired a lock held by another session")
		}
		done <- err
	}()

	// The fake server answers at once, so Lock() keeps polling until stopped.
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(waits)
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Lock() did not query the key")
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(stopCh)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Lock(): %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Lock() did not return after stopCh was closed")
	}

	mu.Lock()
	defer mu.Unlock()
	for i, wait := range waits {
		if wait != "2000ms" {
			t.Errorf("query %d: wait=%q, want 2000ms", i, wait)
		}
	}
}
