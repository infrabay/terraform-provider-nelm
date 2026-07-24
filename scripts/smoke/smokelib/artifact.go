//go:build smoke

package smokelib

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/werf/nelm/pkg/common"
	"github.com/werf/nelm/pkg/plan"
)

// DecodedArtifact mirrors plan.PlanArtifact but nests the actually-decoded
// plan.PlanArtifactData in place of the opaque DataRaw JSON-string field, so
// json.MarshalIndent on it produces a genuinely readable fixture (a plain
// text editor / `jq` can walk into `.data.changes[].after` directly instead
// of unescaping a JSON-in-a-string blob). Every "*.decoded.json" fixture
// file in internal/planconv/testdata/ is produced by marshaling one of
// these. The raw "*.artifact.json.gz" sibling file is always saved too
// (CopyFile of the exact bytes nelm wrote) — that is the artifact AS THE
// PROVIDER WILL ENCOUNTER IT, gzip envelope and DataRaw JSON-string and all.
type DecodedArtifact struct {
	APIVersion string                   `json:"apiVersion"`
	DeployType common.DeployType        `json:"deployType"`
	Encrypted  bool                     `json:"encrypted"`
	Release    plan.PlanArtifactRelease `json:"release"`
	Timestamp  string                   `json:"timestamp"`
	Data       *plan.PlanArtifactData   `json:"data"`
}

// Decode converts a *plan.PlanArtifact (as returned by ReadArtifact) into
// its fixture-friendly, fully-nested JSON form.
func Decode(artifact *plan.PlanArtifact) *DecodedArtifact {
	return &DecodedArtifact{
		APIVersion: artifact.APIVersion,
		DeployType: artifact.DeployType,
		Encrypted:  artifact.Encrypted,
		Release:    artifact.Release,
		Timestamp:  artifact.Timestamp.UTC().Format("2006-01-02T15:04:05.000000000Z"),
		Data:       artifact.Data,
	}
}

// ReadArtifact reads+gunzips+JSON-decodes a plan artifact exactly the way
// production code will (plan.ReadPlanArtifact with no secretKey/secretWorkDir
// — CONTRACTS.md: no SecretKey/WERF_SECRET_KEY anywhere in this codebase,
// werf secret values are out of scope for v1).
func ReadArtifact(ctx context.Context, path string) (*plan.PlanArtifact, error) {
	artifact, err := plan.ReadPlanArtifact(ctx, path, "", "")
	if err != nil {
		return nil, fmt.Errorf("read plan artifact %s: %w", path, err)
	}

	return artifact, nil
}

// MarshalIndent is a small json.MarshalIndent wrapper that panics on error
// (acceptable in a throwaway capture program: a marshal failure here means
// the fixture is unusable anyway and the run should stop loudly).
func MarshalIndent(v interface{}) []byte {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(fmt.Errorf("marshal fixture json: %w", err))
	}

	return b
}
