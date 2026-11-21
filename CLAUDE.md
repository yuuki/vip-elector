# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

vip-elector is a leader election daemon that works in conjunction with [vip-manager](https://github.com/cybertec-postgresql/vip-manager). It performs distributed leader election using Consul Session and KV Lock, or an etcd lease, while vip-manager handles the actual VIP (Virtual IP) assignment. **Both tools are required for a complete VIP failover solution.**

### Critical Design Principle

**Configuration as Single Source of Truth**: vip-elector reads `vip-manager.yml` to obtain the DCS endpoints and the trigger key. This ensures both vip-elector and vip-manager use consistent configuration without duplication. The three required fields from vip-manager.yml are:
- `dcs-type` (`consul`, or `etcd` / `etcd3` as in vip-manager; case-sensitive like vip-manager)
- `dcs-endpoints` (DCS address list)
- `trigger-key` (KV path for leader hostname)

Optional field also read: `trigger-value` (the lock value). It must be a YAML string (`yamlString`): vip-manager reads the file through viper, which converts numbers and booleans (`01` → `1`), so unquoted ones are rejected. With etcd, `etcd-ca-file` (also a `yamlString`) is read too, since verifying the server is the same for both tools.

`consul-token` is deliberately not read: vip-manager needs only key read access while vip-elector needs session and key write access, so falling back to vip-manager's token could break a setup that works with the agent's default token. For the same reason vip-manager's `etcd-user`, `etcd-password`, `etcd-cert-file` and `etcd-key-file` are not used; `etcd-user` and `etcd-cert-file` are read (as plain strings) only to warn that vip-elector has no credentials of its own.

## Architecture

### Core Flow

`main.go` holds what does not depend on the DCS (flags, vip-manager.yml, lock value, signals) and `run()` dispatches on `dcs-type` to `runConsul` (`consul.go`) or `runEtcd` (`etcd.go`). `validateDCSConfig` rejects flags that do not apply to the selected DCS instead of ignoring them (`--check-id`, `--addr`, `--token` with etcd; `--etcd-*` with Consul). Ignoring `--check-id` would leave a node that is believed to fail over on health but does not.

1. **Configuration Priority Chain**:
   - CLI flags override vip-manager.yml
   - vip-manager.yml overrides environment variables
   - Environment variables are the fallback

2. **Election Loop** (`runElectionLoop`, Consul):
   - Creates Consul Session with `Behavior: "delete"` so keys are auto-cleaned on session invalidation
   - Session includes `ServiceChecks` to tie leader election to HTTP health status
   - Renews the session TTL in the background (`startSessionKeepalive` / `renewSession`)
   - Acquires KV lock with hostname as the value, using that session (`LockOptions.Session`)
   - Blocks on `lockCh` (lock lost) and `keepalive.lost` (session gone) to detect loss of leadership
   - Destroys the session whenever leadership is given up, so the key is deleted. A destroy that fails (Consul unreachable) is retried before the next attempt; otherwise the session expires with its TTL
   - Implements exponential backoff (1s → 30s) for retry resilience

3. **Session Configuration** (`createSession`):
   - Always includes `serfHealth` check (Consul agent health)
   - Optionally includes service check ID (via `--check-id`) for HTTP health integration
   - Uses `Behavior: "delete"` to ensure clean failover (key deletion on session loss)

### Key State Transitions

```
STARTUP → Session Creation → Lock Acquisition → LEADER
                   ↓ (fail)            ↓ (fail)      ↓ (lock lost)
              Backoff Retry ← ←  Backoff Retry ← ← RE-ELECTION
```

When a node loses leadership (health check fails, process stops, or network partition), the Consul session invalidates, the KV lock is released, and another node can acquire it.

## Development Commands

### Build
```bash
go build -o vip-elector
```

### Run locally (requires Consul and vip-manager.yml)
```bash
./vip-elector --vip-manager-config=/path/to/vip-manager.yml --check-id=vip-http
```

### Testing with minimal setup
Create a test vip-manager.yml:
```yaml
dcs-type: consul
dcs-endpoints: ["http://127.0.0.1:8500"]
trigger-key: "test/vip/lock"
```

Run against local Consul:
```bash
consul agent -dev  # In another terminal
./vip-elector --vip-manager-config=test-config.yml
```

For etcd, use `dcs-type: etcd`, `dcs-endpoints: ["http://127.0.0.1:2379"]` and a key such as `/test/vip/lock`, and start etcd with `make etcd-up` (or `etcd` in another terminal).

## Important Implementation Details

### Lock Value (`resolveLockValue`)
- The value written to the trigger key must equal vip-manager's `trigger-value`, otherwise vip-manager never assigns the VIP (no error on either side)
- Precedence: `--hostname` flag > `trigger-value` from vip-manager.yml > `os.Hostname()` unmodified (vip-manager's own default; do not shorten it)
- `warnIfFlagDiffersFromVipManager` warns when `--hostname` differs from vip-manager's default value on this node

### Health Check Verification (`lookupAgentCheck`, `checkWarning`)
- If `--check-id` is provided, the program looks it up on the local Consul agent at startup
- The agent hides service checks from a token without `service:read` and does not tell the Go client (the agent endpoint sends no `X-Consul-LastContact`, so `QueryMeta.ResultsFilteredByACLs` is never parsed). "Not found" may therefore mean "hidden"; the warning says so
- A missing or critical check generates a WARNING but doesn't fail startup. The check ID is still sent: Consul refuses to create the session (`Missing check '<id>' registration`), and the election loop keeps retrying, so the node is not a candidate until the check is registered. This covers the service being registered after vip-elector starts at boot
- Never drop the check ID to "continue without it": that would silently turn a typo into a node that never fails over on health
- Consul also refuses to create a session while one of its checks is critical (`Check '<id>' is in critical state`; `warning` is accepted). `sessionCreateHint` adds a `hint` to the error log for both cases
- Consul validates the checks against the catalog entry of this node, not the agent: a check registered on the agent but not yet synced also fails session creation, which is one more reason to retry rather than exit

### Configuration Precedence (createConsulClient)
Address resolution: `--addr` CLI flag > `dcs-endpoints[0]` from vip-manager.yml (`dcs-endpoints` is required, so `CONSUL_HTTP_ADDR` never applies)
- `normalizeEndpoint` reads the endpoint like vip-manager (`url.Parse`, lowercase scheme, scheme + host only, path dropped) and keeps the scheme: `api.NewClient` switches to HTTPS for `https://`; stripping the scheme would talk plain HTTP to a TLS endpoint
- Only the first `dcs-endpoints` entry is used, as in vip-manager's Consul checker
- `--addr` is passed to the Consul client unmodified
Token resolution: `--token` CLI flag > `CONSUL_HTTP_TOKEN` env

### Session Ownership and Renewal
- The lock must be held by the session `createSession` returns. Pass it as `LockOptions.Session`; `LockOptions.SessionOpts` makes the Lock API create a second, default session (no TTL, no service check, `Behavior: release`) and ignores the ID
- The Lock API only renews sessions it creates itself, so vip-elector renews the TTL every TTL/2
- A renew answer of "session not found" means leadership is lost; other renew errors are retried indefinitely (leadership is not given up while Consul is unreachable)
- `--ttl` must be > 0 (Consul treats 0 as "never expires"). `--lock-delay=0s` is sent as 1ms because the Consul client drops a zero value and the server would apply its 15s default

### Graceful Shutdown
- SIGINT/SIGTERM trigger context cancellation
- A standby blocked in `lock.Lock()` only sees its stop channel between blocking queries, so `LockOptions.LockWaitTime` is set to 2s (Lock API default: 15s). This bounds both shutdown of a standby and its reaction to a lost session. A hung Consul still blocks the query (the Lock API takes no context). Trade-off: a standby issues a query every 2s, so during a Consul leader election it is more likely to get a 500, give up the attempt and retry with backoff (the leader is unaffected: `monitorLock` sends no `wait`). Do not go much lower
- Renewal stops and the session is destroyed; `Behavior: "delete"` removes the key and lock-delay applies
- The lock is deliberately not released with `lock.Unlock()`: that leaves the old hostname in the key and lets a standby take over before vip-manager on this node has removed the VIP

## Consul Integration Specifics

### Required ACL Permissions
```hcl
session "write" {}  # Create sessions
key_prefix "your-trigger-prefix/" { policy = "write" }  # KV lock operations
agent_prefix "" { policy = "read" }  # Check verification
service_prefix "" { policy = "read" }  # Optional: lets the startup check see the --check-id service check
```

### Environment Variables Supported
- `CONSUL_HTTP_ADDR`: not used for the address (`dcs-endpoints` is required and wins)
- `CONSUL_HTTP_TOKEN`: ACL token
- `CONSUL_CACERT`, `CONSUL_CLIENT_CERT`, `CONSUL_CLIENT_KEY`: TLS configuration

These are handled by the Consul SDK's `api.DefaultConfig()` automatically.

## etcd Integration Specifics

### Election (`etcdElector`)
- Each attempt grants a lease (`--ttl`, whole seconds: `validateDCSConfig` rejects fractions; etcd silently raises a TTL below its minimum, so `grantLease` warns) and keeps it alive with `startLeaseKeepalive`
- The key is taken by `tryAcquire`: a txn `CreateRevision(key) == 0` → `Put(key, value, WithLease)`. etcd deletes the key when the lease is revoked or expires, which is what `Behavior: "delete"` does for Consul
- etcd has no lock-delay: `campaign` waits `--lock-delay` every time it finds the key free, also at startup (it cannot tell how recently the key was deleted). The delay is only kept by vip-elector nodes
- `campaign` treats a key already held by its own lease as acquired (a txn that timed out on the client after etcd applied it); otherwise this node would hold the key without watching it
- A key without a lease (`Lease == 0`, e.g. `etcdctl put`) never expires: `verifyEtcdKeyState` refuses it at startup and `campaign` refuses it later (`plainKeyError`, fatal like Consul's lock conflict). A standby's wait also ends on a plain write over the held key, so it is reported instead of waited on forever
- Leadership ends (`holdLock`) when the key is deleted or written by another lease, or `keepalive.lost` closes. Giving up always revokes the lease; a revoke that fails is retried before the next attempt (`staleLeases`), otherwise the lease expires with its TTL. `ErrLeaseNotFound` counts as revoked
- The run loop logs why an attempt ended before it revokes the lease
- Unlike Consul, leadership is given up when the etcd client has had no keepalive answer for the TTL: the client closes the KeepAlive channel then, and etcd expires the lease and deletes the key anyway. vip-manager has dropped the VIP before that (it sends `false` on any failed read)

### Watches and Requests
- `waitForKey` watches from the read revision + 1 with `clientv3.WithRequireLeader`, so a member cut off from the cluster cancels the watch instead of staying silent. When a watch ends (compaction, no leader) it re-reads the key and watches again; read errors are retried every `etcdRereadInterval` until the lease is lost
- Every request has a deadline (`etcdRequestTimeout`, 5s): the etcd client retries an unreachable cluster until its context ends
- gRPC keepalive pings stay above 5s (`etcdDialKeepAliveTime`): the etcd server closes connections that ping more often (`--grpc-keepalive-min-time`)

### Connection Settings (`resolveEtcdSettings`, `buildEtcdTLS`)
- All `dcs-endpoints` are passed to the etcd client as is (it fails over between them), as vip-manager does
- CA: `--etcd-ca-file` > `etcd-ca-file` in vip-manager.yml > `ETCDCTL_CACERT`. Client certificate: `--etcd-cert-file`/`--etcd-key-file` > `ETCDCTL_CERT`/`ETCDCTL_KEY`. User: `--etcd-user`/`--etcd-password` > `ETCDCTL_USER`/`ETCDCTL_PASSWORD`; a user `name:password` without a password is split at the first colon, like etcdctl
- The etcd client connects to `http://` endpoints without TLS even when a TLS config is set (`CredsDrop`), so `etcdSettingsWarnings` warns. `https://` without a CA file uses the system CAs. A CA file without a PEM certificate is an error (vip-manager ignores it and then fails every handshake)
- The etcd client logger keeps only errors (its warnings repeat on every retry). Those warnings are the only place that says why a connection fails, so `ETCD_CLIENT_DEBUG` (read with `clientv3.ClientLogLevel`, as the client does) lowers the level, and `etcdConnectHint` says so on a pre-flight timeout

### Required Permissions
With etcd auth: `readwrite` on the trigger key for vip-elector, `read` for vip-manager. Granting a lease needs only a valid user; revoking it needs write access to the attached key.

## Common Issues and Design Rationale

### Why Behavior: "delete"?
When a session invalidates, the KV entry is deleted entirely. This is intentional: vip-manager interprets a missing or non-matching value as "not this node's VIP", preventing split-brain scenarios.

### Why Verify Check ID at Startup?
Early validation makes misconfiguration visible. A typo in check-id means the session can never be created, so the node never becomes a candidate; the startup warning says so instead of leaving only repeated session creation errors.

### Why Exponential Backoff?
Consul may be temporarily unavailable during deployments or network issues. Exponential backoff (max 30s) prevents tight retry loops while allowing reasonable recovery time.

### Why Loop After Lock Loss?
Automatic re-election after lock loss enables nodes to reclaim leadership when they become healthy again, supporting rolling updates and transient failures.

## Files Not to Modify

- `examples/*`: These are reference configurations for users
- `go.mod`/`go.sum`: Only update via `go mod tidy` when dependencies change

## Testing Considerations

- Unit tests: `make test` (`main_test.go`, `consul_client_test.go`, `lock_behavior_test.go`, `lock_value_test.go`, `session_config_test.go`, `session_keepalive_test.go`, `etcd_config_test.go`, `etcd_election_test.go`). Consul calls are mocked through small interfaces (`kvGetter`, `sessionRenewer`); etcd calls through fakes of the etcd client's `KV`, `Lease` and `Watcher` interfaces (`fakeKV`, `fakeLease`, `fakeWatcher`)
- E2E tests: `make test-e2e` (`e2e/`, build tag `e2e`). They need a real Consul agent on `127.0.0.1:8500` and etcd on `127.0.0.1:2379` (`make dcs-up`, or `consul agent -dev` and `etcd`). The etcd image has no shell, so it is configured through `ETCD_*` environment variables (GitHub service containers cannot override the command) and checked with `etcdctl` or `/health`
- E2E scenarios include: standby exiting promptly on SIGTERM, lock value from `trigger-value` / OS hostname / `--hostname`, lock held by the configured session, session renewal past the TTL, health-check failover, SIGKILL failover, graceful shutdown deleting the key, session destroyed after lock loss, standby recovering from a lost session
- etcd E2E scenarios (`e2e/etcd_test.go`): lease TTL, lock value from `trigger-value`, concurrent startup, graceful failover only after lock-delay, SIGKILL failover through the lease TTL, prompt standby shutdown revoking its lease, lease revoked or key deleted by someone else, plain key refused, Consul-only flags and fractional TTL refused