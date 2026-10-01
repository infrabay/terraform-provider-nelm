package provider

import (
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/infrabay/terraform-provider-nelm/internal/nelmclient"
)

// minPendingTakeoverAge is the youngest a pending-* release revision may be
// before createOrUpdate treats it as left behind by an interrupted operation
// rather than as a lock held by one that is still running. Helm's default
// operation timeout is 5m; this leaves room for long --wait/--timeout runs.
const minPendingTakeoverAge = 15 * time.Minute

// pendingTakeoverAge is how old a pending-* revision must be before it is
// taken over: the operation's own timeout (a sibling run of the same
// configuration may legitimately hold the lock that long), but never less
// than minPendingTakeoverAge.
func pendingTakeoverAge(opTimeout time.Duration) time.Duration {
	return max(opTimeout, minPendingTakeoverAge)
}

// installGuardDiags is the pre-Install check createOrUpdate runs against the
// release's freshly read history h (nil or !h.Exists(): no release). It
// returns an error diagnostic when Install must not run and a warning when it
// may, but only by taking over a stale pending revision:
//
//   - A pending-* last revision younger than takeoverAfter is a LOCK: Helm
//     stores an in-flight install/upgrade/rollback as a pending-* record and
//     refuses to start another operation over it, but nelm classes pending-*
//     as failed and installs over it — so without this check an apply racing
//     an on-call `helm rollback` would re-deploy the version being rolled
//     back. Refused for Create and Update alike. An older one is taken over
//     (the operation that wrote it is gone), which keeps a killed apply's own
//     leftover recoverable.
//   - On Create (isCreate) without adopt_existing, an EXISTING release is
//     refused: one with a deployed revision (nelm would upgrade it) or a
//     pending-* last revision. Create's install would otherwise silently take
//     the release over — a forgotten import, a duplicate resource — and under
//     create_before_destroy the deposed object's destroy then uninstalls the
//     release Create just adopted. A history of only failed or uninstalled
//     revisions (a failed first install) is installed over, as nelm does, so
//     recovering from it keeps working.
//
// Import is unaffected: an imported resource is only ever Updated.
func installGuardDiags(plan releaseModel, h *nelmclient.ReleaseHistory, isCreate bool, takeoverAfter time.Duration, now time.Time) diag.Diagnostics {
	var diags diag.Diagnostics

	if !h.Exists() {
		return diags
	}

	ns := plan.Namespace.ValueString()
	name := plan.Name.ValueString()

	// A record without a timestamp cannot be aged; treat it as stale rather
	// than as a lock nothing could ever release.
	age := now.Sub(h.LastDeployed)
	stale := h.LastDeployed.IsZero() || age >= takeoverAfter

	if h.IsPending() && !stale {
		diags.AddError(
			fmt.Sprintf("nelm release %s/%s is locked by another operation", ns, name),
			fmt.Sprintf("Revision %d is %q and was written %s ago: an install, upgrade or rollback (helm, nelm, or "+
				"another Terraform run) is still in progress. Helm treats a pending revision as the release's lock "+
				"and so does this provider; nothing was changed.\n\n"+
				"Retry once that operation has finished. If nothing is running any more (it was killed and left the "+
				"revision behind), an apply takes the revision over once it is older than %s. To recover sooner, "+
				"roll back to the last good revision (helm rollback %s <revision> -n %s) or delete the stuck "+
				"revision's release record (the Secret, or ConfigMap, sh.helm.release.v1.%s.v%d in namespace %s).",
				h.Revision, h.Status, age.Round(time.Second), takeoverAfter, name, ns, name, h.Revision, ns),
		)

		return diags
	}

	if isCreate && !plan.AdoptExisting.ValueBool() && (h.Deployed || h.IsPending()) {
		diags.AddError(
			fmt.Sprintf("nelm release %s/%s already exists", ns, name),
			fmt.Sprintf("A release named %q already exists in namespace %q (revision %d, status %q), and this resource "+
				"is being created rather than updated. Creating it would silently take that release over — and when "+
				"the create is half of a create_before_destroy replacement, the destroy that follows would then "+
				"uninstall it. Nothing was changed.\n\n"+
				"To manage the existing release with Terraform, import it:\n\n"+
				"  terraform import <address> %s/%s\n\n"+
				"(or an import block with id = %q), or set adopt_existing = true to let this create take it over. "+
				"If this is a create_before_destroy replacement (\"+/-\" in the plan), remove create_before_destroy, "+
				"including where it is inherited from a dependent resource: nelm_release cannot be replaced "+
				"create-before-destroy under the same name.",
				name, ns, h.Revision, h.Status, ns, name, ns+"/"+name),
		)

		return diags
	}

	if h.IsPending() {
		diags.AddWarning(
			fmt.Sprintf("Taking over a stale pending nelm release %s/%s", ns, name),
			fmt.Sprintf("Revision %d is %q and is older than %s, so the operation that wrote it is assumed to be "+
				"gone (killed or crashed); installing over it.",
				h.Revision, h.Status, takeoverAfter),
		)
	}

	return diags
}
