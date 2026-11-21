# vip-elector

[![AI Generated](https://img.shields.io/badge/AI%20Generated-Claude-orange?logo=anthropic)](https://claude.ai/claude-code)
[![CI](https://github.com/yuuki/vip-elector/actions/workflows/test.yml/badge.svg)](https://github.com/yuuki/vip-elector/actions/workflows/test.yml)
[![Security](https://github.com/yuuki/vip-elector/actions/workflows/security.yml/badge.svg)](https://github.com/yuuki/vip-elector/actions/workflows/security.yml)
[![Release](https://img.shields.io/github/v/release/yuuki/vip-elector)](https://github.com/yuuki/vip-elector/releases)
[![License](https://img.shields.io/github/license/yuuki/vip-elector)](LICENSE)
[![Go version](https://img.shields.io/github/go-mod/go-version/yuuki/vip-elector)](go.mod)

A leader election daemon using Consul Session and KV Lock (or an etcd lease), designed to work with [vip-manager](https://github.com/cybertec-postgresql/vip-manager) for HTTP health-based VIP failover without Patroni.

> **⚠️ Important**: This tool is designed to work in conjunction with [vip-manager](https://github.com/cybertec-postgresql/vip-manager). vip-elector alone does not manage VIPs - it only performs leader election. You must install and run both vip-elector and vip-manager to achieve automatic VIP failover.

## Overview

**vip-elector** determines "who should hold the VIP" using Consul's Session and KV Lock mechanism, and writes the hostname of the current leader to a Consul key that [vip-manager](https://github.com/cybertec-postgresql/vip-manager) monitors. By separating concerns (vip-elector = leader election, vip-manager = VIP management), this achieves Keepalived-equivalent automatic failover.

With `dcs-type: etcd` in vip-manager.yml, vip-elector runs the election on etcd instead (see [Using etcd](#using-etcd)). etcd has no health checks, so there leadership follows the vip-elector process and its connection to etcd only.

### Key Features

- **HTTP Health-Based Failover**: Integrates with Consul service health checks (Consul only)
- **Automatic Leader Election**: Uses Consul's distributed locking, or an etcd lease
- **Configuration Reuse**: Reads `vip-manager.yml` as the single source of truth
- **Graceful Shutdown**: Properly releases locks on termination
- **ACL/TLS Support**: Works with secured Consul and etcd clusters

## How It Works

1. **vip-elector** creates a Consul Session linked to a local HTTP health check
2. **vip-elector** acquires a KV lock on the trigger-key, writing its hostname (vip-manager's `trigger-value`) as the value
3. **[vip-manager](https://github.com/cybertec-postgresql/vip-manager)** monitors the same key and assigns the VIP to the node whose `trigger-value` matches
4. If the HTTP check fails → Session invalidates → trigger-key is deleted → Another node acquires lock after the lock delay
5. VIP automatically moves to the new leader (via vip-manager)

## Installation

### Requirements

1. **[vip-manager](https://github.com/cybertec-postgresql/vip-manager)** - Required for actual VIP management
   ```bash
   # Install vip-manager (example for Linux)
   wget https://github.com/cybertec-postgresql/vip-manager/releases/download/v2.7.0/vip-manager_2.7.0_Linux_x86_64.tar.gz
   tar xzf vip-manager_2.7.0_Linux_x86_64.tar.gz
   sudo install -m 755 vip-manager /usr/local/bin/
   ```

2. **Consul** or **etcd** (v3 API) - Required for distributed coordination

### Build from source

```bash
go build -o vip-elector
sudo install -m 755 vip-elector /usr/local/bin/
```

### Binary release

Download from [releases page](https://github.com/yuuki/vip-elector/releases).

## Configuration

### Prerequisites

1. **Consul service with HTTP health check**

   Create `/etc/consul.d/vip-guard.hcl`:

   ```hcl
   service {
     name = "vip-guard"
     id   = "vip-http"
     port = 9000
     check {
       id       = "vip-http"
       name     = "VIP HTTP"
       http     = "http://127.0.0.1:9000/healthz"
       interval = "2s"
       timeout  = "1s"
     }
   }
   ```

   Reload Consul:
   ```bash
   consul reload
   ```

2. **vip-manager configuration**

   Create `/etc/vip-manager.yml` (this file is shared by both vip-elector and [vip-manager](https://github.com/cybertec-postgresql/vip-manager)):

   ```yaml
   dcs-type: consul
   dcs-endpoints:
     - "http://127.0.0.1:8500"
   trigger-key: "network/sakura-internal/vip/lock"

   ip: 192.168.227.200
   netmask: 24
   interface: bond0
   interval: 1000
   ```

   See the [vip-manager documentation](https://github.com/cybertec-postgresql/vip-manager#configuration) for full configuration options.

   vip-elector reads `dcs-endpoints` the way vip-manager's Consul checker does: only the **first** entry is used, and only its scheme, host and port (any path is ignored). An `https://` endpoint is connected to over TLS (see [TLS Configuration](#tls-configuration)). Note that `CONSUL_HTTP_SSL=true` makes vip-elector use HTTPS even for an `http://` endpoint, while vip-manager does not read that variable.

### Command-line Options

```
--vip-manager-config  Path to vip-manager.yml (required)
--check-id            Consul service check ID to link to session (e.g., vip-http).
                      It must be registered on the local Consul agent and not
                      critical for this node to take part in the election.
                      Consul only: refused with etcd
--ttl                 Session TTL (default: 10s, Consul's minimum; must be > 0).
                      Renewed every TTL/2 while vip-elector runs. If the process dies,
                      Consul expires the session after up to twice the TTL.
                      With etcd: the lease TTL, in whole seconds
--lock-delay          Time other nodes must wait after the leader gives up the lock
                      before they can acquire it (default: 2s, 0s = no delay)
--addr                Consul address, e.g. https://127.0.0.1:8501 (overrides
                      dcs-endpoints; passed to the Consul client as is, like
                      CONSUL_HTTP_ADDR). Consul only
--token               Consul ACL token (overrides CONSUL_HTTP_TOKEN). Consul only
--hostname            Value to write to the trigger-key. Must equal vip-manager's
                      trigger-value on this node (default: trigger-value from
                      vip-manager.yml, otherwise the OS hostname as is)
--etcd-user           etcd user, or user:password (overrides ETCDCTL_USER). etcd only
--etcd-password       etcd password (overrides ETCDCTL_PASSWORD; prefer the
                      environment variable, other local users can see flags). etcd only
--etcd-ca-file        CA certificate to verify etcd (overrides etcd-ca-file in
                      vip-manager.yml and ETCDCTL_CACERT). etcd only
--etcd-cert-file      Client certificate for etcd (overrides ETCDCTL_CERT). etcd only
--etcd-key-file       Private key of --etcd-cert-file (overrides ETCDCTL_KEY). etcd only
```

A flag that does not apply to the `dcs-type` in vip-manager.yml stops vip-elector at startup instead of being ignored.

### Environment Variables

Consul:

- `CONSUL_HTTP_ADDR`: not used: the address comes from `dcs-endpoints` (required) or `--addr`
- `CONSUL_HTTP_SSL`: `true` forces HTTPS
- `CONSUL_HTTP_TOKEN`: ACL token
- `CONSUL_CACERT`, `CONSUL_CLIENT_CERT`, `CONSUL_CLIENT_KEY`: TLS certificates

etcd (the names etcdctl uses):

- `ETCDCTL_USER`, `ETCDCTL_PASSWORD`: user and password; `ETCDCTL_USER` may also be `user:password`
- `ETCDCTL_CACERT`: CA certificate (after `--etcd-ca-file` and `etcd-ca-file` in vip-manager.yml)
- `ETCDCTL_CERT`, `ETCDCTL_KEY`: client certificate and its key
- `ETCD_CLIENT_DEBUG`: log level of the etcd client (e.g. `warn`). By default only its errors are logged; set `warn` to see why a connection fails, such as an untrusted certificate

### Using etcd

vip-manager reads the trigger key from etcd with `dcs-type: etcd` (or `etcd3`). vip-elector then runs the election on etcd, from the same file:

```yaml
dcs-type: etcd
dcs-endpoints:
  - "https://10.0.0.1:2379"
  - "https://10.0.0.2:2379"
  - "https://10.0.0.3:2379"
trigger-key: "/service/vip/leader"
etcd-ca-file: "/etc/etcd/ca.pem"
etcd-user: "vip-manager"        # read by vip-manager only, see below
etcd-password: "..."

ip: 192.168.227.200
netmask: 24
interface: bond0
interval: 1000
```

- All `dcs-endpoints` are used; the etcd client moves to the next one when an endpoint cannot be reached, as in vip-manager. TLS is used for all endpoints unless the **first** one starts with `http://` (the etcd client decides once, from the first endpoint; vip-elector warns when the list mixes `http://` with other schemes). An endpoint without a scheme (`10.0.0.1:2379`) is therefore connected to with TLS, as in vip-manager. The system CA certificates verify the server unless a CA file is set.
- `etcd-ca-file` from vip-manager.yml is used, because both tools verify the same etcd server.
- vip-manager's `etcd-user`, `etcd-password`, `etcd-cert-file` and `etcd-key-file` are **not** used: vip-manager only needs to read the trigger key, while vip-elector writes it. Give vip-elector its own credentials with `--etcd-user`/`--etcd-password` or `--etcd-cert-file`/`--etcd-key-file`, or the `ETCDCTL_*` variables. vip-elector warns at startup when vip-manager.yml has credentials and vip-elector has none.
- `--check-id` is refused: etcd has no health checks a lease can be tied to. Leadership moves when vip-elector stops, dies, or cannot reach etcd for the TTL. To move the VIP when the service itself stops, stop vip-elector with it (for example with a systemd `BindsTo=` dependency on the service).
- Use a trigger-key that only vip-elector writes. etcd keys carry no marker like the flag of a Consul lock, so vip-elector cannot tell a key another program holds with a lease (for example Patroni's leader key) from a vip-elector lock: it would wait for that key and take it over whenever it is absent for `--lock-delay`.

How the election works on etcd:

1. vip-elector grants a lease with the `--ttl` and renews it every TTL/3. Each renewal has its own deadline and may go to another etcd member, so that one hung member does not let the lease expire
2. It writes its hostname to the trigger-key, attached to the lease, in a transaction that succeeds only if the key does not exist
3. A standby watches the key. When the key is deleted, the standby waits `--lock-delay` and then tries to take it
4. When the lease is revoked (graceful shutdown) or expires (the process or the node died, or its renewals did not reach etcd for the TTL), etcd deletes the key, and vip-manager removes the VIP

## Usage

### Basic usage

```bash
vip-elector \
  --vip-manager-config=/etc/vip-manager.yml \
  --check-id=vip-http
```

### With custom settings

```bash
vip-elector \
  --vip-manager-config=/etc/vip-manager.yml \
  --check-id=vip-http \
  --ttl=15s \
  --lock-delay=2s
```

### With etcd

```bash
ETCDCTL_USER=vip-elector ETCDCTL_PASSWORD=secret \
vip-elector \
  --vip-manager-config=/etc/vip-manager.yml
```

### systemd Service

Create `/etc/systemd/system/vip-elector.service`:

```ini
[Unit]
Description=vip-elector (Consul Session + KV Lock)
After=consul.service
Requires=consul.service

[Service]
Environment=CONSUL_HTTP_TOKEN=your-token-here
ExecStart=/usr/local/bin/vip-elector \
  --vip-manager-config=/etc/vip-manager.yml \
  --check-id=vip-http \
  --ttl=10s \
  --lock-delay=2s
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
```

Enable and start:

```bash
sudo systemctl daemon-reload
sudo systemctl enable vip-elector
sudo systemctl start vip-elector
```

## Security

### ACL Permissions

Minimum required Consul ACL permissions:

```hcl
# Session creation
session "write" {}

# KV operations on trigger key
key_prefix "network/sakura-internal/vip/" {
  policy = "write"
}

# Agent read for health checks
agent_prefix "" {
  policy = "read"
}

# Optional: lets the startup check see the --check-id service check.
# Without it the agent hides the check and vip-elector warns that it
# was not found (the session itself still works).
service_prefix "" {
  policy = "read"
}
```

### etcd Permissions

With etcd authentication enabled, vip-elector needs write access to the trigger key; vip-manager only needs read access. Granting a lease needs no permission beyond a valid user; revoking it needs write access to the key attached to it.

```bash
etcdctl role add vip-elector
etcdctl role grant-permission vip-elector readwrite /service/vip/leader
etcdctl user add vip-elector
etcdctl user grant-role vip-elector vip-elector

etcdctl role add vip-manager
etcdctl role grant-permission vip-manager read /service/vip/leader
```

### TLS Configuration

Use an `https://` address in `dcs-endpoints` (or `--addr`), and set environment variables for the certificates:

```bash
export CONSUL_CACERT=/etc/consul.d/ca.pem
export CONSUL_CLIENT_CERT=/etc/consul.d/client.pem
export CONSUL_CLIENT_KEY=/etc/consul.d/client-key.pem
```

With etcd, use `https://` endpoints, and `etcd-ca-file` in vip-manager.yml (or `--etcd-ca-file`, `ETCDCTL_CACERT`) for a private CA. A client certificate goes in `--etcd-cert-file`/`--etcd-key-file` or `ETCDCTL_CERT`/`ETCDCTL_KEY`. If the first endpoint starts with `http://`, all endpoints are connected to without TLS even when certificates are set; vip-elector warns about it.

## Failover Scenarios

### 1. HTTP Endpoint Failure

```
t0: Node A holds lock, trigger-key = "node-a", VIP on A
t1: A's HTTP check fails (5xx/timeout)
t2: Consul marks check CRITICAL → A's session invalidates
t3: Lock released, trigger-key deleted
t4: Node B acquires lock, writes "node-b"
t5: vip-manager (https://github.com/cybertec-postgresql/vip-manager) on B detects match → assigns VIP to B
```

**Expected time**: about 5 seconds after Consul marks the check critical, plus the check interval before that and vip-manager's interval after. The key is deleted immediately, but a standby is refused once during the lock delay and retries after 5 seconds (fixed by the Consul Lock API).

### 2. Process Stop

```bash
sudo systemctl stop vip-elector  # on current leader
```

vip-elector destroys its session → trigger-key deleted → Failover to standby node after the lock delay (about 5 seconds, see above)

If the process is killed without a chance to clean up (`SIGKILL`, crash), the session is no longer renewed and Consul expires it after up to twice the TTL, then failover proceeds as above. With the default `--ttl=10s` expect roughly 25 seconds in total.

Stopping a standby takes about 2 seconds at most: a standby waits for the lock in blocking queries of 2 seconds and notices the shutdown between them. If Consul hangs instead of answering, the stop waits for it.

### 3. Node Failure

Network partition or node crash → Consul agent disconnects → Session invalidates → Automatic failover

### With etcd

- **Process stop**: vip-elector revokes its lease → etcd deletes the trigger-key at once → a waiting standby takes it after `--lock-delay` (2 seconds by default).
- **Process killed, node crash**: nothing renews the lease, and etcd deletes the key when the TTL ends (10 seconds by default, counted from the last renewal). A standby takes it after `--lock-delay` more.
- **Leader cut off from etcd**: vip-manager on that node cannot read the key and removes the VIP within its `interval`. etcd expires the lease after the TTL, and the standby takes over after `--lock-delay` more. vip-elector on the cut-off node keeps retrying its renewals and learns that the lease is gone once it reaches etcd again.
- **etcd down or without quorum**: no node can read the key, so vip-manager removes the VIP everywhere. etcd keeps its leases through the outage, so when it is back the leader renews its lease and keeps the key, and vip-manager assigns the VIP to it again.
- **HTTP health check failure**: not detected; see [Using etcd](#using-etcd).

## Monitoring

### Log Messages

- `INFO: *** LOCK ACQUIRED ***` - This node became leader
- `WARN: *** LOCK LOST ***` - Lost leadership, re-entering election
- `WARN: Lock acquisition failed (error_type=consul_retryable)` - Transient Consul unavailability; vip-elector will retry
- `ERROR: Lock acquisition failed (error_type=lock_conflict)` - Trigger key is incompatible with Lock API; process exits
- `WARN: SESSION LOST` - Consul reports the session no longer exists; re-entering election
- `WARN: Failed to renew session, retrying` - Consul did not answer a TTL renewal; retried every second
- `ERROR: Failed to create session` - Consul connectivity issues, or (with a `hint`) a health check of the session is missing from the Consul catalog for this node (not registered on the local agent, or not synced yet) or is critical. vip-elector retries with backoff; the node does not take part in the election meanwhile
- `ERROR: Failed to destroy session` - Consul did not confirm the session was destroyed; retried on the next election attempt, otherwise the session expires with its TTL
- `WARN: Health check not found on the local Consul agent` - The `--check-id` check does not exist (yet), or the ACL token cannot read it (`service:read`). If it does not exist, Consul refuses to create the session until it does, so vip-elector keeps retrying and the node is not a candidate meanwhile. Check for a typo, or for the service being registered later at boot
- `WARN: Health check is critical` - The `--check-id` check exists but is critical at startup; no session can be created until it is no longer critical
- `WARN: Could not look up the health check on the local Consul agent` - The startup lookup failed (Consul unreachable, or an ACL error); vip-elector continues, and a missing or critical check then shows up as `Failed to create session` with a `hint`

With etcd:

- `INFO: Lock is held by another node, waiting` - This node is a standby (`holder` is the leader's value)
- `INFO: Trigger key is free, waiting lock-delay before taking it` - Also logged at startup: vip-elector cannot tell how recently the key was deleted, so it always waits. Logged again when another node took and released the key during the wait
- `WARN: LOCK LOST` - The key was deleted or written by someone else; re-entering election
- `WARN: LEASE LOST` - etcd reports the lease gone (it expired, or someone else revoked it); re-entering election
- `WARN: Failed to renew lease, retrying` - A renewal got no answer in time (etcd unreachable, or a hung member); retried at once, possibly on another member, for as long as etcd does not answer
- `WARN: Lease lost while waiting for lock` - The same for a standby
- `ERROR: Lock acquisition failed` - An etcd request failed (e.g. `permission denied`, or etcd unreachable); retried with backoff
- `ERROR: Failed to revoke lease` - etcd did not confirm; retried before the next attempt, otherwise the lease expires with its TTL
- `WARN: etcd changed the lease TTL` - etcd raised `--ttl` to its minimum (2 seconds with default etcd settings)

### Verification

Check current leader:

```bash
consul kv get network/sakura-internal/vip/lock
```

View session details:

```bash
consul kv get -detailed network/sakura-internal/vip/lock
```

List active sessions:

```bash
consul operator raft list-peers
```

With etcd, show the leader, then the time left on the leader's lease and the keys attached to it (`lease_id` as vip-elector logs it in `Lease granted`):

```bash
etcdctl get /service/vip/leader
etcdctl lease timetolive --keys <lease_id>
```

## Troubleshooting

### "Existing key does not match lock use" error

If vip-elector fails to start with an error like:

```
trigger-key pre-flight check failed: existing key at 'network/sakura-internal/vip/lock' is a regular KV entry (Flags=0), not a lock key (expected Flags=3304740253564472344)
```

**Cause**: The trigger-key was manually created with `consul kv put` or by another tool, making it a regular KV entry instead of a lock key. Consul's Lock API requires a specific flag value to identify lock keys.

**Resolution**:

1. Delete the existing key:
   ```bash
   consul kv delete network/sakura-internal/vip/lock
   ```

2. Restart vip-elector - it will create the key correctly with lock flags

**Prevention**:
- **Do NOT pre-create the trigger-key** with `consul kv put`
- Let vip-elector create and manage the trigger-key automatically
- The trigger-key is an internal lock mechanism, not a configuration value

### etcd: "has no lease" error

```
trigger-key pre-flight check failed: existing key at '/service/vip/leader' has no lease (a plain key, e.g. written by 'etcdctl put'). ...
```

**Cause**: The trigger-key was written without a lease (for example with `etcdctl put`). Such a key never expires, so no node could ever take it.

**Resolution**: delete it with `etcdctl del /service/vip/leader` and restart vip-elector. Do not pre-create the trigger-key.

### etcd: "context deadline exceeded" at startup

vip-elector could not get an answer from etcd within 5 seconds. The etcd client does not say why, so the error comes with a hint. Check that `dcs-endpoints` are reachable, and for `https://` the CA certificate and the client certificate. Run once with `ETCD_CLIENT_DEBUG=warn` to see the etcd client's connection errors (e.g. `x509: certificate signed by unknown authority`).

### Lock not acquired

1. Check Consul connectivity:
   ```bash
   consul members
   ```

2. Verify health check exists and is passing:
   ```bash
   consul catalog services -tags
   curl http://127.0.0.1:8500/v1/agent/checks
   ```

3. Check ACL permissions:
   ```bash
   consul acl token read -self
   ```

### Consul 500 / i/o deadline reached during lock acquisition

If vip-elector logs include `Lock acquisition failed` with `error_type=consul_retryable`,
the failure is usually a transient Consul-side problem (for example: leader election, RPC timeout, or network jitter),
not a confirmed lock contention with another node.

Example log pattern:

```json
{"level":"WARN","msg":"Lock acquisition failed","error_type":"consul_retryable","error":"failed to read lock: Unexpected response code: 500 (... i/o deadline reached)"}
```

In this case vip-elector will:

1. Destroy the current session
2. Retry lock acquisition with backoff
3. Continue leader election when Consul recovers

What to check:

1. Consul membership and raft leadership stability:
   ```bash
   consul members
   consul operator raft list-peers
   ```
2. Consul agent/server logs around the same timestamp for timeout or election events.
3. All components are watching the same trigger key (`<trigger-key>`).
4. vip-manager status/logs, because L2 neighbor updates (including gratuitous ARP) are vip-manager's responsibility:
   ```bash
   sudo systemctl status vip-manager
   sudo journalctl -u vip-manager -f
   ```

### Frequent failovers (flapping)

- Increase `--lock-delay` (e.g., `2s` or `5s`)
- Increase Consul health check interval
- Tune `--ttl` based on your network latency

### VIP not moving

1. Verify [vip-manager](https://github.com/cybertec-postgresql/vip-manager) is running:
   ```bash
   sudo systemctl status vip-manager
   ```

2. Check vip-manager logs:
   ```bash
   sudo journalctl -u vip-manager -f
   ```

3. Ensure vip-manager is monitoring the same trigger-key as vip-elector.

4. Verify vip-manager configuration matches vip-elector's vip-manager.yml.

5. Confirm vip-manager is configured correctly for post-switch L2 announcements.
   vip-elector handles leader election and trigger-key updates only.

## Design Considerations

### Why Behavior: "delete"?

When a session invalidates, the KV entry is deleted. This ensures vip-manager sees an empty value (≠ hostname), preventing the old leader from holding the VIP.

vip-elector gives up leadership the same way in every case (health check failure, lock loss, shutdown): the session ends and the key is deleted. The key is never handed over with the old hostname still in it, so the old leader's vip-manager stops seeing its own hostname as soon as leadership ends.

This is not a fencing guarantee. The old leader's vip-manager still needs up to its `interval` to notice the deleted key, and longer if it cannot reach Consul. The lock delay (see below) is what keeps other nodes out meanwhile.

### Session TTL and renewal

The lock is held by the session vip-elector creates (`Behavior: delete`, `--ttl`, `--lock-delay`, `serfHealth` and `--check-id`). vip-elector renews it every TTL/2. If Consul reports the session no longer exists, vip-elector treats that as loss of leadership and re-enters the election. Renew errors are retried for as long as Consul does not answer: vip-elector does not give up leadership on its own while Consul is unreachable, and learns whether the session expired once Consul answers again.

### Lock value (trigger-value)

vip-manager assigns the VIP when the value of the trigger-key equals its `trigger-value`, which defaults to the OS hostname. vip-elector resolves the value it writes the same way, so both agree without extra configuration:

1. `--hostname` flag
2. `trigger-value` from vip-manager.yml
3. The OS hostname, unmodified (`os.Hostname()`, the same call vip-manager makes)

If vip-manager gets its `trigger-value` from somewhere else (its `--trigger-value` flag or the `VIP_TRIGGER_VALUE` environment variable), pass the same value to vip-elector with `--hostname`. vip-elector logs the value it uses at startup (`Using hostname`, with its `source`), and warns when `--hostname` differs from what vip-manager uses by default (`trigger-value` from vip-manager.yml, otherwise the OS hostname).

Quote a `trigger-value` that YAML would read as a number, boolean or date (`"01"`, not `01`). vip-manager converts such values (`01` becomes `1`), so vip-elector refuses to start with an unquoted one rather than write a value vip-manager never matches.

> **Upgrade note**: versions up to 0.1.5 wrote the short hostname (up to the first dot) and ignored `trigger-value`. This only changes anything on a host whose hostname is an FQDN and that does not pass `--hostname`:
>
> - There, the old value never matched vip-manager's default, so the VIP was not assigned. It now is.
> - If you worked around that by setting vip-manager's `trigger-value` to the short name in vip-manager.yml, the value stays the same. If you set it with vip-manager's flag or environment variable instead, pass it to vip-elector with `--hostname`.
> - The session name (`vip-elector-<value>`) and the value in the trigger-key now carry the FQDN; adjust any tooling that matches them.

### Lock Delay

`--lock-delay` keeps other nodes from acquiring the lock right after the leader's session ends. This gives vip-manager on the old leader time to notice the deleted key and remove the VIP before a new leader announces it. It also reduces flapping.

- Keep it longer than vip-manager's `interval`. The default `2s` leaves about a second of margin over vip-manager's default interval (1000 ms); if you raise vip-manager's `interval`, raise `--lock-delay` with it.
- A standby that was already waiting is refused once during the lock delay and retries 5 seconds later (fixed by the Consul Lock API), so any value up to `5s` gives the same failover time for it.
- `0s` disables the delay: a standby can then take over before the old leader's vip-manager has removed the VIP.

etcd has no lock delay of its own. With etcd, every vip-elector waits `--lock-delay` after it finds the trigger-key free before it takes it, also at startup, because it cannot tell how recently the key was deleted. It watches the key during the wait, and starts the wait over if another node takes the key (and maybe releases it again) meanwhile, so the delay always counts from the last release. A standby therefore takes over exactly `--lock-delay` after the key is deleted (there is no 5-second retry as with Consul). The delay is kept by vip-elector, not by etcd: another program writing the key is not held back.

### Lease TTL and renewal (etcd)

The lock is held by a lease of `--ttl` (whole seconds; etcd raises values below its minimum). vip-elector renews it every TTL/3 with separate requests, each with a deadline of half that interval, instead of the etcd client's keepalive stream. That stream stays on one etcd member and moves only when gRPC declares the connection dead, which takes at least 15 seconds; if that member hung, the other members would let the lease expire meanwhile. A separate request goes to the next member.

As with a Consul session, renewal errors are retried for as long as etcd does not answer, and leadership is given up only when etcd reports the lease gone (or the key is deleted). etcd keeps leases through a loss of quorum or a restart, so a leader keeps its key across an etcd outage. vip-manager removes the VIP while it cannot read the key, so this does not leave a VIP on a node that etcd no longer sees as leader.

## Comparison with Alternatives

| Solution | Leader Election | VIP Management | Dependencies |
|----------|----------------|----------------|--------------|
| **vip-elector + vip-manager** | Consul or etcd | vip-manager | Consul or etcd, Go app |
| Patroni | etcd/Consul/Zookeeper | Built-in | PostgreSQL, Python |
| Keepalived | VRRP | Built-in | None (kernel VRRP) |
| Pacemaker/Corosync | Corosync | Resource agents | Complex cluster stack |

vip-elector is ideal when:
- You want HTTP health-based failover without Patroni
- You already use Consul (for health-based failover) or etcd
- You need simple, single-purpose components

## License

MIT

## Contributing

Issues and PRs welcome at https://github.com/yuuki/vip-elector

## See Also

- [vip-manager](https://github.com/cybertec-postgresql/vip-manager)
- [Consul Sessions](https://developer.hashicorp.com/consul/docs/dynamic-app-config/sessions)
- [Consul KV Lock](https://developer.hashicorp.com/consul/docs/dynamic-app-config/kv)
- [etcd leases](https://etcd.io/docs/v3.6/learning/api/#lease-api)
