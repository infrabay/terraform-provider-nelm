package nelmclient

import (
	"errors"
	"sync"
	"time"

	"github.com/werf/nelm/pkg/common"
	"github.com/werf/nelm/pkg/kube"
)

// ErrConfigUnknown is returned by every cluster-facing method of a Client
// built by NewUnknownConfigClient.
var ErrConfigUnknown = errors.New("the nelm provider configuration depends on values that are not known until " +
	"apply (e.g. a cluster created or replaced in this run), so there is no cluster to talk to yet. A new " +
	"nelm_release is planned at apply instead, but an existing one cannot be read or planned without its " +
	"cluster: apply the cluster change first (terraform apply -target=...) or keep the cluster and its " +
	"releases in separate root modules")

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

	// configUnknown marks the NewUnknownConfigClient placeholder.
	configUnknown bool
}

// NewClient constructs a Client bound to the given connection Config. It
// performs no I/O; callers must invoke Init once per process before using
// any Client method.
func NewClient(cfg Config) *Client {
	return &Client{cfg: cfg}
}

// NewUnknownConfigClient returns the placeholder Client the provider hands to
// resources when its own configuration is not fully known at plan time (e.g.
// host comes from a GKE cluster created in the same run). It has no
// connection settings at all, so every cluster-facing method (Plan, Install,
// Uninstall, Get, Render, LiveObjects, IsNamespaced) fails with
// ErrConfigUnknown before doing anything: a zero Config must never reach
// nelm, whose defaults would silently load ~/.kube/config's current-context.
func NewUnknownConfigClient() *Client {
	return &Client{configUnknown: true}
}

// ConfigUnknown reports whether c is the NewUnknownConfigClient placeholder.
// A nil Client (resource not yet configured) is not.
func (c *Client) ConfigUnknown() bool {
	return c != nil && c.configUnknown
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
