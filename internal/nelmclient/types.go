// Package nelmclient is the SINGLE point of contact with the Nelm
// (github.com/werf/nelm) Go library. Nothing outside this package may import
// github.com/werf/nelm/pkg/action (CONTRACTS.md seam 1).
package nelmclient

import (
	"github.com/werf/nelm/pkg/plan"
)

// ReleaseSpec is the provider-agnostic description of a desired nelm_release
// resource state, translated from internal/provider's releaseModel and
// consumed by Client.Plan / Client.Install.
type ReleaseSpec struct {
	Name      string
	Namespace string
	Chart     string

	Repository string
	Version    string

	// ValuesYAML holds one raw YAML document per "values" list entry, in
	// config order. Each entry is written to a temp file and passed as a
	// ValuesOptions.ValuesFiles entry (later files override earlier ones).
	ValuesYAML []string

	// Set/SetString/SetLiteral/SetJSON are "name=value" strings destined for
	// ValuesOptions.ValuesSet / ValuesSetString / ValuesSetLiteral /
	// ValuesSetJSON respectively. "set" entries are translated by type and
	// appended before "set_sensitive" entries so the latter win on key
	// conflicts (strvals later-wins semantics).
	Set        []string
	SetString  []string
	SetLiteral []string
	SetJSON    []string

	StorageDriver string
	HistoryLimit  int

	ForceAdoption         bool
	NoRemoveManualChanges bool
	NoInstallCRDs         bool
	AutoRollback          bool

	// RenderAsFirstInstall makes Render ignore the release's stored history
	// and render the chart exactly as a first install would (deploy type
	// "Initial", revision 1), whatever is live. Only Render reads it: Plan
	// and Install always run against the real history.
	RenderAsFirstInstall bool
}

// PlanResult is the result of Client.Plan: the resource changes read back
// from a Nelm plan artifact (action.ReleasePlanInstall + plan.ReadPlanArtifact),
// after the artifact file itself has already been deleted. *plan.ResourceChange
// passes through opaquely from Nelm (CONTRACTS.md seam 1) — internal/planconv
// consumes it directly, never re-deriving its own change-classification logic.
type PlanResult struct {
	Changes    []*plan.ResourceChange
	DeployType string
}

// ReleaseInfo is the provider-agnostic view of a deployed release, derived
// from action.ReleaseGet(OutputNoPrint:true, PrintValues:true). DeployedAt is
// intentionally NOT included: it is always zero as of nelm v1.26.2
// (verified fact) and must not be mapped.
type ReleaseInfo struct {
	Name      string
	Namespace string
	Revision  int
	Status    string

	ChartName    string
	ChartVersion string
	AppVersion   string

	// Values holds the coalesced values used to render the release
	// (canonicalized to JSON by internal/provider for metadata.values_json).
	Values map[string]any

	// Resources are the resource identities recorded in the stored release
	// (used by Read to know what to live-GET; see design §2.4).
	Resources []ResourceRef
}

// ResourceRef identifies a single Kubernetes resource by group/version/kind
// and namespace/name. It is the shared identity type between nelmclient
// (LiveObjects) and internal/planconv (Key/KeyScoper).
type ResourceRef struct {
	Group     string
	Version   string
	Kind      string
	Namespace string
	Name      string
}
