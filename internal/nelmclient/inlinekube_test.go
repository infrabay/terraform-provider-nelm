package nelmclient

import (
	"encoding/base64"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
)

// TestBuildInlineKubeconfig asserts the synthesized kubeconfig is a complete,
// standalone config with a SINGLE auth method (the bearer token) — the property
// that avoids the "more than one authentication method found" collision when a
// caller has an ambient client-cert current-context. It also round-trips
// through clientcmd to prove validity.
func TestBuildInlineKubeconfig(t *testing.T) {
	const (
		host = "https://10.0.0.1"
		tok  = "bearer-xyz"
		ca   = "-----BEGIN CERTIFICATE-----\nMIIBfake\n-----END CERTIFICATE-----"
	)

	b64, err := BuildInlineKubeconfig(host, tok, ca, false, "api.internal")
	if err != nil {
		t.Fatalf("BuildInlineKubeconfig: %v", err)
	}

	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("result is not valid base64: %v", err)
	}

	cfg, err := clientcmd.Load(raw)
	if err != nil {
		t.Fatalf("synthesized kubeconfig does not parse: %v", err)
	}

	if cfg.CurrentContext != "nelm" {
		t.Errorf("current-context = %q, want nelm", cfg.CurrentContext)
	}

	cluster := cfg.Clusters["nelm"]
	if cluster == nil || cluster.Server != host {
		t.Fatalf("cluster server = %v, want %q", cluster, host)
	}
	if string(cluster.CertificateAuthorityData) != ca {
		t.Errorf("CA data not set as PEM")
	}
	if cluster.TLSServerName != "api.internal" {
		t.Errorf("tls-server-name = %q", cluster.TLSServerName)
	}

	auth := cfg.AuthInfos["nelm"]
	if auth == nil || auth.Token != tok {
		t.Fatalf("auth token = %v, want %q", auth, tok)
	}
	// Exactly one auth method — no client cert/basic auth alongside the token.
	if len(auth.ClientCertificateData) != 0 || auth.Username != "" || auth.AuthProvider != nil || auth.Exec != nil {
		t.Errorf("expected token-only auth, got extra auth material: %+v", auth)
	}

	// It must be usable (this is what nelm ultimately calls).
	if _, err := clientcmd.NewDefaultClientConfig(*cfg, &clientcmd.ConfigOverrides{}).ClientConfig(); err != nil {
		t.Fatalf("synthesized kubeconfig is not usable: %v", err)
	}
}

// TestBuildInlineKubeconfig_InsecureOmitsCA verifies insecure and a CA are not
// emitted together (the API server rejects that combination).
func TestBuildInlineKubeconfig_InsecureOmitsCA(t *testing.T) {
	b64, err := BuildInlineKubeconfig("https://10.0.0.1", "t", "PEM-CA", true, "")
	if err != nil {
		t.Fatalf("BuildInlineKubeconfig: %v", err)
	}
	raw, _ := base64.StdEncoding.DecodeString(b64)
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	cluster := cfg.Clusters["nelm"]
	if !cluster.InsecureSkipTLSVerify {
		t.Error("expected insecure-skip-tls-verify true")
	}
	if len(cluster.CertificateAuthorityData) != 0 {
		t.Error("CA must be omitted when insecure is set (mutually exclusive)")
	}

	if _, err := clientcmd.NewDefaultClientConfig(*cfg, &clientcmd.ConfigOverrides{}).ClientConfig(); err != nil {
		t.Fatalf("insecure kubeconfig is not usable: %v", err)
	}
}
