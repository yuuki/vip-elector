package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/hashicorp/consul/api"
	"gopkg.in/yaml.v3"
)

const (
	defaultTTL       = "10s"
	defaultLockDelay = "1s"
)

// VipManagerConfig represents the structure of vip-manager.yml
type VipManagerConfig struct {
	DCSType      string   `yaml:"dcs-type"`
	DCSEndpoints []string `yaml:"dcs-endpoints"`
	TriggerKey   string   `yaml:"trigger-key"`
}

// Config holds the runtime configuration
type Config struct {
	VipManagerConfigPath string
	CheckID              string
	TTL                  time.Duration
	LockDelay            time.Duration
	ConsulAddr           string
	ConsulToken          string
	Hostname             string
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := run(); err != nil {
		slog.Error("Fatal error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	// Parse command-line arguments
	config, err := parseFlags()
	if err != nil {
		return fmt.Errorf("failed to parse flags: %w", err)
	}

	// Load vip-manager.yml
	vipMgrConfig, err := loadVipManagerConfig(config.VipManagerConfigPath)
	if err != nil {
		return fmt.Errorf("failed to load vip-manager config: %w", err)
	}

	// Validate DCS type
	if vipMgrConfig.DCSType != "consul" {
		return fmt.Errorf("unsupported dcs-type: %s (only 'consul' is supported)", vipMgrConfig.DCSType)
	}

	// Log startup configuration
	logStartupConfig(config, vipMgrConfig)

	// Initialize Consul client
	consulClient, err := createConsulClient(config, vipMgrConfig)
	if err != nil {
		return fmt.Errorf("failed to create Consul client: %w", err)
	}

	// Get hostname
	hostname := config.Hostname
	if hostname == "" {
		hostname, err = getShortHostname()
		if err != nil {
			return fmt.Errorf("failed to get hostname: %w", err)
		}
	}

	slog.Info("Using hostname", "hostname", hostname)

	// Verify check exists (if specified)
	if config.CheckID != "" {
		if err := verifyCheckExists(consulClient, config.CheckID); err != nil {
			slog.Warn("Health check verification failed", "error", err)
			slog.Warn("Session will be created without service check - health-based failover will not work")
		}
	}

	// Setup signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	go func() {
		sig := <-sigCh
		slog.Info("Received signal, initiating shutdown", "signal", sig)
		cancel()
	}()

	// Main election loop
	return runElectionLoop(ctx, consulClient, vipMgrConfig, config, hostname)
}

func parseFlags() (*Config, error) {
	config := &Config{}

	flag.StringVar(&config.VipManagerConfigPath, "vip-manager-config", "", "Path to vip-manager.yml (required)")
	flag.StringVar(&config.CheckID, "check-id", "", "Consul service check ID to associate with session")
	ttlStr := flag.String("ttl", defaultTTL, "Session TTL (e.g., 10s, 15s)")
	lockDelayStr := flag.String("lock-delay", defaultLockDelay, "Lock delay to prevent lock flapping (e.g., 1s)")
	flag.StringVar(&config.ConsulAddr, "addr", "", "Consul address (overrides vip-manager.yml and CONSUL_HTTP_ADDR)")
	flag.StringVar(&config.ConsulToken, "token", "", "Consul ACL token (overrides CONSUL_HTTP_TOKEN)")
	flag.StringVar(&config.Hostname, "hostname", "", "Hostname to use for lock value (defaults to short hostname)")

	flag.Parse()

	if config.VipManagerConfigPath == "" {
		return nil, fmt.Errorf("--vip-manager-config is required")
	}

	var err error
	config.TTL, err = time.ParseDuration(*ttlStr)
	if err != nil {
		return nil, fmt.Errorf("invalid --ttl: %w", err)
	}

	config.LockDelay, err = time.ParseDuration(*lockDelayStr)
	if err != nil {
		return nil, fmt.Errorf("invalid --lock-delay: %w", err)
	}

	// Get token from environment if not specified
	if config.ConsulToken == "" {
		config.ConsulToken = os.Getenv("CONSUL_HTTP_TOKEN")
	}

	return config, nil
}

func loadVipManagerConfig(path string) (*VipManagerConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config VipManagerConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}

	if config.DCSType == "" {
		return nil, fmt.Errorf("dcs-type is required in vip-manager config")
	}

	if len(config.DCSEndpoints) == 0 {
		return nil, fmt.Errorf("dcs-endpoints is required in vip-manager config")
	}

	if config.TriggerKey == "" {
		return nil, fmt.Errorf("trigger-key is required in vip-manager config")
	}

	return &config, nil
}

func createConsulClient(config *Config, vipMgrConfig *VipManagerConfig) (*api.Client, error) {
	consulConfig := api.DefaultConfig()

	// Priority: CLI flag > vip-manager.yml > environment variable
	if config.ConsulAddr != "" {
		consulConfig.Address = config.ConsulAddr
	} else if len(vipMgrConfig.DCSEndpoints) > 0 {
		// Use first endpoint from vip-manager.yml
		consulConfig.Address = strings.TrimPrefix(vipMgrConfig.DCSEndpoints[0], "http://")
		consulConfig.Address = strings.TrimPrefix(consulConfig.Address, "https://")
	}

	if config.ConsulToken != "" {
		consulConfig.Token = config.ConsulToken
	}

	return api.NewClient(consulConfig)
}

func getShortHostname() (string, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return "", err
	}

	// Return short hostname (before first dot)
	if idx := strings.Index(hostname, "."); idx != -1 {
		return hostname[:idx], nil
	}

	return hostname, nil
}

func verifyCheckExists(client *api.Client, checkID string) error {
	checks, err := client.Agent().Checks()
	if err != nil {
		return fmt.Errorf("failed to query checks: %w", err)
	}

	if _, exists := checks[checkID]; !exists {
		return fmt.Errorf("check ID '%s' not found in Consul agent", checkID)
	}

	return nil
}

func logStartupConfig(config *Config, vipMgrConfig *VipManagerConfig) {
	slog.Info("vip-elector starting")
	slog.Info("Configuration",
		"dcs_type", vipMgrConfig.DCSType,
		"dcs_endpoints", vipMgrConfig.DCSEndpoints,
		"trigger_key", vipMgrConfig.TriggerKey,
		"check_id", config.CheckID,
		"session_ttl", config.TTL,
		"lock_delay", config.LockDelay,
	)
}

func runElectionLoop(ctx context.Context, client *api.Client, vipMgrConfig *VipManagerConfig, config *Config, hostname string) error {
	backoff := time.Second
	maxBackoff := 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			slog.Info("Election loop shutting down")
			return nil
		default:
		}

		slog.Info("Attempting to create session and acquire lock")

		// Create session
		sessionID, err := createSession(client, config, hostname)
		if err != nil {
			slog.Error("Failed to create session", "error", err)
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

		// Try to acquire lock
		lock, err := client.LockOpts(&api.LockOptions{
			Key:   vipMgrConfig.TriggerKey,
			Value: []byte(hostname),
			SessionOpts: &api.SessionEntry{
				ID: sessionID,
			},
		})
		if err != nil {
			slog.Error("Failed to create lock", "error", err)
			// Destroy session
			destroySession(client, sessionID)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		// Acquire lock (blocks until acquired or context cancelled)
		lockCh, err := lock.Lock(ctx.Done())
		if err != nil {
			slog.Info("Lock already held by another node, waiting for next election cycle", "error", err)
			destroySession(client, sessionID)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		if lockCh == nil {
			slog.Info("Lock acquisition cancelled")
			destroySession(client, sessionID)
			return nil
		}

		slog.Info("LOCK ACQUIRED - This node is now the leader", "hostname", hostname)

		// Wait for lock to be lost or context cancelled
		select {
		case <-lockCh:
			slog.Warn("LOCK LOST - Re-entering election")
			// Lock was lost, loop will retry
		case <-ctx.Done():
			slog.Info("Shutting down, releasing lock")
			// Use timeout context for unlock operation
			unlockCtx, unlockCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer unlockCancel()

			// Unlock with timeout context
			unlockDone := make(chan error, 1)
			go func() {
				unlockDone <- lock.Unlock()
			}()

			select {
			case err := <-unlockDone:
				if err != nil {
					slog.Error("Failed to unlock", "error", err)
					// Even if unlock fails, still destroy session
				} else {
					slog.Info("Lock released successfully")
				}
			case <-unlockCtx.Done():
				slog.Warn("Unlock operation timed out, proceeding with session destruction")
			}

			destroySession(client, sessionID)
			return nil
		}

		// Small delay before re-election to avoid tight loop
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			return nil
		}
	}
}

func createSession(client *api.Client, config *Config, name string) (string, error) {
	sessionEntry := &api.SessionEntry{
		Name:      fmt.Sprintf("vip-elector-%s", name),
		Behavior:  "delete",
		TTL:       config.TTL.String(),
		LockDelay: config.LockDelay,
		Checks:    []string{"serfHealth"},
	}

	// Add service check if specified
	if config.CheckID != "" {
		sessionEntry.ServiceChecks = []api.ServiceCheck{
			{ID: config.CheckID},
		}
	}

	sessionID, _, err := client.Session().Create(sessionEntry, nil)
	if err != nil {
		return "", err
	}

	return sessionID, nil
}

func destroySession(client *api.Client, sessionID string) {
	// Use independent context with timeout for session destruction
	destroyCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	opts := &api.WriteOptions{}
	opts = opts.WithContext(destroyCtx)

	_, err := client.Session().Destroy(sessionID, opts)
	if err != nil {
		slog.Error("Failed to destroy session", "session_id", sessionID, "error", err)
	} else {
		slog.Info("Session destroyed", "session_id", sessionID)
	}
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}