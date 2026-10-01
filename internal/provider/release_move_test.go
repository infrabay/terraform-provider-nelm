package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// helmReleaseStateJSON is a hashicorp/helm v3.x helm_release state (schema
// version 2) as Terraform stores it: the split OCI form (repository plus a
// bare chart name), a set list, max_history and atomic, plus the attributes
// nelm_release has no use for.
const helmReleaseStateJSON = `{
  "atomic": true,
  "chart": "nginx",
  "cleanup_on_fail": false,
  "create_namespace": false,
  "dependency_update": false,
  "description": null,
  "devel": null,
  "disable_crd_hooks": false,
  "disable_openapi_validation": false,
  "disable_webhooks": false,
  "force_update": false,
  "id": "app",
  "keyring": null,
  "lint": false,
  "manifest": null,
  "max_history": 10,
  "metadata": {
    "app_version": "1.27.0",
    "chart": "nginx",
    "first_deployed": 1727000000,
    "last_deployed": 1727700000,
    "name": "app",
    "namespace": "app",
    "notes": "",
    "revision": 7,
    "values": "{}",
    "version": "1.2.3"
  },
  "name": "app",
  "namespace": "app",
  "pass_credentials": false,
  "postrender": null,
  "recreate_pods": false,
  "render_subchart_notes": true,
  "replace": false,
  "repository": "oci://registry-1.docker.io/bitnamicharts",
  "repository_ca_file": null,
  "repository_cert_file": null,
  "repository_key_file": null,
  "repository_password": null,
  "repository_username": null,
  "reset_values": false,
  "resources": null,
  "reuse_values": false,
  "set": [
    {"name": "replicaCount", "type": "", "value": "2"},
    {"name": "service.type", "type": "string", "value": "ClusterIP"}
  ],
  "set_list": null,
  "set_sensitive": null,
  "set_wo": null,
  "set_wo_revision": null,
  "skip_crds": true,
  "status": "deployed",
  "take_ownership": false,
  "timeout": 600,
  "upgrade_install": false,
  "values": ["resources:\n  requests:\n    cpu: 50m\n"],
  "verify": false,
  "version": "1.2.3",
  "wait": true,
  "wait_for_jobs": false
}`

func runMove(t *testing.T, providerAddress, typeName, stateJSON string) *resource.MoveStateResponse {
	t.Helper()

	ctx := context.Background()
	req := resource.MoveStateRequest{
		SourceProviderAddress: providerAddress,
		SourceTypeName:        typeName,
		SourceSchemaVersion:   2,
		SourceRawState:        &tfprotov6.RawState{JSON: []byte(stateJSON)},
	}
	resp := &resource.MoveStateResponse{TargetState: nullState(ctx)}

	moveFromHelmRelease(ctx, req, resp)

	return resp
}

func TestMoveFromHelmRelease(t *testing.T) {
	resp := runMove(t, helmProviderAddress, "helm_release", helmReleaseStateJSON)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected errors: %v", resp.Diagnostics)
	}

	got := stateModel(t, context.Background(), resp.TargetState)

	checks := []struct {
		name      string
		got, want any
	}{
		{"id", got.ID.ValueString(), "app/app"},
		{"name", got.Name.ValueString(), "app"},
		{"namespace", got.Namespace.ValueString(), "app"},
		// The split OCI form is joined into one chart reference.
		{"chart", got.Chart.ValueString(), "oci://registry-1.docker.io/bitnamicharts/nginx"},
		{"repository is null", got.Repository.IsNull(), true},
		{"version", got.Version.ValueString(), "1.2.3"},
		{"values", len(got.Values.Elements()), 1},
		{"set entries", len(got.Set), 2},
		{"set[0] default type is null", got.Set[0].Type.IsNull(), true},
		{"set[1] type", got.Set[1].Type.ValueString(), "string"},
		{"set_sensitive is null", got.SetSensitive == nil, true},
		{"release_history_limit", got.ReleaseHistoryLimit.ValueInt64(), int64(10)},
		{"auto_rollback (atomic)", got.AutoRollback.ValueBool(), true},
		{"no_install_crds (skip_crds)", got.NoInstallCRDs.ValueBool(), true},
		{"force_adoption", got.ForceAdoption.ValueBool(), false},
		{"adopt_existing", got.AdoptExisting.ValueBool(), false},
		{"diff_mode", got.DiffMode.ValueString(), "full"},
		{"release_storage_driver", got.ReleaseStorageDriver.ValueString(), "secret"},
		// Computed attributes are left for the next refresh to fill in.
		{"status is null", got.Status.IsNull(), true},
		{"revision is null", got.Revision.IsNull(), true},
		{"metadata is null", got.Metadata.IsNull(), true},
		{"resources is null", got.Resources.IsNull(), true},
	}

	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}

	if v := got.Values.Elements()[0].(types.String).ValueString(); v != "resources:\n  requests:\n    cpu: 50m\n" {
		t.Errorf("values[0] = %q", v)
	}
}

func TestMoveFromHelmRelease_Defaults(t *testing.T) {
	// helm_release computes version from a local chart's Chart.yaml; a
	// nelm_release config for a local chart never sets it.
	resp := runMove(t, helmProviderAddress, "helm_release",
		`{"name": "web", "namespace": null, "chart": "./charts/web", "repository": null, "version": "0.1.0", "values": [], "max_history": 0, "set": null}`)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected errors: %v", resp.Diagnostics)
	}

	got := stateModel(t, context.Background(), resp.TargetState)

	if got.Namespace.ValueString() != "default" || got.ID.ValueString() != "default/web" {
		t.Errorf("namespace/id = %q/%q, want default/default/web", got.Namespace.ValueString(), got.ID.ValueString())
	}

	if got.Chart.ValueString() != "./charts/web" || !got.Repository.IsNull() || !got.Version.IsNull() {
		t.Errorf("chart/repository/version = %q/%v/%v", got.Chart.ValueString(), got.Repository, got.Version)
	}

	if !got.Values.IsNull() || got.Set != nil || !got.ReleaseHistoryLimit.IsNull() {
		t.Errorf("empty values/set and max_history 0 must map to null, got %v/%v/%v", got.Values, got.Set, got.ReleaseHistoryLimit)
	}
}

func TestIsLocalChartPath(t *testing.T) {
	tests := []struct {
		chart, repository string
		want              bool
	}{
		{"/abs/charts/web", "", true},
		{"./charts/web", "", true},
		{"../charts/web", "", true},
		{"web-0.1.0.tgz", "", true},
		{"oci://registry.example/charts/web", "", false},
		{"web", "https://charts.example", false},
		{"bitnami/redis", "", false},
	}

	for _, tt := range tests {
		if got := isLocalChartPath(tt.chart, tt.repository); got != tt.want {
			t.Errorf("isLocalChartPath(%q, %q) = %v, want %v", tt.chart, tt.repository, got, tt.want)
		}
	}
}

func TestMoveFromHelmRelease_OtherSourcesAreSkipped(t *testing.T) {
	for _, src := range []struct{ provider, typ string }{
		{"registry.terraform.io/hashicorp/kubernetes", "kubernetes_manifest"},
		{"registry.terraform.io/example/helm", "helm_release"},
	} {
		resp := runMove(t, src.provider, src.typ, helmReleaseStateJSON)

		if resp.Diagnostics.HasError() || !resp.TargetState.Raw.IsNull() {
			t.Errorf("%s %s: mover must skip (no diagnostics, no state), got %v / %v", src.provider, src.typ, resp.Diagnostics, resp.TargetState.Raw)
		}
	}
}

func TestMoveFromHelmRelease_Invalid(t *testing.T) {
	for name, stateJSON := range map[string]string{
		"not JSON":  `{`,
		"no name":   `{"namespace": "apps", "chart": "web"}`,
		"null name": `{"name": null, "chart": "web"}`,
	} {
		if resp := runMove(t, helmProviderAddress, "helm_release", stateJSON); !resp.Diagnostics.HasError() {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// TestMoveResourceState_Protocol drives a moved {} from helm_release through
// the real provider server (the RPC Terraform calls), proving the resource
// advertises and wires ResourceWithMoveState.
func TestMoveResourceState_Protocol(t *testing.T) {
	ctx := context.Background()

	srv, err := providerserver.NewProtocol6WithError(New("test")())()
	if err != nil {
		t.Fatalf("provider server: %v", err)
	}

	if _, err := srv.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{}); err != nil {
		t.Fatalf("GetProviderSchema: %v", err)
	}

	resp, err := srv.MoveResourceState(ctx, &tfprotov6.MoveResourceStateRequest{
		SourceProviderAddress: helmProviderAddress,
		SourceTypeName:        "helm_release",
		SourceSchemaVersion:   2,
		SourceState:           &tfprotov6.RawState{JSON: []byte(helmReleaseStateJSON)},
		TargetTypeName:        "nelm_release",
	})
	if err != nil {
		t.Fatalf("MoveResourceState: %v", err)
	}

	for _, d := range resp.Diagnostics {
		if d.Severity == tfprotov6.DiagnosticSeverityError {
			t.Fatalf("MoveResourceState error: %s: %s", d.Summary, d.Detail)
		}
	}

	if resp.TargetState == nil {
		t.Fatal("MoveResourceState returned no target state")
	}

	stateType := releaseResourceSchema(ctx).Type().TerraformType(ctx)

	val, err := resp.TargetState.Unmarshal(stateType)
	if err != nil {
		t.Fatalf("decode target state: %v", err)
	}

	var attrs map[string]tftypes.Value
	if err := val.As(&attrs); err != nil {
		t.Fatalf("target state attributes: %v", err)
	}

	var id string
	if err := attrs["id"].As(&id); err != nil || id != "app/app" {
		t.Errorf("target id = %q (%v), want app/app", id, err)
	}

	if !attrs["status"].IsNull() || !attrs["resources"].IsNull() {
		t.Errorf("status/resources = %v/%v, want null until the next refresh", attrs["status"], attrs["resources"])
	}
}
