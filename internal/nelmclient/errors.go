package nelmclient

import (
	"errors"
	"strings"

	"github.com/werf/nelm/pkg/action"
)

// IsReleaseNotFound reports whether err is (or wraps) nelm's
// action.ReleaseNotFoundError, meaning the requested release does not exist
// in the configured storage driver/namespace.
func IsReleaseNotFound(err error) bool {
	var notFound *action.ReleaseNotFoundError
	return errors.As(err, &notFound)
}

// clusterUnreachableSignal is the connectivity-check phrase nelm's
// kube.NewClientFactory emits when its initial ServerVersion() call fails
// (pkg/kube/factory.go: "construct kube client factory: check kubernetes
// cluster version to check kubernetes connectivity: <underlying dial
// error>"). Captured verbatim in
// testdata/errors/unreachable_cluster.txt.
const clusterUnreachableSignal = "check kubernetes cluster version to check kubernetes connectivity"

// clusterUnreachableCauses are low-level dial-failure substrings that must
// co-occur with clusterUnreachableSignal for a match. Requiring both is
// deliberate: some other failure could in principle happen to embed the
// connectivity-check phrase without actually being a network-reachability
// problem, and being wrong in that direction (degrading a real error to a
// silent "plan at apply time" warning, design §2.2 step 5a) is worse than
// being wrong in the other direction (surfacing a real cluster-unreachable
// condition as a hard error).
var clusterUnreachableCauses = []string{
	"dial tcp",
	"connection refused",
	"i/o timeout",
	"no route to host",
	"network is unreachable",
	"no such host",
	// Timeout/reset shapes seen when an endpoint accepts a connection but
	// then stops responding (a blackholed API server, an expiring
	// kube_request_timeout, a mid-handshake stall). Still gated on the
	// connectivity-check phrase, so this stays a narrow match.
	"context deadline exceeded",
	"tls: handshake timeout",
	"connection reset by peer",
	"unexpected eof",
}

// IsClusterUnreachable narrowly matches nelm's kube-connectivity-check
// failure shape (testdata/errors/unreachable_cluster.txt). It is
// deliberately conservative: on any doubt it returns false so a real error
// (bad chart ref, bad values, remote chart without the featgate, ...) is
// never silently misclassified as "cluster unreachable, degrade to
// Unknown". In particular, a remote chart reference used without
// featgate.FeatGateRemoteCharts enabled is INDISTINGUISHABLE by error text
// from a bad local path (both fail with a "load chart: ... stat ...: no
// such file or directory" error, see testdata/errors/bad_chart_ref.txt and
// testdata/errors/remote_chart_no_featgate.txt) — this function does not
// attempt to classify that case at all, by design.
func IsClusterUnreachable(err error) bool {
	if err == nil {
		return false
	}

	msg := err.Error()
	if !strings.Contains(msg, clusterUnreachableSignal) {
		return false
	}

	for _, cause := range clusterUnreachableCauses {
		if strings.Contains(msg, cause) {
			return true
		}
	}

	return false
}
