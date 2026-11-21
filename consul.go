package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/hashicorp/consul/api"
)

const (
	// Retry lock monitoring briefly on Consul 500/transient unavailability.
	defaultMonitorRetries   = 3
	defaultMonitorRetryTime = 10 * time.Second
	// How long each blocking query of a pending lock attempt waits. The Lock
	// API checks its stop channel only between queries, so this bounds how
	// long a standby takes to react to shutdown or to losing its session.
	// The Lock API default is 15s.
	defaultLockWaitTime = 2 * time.Second
	// How long to wait before retrying a failed session renewal.
	sessionRenewRetryInterval = time.Second
	// LockFlagValue is the magic flag Consul uses to identify lock keys
	// See: https://github.com/hashicorp/consul/blob/main/api/lock.go
	LockFlagValue uint64 = 0x2ddccbc058a50c18
)

type lockAcquireErrorType string

const (
	lockAcquireErrorConflict        lockAcquireErrorType = "lock_conflict"
	lockAcquireErrorConsulRetryable lockAcquireErrorType = "consul_retryable"
	lockAcquireErrorUnknown         lockAcquireErrorType = "lock_unknown"
)

func createConsulClient(config *Config, vipMgrConfig *VipManagerConfig) (*api.Client, error) {
	consulConfig := buildConsulConfig(config, vipMgrConfig)
	client, err := api.NewClient(consulConfig)
	if err != nil {
		return nil, err
	}
	// api.NewClient resolves the scheme from the address; log the result.
	slog.Info("Consul client", "address", consulConfig.Address, "scheme", consulConfig.Scheme)
	return client, nil
}

// buildConsulConfig returns the Consul client configuration.
//
// The address comes from --addr if set, otherwise from the first
// dcs-endpoints entry (only the first one is used, as in vip-manager's Consul
// checker). dcs-endpoints is required, so CONSUL_HTTP_ADDR never applies.
func buildConsulConfig(config *Config, vipMgrConfig *VipManagerConfig) *api.Config {
	consulConfig := api.DefaultConfig()

	if config.ConsulAddr != "" {
		// Passed as is, like CONSUL_HTTP_ADDR to the Consul CLI.
		consulConfig.Address = config.ConsulAddr
	} else if len(vipMgrConfig.DCSEndpoints) > 0 {
		consulConfig.Address = normalizeEndpoint(vipMgrConfig.DCSEndpoints[0])
	}

	if config.ConsulToken != "" {
		consulConfig.Token = config.ConsulToken
	}

	return consulConfig
}

// normalizeEndpoint returns a dcs-endpoints entry the way vip-manager reads
// it: vip-manager parses it with url.Parse (which lowercases the scheme) and
// connects to its scheme, host and port, ignoring any path.
//
// The scheme must be kept: api.NewClient switches to HTTPS for an "https://"
// address. Stripping it would connect to a TLS endpoint over plain HTTP. A
// path must be dropped: api.NewClient would use it as a path prefix (even a
// lone trailing "/"), and every request would miss.
//
// Entries without an http(s) scheme ("host:port", "unix://...") are returned
// unchanged for api.NewClient to handle.
func normalizeEndpoint(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return endpoint
	}
	switch u.Scheme {
	case "http", "https":
		return u.Scheme + "://" + u.Host
	default:
		return endpoint
	}
}

// agentCheckStatus is what the local Consul agent reports about a check.
type agentCheckStatus struct {
	Found  bool
	Status string // when Found
}

// agentChecks is an interface for listing agent checks (implemented by
// *api.Agent), allowing for testing with mocks.
type agentChecks interface {
	Checks() (map[string]*api.AgentCheck, error)
}

// lookupAgentCheck looks the check up on the local agent. The agent hides
// service checks from an ACL token without service:read, and does not tell
// the Go client so, so a check that is not found may still exist.
func lookupAgentCheck(agent agentChecks, checkID string) (agentCheckStatus, error) {
	checks, err := agent.Checks()
	if err != nil {
		return agentCheckStatus{}, fmt.Errorf("failed to query checks: %w", err)
	}

	if check, ok := checks[checkID]; ok && check != nil {
		return agentCheckStatus{Found: true, Status: check.Status}, nil
	}
	return agentCheckStatus{}, nil
}

// checkWarning returns the startup warning for the --check-id lookup, or ""
// if there is nothing to warn about. vip-elector never drops the check: that
// would silently turn a typo into a node that never fails over on health.
// Consul refuses to create a session whose check is missing or critical, and
// the election loop keeps retrying, so the node is not a candidate until the
// check exists and is not critical (e.g. the service is registered after
// vip-elector starts at boot).
func checkWarning(st agentCheckStatus, err error) string {
	const consequence = "no session can be created until it is, so this node does not take part in the election meanwhile; vip-elector keeps retrying"
	switch {
	case err != nil:
		return "Could not look up the health check on the local Consul agent. " +
			"If it is not registered, " + consequence
	case !st.Found:
		return "Health check not found on the local Consul agent. " +
			"If it is not registered, " + consequence + ". " +
			"Check --check-id for typos; an ACL token without service:read also hides it from this lookup"
	case st.Status == api.HealthCritical:
		return "Health check is critical: no session can be created until it is no longer critical, " +
			"so this node does not take part in the election meanwhile; vip-elector keeps retrying"
	default:
		return ""
	}
}

// sessionCreateHint explains the session creation errors Consul returns for
// the checks a session is tied to, or returns "" for any other error.
// Consul validates the checks against the catalog entry of this node, which
// the local agent keeps in sync with its own registrations.
func sessionCreateHint(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "Missing check"):
		// "Missing check '<id>' registration"
		return "a health check of the session is not registered in the Consul catalog for this node " +
			"(not registered on the local agent, or not synced yet); " +
			"this node cannot take part in the election until it is"
	case strings.Contains(msg, "in critical state"):
		// "Check '<id>' is in critical state"
		return "a health check of the session is critical; " +
			"this node cannot take part in the election until it is no longer critical"
	default:
		return ""
	}
}

// kvGetter is an interface for getting KV pairs, allowing for testing with mocks
type kvGetter interface {
	Get(key string, q *api.QueryOptions) (*api.KVPair, *api.QueryMeta, error)
}

// verifyLockKeyState checks if the trigger-key is compatible with Consul Lock API.
// Returns an error if a regular KV (non-lock) entry exists at the key path.
func verifyLockKeyState(kv kvGetter, key string) error {
	pair, _, err := kv.Get(key, nil)
	if err != nil {
		return fmt.Errorf("failed to query key state: %w", err)
	}

	// Key doesn't exist - this is fine, Lock will create it
	if pair == nil {
		slog.Info("Trigger key does not exist (will be created on first lock acquisition)", "key", key)
		return nil
	}

	// Key exists - check if it's a lock key or regular KV
	if pair.Flags != LockFlagValue {
		return fmt.Errorf(
			"existing key at '%s' is a regular KV entry (Flags=%d), not a lock key (expected Flags=%d). "+
				"This will cause 'ErrLockConflict: Existing key does not match lock use'. "+
				"Resolution: Run 'consul kv delete %s' before starting vip-elector, or change trigger-key to an unused path. "+
				"See: https://github.com/hashicorp/consul/blob/main/api/lock.go",
			key, pair.Flags, LockFlagValue, key,
		)
	}

	// Key exists with correct lock flag - this is fine (previous lock)
	slog.Info("Trigger key exists with correct lock flag", "key", key, "flags", pair.Flags, "session", pair.Session)
	return nil
}

// runConsul runs the election on Consul until ctx is cancelled.
func runConsul(ctx context.Context, config *Config, vipMgrConfig *VipManagerConfig, hostname string) error {
	// Initialize Consul client
	consulClient, err := createConsulClient(config, vipMgrConfig)
	if err != nil {
		return fmt.Errorf("failed to create Consul client: %w", err)
	}

	// Look up the check (if specified). This only warns; see checkWarning.
	if config.CheckID != "" {
		st, err := lookupAgentCheck(consulClient.Agent(), config.CheckID)
		if msg := checkWarning(st, err); msg != "" {
			if err != nil {
				slog.Warn(msg, "check_id", config.CheckID, "error", err)
			} else {
				slog.Warn(msg, "check_id", config.CheckID)
			}
		}
	}

	// Verify trigger-key state before attempting lock acquisition
	if err := verifyLockKeyState(consulClient.KV(), vipMgrConfig.TriggerKey); err != nil {
		return fmt.Errorf("trigger-key pre-flight check failed: %w", err)
	}

	// Main election loop
	return runElectionLoop(ctx, consulClient, vipMgrConfig, config, hostname)
}

func runElectionLoop(ctx context.Context, client *api.Client, vipMgrConfig *VipManagerConfig, config *Config, hostname string) error {
	backoff := time.Second
	maxBackoff := 30 * time.Second

	// Sessions we gave up on but could not destroy (Consul unreachable).
	// Such a session may still own the key, so it is destroyed before a new
	// attempt; otherwise the new session would queue behind our own stale
	// one until its TTL expires.
	var staleSessions []string

	for {
		select {
		case <-ctx.Done():
			slog.Info("Election loop shutting down")
			return nil
		default:
		}

		staleSessions = destroySessions(client, staleSessions)

		slog.Info("Attempting to create session and acquire lock")

		// Create session
		sessionID, err := createSession(client, config, hostname)
		if err != nil {
			if hint := sessionCreateHint(err); hint != "" {
				slog.Error("Failed to create session", "error", err, "hint", hint)
			} else {
				slog.Error("Failed to create session", "error", err)
			}
			slog.Info("Retrying", "backoff", backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		slog.Info("Session created", "session_id", sessionID)

		// Reset backoff on success
		backoff = time.Second

		// The Lock API only renews sessions it creates itself, so keep our
		// session alive for as long as this election attempt lasts.
		keepalive := startSessionKeepalive(ctx, client.Session(), sessionID, config.TTL)

		// giveUp ends this attempt: stop renewing and destroy the session.
		// Destroying it is what releases leadership: Behavior=delete removes
		// the key, and lock-delay holds other nodes off so that vip-manager
		// on this node gets time to drop the VIP. If the destroy fails the
		// session is retried on the next attempt, and expires with its TTL
		// if the process exits first.
		giveUp := func() {
			keepalive.stop()
			if !destroySession(client, sessionID) {
				staleSessions = append(staleSessions, sessionID)
			}
		}

		// Try to acquire lock
		lock, err := client.LockOpts(buildLockOptions(vipMgrConfig.TriggerKey, hostname, sessionID))
		if err != nil {
			slog.Error("Failed to create lock", "error", err)
			giveUp()
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		// Acquire lock (blocks until acquired, shutdown, or the session is lost)
		lockCh, err := lock.Lock(keepalive.stopCh())
		if err != nil {
			errorType := classifyLockAcquireError(err)
			switch errorType {
			case lockAcquireErrorConflict:
				giveUp()
				return fmt.Errorf("lock acquisition failed (%s): %w", errorType, err)
			case lockAcquireErrorConsulRetryable:
				slog.Warn("Lock acquisition failed", "error_type", errorType, "error", err)
			default:
				slog.Error("Lock acquisition failed", "error_type", errorType, "error", err)
			}

			giveUp()
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		if lockCh == nil {
			giveUp()
			if ctx.Err() != nil {
				slog.Info("Lock acquisition cancelled")
				return nil
			}
			// The session was invalidated while we were waiting for the lock
			// (e.g. the health check went critical on a standby node).
			slog.Warn("Session lost while waiting for lock - Re-entering election", "session_id", sessionID)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		slog.Info("LOCK ACQUIRED - This node is now the leader", "hostname", hostname, "session_id", sessionID)

		// Wait for leadership to be lost or context cancelled
		select {
		case <-lockCh:
			slog.Warn("LOCK LOST - Re-entering election")
		case <-keepalive.lost:
			slog.Warn("SESSION LOST - Re-entering election", "session_id", sessionID)
		case <-ctx.Done():
			slog.Info("Shutting down, releasing leadership")
			giveUp()
			return nil
		}

		// Leadership is gone as far as we can tell. Make sure Consul agrees
		// before trying again, so a session we no longer watch does not keep
		// the key.
		giveUp()

		// Small delay before re-election to avoid tight loop
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			return nil
		}
	}
}

func buildLockOptions(triggerKey, hostname, sessionID string) *api.LockOptions {
	return &api.LockOptions{
		Key:   triggerKey,
		Value: []byte(hostname),
		// Session (not SessionOpts) makes the lock use the session we created.
		// With SessionOpts the Lock API creates a second session of its own
		// and ignores the ID, so TTL, check-id and Behavior would not apply.
		Session: sessionID,
		// Retry monitor checks briefly so transient Consul 500 errors do not
		// immediately trigger lock-loss handling.
		MonitorRetries:   defaultMonitorRetries,
		MonitorRetryTime: defaultMonitorRetryTime,
		// Without LockTryOnce this only shortens each blocking query; the
		// attempt itself still waits for the lock indefinitely.
		LockWaitTime: defaultLockWaitTime,
	}
}

func classifyLockAcquireError(err error) lockAcquireErrorType {
	if errors.Is(err, api.ErrLockConflict) {
		return lockAcquireErrorConflict
	}
	if api.IsRetryableError(err) {
		return lockAcquireErrorConsulRetryable
	}
	return lockAcquireErrorUnknown
}

// sessionRenewer is an interface for renewing sessions, allowing for testing with mocks
type sessionRenewer interface {
	Renew(id string, q *api.WriteOptions) (*api.SessionEntry, *api.WriteMeta, error)
}

// sessionKeepalive renews a session in the background.
type sessionKeepalive struct {
	ctx    context.Context
	cancel context.CancelFunc
	// lost is closed when Consul reports that the session no longer exists.
	lost chan struct{}
	done chan struct{}
}

func startSessionKeepalive(parent context.Context, renewer sessionRenewer, sessionID string, ttl time.Duration) *sessionKeepalive {
	ctx, cancel := context.WithCancel(parent)
	k := &sessionKeepalive{
		ctx:    ctx,
		cancel: cancel,
		lost:   make(chan struct{}),
		done:   make(chan struct{}),
	}

	go func() {
		defer close(k.done)
		if err := renewSession(ctx, renewer, sessionID, ttl); err != nil {
			slog.Warn("Session is no longer valid", "session_id", sessionID, "error", err)
			close(k.lost)
			cancel()
		}
	}()

	return k
}

// stopCh is closed on shutdown, when the session is lost, or after stop().
func (k *sessionKeepalive) stopCh() <-chan struct{} {
	return k.ctx.Done()
}

// stop ends renewal and waits for the renew goroutine to exit.
func (k *sessionKeepalive) stop() {
	k.cancel()
	<-k.done
}

// renewSession renews the session every TTL/2 until ctx is cancelled.
// It returns api.ErrSessionExpired once Consul reports the session is gone,
// and nil when ctx is cancelled. Other renew errors are treated as transient:
// if Consul stays unreachable past the TTL the session expires server-side,
// which the next successful request reports.
func renewSession(ctx context.Context, renewer sessionRenewer, sessionID string, ttl time.Duration) error {
	if ttl <= 0 {
		// A session without TTL needs no renewal. validateConfig rejects
		// this; never spin if it gets here anyway.
		<-ctx.Done()
		return nil
	}

	interval := ttl / 2
	wait := interval

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}

		reqCtx, cancel := context.WithTimeout(ctx, interval)
		opts := &api.WriteOptions{}
		entry, _, err := renewer.Renew(sessionID, opts.WithContext(reqCtx))
		cancel()

		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			slog.Warn("Failed to renew session, retrying", "session_id", sessionID, "error", err)
			wait = min(sessionRenewRetryInterval, interval)
			continue
		}
		if entry == nil {
			return api.ErrSessionExpired
		}

		// Consul may adjust the TTL (e.g. to its configured minimum).
		if serverTTL, perr := time.ParseDuration(entry.TTL); perr == nil && serverTTL > 0 {
			interval = serverTTL / 2
		}
		wait = interval
	}
}

func buildSessionEntry(config *Config, name string) *api.SessionEntry {
	lockDelay := config.LockDelay
	if lockDelay == 0 {
		// The Consul client omits a zero LockDelay from the request and the
		// server then applies its 15s default. Send the smallest value the
		// API can express so that --lock-delay=0s means "no delay".
		lockDelay = time.Millisecond
	}

	sessionEntry := &api.SessionEntry{
		Name:      fmt.Sprintf("vip-elector-%s", name),
		Behavior:  "delete",
		TTL:       config.TTL.String(),
		LockDelay: lockDelay,
		Checks:    []string{"serfHealth"},
	}

	// Add service check if specified
	if config.CheckID != "" {
		sessionEntry.ServiceChecks = []api.ServiceCheck{
			{ID: config.CheckID},
		}
	}

	return sessionEntry
}

func createSession(client *api.Client, config *Config, name string) (string, error) {
	sessionID, _, err := client.Session().Create(buildSessionEntry(config, name), nil)
	if err != nil {
		return "", err
	}

	return sessionID, nil
}

// destroySessions destroys each session and returns the ones that could not
// be destroyed.
func destroySessions(client *api.Client, sessionIDs []string) []string {
	var remaining []string
	for _, id := range sessionIDs {
		if !destroySession(client, id) {
			remaining = append(remaining, id)
		}
	}
	return remaining
}

// destroySession destroys the session and reports whether Consul confirmed
// it. Destroying a session that no longer exists counts as success.
func destroySession(client *api.Client, sessionID string) bool {
	// Use independent context with timeout for session destruction
	destroyCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	opts := &api.WriteOptions{}
	opts = opts.WithContext(destroyCtx)

	_, err := client.Session().Destroy(sessionID, opts)
	if err != nil {
		slog.Error("Failed to destroy session", "session_id", sessionID, "error", err)
		return false
	}
	slog.Info("Session destroyed", "session_id", sessionID)
	return true
}
