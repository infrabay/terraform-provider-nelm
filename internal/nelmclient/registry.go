package nelmclient

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// registryHost extracts the Docker-config auths key (a bare host) from a
// registry URL that may carry a scheme and/or a path, e.g.
// "oci://us-central1-docker.pkg.dev/some/path" -> "us-central1-docker.pkg.dev".
// It is intentionally scheme-agnostic (oci://, https://, or none).
func registryHost(url string) string {
	h := strings.TrimSpace(url)

	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}

	if i := strings.IndexByte(h, '/'); i >= 0 {
		h = h[:i]
	}

	return h
}

// dockerConfigJSON builds the content of a Docker config.json holding one
// static Basic-auth entry per registry ({"auths":{"<host>":{"auth":"<base64
// user:password>"}}}). This is the exact shape nelm's helm registry client
// reads via RegistryCredentialsPath.
//
// A STATIC entry is used on purpose: helm v4's oras client does not reliably
// drive an external credential helper (e.g. docker-credential-gcloud) for some
// registries such as Google Artifact Registry — it returns a valid token yet
// the pull still 401s — whereas a base64 "user:password" entry always works
// (verified against GAR).
func dockerConfigJSON(registries []RegistryAuth) ([]byte, error) {
	auths := make(map[string]map[string]string, len(registries))

	for _, r := range registries {
		host := registryHost(r.URL)
		if host == "" {
			return nil, fmt.Errorf("registry URL %q has no host", r.URL)
		}

		// Docker config auths are keyed by host, so a second entry for the
		// same host would silently overwrite the first — surface the
		// ambiguity instead of guessing which credential the user meant.
		if _, dup := auths[host]; dup {
			return nil, fmt.Errorf("duplicate registries entry for host %q (only one credential per registry host is allowed)", host)
		}

		token := base64.StdEncoding.EncodeToString([]byte(r.Username + ":" + r.Password))
		auths[host] = map[string]string{"auth": token}
	}

	return json.Marshal(map[string]interface{}{"auths": auths})
}

// writeRegistryConfig materializes registries into a 0600 config.json under dir
// (a per-operation temp dir the caller cleans up, so the short-lived token
// never outlives the action) and returns its path. It returns "" (no error)
// when there are no registries, so callers leave RegistryCredentialsPath unset
// and nelm falls back to its default ~/.docker/config.json.
func writeRegistryConfig(dir string, registries []RegistryAuth) (string, error) {
	if len(registries) == 0 {
		return "", nil
	}

	data, err := dockerConfigJSON(registries)
	if err != nil {
		return "", err
	}

	path := filepath.Join(dir, "registry-config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("write registry config: %w", err)
	}

	return path, nil
}
