package nelmclient

import (
	"errors"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/werf/nelm/pkg/action"
)

// IsReleaseNotFound reports whether err is (or wraps) nelm's
// action.ReleaseNotFoundError, meaning the requested release does not exist
// in the configured storage driver/namespace.
func IsReleaseNotFound(err error) bool {
	var notFound *action.ReleaseNotFoundError
	return errors.As(err, &notFound)
}

// IsForbidden reports whether err is (or wraps) a Kubernetes API 403: the
// credentials are not allowed to make the request (e.g. a History read of a
// storage backend the deployer's RBAC does not cover).
func IsForbidden(err error) bool {
	return apierrors.IsForbidden(err)
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
// silent "plan at apply time" warning, ModifyPlan step 5a) is worse than being
// wrong in the other direction (surfacing a real cluster-unreachable condition
// as a hard error).
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

// missingKindPrefix starts apimachinery's missing-kind decode error
// (runtime.NewMissingKindErr), which quotes the whole manifest it could not
// decode: "Object 'Kind' is missing in '<manifest as JSON>'".
const missingKindPrefix = "Object 'Kind' is missing in '"

// missingKindWithheld replaces that error's text in a redacted message.
const missingKindWithheld = "Object 'Kind' is missing in the rendered manifest (content withheld: it may contain Secret data)"

// yamlSourceError is goccy/go-yaml's pretty error (SyntaxError, TypeError,
// DuplicateKeyError, ...): Error() is FormatError(false, true), the message
// with its position followed by the source lines around the bad token.
type yamlSourceError interface {
	FormatError(colored, inclSource bool) string
}

// redactManifestErr withholds the rendered manifest content a nelm render
// error can quote, before the provider puts it in a diagnostic: a chart
// renders Secret data like any other value, and a diagnostic is plan and CI
// output that nelm_release's own resources attribute keeps Secret data out
// of. Two errors on nelm's render path (pkg/chart/chart_render.go) quote
// it: the decode of an object without a kind
// (spec.NewResourceSpecFromManifest, for a template and a crds/ file alike),
// which quotes the whole manifest, and goccy/go-yaml's parse of a rendered
// template (renderedTemplatesToResourceSpecs), which prints up to three
// source lines before and after the bad token. The first loses its quote,
// the second its source lines; every wrapper's text (the template path, the
// position, what is wrong) stays. The result carries the redacted message
// and unwraps to err, so errors.Is and errors.As classify it exactly as err
// (a timeout, a missing release, a lock conflict joined to the render
// error); err itself is returned when there is nothing to redact. Only the
// message is redacted: nothing may print an error unwrapped from the result.
func redactManifestErr(err error) error {
	if err == nil {
		return nil
	}

	msg := err.Error()
	redacted := msg

	// runtime.IsMissingKind matches the error itself, not a chain, so the
	// walk visits every error of it.
	var walk func(error)
	walk = func(e error) {
		if runtime.IsMissingKind(e) {
			redacted = strings.ReplaceAll(redacted, e.Error(), missingKindWithheld)
			return
		}

		if y, ok := e.(yamlSourceError); ok {
			redacted = strings.ReplaceAll(redacted, e.Error(), y.FormatError(false, false))
			return
		}

		switch u := e.(type) {
		case interface{ Unwrap() error }:
			if next := u.Unwrap(); next != nil {
				walk(next)
			}
		case interface{ Unwrap() []error }:
			for _, next := range u.Unwrap() {
				walk(next)
			}
		}
	}
	walk(err)

	// A wrapper that formatted the decode error with %v rather than %w hides
	// it from the walk: its quote runs to the end of the message.
	if i := strings.Index(redacted, missingKindPrefix); i >= 0 {
		redacted = redacted[:i] + missingKindWithheld
	}

	if redacted == msg {
		return err
	}

	return &redactedError{msg: redacted, err: err}
}

// redactedError is redactManifestErr's result: Error() is the redacted
// message, and the error it redacts stays reachable for errors.Is and
// errors.As only.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }

func (e *redactedError) Unwrap() error { return e.err }

// liveConflictSignals are the phrases of nelm plan failures that are relative
// to the objects CURRENTLY LIVE in the cluster rather than to the chart or
// values: the adoption check (pkg/plan/validate.go, "validate adoptable
// resources: adopt ...": a live object carries another release's ownership
// annotations) and an immutable-field change nelm will not recreate for
// (pkg/plan/resource_info.go, "immutable fields change in resource ..., but
// recreation is not requested"). Both are re-checked by Install itself.
var liveConflictSignals = []string{
	"validate adoptable resources",
	"immutable fields change in resource",
}

// IsLiveConflict reports whether err is a nelm plan failure caused by a
// conflict with objects that are live right now (see liveConflictSignals).
// On the plan of a replacement those objects belong to the release being
// replaced, whose destroy removes them before the create runs, so the
// conflict says nothing about whether the create itself will succeed.
func IsLiveConflict(err error) bool {
	if err == nil {
		return false
	}

	msg := err.Error()
	for _, signal := range liveConflictSignals {
		if strings.Contains(msg, signal) {
			return true
		}
	}

	return false
}
