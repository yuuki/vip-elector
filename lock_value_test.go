package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestLoadVipManagerConfig_TriggerValue(t *testing.T) {
	tests := []struct {
		name             string
		extra            string
		wantTriggerValue string
		wantErr          bool
	}{
		{name: "not set"},
		{name: "empty", extra: "trigger-value:\n"},
		{name: "null", extra: "trigger-value: ~\n"},
		{name: "empty string", extra: "trigger-value: \"\"\n"},
		{name: "plain string", extra: "trigger-value: node-a\n", wantTriggerValue: "node-a"},
		{name: "quoted string", extra: "trigger-value: \"node-a.example.com\"\n", wantTriggerValue: "node-a.example.com"},
		{name: "quoted number", extra: "trigger-value: \"01\"\n", wantTriggerValue: "01"},
		// vip-manager (viper) would read these as 1, 31, 1 and 1000: refuse
		// rather than write a value it never matches.
		{name: "zero-padded number", extra: "trigger-value: 01\n", wantErr: true},
		{name: "hex number", extra: "trigger-value: 0x1F\n", wantErr: true},
		{name: "boolean", extra: "trigger-value: true\n", wantErr: true},
		{name: "float", extra: "trigger-value: 1e3\n", wantErr: true},
		{name: "list", extra: "trigger-value: [a]\n", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "vip-manager.yml")
			content := "dcs-type: consul\ndcs-endpoints: [\"http://127.0.0.1:8500\"]\ntrigger-key: \"test/vip/lock\"\n" + tt.extra
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatalf("write config: %v", err)
			}

			cfg, err := loadVipManagerConfig(path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got trigger-value %q", cfg.TriggerValue)
				}
				if !strings.Contains(err.Error(), "quote it") && !strings.Contains(err.Error(), "expected a string") {
					t.Errorf("error does not explain the fix: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(cfg.TriggerValue) != tt.wantTriggerValue {
				t.Errorf("TriggerValue = %q, want %q", cfg.TriggerValue, tt.wantTriggerValue)
			}
		})
	}
}

func TestWarnIfFlagDiffersFromVipManager(t *testing.T) {
	osHostname := func() (string, error) { return "node-a", nil }

	tests := []struct {
		name         string
		flagValue    string
		triggerValue string
		wantWarn     bool
	}{
		{name: "matches trigger-value", flagValue: "tv-node", triggerValue: "tv-node"},
		{name: "differs from trigger-value", flagValue: "flag-node", triggerValue: "tv-node", wantWarn: true},
		{name: "matches OS hostname", flagValue: "node-a"},
		{
			// e.g. --hostname=$(hostname -f) on a host whose hostname is short:
			// vip-manager compares against "node-a" by default.
			name:      "differs from OS hostname",
			flagValue: "node-a.example.com",
			wantWarn:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := warnIfFlagDiffersFromVipManager(tt.flagValue, tt.triggerValue, osHostname); got != tt.wantWarn {
				t.Errorf("warned = %v, want %v", got, tt.wantWarn)
			}
		})
	}

	t.Run("OS hostname unavailable", func(t *testing.T) {
		failing := func() (string, error) { return "", errors.New("boom") }
		if warnIfFlagDiffersFromVipManager("flag-node", "", failing) {
			t.Error("expected no warning when the OS hostname is unknown")
		}
	})
}
