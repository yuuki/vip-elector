# Consul service definition with HTTP health check
# Place this file in /etc/consul.d/ and run: consul reload

service {
  name = "vip-guard"
  id   = "vip-http"
  port = 9000

  check {
    id       = "vip-http"
    name     = "VIP HTTP Health Check"
    http     = "http://127.0.0.1:9000/healthz"
    interval = "2s"
    timeout  = "1s"
  }
}

# Notes:
# - The check ID "vip-http" must match the --check-id argument to vip-elector
# - The HTTP endpoint should return 2xx when healthy
# - Adjust interval/timeout based on your application's characteristics
# - Shorter intervals = faster failover detection but more load
# - Recommended: interval >= 2s, timeout >= 1s