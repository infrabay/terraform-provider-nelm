package provider

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/werf/nelm/pkg/plan"
	"github.com/werf/nelm/pkg/resource/spec"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/infrabay/terraform-provider-nelm/internal/nelmclient"
)

const (
	sensitiveDeployKey = "apps/v1/Deployment/default/my-release"
	sensitiveCMKey     = "v1/ConfigMap/default/my-release"
)

// sensitiveRender is what a chart renders for set_sensitive env.DATABASE_URL
// = dbURL and apiKey = apiKey: a container env value and ConfigMap data —
// non-Secret objects, which redaction by kind does not cover.
func sensitiveRender(dbURL, apiKey string) []*unstructured.Unstructured {
	return []*unstructured.Unstructured{
		{Object: map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata":   map[string]any{"name": "my-release"},
			"spec": map[string]any{
				"template": map[string]any{"spec": map[string]any{"containers": []any{
					map[string]any{
						"name":  "api",
						"image": "api:1.0.0",
						"env":   []any{map[string]any{"name": "DATABASE_URL", "value": dbURL}},
					},
				}}},
			},
		}},
		{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]any{"name": "my-release"},
			"data": map[string]any{
				"DATABASE_URL":  dbURL,
				"settings.json": `{"apiKey":"` + apiKey + `"}`,
			},
		}},
	}
}

// withSecrets is model with the set_sensitive entries sensitiveRender's
// values come from (apiKey through a type = "json" entry).
func withSecrets(model releaseModel, dbURL, apiKey string) releaseModel {
	model.SetSensitive = []setModel{
		{Name: types.StringValue("env.DATABASE_URL"), Value: types.StringValue(dbURL), Type: types.StringNull()},
		{Name: types.StringValue("api"), Value: types.StringValue(`{"key":"` + apiKey + `"}`), Type: types.StringValue("json")},
	}

	return model
}

// releaseWithLive is a deployed my-release whose stored manifests and live
// objects are objs.
func releaseWithLive(objs []*unstructured.Unstructured) (*nelmclient.ReleaseInfo, map[nelmclient.ResourceRef]*unstructured.Unstructured) {
	info := deployedInfo(1, "deployed")
	info.Manifests = objs
	live := map[nelmclient.ResourceRef]*unstructured.Unstructured{}

	for _, obj := range objs {
		gvk := obj.GroupVersionKind()
		ref := nelmclient.ResourceRef{Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind, Namespace: "default", Name: obj.GetName()}
		info.Resources = append(info.Resources, ref)
		live[ref] = liveObject(obj)
	}

	return info, live
}

// assertScrubbed fails when a resources value carries one of secrets in
// cleartext, or carries no redaction placeholder at all.
func assertScrubbed(t *testing.T, where string, resources map[string]string, secrets ...string) {
	t.Helper()

	if len(resources) == 0 {
		t.Fatalf("%s: no resources", where)
	}

	for key, value := range resources {
		for _, secret := range secrets {
			if strings.Contains(value, secret) {
				t.Errorf("%s: resources[%q] carries a set_sensitive value in cleartext: %s", where, key, value)
			}
		}

		if !strings.Contains(value, "sensitive bytes, hash") {
			t.Errorf("%s: resources[%q] has no redaction placeholder: %s", where, key, value)
		}
	}
}

// stringMap decodes a known "resources" value.
func stringMap(t *testing.T, m types.Map) map[string]string {
	t.Helper()

	out := map[string]string{}
	if diags := m.ElementsAs(context.Background(), &out, false); diags.HasError() {
		t.Fatalf("decode resources: %v", diags)
	}

	return out
}

// TestSetSensitiveScrubbedFromResources is the F06 regression test. A
// set_sensitive value that a chart renders into a non-Secret object (an env
// value, ConfigMap data) went verbatim into the "resources" map: printed by
// every plan and stored in state, where helm_release shows no manifest at
// all. The planned side (ModifyPlan) and the live side (Read) now both carry
// nelm's redaction placeholder instead, and still agree when nothing changed,
// so the steady-state plan stays empty; a rotated value still shows as a
// change.
func TestSetSensitiveScrubbedFromResources(t *testing.T) {
	ctx := context.Background()

	const (
		dbURL  = "postgres://app:P4ss@10.0.0.5/db"
		apiKey = "sk_live_TOPSECRET_123"
	)

	client := &fakeReleaseClient{renderObjs: sensitiveRender(dbURL, apiKey)}

	createPlan := runModifyPlan(t, client, withSecrets(baseTestReleaseModel(), dbURL, apiKey), nil)
	if createPlan.Diagnostics.HasError() {
		t.Fatalf("create plan: unexpected errors: %v", createPlan.Diagnostics)
	}

	planned := plannedResources(t, createPlan.Plan)
	assertScrubbed(t, "create plan", planned, dbURL, apiKey)

	// The release as installed by that plan: Read rebuilds "resources" from
	// the live objects.
	installed := withSecrets(appliedModel(1, "deployed"), dbURL, apiKey)
	installed.Resources = types.MapValueMust(types.StringType, map[string]attr.Value{
		sensitiveDeployKey: types.StringValue(planned[sensitiveDeployKey]),
		sensitiveCMKey:     types.StringValue(planned[sensitiveCMKey]),
	})

	client.getInfo, client.live = releaseWithLive(sensitiveRender(dbURL, apiKey))

	readReq := resource.ReadRequest{State: buildState(t, ctx, installed)}
	readResp := &resource.ReadResponse{State: readReq.State}
	(&releaseResource{client: client}).Read(ctx, readReq, readResp)

	if readResp.Diagnostics.HasError() {
		t.Fatalf("Read: unexpected errors: %v", readResp.Diagnostics)
	}

	refreshed := stateModel(t, ctx, readResp.State)
	live := stringMap(t, refreshed.Resources)
	assertScrubbed(t, "Read", live, dbURL, apiKey)

	if !maps.Equal(live, planned) {
		t.Fatalf("Read and the plan disagree on unchanged objects (phantom diff):\nread:    %v\nplanned: %v", live, planned)
	}

	client.planResult = &nelmclient.PlanResult{DeployType: nelmclient.DeployTypeUpgrade}

	steady := runModifyPlan(t, client, refreshed, &refreshed)
	if steady.Diagnostics.HasError() {
		t.Fatalf("steady-state plan: unexpected errors: %v", steady.Diagnostics)
	}

	if got := plannedResourcesValue(t, steady.Plan); !got.Equal(refreshed.Resources) {
		t.Errorf("steady-state resources = %v, want the refreshed value (an empty plan)", got)
	}

	assertComputedUnknown(t, steady.Plan, false)

	// Rotating the database URL: both objects change, from one placeholder to
	// another.
	const rotatedURL = "postgres://app:N3wP4ss@10.0.0.5/db"

	client.renderObjs = sensitiveRender(rotatedURL, apiKey)
	client.planResult = &nelmclient.PlanResult{DeployType: nelmclient.DeployTypeUpgrade}

	for i, after := range sensitiveRender(rotatedURL, apiKey) {
		after.SetNamespace("default")
		client.planResult.Changes = append(client.planResult.Changes, &plan.ResourceChange{
			Type:         "update",
			ResourceMeta: &spec.ResourceMeta{Name: after.GetName(), GroupVersionKind: after.GroupVersionKind()},
			Before:       liveObject(sensitiveRender(dbURL, apiKey)[i]),
			After:        after,
		})
	}

	rotation := runModifyPlan(t, client, withSecrets(refreshed, rotatedURL, apiKey), &refreshed)
	if rotation.Diagnostics.HasError() {
		t.Fatalf("rotation plan: unexpected errors: %v", rotation.Diagnostics)
	}

	rotated := plannedResources(t, rotation.Plan)
	assertScrubbed(t, "rotation plan", rotated, dbURL, rotatedURL, apiKey)

	for key, value := range rotated {
		if value == live[key] {
			t.Errorf("rotation plan: resources[%q] unchanged, want the rotated value's placeholder", key)
		}
	}
}

// TestCreateOrUpdate_ScrubsSetSensitiveFromLiveReads covers the Create/Update
// paths that build "resources" from a live read rather than copying the plan:
// a diff left to the apply (an unreachable cluster at plan time,
// diff_mode = "none"), and a failed update that rotated a value, whose
// objects can carry the old value or the new one.
func TestCreateOrUpdate_ScrubsSetSensitiveFromLiveReads(t *testing.T) {
	ctx := context.Background()

	const (
		dbURL  = "postgres://app:P4ss@10.0.0.5/db"
		apiKey = "sk_live_TOPSECRET_123"
	)

	t.Run("diff computed at apply", func(t *testing.T) {
		client := &fakeReleaseClient{}
		client.getInfo, client.live = releaseWithLive(sensitiveRender(dbURL, apiKey))

		planModel := withSecrets(plannedCreateModel(), dbURL, apiKey)
		planModel.Resources = types.MapUnknown(types.StringType)

		resp := runCreate(t, client, planModel)
		if resp.Diagnostics.HasError() {
			t.Fatalf("Create: unexpected errors: %v", resp.Diagnostics)
		}

		assertScrubbed(t, "Create", stringMap(t, stateModel(t, ctx, resp.State).Resources), dbURL, apiKey)
	})

	t.Run("failed update rotating a value", func(t *testing.T) {
		const rotatedURL = "postgres://app:N3wP4ss@10.0.0.5/db"

		// The install got as far as the Deployment before it failed: it runs
		// the new value, while the ConfigMap and the stored manifests still
		// hold the old one.
		client := &fakeReleaseClient{
			history:    &nelmclient.ReleaseHistory{Revision: 1, Status: "deployed", Deployed: true},
			installErr: errors.New("context deadline exceeded"),
		}

		var live map[nelmclient.ResourceRef]*unstructured.Unstructured

		client.getInfo, live = releaseWithLive(sensitiveRender(dbURL, apiKey))
		_, client.live = releaseWithLive(sensitiveRender(rotatedURL, apiKey)[:1])

		for ref, obj := range live {
			if ref.Kind == "ConfigMap" {
				client.live[ref] = obj
			}
		}

		prior := withSecrets(appliedModel(1, "deployed"), dbURL, apiKey)
		planModel := withSecrets(appliedModel(1, "deployed"), rotatedURL, apiKey)
		planModel.Resources = types.MapUnknown(types.StringType)

		resp := runUpdate(t, client, planModel, prior)
		if !resp.Diagnostics.HasError() {
			t.Fatal("expected the failed install to fail the apply")
		}

		got := stringMap(t, stateModel(t, ctx, resp.State).Resources)
		assertScrubbed(t, "failed Update", got, dbURL, rotatedURL, apiKey)

		if len(got) != 2 {
			t.Errorf("resources = %v, want both objects", got)
		}
	})
}

// TestRead_ScrubsTheValueOfAFailedRotation: a failed update that rotated a
// set_sensitive value keeps the previous configuration in state (so the
// change is retried), and the state's set_sensitive values no longer cover
// what the objects that update partly applied carry. Read used to put the
// new value into state in cleartext, and the next plan's diff showed it.
// It now also scrubs the values the release's last revision stores at the
// set_sensitive names, whatever the previous value was, empty included.
func TestRead_ScrubsTheValueOfAFailedRotation(t *testing.T) {
	ctx := context.Background()

	const (
		dbURL      = "postgres://app:P4ss@10.0.0.5/db"
		rotatedURL = "postgres://app:N3wP4ss@10.0.0.5/db"
		apiKey     = "sk_live_TOPSECRET_123"
	)

	for name, previous := range map[string]string{
		"value -> rotated": dbURL,
		"empty -> rotated": "",
	} {
		t.Run(name, func(t *testing.T) {
			// Revision 2 failed after updating the Deployment: Nelm stored it
			// with the new value, which the Deployment now runs, while the
			// ConfigMap still holds the previous one.
			info, _ := releaseWithLive(sensitiveRender(rotatedURL, apiKey))
			info.Revision, info.Status = 2, "failed"
			info.Values = map[string]any{
				"env": map[string]any{"DATABASE_URL": rotatedURL},
				"api": map[string]any{"key": apiKey},
			}

			_, live := releaseWithLive([]*unstructured.Unstructured{
				sensitiveRender(rotatedURL, apiKey)[0],
				sensitiveRender(previous, apiKey)[1],
			})

			client := &fakeReleaseClient{getInfo: info, live: live}
			state := withSecrets(appliedModel(2, "failed"), previous, apiKey)
			// No stored values to project onto: the stored manifests are the template.
			state.Resources = emptyResourcesMap()

			req := resource.ReadRequest{State: buildState(t, ctx, state)}
			resp := &resource.ReadResponse{State: req.State}
			(&releaseResource{client: client}).Read(ctx, req, resp)

			if resp.Diagnostics.HasError() {
				t.Fatalf("Read: unexpected errors: %v", resp.Diagnostics)
			}

			secrets := []string{rotatedURL, apiKey}
			if previous != "" {
				secrets = append(secrets, previous)
			}

			got := stringMap(t, stateModel(t, ctx, resp.State).Resources)
			assertScrubbed(t, "Read", got, secrets...)

			if len(got) != 2 {
				t.Errorf("resources = %v, want both objects", got)
			}
		})
	}
}
