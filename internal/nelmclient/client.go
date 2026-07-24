package nelmclient

import (
	"sync"
	"time"

	"github.com/werf/nelm/pkg/common"
	"github.com/werf/nelm/pkg/kube"
)

// Config holds the subset of Nelm Kubernetes connection options exposed by
// the provider's own configuration block (internal/provider/provider.go
// Schema). Zero values let Nelm apply its own defaults (see
// common.KubeConnectionOptions.ApplyDefaults).
type Config struct {
	KubeConfigPaths  []string
	KubeConfigBase64 string
	KubeContext      string
	QPS              int64
	Burst            int64
	RequestTimeout   time.Duration

	// Registries holds static OCI registry credentials (mirrors the helm
	// provider's `registries` block). They are materialized into a per-op
	// Docker config.json passed to nelm as RegistryCredentialsPath, because
	// helm v4's oras client does not reliably drive an external credential
	// helper for some registries (e.g. Google Artifact Registry) — a static
	// Basic-auth entry always works. Empty leaves nelm on its default
	// ~/.docker/config.json.
	Registries []RegistryAuth
}

// RegistryAuth is one static OCI registry credential. URL may carry an oci://
// (or other) scheme and/or path; only its host is used as the Docker-config
// auths key (see registryHost).
type RegistryAuth struct {
	URL      string
	Username string
	Password string
}

// Client is the single point of contact with the Nelm Go library for a
// configured provider instance. Every action method MUST pass
// TempDirPath/OutputNoPrint discipline per CONTRACTS.md; Init (bootstrap.go)
// must have been called successfully exactly once before any Client method
// is used.
type Client struct {
	cfg Config

	// kubeMu guards the lazily-built, cached kube.ClientFactory shared by
	// liveread.go's LiveObjects and IsNamespaced (the planconv.KeyScoper
	// implementation). Only a SUCCESSFULLY built factory is cached (built once
	// per Client, not per call): both methods pay the connectivity-check cost
	// of kube.NewClientFactory only on first use, then reuse its
	// RESTMapper/dynamic client. A construction error is deliberately NOT
	// memoized (see ensureKubeFactory) so a transient blip does not poison
	// every later read for the process lifetime.
	kubeMu      sync.Mutex
	kubeFactory *kube.ClientFactory
}

// NewClient constructs a Client bound to the given connection Config. It
// performs no I/O; callers must invoke Init once per process before using
// any Client method.
func NewClient(cfg Config) *Client {
	return &Client{cfg: cfg}
}

// toKubeConnectionOptions maps Config onto Nelm's common.KubeConnectionOptions,
// the struct embedded into every pkg/action *Options type that talks to a
// cluster (ReleasePlanInstallOptions, ReleaseInstallOptions, ...).
func (c *Client) toKubeConnectionOptions() common.KubeConnectionOptions {
	return common.KubeConnectionOptions{
		KubeConfigPaths:    c.cfg.KubeConfigPaths,
		KubeConfigBase64:   c.cfg.KubeConfigBase64,
		KubeContextCurrent: c.cfg.KubeContext,
		KubeQPSLimit:       int(c.cfg.QPS),
		KubeBurstLimit:     int(c.cfg.Burst),
		KubeRequestTimeout: c.cfg.RequestTimeout,
	}
}
