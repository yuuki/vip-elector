package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/consul/api"
)

// mockSessionRenewer replays a scripted sequence of Renew results.
// Once the script is exhausted it keeps returning a valid session.
type mockSessionRenewer struct {
	mu      sync.Mutex
	results []renewResult
	calls   int
}

type renewResult struct {
	entry *api.SessionEntry
	err   error
}

func (m *mockSessionRenewer) Renew(id string, q *api.WriteOptions) (*api.SessionEntry, *api.WriteMeta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.calls++
	if len(m.results) == 0 {
		return &api.SessionEntry{ID: id}, nil, nil
	}
	r := m.results[0]
	m.results = m.results[1:]
	return r.entry, nil, r.err
}

func (m *mockSessionRenewer) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func waitForCalls(t *testing.T, m *mockSessionRenewer, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for m.callCount() < want {
		if time.Now().After(deadline) {
			t.Fatalf("expected at least %d Renew calls, got %d", want, m.callCount())
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRenewSession_ReturnsExpiredWhenSessionGone(t *testing.T) {
	// Renew returns (nil, nil) when Consul answers 404 for the session.
	renewer := &mockSessionRenewer{results: []renewResult{
		{entry: &api.SessionEntry{ID: "s1"}},
		{entry: nil},
	}}

	err := renewSession(context.Background(), renewer, "s1", 10*time.Millisecond)
	if !errors.Is(err, api.ErrSessionExpired) {
		t.Fatalf("expected ErrSessionExpired, got %v", err)
	}
	if got := renewer.callCount(); got != 2 {
		t.Errorf("expected 2 Renew calls, got %d", got)
	}
}

func TestRenewSession_RetriesTransientErrors(t *testing.T) {
	renewer := &mockSessionRenewer{results: []renewResult{
		{err: errors.New("Unexpected response code: 500")},
		{err: errors.New("Unexpected response code: 500")},
		{entry: &api.SessionEntry{ID: "s1"}},
		{entry: nil},
	}}

	// A transient error must not be reported as a lost session; only the
	// final "session gone" answer ends the loop.
	err := renewSession(context.Background(), renewer, "s1", 10*time.Millisecond)
	if !errors.Is(err, api.ErrSessionExpired) {
		t.Fatalf("expected ErrSessionExpired, got %v", err)
	}
	if got := renewer.callCount(); got != 4 {
		t.Errorf("expected 4 Renew calls, got %d", got)
	}
}

func TestRenewSession_StopsOnContextCancel(t *testing.T) {
	renewer := &mockSessionRenewer{}
	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- renewSession(ctx, renewer, "s1", 10*time.Millisecond)
	}()

	waitForCalls(t, renewer, 2)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("expected nil on cancellation, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("renewSession did not return after context cancellation")
	}
}

func TestSessionKeepalive_SignalsLostSession(t *testing.T) {
	renewer := &mockSessionRenewer{results: []renewResult{{entry: nil}}}

	k := startSessionKeepalive(context.Background(), renewer, "s1", 10*time.Millisecond)
	defer k.stop()

	select {
	case <-k.lost:
	case <-time.After(5 * time.Second):
		t.Fatal("expected lost to be closed when the session is gone")
	}

	// A lost session must also abort a pending lock acquisition.
	select {
	case <-k.stopCh():
	case <-time.After(5 * time.Second):
		t.Fatal("expected stopCh to be closed when the session is gone")
	}
}

func TestSessionKeepalive_StopDoesNotSignalLost(t *testing.T) {
	renewer := &mockSessionRenewer{}

	k := startSessionKeepalive(context.Background(), renewer, "s1", 10*time.Millisecond)
	waitForCalls(t, renewer, 1)
	k.stop()

	select {
	case <-k.lost:
		t.Error("lost must not be closed by stop()")
	default:
	}

	select {
	case <-k.stopCh():
	default:
		t.Error("expected stopCh to be closed after stop()")
	}

	// stop() waits for the goroutine, so no further renewals may happen.
	calls := renewer.callCount()
	time.Sleep(30 * time.Millisecond)
	if got := renewer.callCount(); got != calls {
		t.Errorf("expected no Renew calls after stop(), got %d more", got-calls)
	}
}

func TestSessionKeepalive_ParentCancelDoesNotSignalLost(t *testing.T) {
	renewer := &mockSessionRenewer{}
	parent, cancel := context.WithCancel(context.Background())

	k := startSessionKeepalive(parent, renewer, "s1", 10*time.Millisecond)
	cancel()

	select {
	case <-k.done:
	case <-time.After(5 * time.Second):
		t.Fatal("keepalive did not exit after parent cancellation")
	}

	select {
	case <-k.lost:
		t.Error("lost must not be closed on shutdown")
	default:
	}
}
