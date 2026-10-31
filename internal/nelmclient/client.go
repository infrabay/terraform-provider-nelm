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

	// clusterErr, when non-nil, is returned by every cluster-facing method
	// before it calls nelm: the client has no usable connection settings
	// (NewUnknownConfigClient, NewNoConnectionClient), and a zero Config must
	// never reach nelm, whose defaults load ~/.kube/config's current-context.
	clusterErr error

	// configUnknown marks the NewUnknownConfigClient placeholder.
	configUnknown bool

	// noConnection marks the NewNoConnectionClient client.
	noConnection bool

	// registriesUnknown marks a NewUnknownConfigClient placeholder whose
	// provider registries were not known either: an offline Template of an
	// oci:// chart then refuses too, rather than pulling with nelm's default
	// ~/.docker/config.json instead of the configured credentials.
	registriesUnknown bool
}

// NewClient constructs a Client bound to the given connection Config. It
// performs no I/O; callers must invoke Init once per process before using
// any Client method.
func NewClient(cfg Config) *Client {
	return &Client{cfg: cfg}
}

// NewUnknownConfigClient returns the placeholder Client the provider hands to
// resources and data sources when its own configuration is not fully known at
// plan time (e.g. host comes from a GKE cluster created in the same run). It
// has no Kubernetes connection settings at all, so every cluster-facing
// method (Plan, Install, Uninstall, Get, History, Render, a remote Template,
// HandOverHelmProviderFieldManagers, LiveObjects, IsNamespaced) fails with
// ErrConfigUnknown before doing anything: a zero Config must never reach
// nelm, whose defaults would silently load ~/.kube/config's current-context.
//
// registries are the provider's static OCI credentials when that attribute is
// known (registriesKnown): an offline Template needs no cluster, and only an
// oci:// chart needs them. When they are not known an offline Template of an
// oci:// chart fails with ErrConfigUnknown too; a local chart or one from a
// classic repository still renders.
func NewUnknownConfigClient(registries []RegistryAuth, registriesKnown bool) *Client {
	return &Client{
		cfg:               Config{Registries: registries},
		clusterErr:        ErrConfigUnknown,
		configUnknown:     true,
		registriesUnknown: !registriesKnown,
	}
}

// errNoConnection is the error of a NewNoConnectionClient built with a nil
// one.
var errNoConnection = errors.New("no Kubernetes connection configured")

// NewNoConnectionClient returns the Client the provider hands out when its
// configuration names no cluster at all. Only an offline Template works:
// every cluster-facing method fails with err (the actionable "no Kubernetes
// connection configured" explanation) before calling nelm, and
// nelm_release refuses to run at all (NoConnection). registries are the
// provider's static OCI credentials, used by an offline Template.
//
// A nil err is replaced by errNoConnection: every guard tests clusterErr,
// so a nil one would make this a client with a zero Config that every
// method lets through to nelm, whose defaults load ~/.kube/config's
// current-context.
func NewNoConnectionClient(registries []RegistryAuth, err error) *Client {
	if err == nil {
		err = errNoConnection
	}

	return &Client{
		cfg:          Config{Registries: registries},
		clusterErr:   err,
		noConnection: true,
	}
}

// ConfigUnknown reports whether c is the NewUnknownConfigClient placeholder.
// A nil Client (resource not yet configured) is not.
func (c *Client) ConfigUnknown() bool {
	return c != nil && c.configUnknown
}

// NoConnection returns the error a NewNoConnectionClient client was built
// with, and nil for any other Client (a nil Client included).
func (c *Client) NoConnection() error {
	if c == nil || !c.noConnection {
		return nil
	}

	return c.clusterErr
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
