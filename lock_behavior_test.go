package main

import (
	"errors"
	"fmt"
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
	if opts.SessionOpts == nil {
		t.Fatal("expected SessionOpts to be set")
	}
	if opts.SessionOpts.ID != "session-123" {
		t.Errorf("expected session ID=session-123, got %s", opts.SessionOpts.ID)
	}

	if opts.MonitorRetries != defaultMonitorRetries {
		t.Errorf("expected MonitorRetries=%d, got %d", defaultMonitorRetries, opts.MonitorRetries)
	}
	if opts.MonitorRetryTime != defaultMonitorRetryTime {
		t.Errorf("expected MonitorRetryTime=%v, got %v", defaultMonitorRetryTime, opts.MonitorRetryTime)
	}

	if opts.MonitorRetries != 3 {
		t.Errorf("expected MonitorRetries=3, got %d", opts.MonitorRetries)
	}
	if opts.MonitorRetryTime != 10*time.Second {
		t.Errorf("expected MonitorRetryTime=10s, got %v", opts.MonitorRetryTime)
	}
}
