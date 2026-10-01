package provider

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/infrabay/terraform-provider-nelm/internal/nelmclient"
)

// updatePlanModel is an update plan of my-release at revision 4 that
// reinstalls: config and resources known, status/revision/metadata Unknown.
func updatePlanModel() releaseModel {
	m := appliedModel(4, "deployed")
	m.Status = types.StringUnknown()
	m.Revision = types.Int64Unknown()
	m.Metadata = types.ObjectUnknown(metadataAttrTypes)

	return m
}

// TestCreateOrUpdate_RefusalHandsNothingOver: the helm_release field-manager
// hand-over is a real managedFields write, so it may only run once the
// History guards have let the install through. A Create refused because the
// release already exists (in its own or the other storage backend) and an
// Update refused because another operation holds the release lock must leave
// the release's objects untouched.
func TestCreateOrUpdate_RefusalHandsNothingOver(t *testing.T) {
	deployed := &nelmclient.ReleaseHistory{Revision: 3, Status: "deployed", LastDeployed: time.Now().Add(-time.Hour), Deployed: true}

	tests := []struct {
		name      string
		client    *fakeReleaseClient
		run       func(*testing.T, *fakeReleaseClient) diag.Diagnostics
		wantError string
	}{
		{
			name:   "create over an existing release",
			client: &fakeReleaseClient{history: deployed, getInfo: deployedInfo(4, "deployed")},
			run: func(t *testing.T, c *fakeReleaseClient) diag.Diagnostics {
				return runCreate(t, c, plannedCreateModel()).Diagnostics
			},
			wantError: "already exists",
		},
		{
			name: "create over a release in the other storage backend",
			client: &fakeReleaseClient{
				historyByDriver: map[string]*nelmclient.ReleaseHistory{"secret": deployed},
				getInfo:         deployedInfo(4, "deployed"),
			},
			run: func(t *testing.T, c *fakeReleaseClient) diag.Diagnostics {
				plan := plannedCreateModel()
				plan.ReleaseStorageDriver = types.StringValue("configmap")

				return runCreate(t, c, plan).Diagnostics
			},
			wantError: "already exists in the secret storage backend",
		},
		{
			name: "update while another operation holds the release lock",
			client: &fakeReleaseClient{
				history: &nelmclient.ReleaseHistory{Revision: 4, Status: "pending-upgrade", LastDeployed: time.Now().Add(-time.Minute), Deployed: true},
				getInfo: deployedInfo(5, "deployed"),
			},
			run: func(t *testing.T, c *fakeReleaseClient) diag.Diagnostics {
				return runUpdate(t, c, updatePlanModel(), appliedModel(4, "pending-upgrade")).Diagnostics
			},
			wantError: "locked by another operation",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertOneDiag(t, tt.run(t, tt.client), diag.SeverityError, tt.wantError)

			if tt.client.handOvers != 0 || tt.client.installs != 0 {
				t.Errorf("hand-overs/installs = %d/%d, want 0/0: a refused apply must not write to the release's objects",
					tt.client.handOvers, tt.client.installs)
			}
		})
	}
}

// TestCreateOrUpdate_HandsOverBeforeInstall: every Create and Update that
// installs hands the field managers over first, exactly once, so that very
// install prunes what the chart no longer renders.
func TestCreateOrUpdate_HandsOverBeforeInstall(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		client := &fakeReleaseClient{getInfo: deployedInfo(1, "deployed")}

		if resp := runCreate(t, client, plannedCreateModel()); resp.Diagnostics.HasError() {
			t.Fatalf("unexpected errors: %v", resp.Diagnostics)
		}

		assertHandedOverBeforeInstall(t, client)
	})

	t.Run("update", func(t *testing.T) {
		client := &fakeReleaseClient{
			history: &nelmclient.ReleaseHistory{Revision: 4, Status: "deployed", Deployed: true},
			getInfo: deployedInfo(5, "deployed"),
		}

		if resp := runUpdate(t, client, updatePlanModel(), appliedModel(4, "deployed")); resp.Diagnostics.HasError() {
			t.Fatalf("unexpected errors: %v", resp.Diagnostics)
		}

		assertHandedOverBeforeInstall(t, client)
	})
}

func assertHandedOverBeforeInstall(t *testing.T, client *fakeReleaseClient) {
	t.Helper()

	if client.installs != 1 || !slices.Equal(client.installsAtHandOver, []int{0}) {
		t.Errorf("installs = %d, installs already run at each hand-over = %v; want one hand-over before the one install",
			client.installs, client.installsAtHandOver)
	}
}

// TestCreateOrUpdate_HandOverFailureChangesNothing: a failed hand-over fails
// the apply before nelm runs, and its error is scrubbed of set_sensitive
// values like every other diagnostic.
func TestCreateOrUpdate_HandOverFailureChangesNothing(t *testing.T) {
	const secret = "hunter2-hunter2"

	client := &fakeReleaseClient{handOverErr: errors.New(`patch managedFields of v1/ConfigMap/default/app: denied: value "` + secret + `"`)}

	plan := plannedCreateModel()
	plan.SetSensitive = []setModel{{Name: types.StringValue("auth.password"), Value: types.StringValue(secret), Type: types.StringNull()}}

	resp := runCreate(t, client, plan)

	assertOneDiag(t, resp.Diagnostics, diag.SeverityError, "field-manager hand-over failed")

	if strings.Contains(resp.Diagnostics[0].Detail(), secret) {
		t.Errorf("hand-over error leaks a set_sensitive value: %s", resp.Diagnostics[0].Detail())
	}

	if client.installs != 0 {
		t.Errorf("Install called %d times, want 0 after a failed hand-over", client.installs)
	}

	if !resp.State.Raw.IsNull() {
		t.Error("a failed create must not persist any state")
	}
}

// TestCreateOrUpdate_HandOverSkippedWarns: objects skipped because an
// admission webhook was unavailable only warn; the install still runs.
func TestCreateOrUpdate_HandOverSkippedWarns(t *testing.T) {
	client := &fakeReleaseClient{
		handOverSkipped: []string{"v1/ConfigMap/default/app: admission webhook unavailable"},
		getInfo:         deployedInfo(1, "deployed"),
	}

	resp := runCreate(t, client, plannedCreateModel())

	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected errors: %v", resp.Diagnostics)
	}

	assertOneDiag(t, resp.Diagnostics, diag.SeverityWarning, "hand-over skipped")

	if client.installs != 1 {
		t.Errorf("Install called %d times, want 1", client.installs)
	}
}
