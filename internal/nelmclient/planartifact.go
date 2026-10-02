package nelmclient

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/werf/nelm/pkg/common"
	"github.com/werf/nelm/pkg/plan"
)

// planArtifactAPIVersion is the plan artifact scheme readPlanArtifact
// understands: nelm's plan.PlanArtifactSchemeVersion, pinned here so that a
// nelm upgrade that changes the scheme fails TestPlanArtifactFormat instead
// of being read with the old layout.
const planArtifactAPIVersion = "v1"

// planArtifact is the part of nelm's plan artifact (pkg/plan plan_artifact.go:
// a gzip-compressed JSON plan.PlanArtifact) that Plan reads back. nelm keeps
// the artifact's data (plan.PlanArtifactData, json:"-") as a JSON document in
// the string field dataRaw, encrypted when a secret key is set; the provider
// never sets one (CONTRACTS.md global-state rules).
type planArtifact struct {
	APIVersion string            `json:"apiVersion"`
	DataRaw    string            `json:"dataRaw"`
	DeployType common.DeployType `json:"deployType"`
	Encrypted  bool              `json:"encrypted"`
}

// planArtifactData is the part of plan.PlanArtifactData that Plan uses.
// changes is kept raw so that a missing field (renamed by a future nelm) can
// be told from a plan without changes (JSON null).
type planArtifactData struct {
	Changes json.RawMessage `json:"changes"`
}

// readPlanArtifact reads back what Plan needs from the plan artifact
// action.ReleasePlanInstall wrote at path: the deploy type and the planned
// resource changes. It replaces nelm's plan.ReadPlanArtifact, which decodes
// the whole plan.PlanArtifactData and cannot read an artifact in which the
// dry-run apply of any object failed (issue #8): nelm writes that error into
// installableResourceInfos[].dryApplyErr, an error interface it marshals as
// a JSON object and cannot unmarshal again. Plan does not need it: nelm
// plans such an object as a "blind apply" whose change carries the error in
// Reason. So everything but deployType and data.changes is skipped without
// being decoded into nelm's types, and unknown fields are ignored.
func readPlanArtifact(path string) (*PlanResult, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open plan artifact file: %w", err)
	}
	defer func() { _ = file.Close() }()

	return decodePlanArtifact(file)
}

// decodePlanArtifact is readPlanArtifact on the artifact's bytes.
func decodePlanArtifact(r io.Reader) (*PlanResult, error) {
	gzipReader, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("create gzip reader: %w", err)
	}
	defer func() { _ = gzipReader.Close() }()

	var artifact planArtifact

	if err := json.NewDecoder(gzipReader).Decode(&artifact); err != nil {
		return nil, fmt.Errorf("decode plan artifact json: %w", err)
	}

	if artifact.APIVersion != planArtifactAPIVersion {
		return nil, fmt.Errorf("unsupported plan artifact apiVersion %q, want %q", artifact.APIVersion, planArtifactAPIVersion)
	}

	if artifact.Encrypted {
		return nil, errors.New("plan artifact is encrypted, but the provider never sets a secret key")
	}

	if artifact.DataRaw == "" {
		return nil, errors.New("artifact data is empty")
	}

	if artifact.DeployType == "" {
		return nil, errors.New("plan artifact has no deployType")
	}

	var data planArtifactData

	if err := json.Unmarshal([]byte(artifact.DataRaw), &data); err != nil {
		return nil, fmt.Errorf("decode artifact data json: %w", err)
	}

	if data.Changes == nil {
		return nil, errors.New(`artifact data has no "changes" field`)
	}

	var changes []*plan.ResourceChange

	if err := json.Unmarshal(data.Changes, &changes); err != nil {
		return nil, fmt.Errorf("decode artifact data changes: %w", err)
	}

	return &PlanResult{
		Changes:    changes,
		DeployType: string(artifact.DeployType),
	}, nil
}
