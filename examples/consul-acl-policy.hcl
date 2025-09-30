# Minimum required ACL policy for vip-elector
# Create token: consul acl token create -policy-name=vip-elector -description="vip-elector token"

# Session management
session "write" {}

# KV access for the trigger key
# Adjust the prefix to match your trigger-key in vip-manager.yml
key_prefix "network/sakura-internal/vip/" {
  policy = "write"
}

# Agent read access (for health check verification)
agent_prefix "" {
  policy = "read"
}

# Node read access (for hostname resolution)
node_prefix "" {
  policy = "read"
}