# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

vip-elector is a leader election daemon that works in conjunction with [vip-manager](https://github.com/cybertec-postgresql/vip-manager). It performs distributed leader election using Consul Session and KV Lock, while vip-manager handles the actual VIP (Virtual IP) assignment. **Both tools are required for a complete VIP failover solution.**

### Critical Design Principle

**Configuration as Single Source of Truth**: vip-elector reads `vip-manager.yml` to obtain Consul endpoints and the trigger key. This ensures both vip-elector and vip-manager use consistent configuration without duplication. The three required fields from vip-manager.yml are:
- `dcs-type` (must be "consul")
- `dcs-endpoints` (Consul address list)
- `trigger-key` (KV path for leader hostname)

Optional fields also read: `trigger-value` (the lock value) and `consul-token` (token fallback).

## Architecture

### Core Flow (main.go)

The program follows a single-file architecture with a clear execution flow:

1. **Configuration Priority Chain** (lines 172-188):
   - CLI flags override vip-manager.yml
   - vip-manager.yml overrides environment variables
   - Environment variables are the fallback

2. **Election Loop** (`runElectionLoop`):
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

## Important Implementation Details

### Lock Value (`resolveLockValue`)
- The value written to the trigger key must equal vip-manager's `trigger-value`, otherwise vip-manager never assigns the VIP (no error on either side)
- Precedence: `--hostname` flag > `trigger-value` from vip-manager.yml > `os.Hostname()` unmodified (vip-manager's own default; do not shorten it)

### Health Check Verification (lines 205-216)
- If `--check-id` is provided, the program verifies it exists in Consul at startup
- Missing check ID generates a WARNING but doesn't fail startup
- Without a valid check ID, health-based failover won't work (session won't invalidate on health failures)

### Configuration Precedence (createConsulClient)
Address resolution: `--addr` CLI flag > `dcs-endpoints[0]` from vip-manager.yml (`dcs-endpoints` is required, so `CONSUL_HTTP_ADDR` never applies)
- `normalizeEndpoint` reads the endpoint like vip-manager (`url.Parse`, lowercase scheme, scheme + host only, path dropped) and keeps the scheme: `api.NewClient` switches to HTTPS for `https://`; stripping the scheme would talk plain HTTP to a TLS endpoint
- Only the first `dcs-endpoints` entry is used, as in vip-manager's Consul checker
- `--addr` is passed to the Consul client unmodified
Token resolution: `--token` CLI flag > `CONSUL_HTTP_TOKEN_FILE` env > `CONSUL_HTTP_TOKEN` env (order between the two applied by `api.NewClient`) > `consul-token` from vip-manager.yml. vip-manager's token is last so that a separate vip-elector token (it needs write access) is not overridden by it

### Session Ownership and Renewal
- The lock must be held by the session `createSession` returns. Pass it as `LockOptions.Session`; `LockOptions.SessionOpts` makes the Lock API create a second, default session (no TTL, no service check, `Behavior: release`) and ignores the ID
- The Lock API only renews sessions it creates itself, so vip-elector renews the TTL every TTL/2
- A renew answer of "session not found" means leadership is lost; other renew errors are retried indefinitely (leadership is not given up while Consul is unreachable)
- `--ttl` must be > 0 (Consul treats 0 as "never expires"). `--lock-delay=0s` is sent as 1ms because the Consul client drops a zero value and the server would apply its 15s default

### Graceful Shutdown
- SIGINT/SIGTERM trigger context cancellation
- Renewal stops and the session is destroyed; `Behavior: "delete"` removes the key and lock-delay applies
- The lock is deliberately not released with `lock.Unlock()`: that leaves the old hostname in the key and lets a standby take over before vip-manager on this node has removed the VIP

## Consul Integration Specifics

### Required ACL Permissions
```hcl
session "write" {}  # Create sessions
key_prefix "your-trigger-prefix/" { policy = "write" }  # KV lock operations
agent_prefix "" { policy = "read" }  # Check verification
```

### Environment Variables Supported
- `CONSUL_HTTP_ADDR`: not used for the address (`dcs-endpoints` is required and wins)
- `CONSUL_HTTP_TOKEN`: ACL token
- `CONSUL_CACERT`, `CONSUL_CLIENT_CERT`, `CONSUL_CLIENT_KEY`: TLS configuration

These are handled by the Consul SDK's `api.DefaultConfig()` automatically.

## Common Issues and Design Rationale

### Why Behavior: "delete"?
When a session invalidates, the KV entry is deleted entirely. This is intentional: vip-manager interprets a missing or non-matching value as "not this node's VIP", preventing split-brain scenarios.

### Why Verify Check ID at Startup?
Early validation prevents misconfiguration. A typo in check-id would result in a session that never invalidates on health failures, defeating the purpose of health-based failover.

### Why Exponential Backoff?
Consul may be temporarily unavailable during deployments or network issues. Exponential backoff (max 30s) prevents tight retry loops while allowing reasonable recovery time.

### Why Loop After Lock Loss?
Automatic re-election after lock loss enables nodes to reclaim leadership when they become healthy again, supporting rolling updates and transient failures.

## Files Not to Modify

- `examples/*`: These are reference configurations for users
- `go.mod`/`go.sum`: Only update via `go mod tidy` when dependencies change

## Testing Considerations

- Unit tests: `make test` (`main_test.go`, `consul_client_test.go`, `lock_behavior_test.go`, `session_config_test.go`, `session_keepalive_test.go`). Consul calls are mocked through small interfaces (`kvGetter`, `sessionRenewer`)
- E2E tests: `make test-e2e` (`e2e/`, build tag `e2e`). They need a real Consul agent on `127.0.0.1:8500` (docker-compose, or `consul agent -dev`)
- E2E scenarios include: lock held by the configured session, session renewal past the TTL, health-check failover, SIGKILL failover, graceful shutdown deleting the key, session destroyed after lock loss, standby recovering from a lost session