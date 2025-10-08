# vip-elector

A leader election daemon using Consul Session and KV Lock, designed to work with [vip-manager](https://github.com/cybertec-postgresql/vip-manager) for HTTP health-based VIP failover without Patroni.

> **⚠️ Important**: This tool is designed to work in conjunction with [vip-manager](https://github.com/cybertec-postgresql/vip-manager). vip-elector alone does not manage VIPs - it only performs leader election. You must install and run both vip-elector and vip-manager to achieve automatic VIP failover.

## Overview

**vip-elector** determines "who should hold the VIP" using Consul's Session and KV Lock mechanism, and writes the hostname of the current leader to a Consul key that [vip-manager](https://github.com/cybertec-postgresql/vip-manager) monitors. By separating concerns (vip-elector = leader election, vip-manager = VIP management), this achieves Keepalived-equivalent automatic failover.

### Key Features

- **HTTP Health-Based Failover**: Integrates with Consul service health checks
- **Automatic Leader Election**: Uses Consul's distributed locking
- **Configuration Reuse**: Reads `vip-manager.yml` as the single source of truth
- **Graceful Shutdown**: Properly releases locks on termination
- **ACL/TLS Support**: Works with secured Consul clusters

## How It Works

1. **vip-elector** creates a Consul Session linked to a local HTTP health check
2. **vip-elector** acquires a KV lock on the trigger-key, writing its hostname as the value
3. **[vip-manager](https://github.com/cybertec-postgresql/vip-manager)** monitors the same key and assigns the VIP to the node whose hostname matches
4. If the HTTP check fails → Session invalidates → Lock releases → Another node acquires lock
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

2. **Consul** - Required for distributed coordination

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

### Command-line Options

```
--vip-manager-config  Path to vip-manager.yml (required)
--check-id            Consul service check ID to link to session (e.g., vip-http)
--ttl                 Session TTL (default: 10s)
--lock-delay          Lock delay to prevent flapping (default: 1s)
--addr                Consul address (overrides config and CONSUL_HTTP_ADDR)
--token               Consul ACL token (overrides CONSUL_HTTP_TOKEN)
--hostname            Hostname to write to lock value (defaults to short hostname)
```

### Environment Variables

- `CONSUL_HTTP_ADDR`: Consul address
- `CONSUL_HTTP_TOKEN`: ACL token
- `CONSUL_CACERT`, `CONSUL_CLIENT_CERT`, `CONSUL_CLIENT_KEY`: TLS certificates

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
  --lock-delay=1s
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
```

### TLS Configuration

Set environment variables for TLS:

```bash
export CONSUL_CACERT=/etc/consul.d/ca.pem
export CONSUL_CLIENT_CERT=/etc/consul.d/client.pem
export CONSUL_CLIENT_KEY=/etc/consul.d/client-key.pem
```

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

**Expected time**: 2-10 seconds (depends on check interval, TTL, and vip-manager interval)

### 2. Process Stop

```bash
sudo systemctl stop vip-elector  # on current leader
```

Session immediately invalidates → Lock releases → Failover to standby node

### 3. Node Failure

Network partition or node crash → Consul agent disconnects → Session invalidates → Automatic failover

## Monitoring

### Log Messages

- `INFO: *** LOCK ACQUIRED ***` - This node became leader
- `WARN: *** LOCK LOST ***` - Lost leadership, re-entering election
- `ERROR: Failed to create session` - Consul connectivity issues
- `WARN: Health check verification failed` - Check ID not found

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

## Design Considerations

### Why Behavior: "delete"?

When a session invalidates, the KV entry is deleted. This ensures vip-manager sees an empty value (≠ hostname), preventing the old leader from holding the VIP.

### Short hostname vs FQDN

By default, vip-elector uses the short hostname (before first dot). Override with `--hostname` if needed:

```bash
--hostname=$(hostname -f)  # Use FQDN
```

### Lock Delay

`--lock-delay` prevents rapid lock acquisition after release, reducing flapping. However, it adds delay to failover. Balance based on your requirements:

- Low latency requirement: `0s` to `1s`
- Stability priority: `2s` to `5s`

## Comparison with Alternatives

| Solution | Leader Election | VIP Management | Dependencies |
|----------|----------------|----------------|--------------|
| **vip-elector + vip-manager** | Consul | vip-manager | Consul, Go app |
| Patroni | etcd/Consul/Zookeeper | Built-in | PostgreSQL, Python |
| Keepalived | VRRP | Built-in | None (kernel VRRP) |
| Pacemaker/Corosync | Corosync | Resource agents | Complex cluster stack |

vip-elector is ideal when:
- You want HTTP health-based failover without Patroni
- You already use Consul
- You need simple, single-purpose components

## License

MIT

## Contributing

Issues and PRs welcome at https://github.com/yuuki/vip-elector

## See Also

- [vip-manager](https://github.com/cybertec-postgresql/vip-manager)
- [Consul Sessions](https://developer.hashicorp.com/consul/docs/dynamic-app-config/sessions)
- [Consul KV Lock](https://developer.hashicorp.com/consul/docs/dynamic-app-config/kv)