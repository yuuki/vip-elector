# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

vip-elector is a leader election daemon that works in conjunction with [vip-manager](https://github.com/cybertec-postgresql/vip-manager). It performs distributed leader election using Consul Session and KV Lock, while vip-manager handles the actual VIP (Virtual IP) assignment. **Both tools are required for a complete VIP failover solution.**

### Critical Design Principle

**Configuration as Single Source of Truth**: vip-elector reads `vip-manager.yml` to obtain Consul endpoints and the trigger key. This ensures both vip-elector and vip-manager use consistent configuration without duplication. The three required fields from vip-manager.yml are:
- `dcs-type` (must be "consul")
- `dcs-endpoints` (Consul address list)
- `trigger-key` (KV path for leader hostname)

## Architecture

### Core Flow (main.go)

The program follows a single-file architecture with a clear execution flow:

1. **Configuration Priority Chain** (lines 172-188):
   - CLI flags override vip-manager.yml
   - vip-manager.yml overrides environment variables
   - Environment variables are the fallback

2. **Election Loop** (lines 229-310 in `runElectionLoop`):
   - Creates Consul Session with `Behavior: "delete"` so keys are auto-cleaned on session invalidation
   - Session includes `ServiceChecks` to tie leader election to HTTP health status
   - Acquires KV lock with hostname as the value
   - Blocks on `lockCh` to detect lock loss
   - Implements exponential backoff (1s → 30s) for retry resilience

3. **Session Configuration** (lines 312-334 in `createSession`):
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

### Hostname Handling (lines 191-203)
- Defaults to "short hostname" (strips domain after first dot)
- Override with `--hostname` flag if FQDN is needed
- The hostname becomes the KV lock value that vip-manager matches against

### Health Check Verification (lines 205-216)
- If `--check-id` is provided, the program verifies it exists in Consul at startup
- Missing check ID generates a WARNING but doesn't fail startup
- Without a valid check ID, health-based failover won't work (session won't invalidate on health failures)

### Configuration Precedence (createConsulClient)
Address resolution: `--addr` CLI flag > `dcs-endpoints[0]` from vip-manager.yml > `CONSUL_HTTP_ADDR` env
Token resolution: `--token` CLI flag > `CONSUL_HTTP_TOKEN` env

### Graceful Shutdown (lines 97-104, 298-304)
- SIGINT/SIGTERM trigger context cancellation
- Lock is explicitly released via `lock.Unlock()`
- Session is destroyed to prevent stale sessions
- This ensures clean leadership transfer during planned restarts

## Consul Integration Specifics

### Required ACL Permissions
```hcl
session "write" {}  # Create sessions
key_prefix "your-trigger-prefix/" { policy = "write" }  # KV lock operations
agent_prefix "" { policy = "read" }  # Check verification
```

### Environment Variables Supported
- `CONSUL_HTTP_ADDR`: Consul address
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

This codebase currently has no automated tests. When adding tests, consider:
- Mock Consul API client for unit tests of election logic
- Integration tests require a real Consul agent (use `consul agent -dev`)
- Test scenarios: session creation failure, lock acquisition failure, lock loss detection, graceful shutdown