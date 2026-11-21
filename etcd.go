package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"go.etcd.io/etcd/api/v3/mvccpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	// How long a single etcd request may take. The etcd client retries an
	// unreachable cluster until the context ends, so every request needs a
	// deadline to report the failure at all.
	etcdRequestTimeout = 5 * time.Second
	// gRPC keepalive pings detect a dead connection to an etcd member. gRPC
	// pings no more often than every 10s, and the etcd server closes
	// connections that ping more often than every 5s
	// (--grpc-keepalive-min-time). Lease renewal does not rely on this (see
	// renewLease).
	etcdDialKeepAliveTime    = 10 * time.Second
	etcdDialKeepAliveTimeout = 5 * time.Second
)

var (
	// How long to wait before reading the trigger key again when a read
	// failed after a watch ended. A variable so that tests can shorten it.
	etcdRereadInterval = time.Second
	// How long to wait before retrying a failed lease renewal. Short, so
	// that a renewal that hit a hung member is retried on another one well
	// within the TTL. A variable so that tests can shorten it.
	leaseRenewRetryInterval = 250 * time.Millisecond
)

var (
	// errLeaseLost means etcd reported the lease gone (expired or revoked).
	errLeaseLost = errors.New("lease lost")
	// errLeadershipLost means this node held the trigger key and no longer
	// does.
	errLeadershipLost = errors.New("leadership lost")
)

// plainKeyError reports a trigger key that has no lease. Such a key never
// expires, so no node could ever take it.
type plainKeyError struct {
	key string
}

func (e *plainKeyError) Error() string {
	return fmt.Sprintf(
		"existing key at '%s' has no lease (a plain key, e.g. written by 'etcdctl put'). "+
			"It never expires, so no node can acquire the lock. "+
			"Resolution: Run 'etcdctl del %s' before starting vip-elector, or change trigger-key to an unused path",
		e.key, e.key)
}

// etcdSettings is how vip-elector connects to etcd.
type etcdSettings struct {
	Endpoints []string
	CAFile    string
	CertFile  string
	KeyFile   string
	User      string
	Password  string
}

// resolveEtcdSettings returns the etcd connection settings.
//
// All dcs-endpoints are used: the etcd client fails over between them, as
// in vip-manager.
//
// The CA certificate comes from --etcd-ca-file, then etcd-ca-file in
// vip-manager.yml, then ETCDCTL_CACERT: verifying the etcd server is the same
// for both tools. Credentials (user and password, client certificate) come
// only from vip-elector's own flags and environment, never from
// vip-manager.yml: vip-manager only needs to read the trigger key, while
// vip-elector writes it. This is the same reason consul-token is not read.
//
// As with etcdctl, a user given as "name:password" without a separate
// password is split at the first colon.
func resolveEtcdSettings(config *Config, vipMgrConfig *VipManagerConfig, getenv func(string) string) (etcdSettings, error) {
	s := etcdSettings{
		Endpoints: vipMgrConfig.DCSEndpoints,
		CAFile:    firstNonEmpty(config.EtcdCAFile, string(vipMgrConfig.EtcdCAFile), getenv("ETCDCTL_CACERT")),
		CertFile:  firstNonEmpty(config.EtcdCertFile, getenv("ETCDCTL_CERT")),
		KeyFile:   firstNonEmpty(config.EtcdKeyFile, getenv("ETCDCTL_KEY")),
		User:      firstNonEmpty(config.EtcdUser, getenv("ETCDCTL_USER")),
		Password:  firstNonEmpty(config.EtcdPassword, getenv("ETCDCTL_PASSWORD")),
	}

	if s.Password == "" {
		if user, password, ok := strings.Cut(s.User, ":"); ok {
			s.User, s.Password = user, password
		}
	}

	switch {
	case s.User != "" && s.Password == "":
		return etcdSettings{}, fmt.Errorf("etcd user %q has no password: set --etcd-password or ETCDCTL_PASSWORD, or give the user as name:password", s.User)
	case s.User == "" && s.Password != "":
		return etcdSettings{}, errors.New("an etcd password is set but no user: set --etcd-user or ETCDCTL_USER")
	case s.CertFile != "" && s.KeyFile == "":
		return etcdSettings{}, errors.New("an etcd client certificate is set but no key: set --etcd-key-file or ETCDCTL_KEY")
	case s.CertFile == "" && s.KeyFile != "":
		return etcdSettings{}, errors.New("an etcd client key is set but no certificate: set --etcd-cert-file or ETCDCTL_CERT")
	}

	return s, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// buildEtcdTLS returns the TLS configuration for the etcd client. It is never
// nil, as in vip-manager: the etcd client uses a TLS configuration it is
// given for every endpoint scheme but http://, including endpoints without a
// scheme, so a nil one would make vip-elector talk plaintext where
// vip-manager talks TLS. Without a CA file the system CA certificates verify
// the server.
func buildEtcdTLS(s etcdSettings) (*tls.Config, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}

	if s.CAFile != "" {
		pem, err := os.ReadFile(s.CAFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read etcd CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no PEM certificate found in etcd CA file %s", s.CAFile)
		}
		tlsConfig.RootCAs = pool
	}

	if s.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(s.CertFile, s.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load etcd client certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	return tlsConfig, nil
}

// isPlaintextEndpoint reports whether the etcd client would connect to the
// endpoint without TLS although a TLS configuration is set: only for an
// http:// endpoint. It parses the endpoint the way the etcd client does.
func isPlaintextEndpoint(ep string) bool {
	if strings.HasPrefix(ep, "unix:") || strings.HasPrefix(ep, "unixs:") || !strings.Contains(ep, "://") {
		return false
	}
	u, err := url.Parse(ep)
	return err == nil && u.Scheme == "http"
}

// etcdUsesTLS reports whether the etcd client connects with TLS. It decides
// once, from the first endpoint, for all of them.
func etcdUsesTLS(endpoints []string) bool {
	return len(endpoints) > 0 && !isPlaintextEndpoint(endpoints[0])
}

// etcdSettingsWarnings returns warnings about settings that are valid but
// probably not what was meant.
func etcdSettingsWarnings(s etcdSettings, vipMgrConfig *VipManagerConfig) []string {
	var warnings []string

	useTLS := etcdUsesTLS(s.Endpoints)
	for i, ep := range s.Endpoints {
		if i > 0 && isPlaintextEndpoint(ep) == useTLS {
			mode := "without TLS"
			if useTLS {
				mode = "with TLS"
			}
			warnings = append(warnings, fmt.Sprintf(
				"dcs-endpoints mix http:// with other schemes, but the etcd client connects to all of them %s, "+
					"as the first one (%s) says, so %s is connected to %s too; use the same scheme for all of them",
				mode, s.Endpoints[0], ep, mode))
			break
		}
	}

	if !useTLS && (s.CAFile != "" || s.CertFile != "") {
		warnings = append(warnings, "TLS files are set, but dcs-endpoints use http://, so the etcd client connects without TLS; use https://")
	}

	if s.User == "" && s.CertFile == "" && (vipMgrConfig.EtcdUser != "" || vipMgrConfig.EtcdCertFile != "") {
		warnings = append(warnings, "vip-manager.yml sets etcd-user or etcd-cert-file, but vip-elector does not use vip-manager's credentials "+
			"(vip-manager only reads the trigger key, vip-elector writes it). "+
			"If etcd requires authentication, give vip-elector its own with --etcd-user/--etcd-password, "+
			"--etcd-cert-file/--etcd-key-file, or the ETCDCTL_* environment variables")
	}

	return warnings
}

// etcdClientLogger returns the logger for the etcd client. Its warnings
// (e.g. every retry while a member is unreachable) would flood the log, so
// only errors are kept; vip-elector logs the failures that matter itself.
//
// The warnings are the only place that says why a connection fails (e.g. a
// TLS certificate that is not trusted). ETCD_CLIENT_DEBUG, the etcd client's
// own variable (a level such as "warn" or "debug"), lowers the level to show
// them. It is read the way the etcd client reads it, which also uses it for
// the gRPC logs.
func etcdClientLogger() (*zap.Logger, error) {
	level := zapcore.ErrorLevel
	if os.Getenv("ETCD_CLIENT_DEBUG") != "" {
		level = clientv3.ClientLogLevel()
	}

	cfg := zap.NewProductionConfig()
	cfg.Level = zap.NewAtomicLevelAt(level)
	cfg.Sampling = nil
	return cfg.Build()
}

// etcdConnectHint explains a pre-flight read that etcd did not answer in
// time, or returns "" for any other error. The etcd client retries until the
// deadline and then reports only that, not why it could not connect.
func etcdConnectHint(err error) string {
	if !errors.Is(err, context.DeadlineExceeded) {
		return ""
	}
	return "etcd did not answer: check that dcs-endpoints are reachable and, for TLS, " +
		"the CA certificate (--etcd-ca-file, etcd-ca-file, ETCDCTL_CACERT) and the client certificate " +
		"(an endpoint without http:// is connected to with TLS, as in vip-manager); " +
		"set ETCD_CLIENT_DEBUG=warn to see the etcd client's connection errors"
}

func createEtcdClient(s etcdSettings) (*clientv3.Client, error) {
	tlsConfig, err := buildEtcdTLS(s)
	if err != nil {
		return nil, err
	}
	logger, err := etcdClientLogger()
	if err != nil {
		return nil, fmt.Errorf("failed to create etcd client logger: %w", err)
	}

	client, err := clientv3.New(clientv3.Config{
		Endpoints:            s.Endpoints,
		TLS:                  tlsConfig,
		Username:             s.User,
		Password:             s.Password,
		DialTimeout:          etcdRequestTimeout,
		DialKeepAliveTime:    etcdDialKeepAliveTime,
		DialKeepAliveTimeout: etcdDialKeepAliveTimeout,
		Logger:               logger,
	})
	if err != nil {
		return nil, err
	}

	slog.Info("etcd client", "endpoints", s.Endpoints, "tls", etcdUsesTLS(s.Endpoints), "user", s.User)
	return client, nil
}

func runEtcd(config *Config, vipMgrConfig *VipManagerConfig, hostname string) error {
	settings, err := resolveEtcdSettings(config, vipMgrConfig, os.Getenv)
	if err != nil {
		return fmt.Errorf("invalid etcd settings: %w", err)
	}
	for _, msg := range etcdSettingsWarnings(settings, vipMgrConfig) {
		slog.Warn(msg)
	}

	client, err := createEtcdClient(settings)
	if err != nil {
		return fmt.Errorf("failed to create etcd client: %w", err)
	}
	defer client.Close()

	if err := verifyEtcdKeyState(context.Background(), client, vipMgrConfig.TriggerKey); err != nil {
		if hint := etcdConnectHint(err); hint != "" {
			return fmt.Errorf("trigger-key pre-flight check failed: %w (hint: %s)", err, hint)
		}
		return fmt.Errorf("trigger-key pre-flight check failed: %w", err)
	}

	// Signals are handled from here on: before, there is nothing to clean up
	// and the default action ends the process at once.
	ctx, stop := shutdownContext()
	defer stop()

	e := &etcdElector{
		kv:        client,
		lease:     client,
		watcher:   client,
		key:       vipMgrConfig.TriggerKey,
		value:     hostname,
		ttl:       int64(config.TTL / time.Second),
		lockDelay: config.LockDelay,
	}
	return e.run(ctx)
}

// verifyEtcdKeyState checks that the trigger key is either absent or held by
// a lease. A plain key never expires, so the election could never start.
func verifyEtcdKeyState(ctx context.Context, kv clientv3.KV, key string) error {
	reqCtx, cancel := context.WithTimeout(ctx, etcdRequestTimeout)
	defer cancel()
	resp, err := kv.Get(reqCtx, key)
	if err != nil {
		return fmt.Errorf("failed to query key state: %w", err)
	}

	if len(resp.Kvs) == 0 {
		slog.Info("Trigger key does not exist (will be created on first lock acquisition)", "key", key)
		return nil
	}

	kv0 := resp.Kvs[0]
	if kv0.Lease == 0 {
		return &plainKeyError{key: key}
	}

	slog.Info("Trigger key is held by a lease", "key", key, "value", string(kv0.Value), "lease_id", leaseHex(clientv3.LeaseID(kv0.Lease)))
	return nil
}

// etcdElector runs the election on etcd.
//
// Each attempt grants a lease and keeps it alive. The node takes the trigger
// key with a transaction that writes it only if it does not exist, attached
// to the lease. etcd deletes the key when the lease is revoked or expires,
// which is what Behavior=delete does for a Consul session.
//
// etcd has no lock-delay. Every node waits lock-delay after it finds the key
// free before it takes it, so that vip-manager on the previous leader has
// time to remove the VIP.
type etcdElector struct {
	kv        clientv3.KV
	lease     clientv3.Lease
	watcher   clientv3.Watcher
	key       string
	value     string
	ttl       int64 // seconds
	lockDelay time.Duration
}

func (e *etcdElector) run(ctx context.Context) error {
	backoff := time.Second
	maxBackoff := 30 * time.Second

	// Leases we gave up on but could not revoke (etcd unreachable). Such a
	// lease may still hold the key, so it is revoked before a new attempt;
	// otherwise the new attempt would wait for our own stale lease to expire.
	var staleLeases []clientv3.LeaseID

	sleep := func(d time.Duration) bool {
		select {
		case <-time.After(d):
			return true
		case <-ctx.Done():
			return false
		}
	}

	for {
		select {
		case <-ctx.Done():
			slog.Info("Election loop shutting down")
			return nil
		default:
		}

		staleLeases = e.revokeLeases(staleLeases)

		slog.Info("Attempting to grant lease and acquire lock")

		leaseID, grantedTTL, err := e.grantLease(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.Error("Failed to grant lease", "error", err)
			slog.Info("Retrying", "backoff", backoff)
			if !sleep(backoff) {
				return nil
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		// Reset backoff on success
		backoff = time.Second

		keepalive := startLeaseKeepalive(ctx, e.lease, leaseID, grantedTTL)

		err = e.campaign(ctx, leaseID, keepalive.lost)

		// Report why the attempt ended before giving it up.
		var plainKey *plainKeyError
		retryAfter, growBackoff := backoff, true
		switch {
		case ctx.Err() != nil, errors.As(err, &plainKey):
		case errors.Is(err, errLeadershipLost):
			// Small delay before re-election to avoid tight loop
			retryAfter, growBackoff = time.Second, false
		case errors.Is(err, errLeaseLost):
			slog.Warn("Lease lost while waiting for lock - Re-entering election", "lease_id", leaseHex(leaseID))
		default:
			slog.Error("Lock acquisition failed", "error", err)
		}

		// Giving up the attempt always revokes the lease. That deletes the
		// key if this node holds it. If the revoke fails, the lease is
		// retried on the next attempt, and expires with its TTL if the
		// process exits first.
		keepalive.stop()
		if !e.revokeLease(leaseID) {
			staleLeases = append(staleLeases, leaseID)
		}

		if ctx.Err() != nil {
			return nil
		}
		if plainKey != nil {
			return fmt.Errorf("lock acquisition failed: %w", err)
		}

		if !sleep(retryAfter) {
			return nil
		}
		if growBackoff {
			backoff = min(backoff*2, maxBackoff)
		}
	}
}

// grantLease grants a lease and returns it with the TTL etcd granted.
func (e *etcdElector) grantLease(ctx context.Context) (clientv3.LeaseID, time.Duration, error) {
	reqCtx, cancel := context.WithTimeout(ctx, etcdRequestTimeout)
	defer cancel()
	resp, err := e.lease.Grant(reqCtx, e.ttl)
	if err != nil {
		return 0, 0, err
	}
	slog.Info("Lease granted", "lease_id", leaseHex(resp.ID), "ttl", time.Duration(resp.TTL)*time.Second)
	if resp.TTL != e.ttl {
		// etcd raises a TTL below its minimum (2s with default settings).
		slog.Warn("etcd changed the lease TTL", "requested", time.Duration(e.ttl)*time.Second, "granted", time.Duration(resp.TTL)*time.Second)
	}
	return resp.ID, time.Duration(resp.TTL) * time.Second, nil
}

// campaign waits until the trigger key is free, takes it with the lease and
// holds it. It returns errLeadershipLost once this node held the key and lost
// it, errLeaseLost if the lease is lost before that, a *plainKeyError if the
// key can never be taken, and nil when ctx is cancelled.
func (e *etcdElector) campaign(ctx context.Context, leaseID clientv3.LeaseID, leaseLost <-chan struct{}) error {
	var lastHolder string
	for {
		st, rev, err := e.readKey(ctx)
		if err != nil {
			return e.cancelled(ctx, err)
		}

		switch {
		case st.exists && st.lease == leaseID:
			// Our own write. Not expected today (a failed transaction ends
			// the attempt, which revokes the lease), but if it happens this
			// node must hold the key as leader, not wait on it as a standby
			// while vip-manager gives it the VIP.
		case st.exists && st.lease == 0:
			return &plainKeyError{key: e.key}
		case st.exists:
			if st.value != lastHolder {
				slog.Info("Lock is held by another node, waiting", "holder", st.value)
				lastHolder = st.value
			}
			// A plain write over the key also ends the wait, so that it is
			// reported instead of waited on forever.
			err := e.waitForKey(ctx, rev, leaseLost, func(s keyState) bool { return !s.exists || s.lease == 0 })
			if err != nil {
				return e.cancelled(ctx, err)
			}
			continue
		default:
			if e.lockDelay > 0 {
				slog.Info("Trigger key is free, waiting lock-delay before taking it", "lock_delay", e.lockDelay)
				quiet, err := e.waitUnchanged(ctx, rev, e.lockDelay, leaseLost)
				if err != nil {
					return e.cancelled(ctx, err)
				}
				if !quiet {
					// Another node took the key (and maybe already let it
					// go) during the wait: lock-delay counts from then.
					continue
				}
			}
			acquired, txnRev, err := e.tryAcquire(ctx, leaseID)
			if err != nil {
				return e.cancelled(ctx, err)
			}
			if !acquired {
				// Another node was faster.
				continue
			}
			rev = txnRev
		}

		slog.Info("LOCK ACQUIRED - This node is now the leader", "hostname", e.value, "lease_id", leaseHex(leaseID))
		return e.holdLock(ctx, leaseID, rev, leaseLost)
	}
}

// cancelled returns nil if ctx was cancelled (logging it), err otherwise.
func (e *etcdElector) cancelled(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		slog.Info("Lock acquisition cancelled")
		return nil
	}
	return err
}

// holdLock watches the trigger key from rev until this node no longer holds
// it, the lease is lost, or ctx is cancelled.
func (e *etcdElector) holdLock(ctx context.Context, leaseID clientv3.LeaseID, rev int64, leaseLost <-chan struct{}) error {
	err := e.waitForKey(ctx, rev, leaseLost, func(s keyState) bool { return !s.exists || s.lease != leaseID })
	switch {
	case ctx.Err() != nil:
		slog.Info("Shutting down, releasing leadership")
		return nil
	case errors.Is(err, errLeaseLost):
		slog.Warn("LEASE LOST - Re-entering election", "lease_id", leaseHex(leaseID))
		return errLeadershipLost
	case err != nil:
		return err
	default:
		slog.Warn("LOCK LOST - Re-entering election")
		return errLeadershipLost
	}
}

func (e *etcdElector) tryAcquire(ctx context.Context, leaseID clientv3.LeaseID) (bool, int64, error) {
	reqCtx, cancel := context.WithTimeout(ctx, etcdRequestTimeout)
	defer cancel()
	resp, err := e.kv.Txn(reqCtx).
		If(clientv3.Compare(clientv3.CreateRevision(e.key), "=", 0)).
		Then(clientv3.OpPut(e.key, e.value, clientv3.WithLease(leaseID))).
		Commit()
	if err != nil {
		return false, 0, fmt.Errorf("failed to write the trigger key: %w", err)
	}
	return resp.Succeeded, resp.Header.Revision, nil
}

// keyState is what the trigger key looks like at some revision.
type keyState struct {
	exists bool
	lease  clientv3.LeaseID
	value  string
}

func stateFromKV(kv *mvccpb.KeyValue) keyState {
	return keyState{exists: true, lease: clientv3.LeaseID(kv.Lease), value: string(kv.Value)}
}

// readKey reads the trigger key and returns its state and the revision of the
// read.
func (e *etcdElector) readKey(ctx context.Context) (keyState, int64, error) {
	reqCtx, cancel := context.WithTimeout(ctx, etcdRequestTimeout)
	defer cancel()
	resp, err := e.kv.Get(reqCtx, e.key)
	if err != nil {
		return keyState{}, 0, fmt.Errorf("failed to read the trigger key: %w", err)
	}
	if len(resp.Kvs) == 0 {
		return keyState{}, resp.Header.Revision, nil
	}
	return stateFromKV(resp.Kvs[0]), resp.Header.Revision, nil
}

// waitForKey returns nil once done reports true for a state of the trigger
// key after rev, errLeaseLost when the lease is lost, and ctx.Err() when ctx
// is cancelled.
//
// When a watch ends without an answer (etcd compacted the revision it
// started from, or the member lost its leader), the key is read again and
// watched from there. Read errors are retried: a leader keeps the key while
// etcd is unreachable, until the lease keepalive gives up.
func (e *etcdElector) waitForKey(ctx context.Context, rev int64, leaseLost <-chan struct{}, done func(keyState) bool) error {
	for {
		matched, err := e.watchKey(ctx, rev, leaseLost, done)
		if err != nil || matched {
			return err
		}

		for {
			st, readRev, err := e.readKey(ctx)
			if err == nil {
				if done(st) {
					return nil
				}
				rev = readRev
				break
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Warn("Failed to read the trigger key, retrying", "error", err)
			select {
			case <-time.After(etcdRereadInterval):
			case <-ctx.Done():
				return ctx.Err()
			case <-leaseLost:
				return errLeaseLost
			}
		}
	}
}

// waitUnchanged returns true once the trigger key has not changed for d after
// rev, and false as soon as it changes or the watch ends (the caller reads
// the key again and starts over). A node that waits lock-delay must not take
// the key if another node held it meanwhile: the delay has to count from
// that node's release, or it could be cut short to almost nothing.
func (e *etcdElector) waitUnchanged(ctx context.Context, rev int64, d time.Duration, leaseLost <-chan struct{}) (bool, error) {
	watchCtx, cancel := context.WithCancel(clientv3.WithRequireLeader(ctx))
	defer cancel()
	watchCh := e.watcher.Watch(watchCtx, e.key, clientv3.WithRev(rev+1))

	timer := time.NewTimer(d)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			return true, nil
		case <-ctx.Done():
			return false, ctx.Err()
		case <-leaseLost:
			return false, errLeaseLost
		case resp, ok := <-watchCh:
			switch {
			case !ok:
				if ctx.Err() != nil {
					return false, ctx.Err()
				}
				return false, nil
			case resp.Err() != nil:
				slog.Warn("Watch on the trigger key ended, reading the key again", "error", resp.Err())
				return false, nil
			case len(resp.Events) > 0:
				return false, nil
			}
		}
	}
}

// watchKey watches the trigger key from rev+1. It returns true when done
// reports true for an event, and false when the watch ended without one.
func (e *etcdElector) watchKey(ctx context.Context, rev int64, leaseLost <-chan struct{}, done func(keyState) bool) (bool, error) {
	// WithRequireLeader: a member cut off from the rest of the cluster
	// cancels the watch instead of staying silent.
	watchCtx, cancel := context.WithCancel(clientv3.WithRequireLeader(ctx))
	defer cancel()
	watchCh := e.watcher.Watch(watchCtx, e.key, clientv3.WithRev(rev+1))

	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-leaseLost:
			return false, errLeaseLost
		case resp, ok := <-watchCh:
			if !ok {
				if ctx.Err() != nil {
					return false, ctx.Err()
				}
				return false, nil
			}
			if err := resp.Err(); err != nil {
				slog.Warn("Watch on the trigger key ended, reading the key again", "error", err)
				return false, nil
			}
			for _, ev := range resp.Events {
				st := keyState{}
				if ev.Type == mvccpb.PUT {
					st = stateFromKV(ev.Kv)
				}
				if done(st) {
					return true, nil
				}
			}
		}
	}
}

// revokeLeases revokes each lease and returns the ones that could not be
// revoked.
func (e *etcdElector) revokeLeases(ids []clientv3.LeaseID) []clientv3.LeaseID {
	var remaining []clientv3.LeaseID
	for _, id := range ids {
		if !e.revokeLease(id) {
			remaining = append(remaining, id)
		}
	}
	return remaining
}

// revokeLease revokes the lease and reports whether etcd confirmed it.
// Revoking a lease that no longer exists counts as success.
func (e *etcdElector) revokeLease(id clientv3.LeaseID) bool {
	// Use independent context with timeout, so that this also runs on
	// shutdown.
	ctx, cancel := context.WithTimeout(context.Background(), etcdRequestTimeout)
	defer cancel()

	_, err := e.lease.Revoke(ctx, id)
	switch {
	case err == nil:
		slog.Info("Lease revoked", "lease_id", leaseHex(id))
		return true
	case errors.Is(err, rpctypes.ErrLeaseNotFound):
		slog.Info("Lease already gone", "lease_id", leaseHex(id))
		return true
	default:
		slog.Error("Failed to revoke lease", "lease_id", leaseHex(id), "error", err)
		return false
	}
}

// leaseKeepalive renews a lease in the background.
type leaseKeepalive struct {
	cancel context.CancelFunc
	// lost is closed when etcd reports the lease gone.
	lost chan struct{}
	done chan struct{}
}

func startLeaseKeepalive(parent context.Context, lease clientv3.Lease, id clientv3.LeaseID, ttl time.Duration) *leaseKeepalive {
	ctx, cancel := context.WithCancel(parent)
	k := &leaseKeepalive{
		cancel: cancel,
		lost:   make(chan struct{}),
		done:   make(chan struct{}),
	}

	go func() {
		defer close(k.done)
		if err := renewLease(ctx, lease, id, ttl); err != nil {
			slog.Warn("Lease is no longer valid", "lease_id", leaseHex(id), "error", err)
			close(k.lost)
		}
	}()

	return k
}

// renewLease renews the lease every TTL/3 until ctx is cancelled. It returns
// errLeaseLost once etcd reports the lease gone, and nil when ctx is
// cancelled.
//
// It does not use the etcd client's KeepAlive stream. That stream stays on
// one etcd member, and the client moves it only when gRPC keepalive declares
// the connection dead, which takes at least 15s (gRPC pings no more often
// than every 10s). If that member hangs, the other members let the lease
// expire meanwhile, while the cluster is healthy. Each KeepAliveOnce opens a
// new stream, which the client's balancer places on the next member, and a
// deadline of half the interval bounds each call, so a renewal that hit a
// hung member is retried on another one well within the TTL.
//
// Other errors are retried for as long as etcd does not answer: leadership
// is not given up while etcd is unreachable, as with Consul. etcd keeps
// leases through a loss of quorum or a restart, and a lease that expired
// meanwhile is reported as not found once etcd answers again (its key is
// deleted with it, which the watch reports too). The KeepAlive stream gives
// up after one TTL without an answer instead, and revoking then would delete
// a key that etcd kept.
func renewLease(ctx context.Context, lease clientv3.Lease, id clientv3.LeaseID, ttl time.Duration) error {
	if ttl <= 0 {
		// validateConfig rejects this; never spin if it gets here anyway.
		<-ctx.Done()
		return nil
	}

	interval := ttl / 3
	wait := interval

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}

		reqCtx, cancel := context.WithTimeout(ctx, interval/2)
		resp, err := lease.KeepAliveOnce(reqCtx, id)
		cancel()

		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, rpctypes.ErrLeaseNotFound) {
			return errLeaseLost
		}
		if err != nil {
			slog.Warn("Failed to renew lease, retrying", "lease_id", leaseHex(id), "error", err)
			wait = min(leaseRenewRetryInterval, interval)
			continue
		}

		// etcd may have raised the TTL (to its minimum).
		if resp.TTL > 0 {
			interval = time.Duration(resp.TTL) * time.Second / 3
		}
		wait = interval
	}
}

// stop ends the keepalive and waits for its goroutine to exit.
func (k *leaseKeepalive) stop() {
	k.cancel()
	<-k.done
}

// leaseHex formats a lease ID the way etcdctl prints it.
func leaseHex(id clientv3.LeaseID) string {
	return fmt.Sprintf("%016x", int64(id))
}
