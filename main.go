package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

var (
	// Build-time variables (set via -ldflags)
	version = "0.2.0"
	commit  = "unknown"
	date    = "unknown"
)

const (
	defaultTTL = "10s"
	// Longer than vip-manager's default interval (1000ms), so that the old
	// leader's vip-manager has noticed the deleted key and removed the VIP
	// before another node can acquire the lock.
	defaultLockDelay = "2s"
)

// VipManagerConfig represents the structure of vip-manager.yml
type VipManagerConfig struct {
	DCSType      string   `yaml:"dcs-type"`
	DCSEndpoints []string `yaml:"dcs-endpoints"`
	TriggerKey   string   `yaml:"trigger-key"`
	// TriggerValue is the value vip-manager compares the trigger key with.
	// vip-manager defaults it to os.Hostname() when it is not set.
	TriggerValue yamlString `yaml:"trigger-value"`
}

// yamlString is a YAML scalar that must be written as a string (or left
// empty). vip-manager reads its config through viper, which converts other
// scalars on its own terms: 01 becomes "1", 0x1F "31", true "1". Reading the
// raw text instead would silently disagree with vip-manager, so such values
// are rejected and have to be quoted.
type yamlString string

func (s *yamlString) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: expected a string", node.Line)
	}
	switch node.ShortTag() {
	case "!!str":
		*s = yamlString(node.Value)
	case "!!null":
		*s = ""
	default:
		return fmt.Errorf("line %d: %s is read as %s, not as a string, and vip-manager may convert it (for example 01 to 1); quote it", node.Line, node.Value, node.ShortTag())
	}
	return nil
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

	// The value written to the trigger key. It must equal vip-manager's
	// trigger-value on this node, or vip-manager never assigns the VIP.
	triggerValue := string(vipMgrConfig.TriggerValue)
	hostname, source, err := resolveLockValue(config.Hostname, triggerValue, os.Hostname)
	if err != nil {
		return fmt.Errorf("failed to get hostname: %w", err)
	}

	slog.Info("Using hostname", "hostname", hostname, "source", source)
	if source == lockValueSourceFlag {
		warnIfFlagDiffersFromVipManager(hostname, triggerValue, os.Hostname)
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

	versionFlag := flag.Bool("version", false, "Show version information")
	flag.StringVar(&config.VipManagerConfigPath, "vip-manager-config", "", "Path to vip-manager.yml (required)")
	flag.StringVar(&config.CheckID, "check-id", "", "Consul service check ID to associate with session")
	ttlStr := flag.String("ttl", defaultTTL, "Session TTL (e.g., 10s, 15s)")
	lockDelayStr := flag.String("lock-delay", defaultLockDelay, "Time other nodes must wait after the leader's session ends before acquiring the lock (e.g., 2s)")
	flag.StringVar(&config.ConsulAddr, "addr", "", "Consul address (overrides vip-manager.yml and CONSUL_HTTP_ADDR)")
	flag.StringVar(&config.ConsulToken, "token", "", "Consul ACL token (overrides CONSUL_HTTP_TOKEN)")
	flag.StringVar(&config.Hostname, "hostname", "", "Value to write to the trigger key; must match vip-manager's trigger-value (defaults to trigger-value from vip-manager.yml, then the OS hostname)")

	flag.Parse()

	if *versionFlag {
		fmt.Printf("vip-elector version %s (commit: %s, date: %s, go: %s)\n", version, commit, date, runtime.Version())
		os.Exit(0)
	}

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

	if err := validateConfig(config); err != nil {
		return nil, err
	}

	// Get token from environment if not specified
	if config.ConsulToken == "" {
		config.ConsulToken = os.Getenv("CONSUL_HTTP_TOKEN")
	}

	return config, nil
}

// validateConfig rejects flag values that would silently change how the
// session behaves.
func validateConfig(config *Config) error {
	// Consul treats a zero TTL as "no TTL": the session would never expire,
	// so a killed vip-elector would keep the lock for as long as the node is
	// healthy.
	if config.TTL <= 0 {
		return fmt.Errorf("invalid --ttl: must be greater than 0 (Consul requires at least 10s by default), got %s", config.TTL)
	}
	if config.LockDelay < 0 {
		return fmt.Errorf("invalid --lock-delay: must not be negative, got %s", config.LockDelay)
	}
	return nil
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

const (
	lockValueSourceFlag         = "--hostname"
	lockValueSourceTriggerValue = "trigger-value"
	lockValueSourceOSHostname   = "os-hostname"
)

// resolveLockValue returns the value to write to the trigger key and where
// it came from. vip-manager assigns the VIP when the key equals its
// trigger-value, which defaults to os.Hostname() unmodified, so the same
// precedence applies here with the --hostname flag on top.
func resolveLockValue(flagValue, triggerValue string, osHostname func() (string, error)) (string, string, error) {
	if flagValue != "" {
		return flagValue, lockValueSourceFlag, nil
	}
	if triggerValue != "" {
		return triggerValue, lockValueSourceTriggerValue, nil
	}
	hostname, err := osHostname()
	if err != nil {
		return "", "", err
	}
	if hostname == "" {
		return "", "", errors.New("OS hostname is empty")
	}
	return hostname, lockValueSourceOSHostname, nil
}

// warnIfFlagDiffersFromVipManager warns when the --hostname value differs from
// the trigger-value vip-manager uses by default on this node (trigger-value
// from vip-manager.yml, otherwise the OS hostname). They can only match if
// vip-manager's trigger-value is set by its own flag or environment variable.
func warnIfFlagDiffersFromVipManager(flagValue, triggerValue string, osHostname func() (string, error)) bool {
	expected, source := triggerValue, "trigger-value in vip-manager config"
	if expected == "" {
		hostname, err := osHostname()
		if err != nil || hostname == "" {
			return false
		}
		expected, source = hostname, "OS hostname (vip-manager's default trigger-value)"
	}
	if flagValue == expected {
		return false
	}
	slog.Warn("--hostname differs from what vip-manager compares the trigger key with: "+
		"vip-manager will not assign the VIP to this node unless its trigger-value is set to the same value by a flag or environment variable",
		"hostname", flagValue, "vip_manager_value", expected, "vip_manager_value_source", source)
	return true
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

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
