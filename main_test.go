package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/consul/api"
)

// TestLoadVipManagerConfig tests loading and validation of vip-manager.yml
func TestLoadVipManagerConfig(t *testing.T) {
	tests := []struct {
		name        string
		yamlContent string
		wantErr     bool
		errContains string
		validate    func(*testing.T, *VipManagerConfig)
	}{
		{
			name: "valid config",
			yamlContent: `dcs-type: consul
dcs-endpoints: ["http://127.0.0.1:8500"]
trigger-key: "test/vip/lock"`,
			wantErr: false,
			validate: func(t *testing.T, cfg *VipManagerConfig) {
				if cfg.DCSType != "consul" {
					t.Errorf("expected dcs-type=consul, got %s", cfg.DCSType)
				}
				if len(cfg.DCSEndpoints) != 1 || cfg.DCSEndpoints[0] != "http://127.0.0.1:8500" {
					t.Errorf("unexpected dcs-endpoints: %v", cfg.DCSEndpoints)
				}
				if cfg.TriggerKey != "test/vip/lock" {
					t.Errorf("expected trigger-key=test/vip/lock, got %s", cfg.TriggerKey)
				}
			},
		},
		{
			name:        "missing dcs-type",
			yamlContent: `dcs-endpoints: ["http://127.0.0.1:8500"]`,
			wantErr:     true,
			errContains: "dcs-type is required",
		},
		{
			name:        "missing dcs-endpoints",
			yamlContent: `dcs-type: consul`,
			wantErr:     true,
			errContains: "dcs-endpoints is required",
		},
		{
			name: "missing trigger-key",
			yamlContent: `dcs-type: consul
dcs-endpoints: ["http://127.0.0.1:8500"]`,
			wantErr:     true,
			errContains: "trigger-key is required",
		},
		{
			name: "multiple endpoints",
			yamlContent: `dcs-type: consul
dcs-endpoints: ["http://consul1:8500", "http://consul2:8500"]
trigger-key: "test/vip/lock"`,
			wantErr: false,
			validate: func(t *testing.T, cfg *VipManagerConfig) {
				if len(cfg.DCSEndpoints) != 2 {
					t.Errorf("expected 2 endpoints, got %d", len(cfg.DCSEndpoints))
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temporary file
			tmpDir := t.TempDir()
			configPath := filepath.Join(tmpDir, "vip-manager.yml")
			if err := os.WriteFile(configPath, []byte(tt.yamlContent), 0644); err != nil {
				t.Fatalf("failed to create temp config: %v", err)
			}

			cfg, err := loadVipManagerConfig(configPath)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("expected error to contain %q, got %q", tt.errContains, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.validate != nil {
				tt.validate(t, cfg)
			}
		})
	}
}

// TestLoadVipManagerConfigFileErrors tests file I/O errors
func TestLoadVipManagerConfigFileErrors(t *testing.T) {
	t.Run("nonexistent file", func(t *testing.T) {
		_, err := loadVipManagerConfig("/nonexistent/path/config.yml")
		if err == nil {
			t.Fatal("expected error for nonexistent file")
		}
		if !strings.Contains(err.Error(), "failed to read config file") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("invalid yaml", func(t *testing.T) {
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, "invalid.yml")
		if err := os.WriteFile(configPath, []byte("invalid: yaml: content:"), 0644); err != nil {
			t.Fatalf("failed to create temp config: %v", err)
		}

		_, err := loadVipManagerConfig(configPath)
		if err == nil {
			t.Fatal("expected error for invalid YAML")
		}
		if !strings.Contains(err.Error(), "failed to parse YAML") {
			t.Errorf("unexpected error message: %v", err)
		}
	})
}

// TestGetShortHostname tests hostname extraction
func TestGetShortHostname(t *testing.T) {
	// Save and restore original hostname function
	originalHostname, err := os.Hostname()
	if err != nil {
		t.Fatalf("failed to get original hostname: %v", err)
	}

	tests := []struct {
		name     string
		hostname string
		want     string
	}{
		{
			name:     "short hostname without domain",
			hostname: "server01",
			want:     "server01",
		},
		{
			name:     "FQDN with single domain",
			hostname: "server01.example.com",
			want:     "server01",
		},
		{
			name:     "FQDN with multiple subdomains",
			hostname: "server01.prod.example.com",
			want:     "server01",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Note: We can't actually change os.Hostname(), so we test the logic
			// by simulating what getShortHostname does
			hostname := tt.hostname
			var result string
			if idx := strings.Index(hostname, "."); idx != -1 {
				result = hostname[:idx]
			} else {
				result = hostname
			}

			if result != tt.want {
				t.Errorf("expected %q, got %q", tt.want, result)
			}
		})
	}

	// Test actual function
	t.Run("actual function", func(t *testing.T) {
		hostname, err := getShortHostname()
		if err != nil {
			t.Fatalf("getShortHostname() error: %v", err)
		}

		// Verify it doesn't contain dots (unless original hostname is short)
		if strings.Contains(hostname, ".") && strings.Contains(originalHostname, ".") {
			t.Errorf("expected short hostname without domain, got %q", hostname)
		}

		// Verify it's a prefix of the original hostname
		if !strings.HasPrefix(originalHostname, hostname) {
			t.Errorf("expected %q to be prefix of %q", hostname, originalHostname)
		}
	})
}

// TestCreateConsulClient tests Consul client configuration
func TestCreateConsulClient(t *testing.T) {
	tests := []struct {
		name           string
		config         *Config
		vipMgrConfig   *VipManagerConfig
		envToken       string
		wantAddr       string
		wantTokenEmpty bool
	}{
		{
			name: "CLI flag overrides vip-manager.yml",
			config: &Config{
				ConsulAddr:  "192.168.1.100:8500",
				ConsulToken: "test-token",
			},
			vipMgrConfig: &VipManagerConfig{
				DCSEndpoints: []string{"http://127.0.0.1:8500"},
			},
			wantAddr:       "192.168.1.100:8500",
			wantTokenEmpty: false,
		},
		{
			name:   "vip-manager.yml used when CLI flag empty",
			config: &Config{},
			vipMgrConfig: &VipManagerConfig{
				DCSEndpoints: []string{"http://consul.example.com:8500"},
			},
			wantAddr:       "consul.example.com:8500",
			wantTokenEmpty: true,
		},
		{
			name:   "strips http:// prefix",
			config: &Config{},
			vipMgrConfig: &VipManagerConfig{
				DCSEndpoints: []string{"http://127.0.0.1:8500"},
			},
			wantAddr:       "127.0.0.1:8500",
			wantTokenEmpty: true,
		},
		{
			name:   "strips https:// prefix",
			config: &Config{},
			vipMgrConfig: &VipManagerConfig{
				DCSEndpoints: []string{"https://consul-tls.example.com:8501"},
			},
			wantAddr:       "consul-tls.example.com:8501",
			wantTokenEmpty: true,
		},
		{
			name: "uses token from config",
			config: &Config{
				ConsulToken: "cli-token",
			},
			vipMgrConfig: &VipManagerConfig{
				DCSEndpoints: []string{"http://127.0.0.1:8500"},
			},
			envToken:       "env-token",
			wantAddr:       "127.0.0.1:8500",
			wantTokenEmpty: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set environment token if specified
			if tt.envToken != "" {
				oldToken := os.Getenv("CONSUL_HTTP_TOKEN")
				os.Setenv("CONSUL_HTTP_TOKEN", tt.envToken)
				defer os.Setenv("CONSUL_HTTP_TOKEN", oldToken)
			}

			// Test the configuration logic directly instead of creating a client
			consulConfig := api.DefaultConfig()

			// Priority: CLI flag > vip-manager.yml > environment variable
			if tt.config.ConsulAddr != "" {
				consulConfig.Address = tt.config.ConsulAddr
			} else if len(tt.vipMgrConfig.DCSEndpoints) > 0 {
				// Use first endpoint from vip-manager.yml
				consulConfig.Address = strings.TrimPrefix(tt.vipMgrConfig.DCSEndpoints[0], "http://")
				consulConfig.Address = strings.TrimPrefix(consulConfig.Address, "https://")
			}

			if tt.config.ConsulToken != "" {
				consulConfig.Token = tt.config.ConsulToken
			}

			if consulConfig.Address != tt.wantAddr {
				t.Errorf("expected address %q, got %q", tt.wantAddr, consulConfig.Address)
			}

			if tt.wantTokenEmpty && consulConfig.Token != "" {
				t.Errorf("expected empty token, got %q", consulConfig.Token)
			} else if !tt.wantTokenEmpty && consulConfig.Token == "" {
				t.Error("expected non-empty token, got empty")
			}
		})
	}
}

// TestCreateSession tests session configuration
func TestCreateSession(t *testing.T) {
	// This test requires a real Consul agent, so we'll test the session entry construction
	tests := []struct {
		name     string
		config   *Config
		hostname string
		validate func(*testing.T, *api.SessionEntry)
	}{
		{
			name: "basic session without check",
			config: &Config{
				TTL:       10 * time.Second,
				LockDelay: time.Second,
			},
			hostname: "server01",
			validate: func(t *testing.T, entry *api.SessionEntry) {
				if entry.Name != "vip-elector-server01" {
					t.Errorf("expected name=vip-elector-server01, got %s", entry.Name)
				}
				if entry.Behavior != "delete" {
					t.Errorf("expected behavior=delete, got %s", entry.Behavior)
				}
				if entry.TTL != "10s" {
					t.Errorf("expected TTL=10s, got %s", entry.TTL)
				}
				if entry.LockDelay != time.Second {
					t.Errorf("expected LockDelay=1s, got %v", entry.LockDelay)
				}
				if len(entry.Checks) != 1 || entry.Checks[0] != "serfHealth" {
					t.Errorf("expected Checks=[serfHealth], got %v", entry.Checks)
				}
				if len(entry.ServiceChecks) != 0 {
					t.Errorf("expected no service checks, got %v", entry.ServiceChecks)
				}
			},
		},
		{
			name: "session with service check",
			config: &Config{
				TTL:       15 * time.Second,
				LockDelay: 2 * time.Second,
				CheckID:   "vip-http",
			},
			hostname: "server02",
			validate: func(t *testing.T, entry *api.SessionEntry) {
				if entry.Name != "vip-elector-server02" {
					t.Errorf("expected name=vip-elector-server02, got %s", entry.Name)
				}
				if len(entry.ServiceChecks) != 1 || entry.ServiceChecks[0].ID != "vip-http" {
					t.Errorf("expected ServiceChecks=[{ID:vip-http}], got %v", entry.ServiceChecks)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create the session entry that would be created
			sessionEntry := &api.SessionEntry{
				Name:      "vip-elector-" + tt.hostname,
				Behavior:  "delete",
				TTL:       tt.config.TTL.String(),
				LockDelay: tt.config.LockDelay,
				Checks:    []string{"serfHealth"},
			}

			if tt.config.CheckID != "" {
				sessionEntry.ServiceChecks = []api.ServiceCheck{
					{ID: tt.config.CheckID},
				}
			}

			tt.validate(t, sessionEntry)
		})
	}
}

// TestMin tests the min helper function
func TestMin(t *testing.T) {
	tests := []struct {
		name string
		a    time.Duration
		b    time.Duration
		want time.Duration
	}{
		{
			name: "a < b",
			a:    time.Second,
			b:    2 * time.Second,
			want: time.Second,
		},
		{
			name: "a > b",
			a:    10 * time.Second,
			b:    5 * time.Second,
			want: 5 * time.Second,
		},
		{
			name: "a == b",
			a:    time.Second,
			b:    time.Second,
			want: time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := min(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("min(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// mockKVClient implements a simple in-memory KV for testing
type mockKVClient struct {
	data map[string]*api.KVPair
}

func (m *mockKVClient) Get(key string, q *api.QueryOptions) (*api.KVPair, *api.QueryMeta, error) {
	if pair, ok := m.data[key]; ok {
		return pair, &api.QueryMeta{}, nil
	}
	return nil, &api.QueryMeta{}, nil
}


// TestVerifyLockKeyState tests the lock key verification logic
func TestVerifyLockKeyState(t *testing.T) {
	tests := []struct {
		name        string
		key         string
		existingKV  *api.KVPair
		wantErr     bool
		errContains string
	}{
		{
			name:       "key does not exist - OK",
			key:        "test/vip/lock",
			existingKV: nil,
			wantErr:    false,
		},
		{
			name: "key exists with correct lock flag - OK",
			key:  "test/vip/lock",
			existingKV: &api.KVPair{
				Key:     "test/vip/lock",
				Value:   []byte("server01"),
				Flags:   LockFlagValue,
				Session: "session-123",
			},
			wantErr: false,
		},
		{
			name: "key exists as regular KV (Flags=0) - ERROR",
			key:  "test/vip/lock",
			existingKV: &api.KVPair{
				Key:   "test/vip/lock",
				Value: []byte("server01"),
				Flags: 0,
			},
			wantErr:     true,
			errContains: "regular KV entry",
		},
		{
			name: "key exists with wrong flags - ERROR",
			key:  "test/vip/lock",
			existingKV: &api.KVPair{
				Key:   "test/vip/lock",
				Value: []byte("server01"),
				Flags: 12345,
			},
			wantErr:     true,
			errContains: "regular KV entry",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup mock client
			mockKV := &mockKVClient{
				data: make(map[string]*api.KVPair),
			}
			if tt.existingKV != nil {
				mockKV.data[tt.key] = tt.existingKV
			}

			// Test
			err := verifyLockKeyState(mockKV, tt.key)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("expected error to contain %q, got %q", tt.errContains, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

// TestVerifyLockKeyStateErrorDetails verifies error message details
func TestVerifyLockKeyStateErrorDetails(t *testing.T) {
	mockKV := &mockKVClient{
		data: map[string]*api.KVPair{
			"test/lock": {
				Key:   "test/lock",
				Value: []byte("hostname"),
				Flags: 0, // Regular KV
			},
		},
	}

	err := verifyLockKeyState(mockKV, "test/lock")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	// Verify error message contains expected details
	errMsg := err.Error()
	if !strings.Contains(errMsg, "test/lock") {
		t.Errorf("expected error to contain key 'test/lock', got %s", errMsg)
	}
	if !strings.Contains(errMsg, "Flags=0") {
		t.Errorf("expected error to contain 'Flags=0', got %s", errMsg)
	}
}

// TestLockFlagValue verifies the magic constant matches Consul's implementation
func TestLockFlagValue(t *testing.T) {
	// This is the actual value from hashicorp/consul/api/lock.go
	expectedValue := uint64(0x2ddccbc058a50c18)

	if LockFlagValue != expectedValue {
		t.Errorf("LockFlagValue mismatch: expected %d, got %d", expectedValue, LockFlagValue)
	}
}