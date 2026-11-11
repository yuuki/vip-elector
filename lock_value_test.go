package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/consul/api"
)

func TestResolveLockValue(t *testing.T) {
	fqdn := func() (string, error) { return "node-a.example.com", nil }

	tests := []struct {
		name         string
		flagValue    string
		triggerValue string
		osHostname   func() (string, error)
		want         string
		wantSource   string
	}{
		{
			name:         "--hostname wins over trigger-value",
			flagValue:    "flag-node",
			triggerValue: "tv-node",
			osHostname:   fqdn,
			want:         "flag-node",
			wantSource:   lockValueSourceFlag,
		},
		{
			name:         "trigger-value wins over OS hostname",
			triggerValue: "tv-node",
			osHostname:   fqdn,
			want:         "tv-node",
			wantSource:   lockValueSourceTriggerValue,
		},
		{
			// vip-manager compares against os.Hostname() as is, so an FQDN
			// must not be shortened.
			name:       "OS hostname is used unmodified",
			osHostname: fqdn,
			want:       "node-a.example.com",
			wantSource: lockValueSourceOSHostname,
		},
		{
			name:       "short OS hostname",
			osHostname: func() (string, error) { return "node-a", nil },
			want:       "node-a",
			wantSource: lockValueSourceOSHostname,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, source, err := resolveLockValue(tt.flagValue, tt.triggerValue, tt.osHostname)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("value = %q, want %q", got, tt.want)
			}
			if source != tt.wantSource {
				t.Errorf("source = %q, want %q", source, tt.wantSource)
			}
		})
	}
}

func TestResolveLockValue_OSHostnameNotCalledWhenConfigured(t *testing.T) {
	failing := func() (string, error) { return "", errors.New("must not be called") }

	if _, _, err := resolveLockValue("flag-node", "", failing); err != nil {
		t.Errorf("--hostname set: unexpected error %v", err)
	}
	if _, _, err := resolveLockValue("", "tv-node", failing); err != nil {
		t.Errorf("trigger-value set: unexpected error %v", err)
	}
}

func TestResolveLockValue_OSHostnameErrors(t *testing.T) {
	if _, _, err := resolveLockValue("", "", func() (string, error) { return "", errors.New("boom") }); err == nil {
		t.Error("expected error when os.Hostname fails")
	}
	if _, _, err := resolveLockValue("", "", func() (string, error) { return "", nil }); err == nil {
		t.Error("expected error for an empty OS hostname")
	}
}

func TestResolveLockValue_DefaultMatchesOSHostname(t *testing.T) {
	want, err := os.Hostname()
	if err != nil {
		t.Skipf("os.Hostname: %v", err)
	}
	got, _, err := resolveLockValue("", "", os.Hostname)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("value = %q, want os.Hostname() %q", got, want)
	}
}

func TestLoadVipManagerConfig_TriggerValueAndToken(t *testing.T) {
	tests := []struct {
		name             string
		extra            string
		wantTriggerValue string
		wantToken        string
	}{
		{name: "not set"},
		{
			name:             "set",
			extra:            "trigger-value: \"node-a\"\nconsul-token: \"secret\"\n",
			wantTriggerValue: "node-a",
			wantToken:        "secret",
		},
		{
			// vip-manager reads it as a string too.
			name:             "numeric trigger-value",
			extra:            "trigger-value: 200\n",
			wantTriggerValue: "200",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "vip-manager.yml")
			content := "dcs-type: consul\ndcs-endpoints: [\"http://127.0.0.1:8500\"]\ntrigger-key: \"test/vip/lock\"\n" + tt.extra
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatalf("write config: %v", err)
			}

			cfg, err := loadVipManagerConfig(path)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.TriggerValue != tt.wantTriggerValue {
				t.Errorf("TriggerValue = %q, want %q", cfg.TriggerValue, tt.wantTriggerValue)
			}
			if cfg.ConsulToken != tt.wantToken {
				t.Errorf("ConsulToken = %q, want %q", cfg.ConsulToken, tt.wantToken)
			}
		})
	}
}

func TestBuildConsulConfig_TokenFallback(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("file-token\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	tests := []struct {
		name      string
		flagToken string // Config.ConsulToken: --token, or CONSUL_HTTP_TOKEN via parseFlags
		env       map[string]string
		ymlToken  string
		wantToken string
	}{
		{
			name:      "consul-token from vip-manager.yml as last resort",
			ymlToken:  "yml-token",
			wantToken: "yml-token",
		},
		{
			name:      "--token wins over consul-token",
			flagToken: "cli-token",
			ymlToken:  "yml-token",
			wantToken: "cli-token",
		},
		{
			name:      "CONSUL_HTTP_TOKEN wins over consul-token",
			env:       map[string]string{api.HTTPTokenEnvName: "env-token"},
			ymlToken:  "yml-token",
			wantToken: "env-token",
		},
		{
			name:      "CONSUL_HTTP_TOKEN_FILE wins over consul-token",
			env:       map[string]string{api.HTTPTokenFileEnvName: tokenFile},
			ymlToken:  "yml-token",
			wantToken: "file-token",
		},
		{
			// Consul's own precedence, applied by api.NewClient.
			name: "CONSUL_HTTP_TOKEN_FILE wins over CONSUL_HTTP_TOKEN",
			env: map[string]string{
				api.HTTPTokenEnvName:     "env-token",
				api.HTTPTokenFileEnvName: tokenFile,
			},
			flagToken: "env-token", // parseFlags copies CONSUL_HTTP_TOKEN
			ymlToken:  "yml-token",
			wantToken: "file-token",
		},
		{
			name:      "no token anywhere",
			wantToken: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearConsulEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			cfg := buildConsulConfig(&Config{ConsulToken: tt.flagToken}, &VipManagerConfig{
				DCSEndpoints: []string{"http://127.0.0.1:8500"},
				ConsulToken:  tt.ymlToken,
			})
			// api.NewClient applies CONSUL_HTTP_TOKEN_FILE.
			if _, err := api.NewClient(cfg); err != nil {
				t.Fatalf("api.NewClient: %v", err)
			}
			if cfg.Token != tt.wantToken {
				t.Errorf("Token = %q, want %q", cfg.Token, tt.wantToken)
			}
		})
	}
}
