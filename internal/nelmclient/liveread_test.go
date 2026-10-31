package nelmclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestEnsureKubeFactory_TransientErrorNotMemoized is the regression test for
// the factory-error-poisoning bug: a single transient failure of the
// connectivity check used to be cached in c.kubeFactoryErr and returned by
// every later ensureKubeFactory call (from any resource's LiveObjects /
// IsNamespaced) for the whole process, even after the cluster recovered. The
// fix caches only a SUCCESSFUL factory, so a failed call never short-circuits
// the next one.
//
// The test is fully offline: call 1 fails deterministically while building the
// KubeConfig (garbage base64), then the config is corrected to a syntactically
// valid kubeconfig pointing at an unreachable local port. With the pre-fix
// memoization, call 2 would return the cached call-1 "construct kube config"
// error verbatim; the fix makes it re-attempt and reach — and fail at — the
// later factory connectivity stage instead ("construct kube client factory").
func TestEnsureKubeFactory_TransientErrorNotMemoized(t *testing.T) {
	ctx := context.Background()

	c := NewClient(Config{KubeConfigBase64: "!!! not valid base64 !!!"})

	if _, err := c.ensureKubeFactory(ctx); err == nil {
		t.Fatal("call 1: expected an error for garbage kube_config_base64")
	}

	// Correct the configuration. A pre-fix Client would ignore this and keep
	// returning the memoized call-1 error forever.
	c.cfg.KubeConfigBase64 = ""
	c.cfg.KubeConfigPaths = []string{writeUnreachableKubeconfig(t)}

	_, err := c.ensureKubeFactory(ctx)
	if err == nil {
		t.Fatal("call 2: expected a connectivity error against the unreachable server")
	}
	if strings.Contains(err.Error(), "construct kube config") {
		t.Fatalf("call 2 returned a memoized construct-kube-config error — the transient failure was cached instead of retried: %v", err)
	}
	if c.kubeFactory != nil {
		t.Fatal("kubeFactory must stay nil after a failed construction so the next call retries")
	}
}

// TestEnsureKubeFactory_ConstructionIsBounded is the regression test for a
// wedged factory: the connectivity check inside factory construction ignores
// the caller context entirely (client-go discovery has no ctx), so a
// BLACKHOLED endpoint (accepts nothing, SYN just hangs — RFC 5737 TEST-NET
// address) used to block ensureKubeFactory forever while holding kubeMu,
// freezing every resource in the process. The watchdog must cut it at
// Config.RequestTimeout.
func TestEnsureKubeFactory_ConstructionIsBounded(t *testing.T) {
	const cfgYAML = `apiVersion: v1
kind: Config
clusters:
- name: blackhole
  cluster:
    server: https://203.0.113.1:6443
contexts:
- name: blackhole
  context:
    cluster: blackhole
    user: blackhole
current-context: blackhole
users:
- name: blackhole
  user: {}
`

	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}

	c := NewClient(Config{
		KubeConfigPaths: []string{path},
		RequestTimeout:  2 * time.Second,
	})

	start := time.Now()
	_, err := c.ensureKubeFactory(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error against a blackholed endpoint")
	}
	if elapsed > 20*time.Second {
		t.Fatalf("factory construction was not bounded: took %s (watchdog should fire at ~2s)", elapsed)
	}
	if c.kubeFactory != nil {
		t.Fatal("nothing must be cached from a timed-out construction attempt")
	}
}

// writeUnreachableKubeconfig writes a syntactically valid kubeconfig whose
// server is an unreachable local port (connection refused, no network needed):
// kube.NewKubeConfig parses it successfully but kube.NewClientFactory's
// ServerVersion connectivity check fails fast.
func writeUnreachableKubeconfig(t *testing.T) string {
	t.Helper()

	const cfg = `apiVersion: v1
kind: Config
clusters:
- name: unreachable
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: unreachable
  context:
    cluster: unreachable
    user: unreachable
current-context: unreachable
users:
- name: unreachable
  user: {}
`

	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write temp kubeconfig: %v", err)
	}

	return path
}

// TestLiveObjectsAndIsNamespaced_RequiresCluster exercises LiveObjects and
// IsNamespaced against a real cluster. Both require nelm's
// kube.NewClientFactory, which dials the API server to check connectivity
// (the TestLiveObjects_* tests below answer it with fakeKubeConfigWith's
// fake API server; this one checks a real one), so this test only
// runs when NELM_TEST_KUBE_CONTEXT is set (the same guard GNUmakefile's
// testacc target uses); ./gates.sh pkg (no such env set) always skips it,
// keeping the package's default unit-test run cluster-free.
func TestLiveObjectsAndIsNamespaced_RequiresCluster(t *testing.T) {
	kubeCtx := os.Getenv("NELM_TEST_KUBE_CONTEXT")
	if kubeCtx == "" {
		t.Skip("NELM_TEST_KUBE_CONTEXT not set; skipping cluster-dependent liveread test")
	}

	ctx := context.Background()

	if err := Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	c := NewClient(Config{
		KubeContext:    kubeCtx,
		RequestTimeout: 10 * time.Second,
	})

	namespaced, err := c.IsNamespaced(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	if err != nil {
		t.Fatalf("IsNamespaced(ConfigMap): %v", err)
	}
	if !namespaced {
		t.Fatal("expected core/v1 ConfigMap to be namespaced")
	}

	clusterScoped, err := c.IsNamespaced(schema.GroupVersionKind{
		Group:   "rbac.authorization.k8s.io",
		Version: "v1",
		Kind:    "ClusterRole",
	})
	if err != nil {
		t.Fatalf("IsNamespaced(ClusterRole): %v", err)
	}
	if clusterScoped {
		t.Fatal("expected rbac.authorization.k8s.io/v1 ClusterRole to be cluster-scoped")
	}

	objs, err := c.LiveObjects(ctx, []ResourceRef{
		{Version: "v1", Kind: "ConfigMap", Namespace: "default", Name: "this-configmap-should-not-exist-anywhere"},
	})
	if err != nil {
		t.Fatalf("LiveObjects: %v", err)
	}
	if len(objs) != 0 {
		t.Fatalf("LiveObjects returned %d entries for a nonexistent object, want 0 (NotFound omitted)", len(objs))
	}
}

// liveTestDeploy is the object the TestLiveObjects_* tests read, served at
// liveTestDeployPath.
var liveTestDeploy = ResourceRef{Group: "apps", Version: "v1", Kind: "Deployment", Namespace: "infra", Name: "updater"}

const liveTestDeployPath = "/apis/apps/v1/namespaces/infra/deployments/updater"

// failAllGets makes fakeKubeFailingGets fail every GET of liveTestDeploy.
const failAllGets = math.MaxInt

// writeAuthorizerErr answers r as kube-apiserver answers a request whose
// authorizer returned an error (filters.WithAuthorization ->
// responsewriters.InternalError): a plain-text 500. GKE answers so when its
// IAM authorizer is briefly unavailable.
func writeAuthorizerErr(w http.ResponseWriter, r *http.Request) {
	http.Error(w, fmt.Sprintf("Internal Server Error: %q: the server is currently unable to handle the request", r.RequestURI),
		http.StatusInternalServerError)
}

// statusWriter returns a responder that answers with a metav1.Status error
// (writeStatus).
func statusWriter(code int, reason metav1.StatusReason, msg string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeStatus(w, code, reason, msg)
	}
}

// liveGetLog records when a fakeKubeFailingGets server received each GET of
// liveTestDeploy.
type liveGetLog struct {
	mu sync.Mutex
	at []time.Time
}

func (l *liveGetLog) record() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.at = append(l.at, time.Now())

	return len(l.at)
}

func (l *liveGetLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.at)
}

func (l *liveGetLog) times() []time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]time.Time(nil), l.at...)
}

// fakeKubeFailingGets returns a Config for a fake API server that answers
// the first failures GETs of liveTestDeploy with fail and later ones with
// the object (failAllGets: never), and the log of those GETs.
func fakeKubeFailingGets(t *testing.T, failures int, fail func(http.ResponseWriter, *http.Request)) (Config, *liveGetLog) {
	t.Helper()

	gets := &liveGetLog{}

	cfg := fakeKubeConfigWith(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet || r.URL.Path != liveTestDeployPath {
			return false
		}

		if gets.record() <= failures {
			fail(w, r)
			return true
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"updater","namespace":"infra"}}`)

		return true
	})

	return cfg, gets
}

// setLiveGetRetryDelay sets liveGetRetryDelay for the test.
func setLiveGetRetryDelay(t *testing.T, d time.Duration) {
	t.Helper()

	orig := liveGetRetryDelay
	liveGetRetryDelay = d

	t.Cleanup(func() { liveGetRetryDelay = orig })
}

// TestLiveObjects_RetriesTransientServerErrors is the regression test for a
// refresh that failed with "Failed to read live resources" on a single 500
// from GKE's authorizer ("the server is currently unable to handle the
// request"): LiveObjects must retry a GET the API server failed transiently
// and return the object once a retry succeeds.
func TestLiveObjects_RetriesTransientServerErrors(t *testing.T) {
	setLiveGetRetryDelay(t, time.Millisecond)

	for _, tc := range []struct {
		name string
		fail func(http.ResponseWriter, *http.Request)
	}{
		{"authorizer error 500", writeAuthorizerErr},
		{"503 ServiceUnavailable", statusWriter(http.StatusServiceUnavailable, metav1.StatusReasonServiceUnavailable,
			"the server is currently unable to handle the request")},
		{"504 Timeout", statusWriter(http.StatusGatewayTimeout, metav1.StatusReasonTimeout,
			"the server was unable to return a response in the time allotted, but may still be processing the request")},
		{"500 ServerTimeout", statusWriter(http.StatusInternalServerError, metav1.StatusReasonServerTimeout,
			"The get operation against deployments could not be completed at this time, please try again.")},
		{"429 TooManyRequests", statusWriter(http.StatusTooManyRequests, metav1.StatusReasonTooManyRequests,
			"the server has received too many requests and has asked us to try again later")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, gets := fakeKubeFailingGets(t, liveGetAttempts-1, tc.fail)

			objs, err := NewClient(cfg).LiveObjects(context.Background(), []ResourceRef{liveTestDeploy})
			if err != nil {
				t.Fatalf("LiveObjects: %v", err)
			}
			if objs[liveTestDeploy] == nil {
				t.Fatal("LiveObjects did not return the object a retry read")
			}
			if got := gets.count(); got != liveGetAttempts {
				t.Fatalf("GETs sent: %d, want %d", got, liveGetAttempts)
			}
		})
	}
}

// TestLiveObjects_RetryWaitsDouble checks the retry schedule the docs
// describe (liveGetRetryDelay, then twice as long each time) by its lower
// bounds only, which a timer firing late cannot break.
func TestLiveObjects_RetryWaitsDouble(t *testing.T) {
	const delay = 20 * time.Millisecond

	setLiveGetRetryDelay(t, delay)

	cfg, gets := fakeKubeFailingGets(t, liveGetAttempts-1, writeAuthorizerErr)

	if _, err := NewClient(cfg).LiveObjects(context.Background(), []ResourceRef{liveTestDeploy}); err != nil {
		t.Fatalf("LiveObjects: %v", err)
	}

	at := gets.times()
	if len(at) != liveGetAttempts {
		t.Fatalf("GETs sent: %d, want %d", len(at), liveGetAttempts)
	}

	want := delay
	for i := 1; i < len(at); i++ {
		if gap := at[i].Sub(at[i-1]); gap < want {
			t.Fatalf("wait before GET %d: %s, want at least %s", i+1, gap, want)
		}

		want *= 2
	}
}

// TestLiveObjects_TransientServerErrorRetriesRunOut checks that a GET that
// keeps failing transiently fails LiveObjects after liveGetAttempts GETs,
// with the API server's error and the number of attempts.
func TestLiveObjects_TransientServerErrorRetriesRunOut(t *testing.T) {
	setLiveGetRetryDelay(t, time.Millisecond)

	cfg, gets := fakeKubeFailingGets(t, failAllGets, writeAuthorizerErr)

	_, err := NewClient(cfg).LiveObjects(context.Background(), []ResourceRef{liveTestDeploy})
	if err == nil {
		t.Fatal("LiveObjects succeeded against a server that fails every GET")
	}
	for _, want := range []string{
		fmt.Sprintf("after %d attempts", liveGetAttempts),
		"the server is currently unable to handle the request",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
	if !apierrors.IsInternalError(err) {
		t.Fatalf("error %v no longer wraps the API server's InternalError", err)
	}
	if got := gets.count(); got != liveGetAttempts {
		t.Fatalf("GETs sent: %d, want %d", got, liveGetAttempts)
	}
}

// TestLiveObjects_DoesNotRetryOtherErrors checks that an error a retry
// cannot fix is not retried: a Forbidden fails LiveObjects and a NotFound
// leaves the object out, each after a single GET.
func TestLiveObjects_DoesNotRetryOtherErrors(t *testing.T) {
	setLiveGetRetryDelay(t, time.Millisecond)

	for _, tc := range []struct {
		name    string
		fail    func(http.ResponseWriter, *http.Request)
		wantErr bool
	}{
		{"403 Forbidden", statusWriter(http.StatusForbidden, metav1.StatusReasonForbidden,
			`deployments.apps "updater" is forbidden: User "ci" cannot get resource "deployments" in API group "apps" in the namespace "infra"`), true},
		{"404 NotFound", statusWriter(http.StatusNotFound, metav1.StatusReasonNotFound,
			`deployments.apps "updater" not found`), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, gets := fakeKubeFailingGets(t, failAllGets, tc.fail)

			objs, err := NewClient(cfg).LiveObjects(context.Background(), []ResourceRef{liveTestDeploy})
			if (err != nil) != tc.wantErr {
				t.Fatalf("LiveObjects error = %v, want error: %t", err, tc.wantErr)
			}
			if objs[liveTestDeploy] != nil {
				t.Fatal("LiveObjects returned an object the server never served")
			}
			if got := gets.count(); got != 1 {
				t.Fatalf("GETs sent: %d, want 1", got)
			}
		})
	}
}

// TestLiveObjects_RetryStopsAtContextDeadline checks that a retry wait
// never outlasts ctx (Read bounds it with timeouts.read): LiveObjects
// returns the API server's error, saying that ctx stopped the retries, once
// ctx is done instead of sleeping on.
func TestLiveObjects_RetryStopsAtContextDeadline(t *testing.T) {
	setLiveGetRetryDelay(t, time.Minute)

	cfg, gets := fakeKubeFailingGets(t, failAllGets, writeAuthorizerErr)
	c := NewClient(cfg)

	// Build the factory and run the RESTMapper's lazy discovery first, so
	// that the deadline below bounds only the GET and its retry wait.
	if _, err := c.IsNamespaced(schema.GroupVersionKind{Group: liveTestDeploy.Group, Version: liveTestDeploy.Version, Kind: liveTestDeploy.Kind}); err != nil {
		t.Fatalf("IsNamespaced: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	start := time.Now()
	_, err := c.LiveObjects(ctx, []ResourceRef{liveTestDeploy})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("LiveObjects succeeded against a server that fails every GET")
	}
	for _, want := range []string{
		fmt.Sprintf("after 1 of %d attempts, stopped by %v", liveGetAttempts, context.DeadlineExceeded),
		"the server is currently unable to handle the request",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
	if !apierrors.IsInternalError(err) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %v does not wrap both the API server's InternalError and the deadline", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("LiveObjects took %s, want it to stop at the 1s deadline", elapsed)
	}
	if got := gets.count(); got != 1 {
		t.Fatalf("GETs sent: %d, want 1", got)
	}
}

// TestResetKindCache_DoesNotHoldKubeMu is the regression test for
// ResetKindCache holding kubeMu while the RESTMapper's Reset waits for the
// mapper's init lock, which a concurrent lookup holds for a whole discovery
// round-trip: every other caller of ensureKubeFactory (History,
// LiveObjects, IsNamespaced of nelm_release) then stalled behind that
// discovery. A lookup here is held in discovery while ResetKindCache waits,
// and ensureKubeFactory must still return the cached factory at once.
func TestResetKindCache_DoesNotHoldKubeMu(t *testing.T) {
	ctx := context.Background()

	var (
		blockOnce sync.Once
		blocking  = make(chan struct{})
		entered   = make(chan struct{})
		release   = make(chan struct{})
	)

	c := NewClient(fakeKubeConfigWith(t, func(w http.ResponseWriter, r *http.Request) bool {
		select {
		case <-blocking:
		default:
			return false
		}

		if r.URL.Path == "/api" || r.URL.Path == "/apis" {
			blockOnce.Do(func() { close(entered) })
			<-release
		}

		return false
	}))

	if _, err := c.ensureKubeFactory(ctx); err != nil {
		t.Fatalf("ensureKubeFactory: %v", err)
	}

	// Drop whatever discovery the factory construction left, so that the
	// lookup below has to discover (and holds the mapper's init lock).
	c.ResetKindCache()
	close(blocking)

	lookupDone := make(chan struct{})
	go func() {
		defer close(lookupDone)
		_, _ = c.IsNamespaced(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	}()

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("the lookup never reached discovery")
	}

	resetDone := make(chan struct{})
	go func() {
		defer close(resetDone)
		c.ResetKindCache()
	}()

	// Give ResetKindCache time to start waiting for the mapper's lock.
	time.Sleep(100 * time.Millisecond)

	factoryDone := make(chan error, 1)
	go func() {
		_, err := c.ensureKubeFactory(ctx)
		factoryDone <- err
	}()

	blocked := false

	select {
	case err := <-factoryDone:
		if err != nil {
			t.Errorf("ensureKubeFactory: %v", err)
		}
	case <-time.After(2 * time.Second):
		blocked = true
		t.Error("ensureKubeFactory blocked while ResetKindCache waited for a discovery: kubeMu is held across the mapper Reset")
	}

	close(release)
	<-lookupDone
	<-resetDone

	if blocked {
		if err := <-factoryDone; err != nil {
			t.Errorf("ensureKubeFactory: %v", err)
		}
	}
}
