package nelmclient

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
)

func TestRegistryHost(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"oci://us-central1-docker.pkg.dev", "us-central1-docker.pkg.dev"},
		{"oci://us-central1-docker.pkg.dev/my-project/charts/app", "us-central1-docker.pkg.dev"},
		{"https://registry.example.com:5000", "registry.example.com:5000"},
		{"registry.example.com", "registry.example.com"},
		{"  oci://host.example  ", "host.example"},
	}

	for _, tt := range tests {
		if got := registryHost(tt.in); got != tt.want {
			t.Errorf("registryHost(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestWriteRegistryConfig checks the generated Docker config.json holds a
// static Basic-auth entry keyed by host with auth = base64("user:password") —
// the exact shape verified to authenticate against Google Artifact Registry
// through nelm's (helm v4) oras client.
func TestWriteRegistryConfig(t *testing.T) {
	dir := t.TempDir()

	regs := []RegistryAuth{
		{URL: "oci://us-central1-docker.pkg.dev/some/path", Username: "oauth2accesstoken", Password: "tok-123"},
	}

	path, err := writeRegistryConfig(dir, regs)
	if err != nil {
		t.Fatalf("writeRegistryConfig: %v", err)
	}
	if path == "" {
		t.Fatal("expected a non-empty path for a non-empty registries list")
	}

	// 0600 perms (holds a bearer token).
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("registry config perms = %o, want 600", perm)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var parsed struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal generated config: %v", err)
	}

	entry, ok := parsed.Auths["us-central1-docker.pkg.dev"]
	if !ok {
		t.Fatalf("no auths entry for the host; got %v", parsed.Auths)
	}

	want := base64.StdEncoding.EncodeToString([]byte("oauth2accesstoken:tok-123"))
	if entry.Auth != want {
		t.Errorf("auth = %q, want base64(user:pass) %q", entry.Auth, want)
	}
}

func TestWriteRegistryConfig_DuplicateHostRejected(t *testing.T) {
	_, err := writeRegistryConfig(t.TempDir(), []RegistryAuth{
		{URL: "oci://registry.example.com", Username: "a", Password: "1"},
		{URL: "https://registry.example.com/other/path", Username: "b", Password: "2"},
	})
	if err == nil {
		t.Fatal("expected an error for two registries entries resolving to the same host")
	}
}

func TestWriteRegistryConfig_EmptyIsNoop(t *testing.T) {
	path, err := writeRegistryConfig(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("writeRegistryConfig(nil): %v", err)
	}
	if path != "" {
		t.Errorf("expected empty path (so nelm falls back to ~/.docker/config.json), got %q", path)
	}
}
