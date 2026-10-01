package provider

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/infrabay/terraform-provider-nelm/internal/nelmclient"
)

// --- installGuardDiags (G3.1/F07 adoption guard, G5.1 pending lock) --------

func TestInstallGuardDiags(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	const takeoverAfter = 15 * time.Minute

	deployed := &nelmclient.ReleaseHistory{Revision: 3, Status: "deployed", LastDeployed: now.Add(-24 * time.Hour), Deployed: true}
	failedFirstInstall := &nelmclient.ReleaseHistory{Revision: 1, Status: "failed", LastDeployed: now.Add(-time.Hour)}
	failedUpgrade := &nelmclient.ReleaseHistory{Revision: 4, Status: "failed", LastDeployed: now.Add(-time.Hour), Deployed: true}
	uninstalled := &nelmclient.ReleaseHistory{Revision: 2, Status: "uninstalled", LastDeployed: now.Add(-time.Hour)}
	freshPending := &nelmclient.ReleaseHistory{Revision: 5, Status: "pending-rollback", LastDeployed: now.Add(-2 * time.Minute), Deployed: true}
	stalePending := &nelmclient.ReleaseHistory{Revision: 5, Status: "pending-upgrade", LastDeployed: now.Add(-2 * time.Hour), Deployed: true}
	staleFirstInstall := &nelmclient.ReleaseHistory{Revision: 1, Status: "pending-install", LastDeployed: now.Add(-2 * time.Hour)}
	freshFirstInstall := &nelmclient.ReleaseHistory{Revision: 1, Status: "pending-install", LastDeployed: now.Add(-time.Minute)}
	untimedPending := &nelmclient.ReleaseHistory{Revision: 2, Status: "pending-upgrade", Deployed: true}

	tests := []struct {
		name          string
		history       *nelmclient.ReleaseHistory
		isCreate      bool
		adoptExisting bool
		wantError     string // substring of the error summary; "" = no error
		wantWarning   string // substring of the warning summary; "" = no warning
	}{
		{name: "create, no release", history: &nelmclient.ReleaseHistory{}, isCreate: true},
		{name: "create, nil history", history: nil, isCreate: true},
		{name: "create over a deployed release is refused", history: deployed, isCreate: true, wantError: "already exists"},
		{name: "create over a failed upgrade of a deployed release is refused", history: failedUpgrade, isCreate: true, wantError: "already exists"},
		{name: "create over a deployed release with adopt_existing", history: deployed, isCreate: true, adoptExisting: true},
		{name: "create after a failed first install is retried", history: failedFirstInstall, isCreate: true},
		{name: "create over an uninstalled (kept-history) release", history: uninstalled, isCreate: true},
		{name: "create over a fresh pending revision is a lock", history: freshPending, isCreate: true, wantError: "locked by another operation"},
		{name: "adopt_existing does not override the lock", history: freshPending, isCreate: true, adoptExisting: true, wantError: "locked by another operation"},
		{name: "create over a stale pending revision still needs adopt_existing", history: stalePending, isCreate: true, wantError: "already exists"},
		{name: "create takes a killed first install's stale pending-install over", history: staleFirstInstall, isCreate: true, wantWarning: "stale pending"},
		{name: "create over a fresh pending-install is a lock", history: freshFirstInstall, isCreate: true, wantError: "locked by another operation"},
		{name: "create with adopt_existing takes a stale pending revision over", history: stalePending, isCreate: true, adoptExisting: true, wantWarning: "stale pending"},
		{name: "update of a deployed release", history: deployed},
		{name: "update over a fresh pending revision is a lock", history: freshPending, wantError: "locked by another operation"},
		{name: "update takes a stale pending revision over", history: stalePending, wantWarning: "stale pending"},
		{name: "update takes an untimestamped pending revision over", history: untimedPending, wantWarning: "stale pending"},
		{name: "update after a failed upgrade", history: failedUpgrade},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := plannedCreateModel()
			plan.AdoptExisting = types.BoolValue(tt.adoptExisting)

			diags := installGuardDiags(plan, tt.history, tt.isCreate, takeoverAfter, now)

			assertOneDiag(t, diags, diag.SeverityError, tt.wantError)
			assertOneDiag(t, diags, diag.SeverityWarning, tt.wantWarning)
		})
	}
}

// assertOneDiag asserts diags holds exactly one diagnostic of sev whose
// summary contains want, or none at all when want is "".
func assertOneDiag(t *testing.T, diags diag.Diagnostics, sev diag.Severity, want string) {
	t.Helper()

	got := diagSummaries(diags, sev)

	if want == "" {
		if len(got) != 0 {
			t.Fatalf("unexpected %s diagnostics: %q", sev, got)
		}

		return
	}

	if len(got) != 1 || !strings.Contains(got[0], want) {
		t.Fatalf("%s diagnostics = %q, want exactly one containing %q", sev, got, want)
	}
}

func TestInstallGuardDiags_ErrorsSayHowToRecover(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	plan := plannedCreateModel()

	adopt := installGuardDiags(plan, &nelmclient.ReleaseHistory{Revision: 1, Status: "deployed", LastDeployed: now, Deployed: true}, true, time.Hour, now)
	if !adopt.HasError() {
		t.Fatal("expected the adoption guard to refuse")
	}

	for _, want := range []string{"terraform import <address> default/my-release", "adopt_existing = true", "create_before_destroy"} {
		if !strings.Contains(adopt[0].Detail(), want) {
			t.Errorf("adoption error detail does not mention %q:\n%s", want, adopt[0].Detail())
		}
	}

	lock := installGuardDiags(plan, &nelmclient.ReleaseHistory{Revision: 7, Status: "pending-upgrade", LastDeployed: now.Add(-time.Minute)}, false, time.Hour, now)
	if !lock.HasError() {
		t.Fatal("expected the lock guard to refuse")
	}

	for _, want := range []string{"helm rollback my-release", "sh.helm.release.v1.my-release.v7", "1h0m0s"} {
		if !strings.Contains(lock[0].Detail(), want) {
			t.Errorf("lock error detail does not mention %q:\n%s", want, lock[0].Detail())
		}
	}
}

func TestPendingTakeoverAge(t *testing.T) {
	if got := pendingTakeoverAge(5 * time.Minute); got != minPendingTakeoverAge {
		t.Errorf("pendingTakeoverAge(5m) = %s, want the %s floor", got, minPendingTakeoverAge)
	}

	if got := pendingTakeoverAge(40 * time.Minute); got != 40*time.Minute {
		t.Errorf("pendingTakeoverAge(40m) = %s, want the operation timeout", got)
	}
}

// --- Create / Update through the fake client -------------------------------

func runCreate(t *testing.T, client *fakeReleaseClient, plan releaseModel) *resource.CreateResponse {
	t.Helper()

	ctx := context.Background()
	req := resource.CreateRequest{Plan: buildPlan(t, ctx, plan)}
	resp := &resource.CreateResponse{State: nullState(ctx)}

	(&releaseResource{client: client}).Create(ctx, req, resp)

	return resp
}

func runUpdate(t *testing.T, client *fakeReleaseClient, plan, prior releaseModel) *resource.UpdateResponse {
	t.Helper()

	ctx := context.Background()
	priorState := buildState(t, ctx, prior)
	req := resource.UpdateRequest{Plan: buildPlan(t, ctx, plan), State: priorState}
	// Mirror the framework: UpdateResponse.State starts as the prior state
	// (fwserver/server_updateresource.go).
	resp := &resource.UpdateResponse{State: priorState}

	(&releaseResource{client: client}).Update(ctx, req, resp)

	return resp
}

// TestCreate_RefusesToAdoptExistingRelease is the G3.1/F07 regression test:
// Create used to install straight over an existing release (nelm install is
// install-or-upgrade), silently adopting it — and under create_before_destroy
// the deposed object's destroy then uninstalled it.
func TestCreate_RefusesToAdoptExistingRelease(t *testing.T) {
	client := &fakeReleaseClient{
		history: &nelmclient.ReleaseHistory{Revision: 3, Status: "deployed", LastDeployed: time.Now().Add(-time.Hour), Deployed: true},
		getInfo: deployedInfo(4, "deployed"),
	}

	resp := runCreate(t, client, plannedCreateModel())

	assertOneDiag(t, resp.Diagnostics, diag.SeverityError, "already exists")

	if client.installs != 0 {
		t.Errorf("Install called %d times, want 0: the existing release must not be touched", client.installs)
	}

	if !resp.State.Raw.IsNull() {
		t.Error("a refused create must not persist any state")
	}
}

func TestCreate_AdoptExistingInstalls(t *testing.T) {
	client := &fakeReleaseClient{
		history: &nelmclient.ReleaseHistory{Revision: 3, Status: "deployed", LastDeployed: time.Now().Add(-time.Hour), Deployed: true},
		getInfo: deployedInfo(4, "deployed"),
	}

	plan := plannedCreateModel()
	plan.AdoptExisting = types.BoolValue(true)

	resp := runCreate(t, client, plan)

	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected errors: %v", resp.Diagnostics)
	}

	if client.installs != 1 {
		t.Fatalf("Install called %d times, want 1", client.installs)
	}

	if got := stateModel(t, context.Background(), resp.State).Revision.ValueInt64(); got != 4 {
		t.Errorf("revision = %d, want the adopted release's new revision 4", got)
	}
}

// TestCreate_RetriesFailedFirstInstall: a release whose only history is a
// failed first install is not "existing" — recovering from it must keep
// working without adopt_existing.
func TestCreate_RetriesFailedFirstInstall(t *testing.T) {
	client := &fakeReleaseClient{
		history: &nelmclient.ReleaseHistory{Revision: 1, Status: "failed", LastDeployed: time.Now().Add(-time.Hour)},
		getInfo: deployedInfo(2, "deployed"),
	}

	resp := runCreate(t, client, plannedCreateModel())

	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected errors: %v", resp.Diagnostics)
	}

	if client.installs != 1 {
		t.Fatalf("Install called %d times, want 1", client.installs)
	}
}

// TestCreate_TakesOverKilledFirstInstall: a first create killed mid-install
// leaves only a pending-install revision and no Terraform state, so neither
// import nor an update can reach it. Once that revision is stale, Create
// installs over it like over a failed first install.
func TestCreate_TakesOverKilledFirstInstall(t *testing.T) {
	client := &fakeReleaseClient{
		history: &nelmclient.ReleaseHistory{Revision: 1, Status: "pending-install", LastDeployed: time.Now().Add(-3 * time.Hour)},
		getInfo: deployedInfo(2, "deployed"),
	}

	resp := runCreate(t, client, plannedCreateModel())

	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected errors: %v", resp.Diagnostics)
	}

	assertOneDiag(t, resp.Diagnostics, diag.SeverityWarning, "stale pending")

	if client.installs != 1 {
		t.Fatalf("Install called %d times, want 1", client.installs)
	}
}

func TestOtherStorageDriver(t *testing.T) {
	for driver, want := range map[string]string{
		"secret":     "configmap",
		"secrets":    "configmap",
		"configmap":  "secret",
		"configmaps": "secret",
	} {
		if got := otherStorageDriver(driver); got != want {
			t.Errorf("otherStorageDriver(%q) = %q, want %q", driver, got, want)
		}
	}
}

// TestCreate_RefusesReleaseInOtherBackend is the G3.1 regression test for a
// create_before_destroy release_storage_driver change: Create only read the
// configured (new, empty) backend, so it installed revision 1 there over the
// live objects, and the deposed object's destroy then uninstalled the
// release from the old backend with a green apply.
func TestCreate_RefusesReleaseInOtherBackend(t *testing.T) {
	deployed := &nelmclient.ReleaseHistory{Revision: 3, Status: "deployed", LastDeployed: time.Now().Add(-time.Hour), Deployed: true}

	tests := []struct {
		name          string
		driver        string
		stored        map[string]*nelmclient.ReleaseHistory
		adoptExisting bool
		wantError     string
		wantReads     []string
	}{
		{
			name:      "secret -> configmap",
			driver:    "configmap",
			stored:    map[string]*nelmclient.ReleaseHistory{"secret": deployed},
			wantError: "already exists in the secret storage backend",
			wantReads: []string{"configmap", "secret"},
		},
		{
			name:      "configmaps -> secrets",
			driver:    "secrets",
			stored:    map[string]*nelmclient.ReleaseHistory{"configmap": deployed},
			wantError: "already exists in the configmap storage backend",
			wantReads: []string{"secrets", "configmap"},
		},
		{
			name:          "adopt_existing cannot take a release over from another backend",
			driver:        "configmap",
			stored:        map[string]*nelmclient.ReleaseHistory{"secret": deployed},
			adoptExisting: true,
			wantError:     "already exists in the secret storage backend",
			wantReads:     []string{"configmap", "secret"},
		},
		{
			// A destroy-first driver change: the destroy has uninstalled the
			// release from the old backend before the create runs.
			name:      "release uninstalled from the other backend",
			driver:    "configmap",
			stored:    map[string]*nelmclient.ReleaseHistory{"secret": {Revision: 3, Status: "uninstalled"}},
			wantReads: []string{"configmap", "secret"},
		},
		{
			name:      "nothing stored anywhere",
			driver:    "secret",
			stored:    map[string]*nelmclient.ReleaseHistory{},
			wantReads: []string{"secret", "configmap"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeReleaseClient{historyByDriver: tt.stored, getInfo: deployedInfo(1, "deployed")}

			plan := plannedCreateModel()
			plan.ReleaseStorageDriver = types.StringValue(tt.driver)
			plan.AdoptExisting = types.BoolValue(tt.adoptExisting)

			resp := runCreate(t, client, plan)

			assertOneDiag(t, resp.Diagnostics, diag.SeverityError, tt.wantError)

			if !slices.Equal(client.historyDrivers, tt.wantReads) {
				t.Errorf("History read drivers %q, want %q", client.historyDrivers, tt.wantReads)
			}

			wantInstalls := 1
			if tt.wantError != "" {
				wantInstalls = 0
			}

			if client.installs != wantInstalls {
				t.Errorf("Install called %d times, want %d", client.installs, wantInstalls)
			}

			if tt.wantError != "" && !resp.State.Raw.IsNull() {
				t.Error("a refused create must not persist any state")
			}
		})
	}
}

func TestCreate_OtherBackendErrorSaysHowToRecover(t *testing.T) {
	client := &fakeReleaseClient{historyByDriver: map[string]*nelmclient.ReleaseHistory{
		"secret": {Revision: 2, Status: "deployed", Deployed: true},
	}}

	plan := plannedCreateModel()
	plan.ReleaseStorageDriver = types.StringValue("configmap")

	resp := runCreate(t, client, plan)
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected the other-backend guard to refuse")
	}

	for _, want := range []string{"create_before_destroy", "destroy-first", `release_storage_driver = "secret"`, "adopt_existing does not override"} {
		if !strings.Contains(resp.Diagnostics[0].Detail(), want) {
			t.Errorf("error detail does not mention %q:\n%s", want, resp.Diagnostics[0].Detail())
		}
	}
}

// TestCreate_OtherBackendForbiddenOnlyWarns: credentials whose RBAC covers
// only the configured backend must still be able to create releases.
func TestCreate_OtherBackendForbiddenOnlyWarns(t *testing.T) {
	forbidden := apierrors.NewForbidden(k8sschema.GroupResource{Resource: "configmaps"}, "", errors.New(`User "deployer" cannot list resource "configmaps"`))
	client := &fakeReleaseClient{
		historyErrs: map[string]error{"configmap": fmt.Errorf("build release history: %w", forbidden)},
		getInfo:     deployedInfo(1, "deployed"),
	}

	resp := runCreate(t, client, plannedCreateModel())

	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected errors: %v", resp.Diagnostics)
	}

	assertOneDiag(t, resp.Diagnostics, diag.SeverityWarning, "Could not check the configmap storage backend")

	if client.installs != 1 {
		t.Errorf("Install called %d times, want 1", client.installs)
	}
}

func TestCreate_OtherBackendReadFailureChangesNothing(t *testing.T) {
	client := &fakeReleaseClient{historyErrs: map[string]error{"configmap": errors.New("connection reset by peer")}}

	resp := runCreate(t, client, plannedCreateModel())

	assertOneDiag(t, resp.Diagnostics, diag.SeverityError, "release history")

	if client.installs != 0 {
		t.Errorf("Install called %d times, want 0", client.installs)
	}
}

// TestUpdate_ReadsOnlyItsOwnBackend: the other-backend check is Create-only
// (an Update never changes release_storage_driver, which forces replacement).
func TestUpdate_ReadsOnlyItsOwnBackend(t *testing.T) {
	client := &fakeReleaseClient{
		historyByDriver: map[string]*nelmclient.ReleaseHistory{"secret": {Revision: 4, Status: "deployed", Deployed: true}},
		getInfo:         deployedInfo(5, "deployed"),
	}

	plan := appliedModel(4, "deployed")
	plan.Status = types.StringUnknown()
	plan.Revision = types.Int64Unknown()
	plan.Metadata = types.ObjectUnknown(metadataAttrTypes)

	resp := runUpdate(t, client, plan, appliedModel(4, "deployed"))

	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected errors: %v", resp.Diagnostics)
	}

	if !slices.Equal(client.historyDrivers, []string{"secret"}) {
		t.Errorf("History read drivers %q, want only the configured secret backend", client.historyDrivers)
	}
}

func TestCreate_HistoryReadFailureChangesNothing(t *testing.T) {
	client := &fakeReleaseClient{historyErr: errors.New("secrets is forbidden")}

	resp := runCreate(t, client, plannedCreateModel())

	assertOneDiag(t, resp.Diagnostics, diag.SeverityError, "release history")

	if client.installs != 0 {
		t.Errorf("Install called %d times, want 0", client.installs)
	}
}

// TestCreate_InstallSucceededReadFailed is the F20 regression test: a
// successful install whose post-install read fails used to fail the apply,
// so Terraform tainted the healthy release and replaced (uninstalled and
// reinstalled) it on the next apply.
func TestCreate_InstallSucceededReadFailed(t *testing.T) {
	client := &fakeReleaseClient{getErr: errors.New("release get: connection reset by peer")}
	plan := plannedCreateModel()

	resp := runCreate(t, client, plan)

	if resp.Diagnostics.HasError() {
		t.Fatalf("a successful install must not fail the apply, got: %v", resp.Diagnostics)
	}

	assertOneDiag(t, resp.Diagnostics, diag.SeverityWarning, "reading it back failed")

	if !strings.Contains(resp.Diagnostics[0].Detail(), "connection reset by peer") {
		t.Errorf("warning does not carry the read error: %s", resp.Diagnostics[0].Detail())
	}

	got := stateModel(t, context.Background(), resp.State)
	if got.ID.ValueString() != "default/my-release" {
		t.Errorf("id = %q, want default/my-release", got.ID.ValueString())
	}

	if !got.Resources.Equal(plan.Resources) {
		t.Errorf("resources = %v, want the planned value %v", got.Resources, plan.Resources)
	}
}

// TestUpdate_InstallSucceededReadFailed_KeepsPlannedValues: when the update
// plan left status/revision/metadata KNOWN (no reinstall expected), the
// fallback state must reproduce them, or the now-successful apply would trip
// Terraform's "inconsistent result after apply" check.
func TestUpdate_InstallSucceededReadFailed_KeepsPlannedValues(t *testing.T) {
	client := &fakeReleaseClient{
		history: &nelmclient.ReleaseHistory{Revision: 4, Status: "deployed", Deployed: true},
		getErr:  errors.New("release get: i/o timeout"),
	}
	plan := appliedModel(4, "deployed")

	resp := runUpdate(t, client, plan, appliedModel(4, "deployed"))

	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected errors: %v", resp.Diagnostics)
	}

	got := stateModel(t, context.Background(), resp.State)
	if got.Status.ValueString() != "deployed" || got.Revision.ValueInt64() != 4 || !got.Metadata.Equal(plan.Metadata) {
		t.Errorf("status/revision/metadata = %s/%d/%v, want the planned deployed/4/%v", got.Status, got.Revision.ValueInt64(), got.Metadata, plan.Metadata)
	}
}

// TestUpdate_FailedInstallKeepsPriorConfig is the F21 regression test: a
// failed Update used to persist the NEW configuration, so a change the retry
// triggers cannot see (a hook-only change after a successful auto_rollback,
// here) was recorded as applied and never retried.
func TestUpdate_FailedInstallKeepsPriorConfig(t *testing.T) {
	client := &fakeReleaseClient{
		history:    &nelmclient.ReleaseHistory{Revision: 4, Status: "deployed", Deployed: true},
		installErr: errors.New("release install: pre-upgrade hook Job/migrate failed; rolled back"),
		// auto_rollback succeeded: a new, deployed revision with the old config.
		getInfo: deployedInfo(6, "deployed"),
		live:    nil,
	}

	prior := appliedModel(4, "deployed")
	prior.Version = types.StringValue("1.0.0")

	plan := appliedModel(4, "deployed")
	plan.Version = types.StringValue("2.0.0")
	plan.AutoRollback = types.BoolValue(true)
	plan.Status = types.StringUnknown()
	plan.Revision = types.Int64Unknown()
	plan.Metadata = types.ObjectUnknown(metadataAttrTypes)

	resp := runUpdate(t, client, plan, prior)

	assertOneDiag(t, resp.Diagnostics, diag.SeverityError, "install failed")

	got := stateModel(t, context.Background(), resp.State)

	if got.Version.ValueString() != "1.0.0" || got.AutoRollback.ValueBool() {
		t.Errorf("persisted config version=%q auto_rollback=%v, want the PRIOR 1.0.0/false so the change is retried",
			got.Version.ValueString(), got.AutoRollback.ValueBool())
	}

	if got.Status.ValueString() != "deployed" || got.Revision.ValueInt64() != 6 {
		t.Errorf("persisted status/revision = %s/%d, want the refreshed deployed/6", got.Status, got.Revision.ValueInt64())
	}
}

// TestUpdate_RefusedWhilePendingLockHeld is the G5.1 regression test: an
// update used to install straight over a release whose last revision is
// pending-* (nelm treats it as failed), racing — and then being clobbered
// by — e.g. an on-call `helm rollback` still waiting for its pods.
func TestUpdate_RefusedWhilePendingLockHeld(t *testing.T) {
	ctx := context.Background()
	client := &fakeReleaseClient{
		history: &nelmclient.ReleaseHistory{Revision: 5, Status: "pending-rollback", LastDeployed: time.Now().Add(-time.Minute), Deployed: true},
		getInfo: deployedInfo(6, "deployed"),
	}

	prior := appliedModel(5, "pending-rollback")
	plan := appliedModel(5, "pending-rollback")
	plan.Status = types.StringUnknown()
	plan.Revision = types.Int64Unknown()
	plan.Metadata = types.ObjectUnknown(metadataAttrTypes)

	resp := runUpdate(t, client, plan, prior)

	assertOneDiag(t, resp.Diagnostics, diag.SeverityError, "locked by another operation")

	if client.installs != 0 {
		t.Errorf("Install called %d times, want 0 while the lock is held", client.installs)
	}

	if !resp.State.Raw.Equal(buildState(t, ctx, prior).Raw) {
		t.Error("a refused update must leave the prior state untouched")
	}
}

func TestUpdate_TakesOverStalePendingRelease(t *testing.T) {
	client := &fakeReleaseClient{
		history: &nelmclient.ReleaseHistory{Revision: 5, Status: "pending-upgrade", LastDeployed: time.Now().Add(-3 * time.Hour), Deployed: true},
		getInfo: deployedInfo(6, "deployed"),
	}

	plan := appliedModel(5, "pending-upgrade")
	plan.Status = types.StringUnknown()
	plan.Revision = types.Int64Unknown()
	plan.Metadata = types.ObjectUnknown(metadataAttrTypes)

	resp := runUpdate(t, client, plan, appliedModel(5, "pending-upgrade"))

	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected errors: %v", resp.Diagnostics)
	}

	assertOneDiag(t, resp.Diagnostics, diag.SeverityWarning, "stale pending")

	if client.installs != 1 {
		t.Errorf("Install called %d times, want 1", client.installs)
	}
}

// TestImportState_SeedsAdoptExistingFalse: import seeds adopt_existing at its
// default, so importing never plans a change to it (an imported resource is
// only ever Updated; the adoption guard is Create-only).
func TestImportState_SeedsAdoptExistingFalse(t *testing.T) {
	ctx := context.Background()
	resp := &resource.ImportStateResponse{State: nullState(ctx)}

	(&releaseResource{}).ImportState(ctx, resource.ImportStateRequest{ID: "apps/web"}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected errors: %v", resp.Diagnostics)
	}

	var adopt types.Bool
	if diags := resp.State.GetAttribute(ctx, path.Root("adopt_existing"), &adopt); diags.HasError() {
		t.Fatalf("read adopt_existing: %v", diags)
	}

	if adopt.IsNull() || adopt.ValueBool() {
		t.Errorf("adopt_existing = %v, want false", adopt)
	}
}

// --- pure state helpers ------------------------------------------------------

func TestFailedUpdateState(t *testing.T) {
	prior := appliedModel(4, "deployed")
	prior.Version = types.StringValue("1.0.0")
	prior.ForceAdoption = types.BoolValue(true)

	refreshed := appliedModel(5, "failed")
	refreshed.Version = types.StringValue("2.0.0")
	refreshed.ForceAdoption = types.BoolValue(false)
	refreshed.Resources = types.MapValueMust(types.StringType, nil)

	got := failedUpdateState(prior, refreshed)

	if got.Version.ValueString() != "1.0.0" || !got.ForceAdoption.ValueBool() {
		t.Errorf("config attrs = version %q force_adoption %v, want the prior 1.0.0/true", got.Version.ValueString(), got.ForceAdoption.ValueBool())
	}

	if got.Status.ValueString() != "failed" || got.Revision.ValueInt64() != 5 || !got.Resources.Equal(refreshed.Resources) {
		t.Errorf("computed attrs = %s/%d/%v, want the refreshed failed/5/%v", got.Status, got.Revision.ValueInt64(), got.Resources, refreshed.Resources)
	}
}

func TestPlanFallbackState(t *testing.T) {
	create, diags := planFallbackState(plannedCreateModel())
	if diags.HasError() {
		t.Fatalf("unexpected errors: %v", diags)
	}

	if create.Status.IsUnknown() || create.Revision.IsUnknown() || create.Metadata.IsUnknown() {
		t.Error("unknown computed attrs must become concrete (state cannot store Unknown)")
	}

	planned := appliedModel(4, "deployed")

	update, diags := planFallbackState(planned)
	if diags.HasError() {
		t.Fatalf("unexpected errors: %v", diags)
	}

	if !update.Status.Equal(planned.Status) || !update.Revision.Equal(planned.Revision) || !update.Metadata.Equal(planned.Metadata) {
		t.Errorf("known planned status/revision/metadata must be kept, got %s/%s/%v", update.Status, update.Revision, update.Metadata)
	}

	unknownResources := plannedCreateModel()
	unknownResources.Resources = types.MapUnknown(types.StringType)

	degraded, _ := planFallbackState(unknownResources)
	if degraded.Resources.IsUnknown() || len(degraded.Resources.Elements()) != 0 {
		t.Errorf("an unknown planned resources value must become an empty map, got %v", degraded.Resources)
	}
}
