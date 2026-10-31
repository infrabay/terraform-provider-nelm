package nelmclient

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/werf/nelm/pkg/action"
	"github.com/werf/nelm/pkg/common"
	helmchart "github.com/werf/nelm/pkg/helm/pkg/chart"
	helmloader "github.com/werf/nelm/pkg/helm/pkg/chart/loader"
	"github.com/werf/nelm/pkg/helm/pkg/werf/helmopts"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"
)

// TemplateOptions are the nelm_template data source's render settings beyond
// the chart and its values (ReleaseSpec).
type TemplateOptions struct {
	// Remote renders against the configured cluster (the data source's
	// validate = true), for its real capabilities and the lookup function
	// only. The release storage stays nelm's empty in-memory driver, as in
	// an offline render: the templates always see a first install at
	// revision 1 (helm_template's semantics), whatever the cluster stores
	// for the release, so the output does not change after every apply and
	// needs no read access to the release Secrets. It does not validate the
	// manifests against the cluster's OpenAPI schemas. Otherwise the render
	// is offline and never contacts a cluster.
	Remote bool

	// KubeVersion is .Capabilities.KubeVersion of an offline render; empty
	// keeps nelm's default. Ignored by a remote render.
	KubeVersion string

	// APIVersions are added to .Capabilities.APIVersions.
	APIVersions []string
}

// TemplateObject is one object of a Template render.
type TemplateObject struct {
	// Path is the template file the object was rendered from, as nelm reports
	// it: prefixed with the chart name ("app/templates/deployment.yaml",
	// "app/charts/sub/templates/cm.yaml", "app/crds/crd.yaml").
	Path string

	// Hook marks a Helm hook (helm.sh/hook annotation).
	Hook bool

	// StandaloneCRD marks an object from a crds/ directory (nelm stores it
	// as neither a hook nor a regular resource), whatever its kind: like
	// helm, which treats every manifest under crds/ as a CRD object. nelm's
	// own printout hides only the CustomResourceDefinitions among them.
	StandaloneCRD bool

	// Object is the rendered object. Like Render's output it carries
	// cleartext Secret data.
	Object *unstructured.Unstructured

	// YAML is ManifestYAML of Object: Object as `nelm chart render` prints
	// it, without the "---" and "# Source:" lines nelm puts before it.
	YAML string
}

// Template renders the chart like `nelm chart render` (action.ChartRender)
// and returns every object of the render: hooks and the standalone CRDs of
// crds/ directories included, in nelm's order (standalone CRDs, hooks, then
// the rest, each group in nelm's install order). The objects are as nelm
// leaves them: it removes metadata.namespace wherever it equals the release
// namespace (spec.NewResourceSpec), so an object written with
// `namespace: {{ .Release.Namespace }}` comes back like one written with no
// namespace at all.
//
// An offline render (opts.Remote false) never contacts a cluster, so it also
// works on a NewNoConnectionClient or a NewUnknownConfigClient; the latter
// refuses it with ErrConfigUnknown only for an oci:// chart when the
// provider's registries were not known either. Nelm loads a local chart
// directory without building its dependencies (noDependencyBuild): Template
// never downloads a chart dependency, and fails with
// ErrChartDependenciesMissing for one its charts/ directory lacks. A remote render needs the
// cluster like Render does, but reads no release history: like an offline
// one it always renders a first install.
func (c *Client) Template(ctx context.Context, spec ReleaseSpec, opts TemplateOptions, timeout time.Duration) ([]TemplateObject, error) {
	if opts.Remote && c.clusterErr != nil {
		return nil, c.clusterErr
	}

	if c.registriesUnknown {
		// Only an OCI pull uses the registries: a local chart is not
		// fetched, and a classic repository authenticates with its own
		// basic auth. An oci:// chart would instead be pulled with nelm's
		// default ~/.docker/config.json rather than the configured
		// credentials. Normalized first, so an oci:// repository with a
		// bare chart name counts as the oci:// chart it is folded into.
		chartRef, _, err := NormalizeChartRef(spec.Chart, spec.Repository)
		if err != nil {
			return nil, fmt.Errorf("normalize chart reference: %w", err)
		}

		if isOCI(chartRef) {
			return nil, ErrConfigUnknown
		}
	}

	result, err := c.chartRender(ctx, "nelm-template-", spec, timeout, noDependencyBuild, func(o *action.ChartRenderOptions) {
		o.ExtraAPIVersions = opts.APIVersions

		if !opts.Remote {
			// No KubeConnectionOptions: an offline render builds no kube
			// client, and nelm uses its empty in-memory release storage.
			o.LocalKubeVersion = opts.KubeVersion
			return
		}

		// The in-memory storage is always empty, so the templates see
		// deploy type "Initial" and revision 1 like an offline render,
		// never spec.StorageDriver's history.
		o.KubeConnectionOptions = c.toKubeConnectionOptions()
		o.ReleaseStorageDriver = common.ReleaseStorageDriverMemory
		o.Remote = true
	})
	if err != nil {
		if isMissingDependencies(err) {
			return nil, fmt.Errorf("%w: %w", ErrChartDependenciesMissing, err)
		}

		return nil, err
	}

	objs := make([]TemplateObject, 0, len(result.Resources))

	for _, res := range result.Resources {
		if res == nil || res.Unstruct == nil {
			continue
		}

		y, err := ManifestYAML(res.Unstruct)
		if err != nil {
			return nil, fmt.Errorf("encode %s from %s: %w", res.IDHuman(), res.FilePath, err)
		}

		objs = append(objs, TemplateObject{
			Path:          res.FilePath,
			Hook:          res.StoreAs == common.StoreAsHook,
			StandaloneCRD: res.StoreAs == common.StoreAsNone,
			Object:        res.Unstruct,
			YAML:          y,
		})
	}

	return objs, nil
}

// ManifestYAML encodes obj exactly as nelm's chart render printout does
// (pkg/action/chart_render.go renderResource): JSON through the unstructured
// scheme, then JSON to YAML. TemplateObject.YAML is ManifestYAML of the
// object as rendered; the data source calls it again for an object it
// changes (one whose release namespace it restores).
func ManifestYAML(obj *unstructured.Unstructured) (string, error) {
	j, err := runtime.Encode(unstructured.UnstructuredJSONScheme, obj)
	if err != nil {
		return "", err
	}

	y, err := yaml.JSONToYAML(j)
	if err != nil {
		return "", err
	}

	return string(y), nil
}

// ErrChartDependenciesMissing is returned by Template for a local chart
// directory whose Chart.yaml (or requirements.yaml) declares a dependency
// that is not in its charts/ directory, as an archive or an unpacked chart.
// It wraps nelm's own error, which names the dependencies.
var ErrChartDependenciesMissing = errors.New("chart dependencies are missing from the chart's charts/ directory")

// missingDependenciesPrefix starts helm's action.CheckDependencies error,
// which nelm's render returns (wrapped with %w) for a declared dependency it
// did not load: "found in Chart.yaml, but missing in charts/ directory:
// <names>".
const missingDependenciesPrefix = "found in Chart.yaml, but missing in charts/ directory: "

// isMissingDependencies reports whether err's tree holds helm's
// CheckDependencies error itself, not merely its text (a template's
// fail message, or the nelm log tail, may quote it).
func isMissingDependencies(err error) bool {
	if err == nil {
		return false
	}

	if strings.HasPrefix(err.Error(), missingDependenciesPrefix) {
		return true
	}

	switch u := err.(type) {
	case interface{ Unwrap() error }:
		return isMissingDependencies(u.Unwrap())
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			if isMissingDependencies(e) {
				return true
			}
		}
	}

	return false
}

// noDependencyBuild makes nelm load opts.Chart, when it is a local chart
// directory, without building its dependencies, so that the render uses
// only the dependencies vendored in its charts/ directory, as helm_template
// does without dependency_update. With a Chart.lock (or a v1 chart's
// requirements.lock) nelm's chart loader (LoadChartDependencies) builds
// every locked dependency missing from charts/ before the render: it
// refreshes the user's Helm repositories, downloads with nelm's registry
// client (the Docker config is always among its credentials, whatever the
// provider's registries) and caches the result under ~/.werf. Nelm's chart
// stub type (helmopts.ChartTypeChartStub, the type nelm's own dependency
// build loads its stub chart with) reads the directory's files
// (loader.GetFilesFromLocalFilesystem) and nothing else, and nelm's render
// then rejects a declared dependency missing from charts/ with helm's
// CheckDependencies error (ErrChartDependenciesMissing). The lock files
// stay in the chart, as for helm: a v1 chart's requirements.lock is in
// .Files. A chart archive, local or fetched, keeps the default type:
// nelm's archive loader never builds dependencies.
//
// The stub type differs from the default in one way that matters here: it
// fills in a missing apiVersion, name or version (as v2,
// "stubchartname", 1.0.0) where the default leaves them for the chart
// validation to reject. requireChartMetadata rejects such a chart first,
// with the error nelm's default type gives (a Chart.yaml changed between
// its read and nelm's can still slip through: a chart edited while
// Terraform reads it is not supported). The stub type also ignores
// ChartAppVersion, DefaultValuesDisable, LegacyExtraValues and the
// DefaultChartAPIVersion, DefaultChartName and DefaultChartVersion
// options, which Template never sets: one it starts setting must be
// handled here, or a local chart directory would silently render without
// it (TestTemplate_SetsNoOptionTheStubIgnores). TestTemplate_StubRendersAsDefault
// pins that the two types render the same chart alike.
func noDependencyBuild(opts *action.ChartRenderOptions) error {
	if info, err := os.Stat(opts.Chart); err != nil || !info.IsDir() {
		return nil
	}

	files, err := helmloader.GetFilesFromLocalFilesystem(opts.Chart)
	if err != nil {
		return fmt.Errorf("load chart directory %s: %w", opts.Chart, err)
	}

	if err := requireChartMetadata(files); err != nil {
		return fmt.Errorf("load chart directory %s: %w", opts.Chart, err)
	}

	opts.LegacyChartType = helmopts.ChartTypeChartStub

	return nil
}

// requireChartMetadata returns the chart validation error for chart files
// whose metadata lacks an apiVersion, a name or a version. The metadata is
// read as helm's loader (loader.LoadFiles) reads it: Chart.yaml, apiVersion
// v1 when it names none, then requirements.yaml over it. A file that does
// not parse is left to the loader, which rejects it with its own error.
func requireChartMetadata(files []*helmloader.BufferedFile) error {
	var md *helmchart.Metadata

	for _, f := range files {
		if f.Name == "Chart.yaml" {
			md = new(helmchart.Metadata)
			if err := yaml.Unmarshal(f.Data, md); err != nil {
				return nil
			}

			if md.APIVersion == "" {
				md.APIVersion = helmchart.APIVersionV1
			}
		}
	}

	for _, f := range files {
		if f.Name == "requirements.yaml" {
			if md == nil {
				md = new(helmchart.Metadata)
			}

			if err := yaml.Unmarshal(f.Data, md); err != nil {
				return nil
			}
		}
	}

	if md == nil {
		md = new(helmchart.Metadata)
	}

	if md.APIVersion != "" && md.Name != "" && md.Version != "" {
		return nil
	}

	return md.Validate()
}
