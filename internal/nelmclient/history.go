package nelmclient

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/samber/lo"

	"github.com/werf/nelm/pkg/common"
	"github.com/werf/nelm/pkg/release"
)

// DeployTypeUpgrade is PlanResult.DeployType when nelm plans an upgrade of a
// release that already has a deployed revision (as opposed to "Initial", no
// history at all, or "Install", only failed/uninstalled history).
const DeployTypeUpgrade = string(common.DeployTypeUpgrade)

// DeployTypeInitial is PlanResult.DeployType when the release has no stored
// record at all in the configured backend: nelm plans its first install.
// Every other deploy type means a release of that name exists there.
const DeployTypeInitial = string(common.DeployTypeInitial)

// ReleaseHistory summarizes a release's stored revision history: what the
// provider's pre-install guards need and ReleaseGet does not expose (whether
// nelm would upgrade rather than install, and when the last record was
// written — ReleaseGet's DeployedAt is always zero).
type ReleaseHistory struct {
	// Revision and Status describe the LAST stored record, the one Helm's
	// pending-* lock lives on. Revision is 0 when the release has no records.
	Revision int
	Status   string

	// LastDeployed is when the last record was written. Helm and nelm both
	// stamp it on every create/update of a record, so for a pending-* record
	// it is when the in-flight operation started. Zero if the record carries
	// no timestamp.
	LastDeployed time.Time

	// Deployed reports whether a deployed (or superseded) revision exists
	// since the last uninstall: exactly nelm's condition for upgrading the
	// release (DeployTypeUpgrade) rather than installing it, i.e. the
	// release's objects are live and owned by it. A history of failed first
	// install attempts, or one ending in an uninstall, is not Deployed.
	Deployed bool
}

// Exists reports whether the release has any stored record at all.
func (h *ReleaseHistory) Exists() bool {
	return h != nil && h.Revision > 0
}

// IsPending reports whether the last record is pending-install,
// pending-upgrade or pending-rollback: an operation is in flight, or one was
// interrupted before it could record its outcome. Helm treats this as the
// release's lock; nelm itself does not (it classes pending-* as failed and
// installs over it).
func (h *ReleaseHistory) IsPending() bool {
	return h != nil && strings.HasPrefix(h.Status, "pending-")
}

// History reads the release's stored records straight from release storage
// (the same BuildHistory nelm's own install runs) and summarizes them. A
// release with no records is not an error: it yields a zero ReleaseHistory
// (Exists false).
//
// It reuses the cached kube.ClientFactory (ensureKubeFactory) rather than
// building a fresh one per call like the action package does.
func (c *Client) History(ctx context.Context, name, namespace, storageDriver string, timeout time.Duration) (*ReleaseHistory, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	factory, err := c.ensureKubeFactory(ctx)
	if err != nil {
		return nil, err
	}

	storage, err := release.NewReleaseStorage(ctx, namespace, storageDriver, factory, release.ReleaseStorageOptions{})
	if err != nil {
		return nil, fmt.Errorf("construct release storage: %w", err)
	}

	history, err := release.BuildHistory(name, storage, release.HistoryOptions{})
	if err != nil {
		return nil, fmt.Errorf("build release history: %w", err)
	}

	return summarizeHistory(history), nil
}

// summarizeHistory is History's pure half: h's releases are sorted by
// revision (release.NewHistory sorts them).
func summarizeHistory(h release.Historier) *ReleaseHistory {
	out := &ReleaseHistory{
		Deployed: len(h.FindAllDeployed()) > 0,
	}

	last := lo.LastOrEmpty(h.Releases())
	if last == nil {
		return out
	}

	out.Revision = last.Version

	if last.Info != nil {
		out.Status = string(last.Info.Status)
		out.LastDeployed = last.Info.LastDeployed.Time
	}

	return out
}
