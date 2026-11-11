package main

import (
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/consul/api"
)

// clearConsulEnv makes the test independent of the CONSUL_* variables that
// api.DefaultConfig reads.
func clearConsulEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		api.HTTPAddrEnvName,
		api.HTTPTokenEnvName,
		api.HTTPTokenFileEnvName,
		api.HTTPSSLEnvName,
		api.HTTPSSLVerifyEnvName,
		api.HTTPCAFile,
		api.HTTPCAPath,
		api.HTTPClientCert,
		api.HTTPClientKey,
		api.HTTPTLSServerName,
	} {
		t.Setenv(name, "")
	}
}

func TestBuildConsulConfig(t *testing.T) {
	tests := []struct {
		name       string
		config     *Config
		endpoints  []string
		env        map[string]string
		wantScheme string
		wantAddr   string
		wantToken  string
	}{
		{
			name:       "http endpoint",
			config:     &Config{},
			endpoints:  []string{"http://consul.example.com:8500"},
			wantScheme: "http",
			wantAddr:   "consul.example.com:8500",
		},
		{
			name:       "https endpoint keeps TLS",
			config:     &Config{},
			endpoints:  []string{"https://consul-tls.example.com:8501"},
			wantScheme: "https",
			wantAddr:   "consul-tls.example.com:8501",
		},
		{
			name:       "endpoint without scheme",
			config:     &Config{},
			endpoints:  []string{"127.0.0.1:8500"},
			wantScheme: "http",
			wantAddr:   "127.0.0.1:8500",
		},
		{
			name:       "endpoint without scheme follows CONSUL_HTTP_SSL",
			config:     &Config{},
			endpoints:  []string{"consul-tls.example.com:8501"},
			env:        map[string]string{api.HTTPSSLEnvName: "true"},
			wantScheme: "https",
			wantAddr:   "consul-tls.example.com:8501",
		},
		{
			// api.NewClient never downgrades to HTTP when TLS is requested
			// through the environment.
			name:       "http endpoint with CONSUL_HTTP_SSL stays https",
			config:     &Config{},
			endpoints:  []string{"http://consul.example.com:8500"},
			env:        map[string]string{api.HTTPSSLEnvName: "true"},
			wantScheme: "https",
			wantAddr:   "consul.example.com:8500",
		},
		{
			name:       "trailing slash is ignored",
			config:     &Config{},
			endpoints:  []string{"https://consul-tls.example.com:8501/"},
			wantScheme: "https",
			wantAddr:   "consul-tls.example.com:8501",
		},
		{
			// vip-manager ignores the path; a path prefix would break every
			// request.
			name:       "path is ignored",
			config:     &Config{},
			endpoints:  []string{"http://consul.example.com:8500/ui"},
			wantScheme: "http",
			wantAddr:   "consul.example.com:8500",
		},
		{
			// vip-manager accepts it (url.Parse lowercases the scheme);
			// api.NewClient rejects an upper-case scheme.
			name:       "upper-case scheme",
			config:     &Config{},
			endpoints:  []string{"HTTPS://consul-tls.example.com:8501"},
			wantScheme: "https",
			wantAddr:   "consul-tls.example.com:8501",
		},
		{
			name:       "IPv6 endpoint",
			config:     &Config{},
			endpoints:  []string{"https://[::1]:8501"},
			wantScheme: "https",
			wantAddr:   "[::1]:8501",
		},
		{
			name:       "unix socket endpoint",
			config:     &Config{},
			endpoints:  []string{"unix:///var/run/consul.sock"},
			wantScheme: "http",
			wantAddr:   "/var/run/consul.sock",
		},
		{
			name:       "only the first endpoint is used",
			config:     &Config{},
			endpoints:  []string{"https://consul1:8501", "http://consul2:8500"},
			wantScheme: "https",
			wantAddr:   "consul1:8501",
		},
		{
			name:       "endpoint overrides CONSUL_HTTP_ADDR",
			config:     &Config{},
			endpoints:  []string{"http://consul.example.com:8500"},
			env:        map[string]string{api.HTTPAddrEnvName: "env.example.com:8500"},
			wantScheme: "http",
			wantAddr:   "consul.example.com:8500",
		},
		{
			name:       "--addr overrides endpoint",
			config:     &Config{ConsulAddr: "192.168.1.100:8500"},
			endpoints:  []string{"https://consul-tls.example.com:8501"},
			wantScheme: "http",
			wantAddr:   "192.168.1.100:8500",
		},
		{
			name:       "--addr with https",
			config:     &Config{ConsulAddr: "https://192.168.1.100:8501"},
			endpoints:  []string{"http://127.0.0.1:8500"},
			wantScheme: "https",
			wantAddr:   "192.168.1.100:8501",
		},
		{
			name:       "token from CONSUL_HTTP_TOKEN",
			config:     &Config{},
			endpoints:  []string{"http://127.0.0.1:8500"},
			env:        map[string]string{api.HTTPTokenEnvName: "env-token"},
			wantScheme: "http",
			wantAddr:   "127.0.0.1:8500",
			wantToken:  "env-token",
		},
		{
			name:       "--token overrides CONSUL_HTTP_TOKEN",
			config:     &Config{ConsulToken: "cli-token"},
			endpoints:  []string{"http://127.0.0.1:8500"},
			env:        map[string]string{api.HTTPTokenEnvName: "env-token"},
			wantScheme: "http",
			wantAddr:   "127.0.0.1:8500",
			wantToken:  "cli-token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearConsulEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			cfg := buildConsulConfig(tt.config, &VipManagerConfig{DCSEndpoints: tt.endpoints})
			// The scheme is resolved by api.NewClient, which updates cfg.
			if _, err := api.NewClient(cfg); err != nil {
				t.Fatalf("api.NewClient: %v", err)
			}

			if cfg.Scheme != tt.wantScheme {
				t.Errorf("Scheme = %q, want %q", cfg.Scheme, tt.wantScheme)
			}
			if cfg.Address != tt.wantAddr {
				t.Errorf("Address = %q, want %q", cfg.Address, tt.wantAddr)
			}
			if cfg.PathPrefix != "" {
				t.Errorf("PathPrefix = %q, want empty", cfg.PathPrefix)
			}
			if cfg.Token != tt.wantToken {
				t.Errorf("Token = %q, want %q", cfg.Token, tt.wantToken)
			}
		})
	}
}

// TestCreateConsulClient_HTTPSEndpoint talks to a TLS server through an
// https:// dcs-endpoint. A client that dropped the scheme would send plain
// HTTP and get "400 Client sent an HTTP request to an HTTPS server".
func TestCreateConsulClient_HTTPSEndpoint(t *testing.T) {
	clearConsulEnv(t)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/status/leader" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `"127.0.0.1:8300"`)
	}))
	defer srv.Close()

	// Trust the test server's certificate the way a user would configure it.
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatalf("write CA: %v", err)
	}
	t.Setenv(api.HTTPCAFile, caFile)

	client, err := createConsulClient(&Config{}, &VipManagerConfig{
		DCSEndpoints: []string{srv.URL}, // https://127.0.0.1:<port>
	})
	if err != nil {
		t.Fatalf("createConsulClient: %v", err)
	}

	leader, err := client.Status().Leader()
	if err != nil {
		t.Fatalf("request over the https:// endpoint failed: %v", err)
	}
	if leader != "127.0.0.1:8300" {
		t.Errorf("leader = %q, want %q", leader, "127.0.0.1:8300")
	}
}
