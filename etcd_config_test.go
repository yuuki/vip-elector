package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestResolveEtcdSettings(t *testing.T) {
	tests := []struct {
		name    string
		config  Config
		vm      VipManagerConfig
		env     map[string]string
		want    etcdSettings
		wantErr string
	}{
		{
			name: "nothing set",
			want: etcdSettings{},
		},
		{
			name:   "CA from the flag wins over vip-manager.yml and the environment",
			config: Config{EtcdCAFile: "/flag/ca.pem"},
			vm:     VipManagerConfig{EtcdCAFile: "/vm/ca.pem"},
			env:    map[string]string{"ETCDCTL_CACERT": "/env/ca.pem"},
			want:   etcdSettings{CAFile: "/flag/ca.pem"},
		},
		{
			name: "CA from vip-manager.yml wins over the environment",
			vm:   VipManagerConfig{EtcdCAFile: "/vm/ca.pem"},
			env:  map[string]string{"ETCDCTL_CACERT": "/env/ca.pem"},
			want: etcdSettings{CAFile: "/vm/ca.pem"},
		},
		{
			name: "CA from the environment",
			env:  map[string]string{"ETCDCTL_CACERT": "/env/ca.pem"},
			want: etcdSettings{CAFile: "/env/ca.pem"},
		},
		{
			name:   "client certificate from the flags wins over the environment",
			config: Config{EtcdCertFile: "/flag/cert.pem", EtcdKeyFile: "/flag/key.pem"},
			env:    map[string]string{"ETCDCTL_CERT": "/env/cert.pem", "ETCDCTL_KEY": "/env/key.pem"},
			want:   etcdSettings{CertFile: "/flag/cert.pem", KeyFile: "/flag/key.pem"},
		},
		{
			name: "client certificate from the environment",
			env:  map[string]string{"ETCDCTL_CERT": "/env/cert.pem", "ETCDCTL_KEY": "/env/key.pem"},
			want: etcdSettings{CertFile: "/env/cert.pem", KeyFile: "/env/key.pem"},
		},
		{
			name:   "user and password from the flags win over the environment",
			config: Config{EtcdUser: "flaguser", EtcdPassword: "flagpw"},
			env:    map[string]string{"ETCDCTL_USER": "envuser", "ETCDCTL_PASSWORD": "envpw"},
			want:   etcdSettings{User: "flaguser", Password: "flagpw"},
		},
		{
			name: "user and password from the environment",
			env:  map[string]string{"ETCDCTL_USER": "envuser", "ETCDCTL_PASSWORD": "envpw"},
			want: etcdSettings{User: "envuser", Password: "envpw"},
		},
		{
			name: "user:password is split like etcdctl does",
			env:  map[string]string{"ETCDCTL_USER": "envuser:pw:with:colons"},
			want: etcdSettings{User: "envuser", Password: "pw:with:colons"},
		},
		{
			name:   "user:password in the flag is split too",
			config: Config{EtcdUser: "flaguser:flagpw"},
			want:   etcdSettings{User: "flaguser", Password: "flagpw"},
		},
		{
			name:   "a separate password keeps the user as is",
			config: Config{EtcdUser: "a:b", EtcdPassword: "pw"},
			want:   etcdSettings{User: "a:b", Password: "pw"},
		},
		{
			// vip-manager's credentials are for reading only.
			name: "credentials in vip-manager.yml are not used",
			vm:   VipManagerConfig{EtcdUser: "vipmanager", EtcdCertFile: "/vm/cert.pem"},
			want: etcdSettings{},
		},
		{
			name:    "user without password",
			config:  Config{EtcdUser: "alice"},
			wantErr: "has no password",
		},
		{
			name:    "password without user",
			env:     map[string]string{"ETCDCTL_PASSWORD": "pw"},
			wantErr: "no user",
		},
		{
			name:    "certificate without key",
			config:  Config{EtcdCertFile: "/cert.pem"},
			wantErr: "no key",
		},
		{
			name:    "key without certificate",
			env:     map[string]string{"ETCDCTL_KEY": "/key.pem"},
			wantErr: "no certificate",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.vm.DCSEndpoints = []string{"http://a:2379", "http://b:2379"}
			getenv := func(k string) string { return tt.env[k] }

			got, err := resolveEtcdSettings(&tt.config, &tt.vm, getenv)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// All endpoints are used, as in vip-manager.
			if len(got.Endpoints) != 2 {
				t.Errorf("expected both endpoints, got %v", got.Endpoints)
			}
			got.Endpoints = nil
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

// writeTestCert writes a self-signed certificate and its key as PEM files
// and returns their paths.
func writeTestCert(t *testing.T) (certFile, keyFile string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "vip-elector-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func TestBuildEtcdTLS(t *testing.T) {
	certFile, keyFile := writeTestCert(t)
	notPEM := filepath.Join(t.TempDir(), "not.pem")
	if err := os.WriteFile(notPEM, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("no files still gives a TLS configuration, as in vip-manager", func(t *testing.T) {
		// A nil one would make endpoints without a scheme plaintext, where
		// vip-manager uses TLS.
		cfg, err := buildEtcdTLS(etcdSettings{})
		if err != nil || cfg == nil {
			t.Fatalf("expected a TLS configuration, got %v, %v", cfg, err)
		}
		if cfg.RootCAs != nil || len(cfg.Certificates) != 0 {
			t.Error("expected the system CAs and no client certificate")
		}
	})

	t.Run("CA file", func(t *testing.T) {
		cfg, err := buildEtcdTLS(etcdSettings{CAFile: certFile})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.RootCAs == nil {
			t.Error("expected RootCAs from the CA file")
		}
		if len(cfg.Certificates) != 0 {
			t.Error("expected no client certificate")
		}
	})

	t.Run("client certificate without CA uses the system CAs", func(t *testing.T) {
		cfg, err := buildEtcdTLS(etcdSettings{CertFile: certFile, KeyFile: keyFile})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.RootCAs != nil {
			t.Error("expected RootCAs to be nil (system CAs)")
		}
		if len(cfg.Certificates) != 1 {
			t.Errorf("expected one client certificate, got %d", len(cfg.Certificates))
		}
	})

	t.Run("missing CA file", func(t *testing.T) {
		if _, err := buildEtcdTLS(etcdSettings{CAFile: "/does/not/exist.pem"}); err == nil {
			t.Error("expected an error")
		}
	})

	t.Run("CA file without a certificate", func(t *testing.T) {
		// vip-manager ignores this and then fails every TLS handshake.
		_, err := buildEtcdTLS(etcdSettings{CAFile: notPEM})
		if err == nil || !strings.Contains(err.Error(), "no PEM certificate") {
			t.Errorf("expected a 'no PEM certificate' error, got %v", err)
		}
	})

	t.Run("key that does not match the certificate", func(t *testing.T) {
		if _, err := buildEtcdTLS(etcdSettings{CertFile: certFile, KeyFile: notPEM}); err == nil {
			t.Error("expected an error")
		}
	})
}

func TestEtcdSettingsWarnings(t *testing.T) {
	tests := []struct {
		name string
		s    etcdSettings
		vm   VipManagerConfig
		want []string // substrings, one per expected warning
	}{
		{
			name: "plain http without TLS files",
			s:    etcdSettings{Endpoints: []string{"http://a:2379", "http://b:2379"}},
		},
		{
			name: "https and no scheme with a CA file all use TLS",
			s:    etcdSettings{Endpoints: []string{"https://a:2379", "b:2379"}, CAFile: "/ca.pem"},
		},
		{
			// The first endpoint decides for all of them.
			name: "http after https is connected to with TLS",
			s:    etcdSettings{Endpoints: []string{"https://a:2379", "HTTP://b:2379"}},
			want: []string{"so HTTP://b:2379 is connected to with TLS"},
		},
		{
			name: "https after http is connected to without TLS",
			s:    etcdSettings{Endpoints: []string{"http://a:2379", "https://b:2379"}, CAFile: "/ca.pem"},
			want: []string{"so https://b:2379 is connected to without TLS", "TLS files are set"},
		},
		{
			name: "http with a client certificate is not encrypted",
			s:    etcdSettings{Endpoints: []string{"http://a:2379"}, CertFile: "/c.pem", KeyFile: "/k.pem"},
			want: []string{"TLS files are set"},
		},
		{
			name: "vip-manager credentials without our own",
			s:    etcdSettings{Endpoints: []string{"http://a:2379"}},
			vm:   VipManagerConfig{EtcdUser: "vipmanager"},
			want: []string{"does not use vip-manager's credentials"},
		},
		{
			name: "vip-manager credentials with our own",
			s:    etcdSettings{Endpoints: []string{"http://a:2379"}, User: "elector", Password: "pw"},
			vm:   VipManagerConfig{EtcdCertFile: "/vm/cert.pem"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := etcdSettingsWarnings(tt.s, &tt.vm)
			if len(got) != len(tt.want) {
				t.Fatalf("expected %d warnings, got %q", len(tt.want), got)
			}
			for i, sub := range tt.want {
				if !strings.Contains(got[i], sub) {
					t.Errorf("warning %d: expected it to contain %q, got %q", i, sub, got[i])
				}
			}
		})
	}
}

func TestEtcdUsesTLS(t *testing.T) {
	tests := []struct {
		endpoints []string
		want      bool
	}{
		{[]string{"http://a:2379"}, false},
		{[]string{"HTTP://a:2379"}, false},
		{[]string{"https://a:2379"}, true},
		// No scheme: TLS, because a TLS configuration is always set.
		{[]string{"a:2379"}, true},
		{[]string{"unix:///run/etcd.sock"}, true},
		{[]string{"unixs:///run/etcd.sock"}, true},
		{[]string{"http://a:2379", "https://b:2379"}, false},
		{[]string{"https://a:2379", "http://b:2379"}, true},
		{nil, false},
	}
	for _, tt := range tests {
		if got := etcdUsesTLS(tt.endpoints); got != tt.want {
			t.Errorf("etcdUsesTLS(%q) = %v, want %v", tt.endpoints, got, tt.want)
		}
	}
}

func TestValidateDCSConfig(t *testing.T) {
	set := func(names ...string) map[string]bool {
		m := map[string]bool{}
		for _, n := range names {
			m[n] = true
		}
		return m
	}

	tests := []struct {
		name    string
		dcsType string
		config  Config
		wantErr string
	}{
		{name: "consul", dcsType: "consul", config: Config{TTL: 10 * time.Second, setFlags: set("check-id", "addr", "token")}},
		{name: "etcd", dcsType: "etcd", config: Config{TTL: 10 * time.Second, setFlags: set("etcd-user", "etcd-password", "etcd-ca-file", "etcd-cert-file", "etcd-key-file")}},
		{name: "etcd3 is accepted like in vip-manager", dcsType: "etcd3", config: Config{TTL: 10 * time.Second}},
		{name: "unknown dcs-type", dcsType: "zookeeper", config: Config{TTL: 10 * time.Second}, wantErr: "unsupported dcs-type: zookeeper"},
		{name: "dcs-type is case sensitive like in vip-manager", dcsType: "Etcd", config: Config{TTL: 10 * time.Second}, wantErr: "unsupported dcs-type"},
		{name: "etcd flag with consul", dcsType: "consul", config: Config{TTL: 10 * time.Second, setFlags: set("etcd-ca-file")}, wantErr: "--etcd-ca-file is only used with dcs-type etcd"},
		// Ignoring it would leave a node believed to fail over on health.
		{name: "check-id with etcd", dcsType: "etcd", config: Config{TTL: 10 * time.Second, setFlags: set("check-id")}, wantErr: "--check-id is not supported with dcs-type etcd"},
		{name: "addr with etcd", dcsType: "etcd", config: Config{TTL: 10 * time.Second, setFlags: set("addr")}, wantErr: "--addr is only used with dcs-type consul"},
		{name: "token with etcd3", dcsType: "etcd3", config: Config{TTL: 10 * time.Second, setFlags: set("token")}, wantErr: "--token is only used with dcs-type consul"},
		{name: "fractional TTL with etcd", dcsType: "etcd", config: Config{TTL: 1500 * time.Millisecond}, wantErr: "whole seconds"},
		{name: "fractional TTL with consul", dcsType: "consul", config: Config{TTL: 1500 * time.Millisecond}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateDCSConfig(tt.dcsType, &tt.config)
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("expected no error, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("expected error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestLoadVipManagerConfig_EtcdFields(t *testing.T) {
	write := func(t *testing.T, extra string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "vip-manager.yml")
		content := "dcs-type: etcd\ndcs-endpoints: [\"http://127.0.0.1:2379\"]\ntrigger-key: \"/service/vip\"\n" + extra
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("etcd fields are read", func(t *testing.T) {
		cfg, err := loadVipManagerConfig(write(t, "etcd-ca-file: \"/etc/etcd/ca.pem\"\netcd-user: \"patroni\"\netcd-cert-file: \"/etc/etcd/client.pem\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.EtcdCAFile != "/etc/etcd/ca.pem" || cfg.EtcdUser != "patroni" || cfg.EtcdCertFile != "/etc/etcd/client.pem" {
			t.Errorf("unexpected etcd fields: %+v", cfg)
		}
	})

	t.Run("an unquoted etcd-user does not stop vip-elector", func(t *testing.T) {
		// It is only checked for presence, so vip-manager's reading of it
		// does not matter.
		cfg, err := loadVipManagerConfig(write(t, "etcd-user: 1234\n"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.EtcdUser == "" {
			t.Error("expected etcd-user to be set")
		}
	})

	t.Run("etcd-ca-file must be a string", func(t *testing.T) {
		_, err := loadVipManagerConfig(write(t, "etcd-ca-file: true\n"))
		if err == nil || !strings.Contains(err.Error(), "quote it") {
			t.Errorf("expected a 'quote it' error, got %v", err)
		}
	})
}
