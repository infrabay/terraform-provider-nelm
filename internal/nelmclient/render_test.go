package nelmclient

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestRender_DoesNotWriteToStdout is the regression test for nelm's
// ChartRender printing every rendered manifest — Secret data included — to
// os.Stdout: it ignores OutputNoPrint and prints unless OutputFilePath is
// set. Inside a plugin process os.Stdout is a pipe that Terraform core logs
// at WARN ("unexpected data: <provider>:stdout=..."), so with TF_LOG on, every
// plan wrote base64 Secret data (and any set_sensitive value a chart renders)
// into CI logs. Render runs on every non-destroy plan, so this is checked
// through the real Client.Render against a fake API server. It swaps the
// process-global os.Stdout, so it must not run in parallel.
func TestRender_DoesNotWriteToStdout(t *testing.T) {
	ctx := context.Background()

	if err := Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	c := NewClient(fakeKubeConfig(t))

	chart, err := filepath.Abs("../../testdata/charts/basic")
	if err != nil {
		t.Fatalf("abs chart path: %v", err)
	}

	const password = "render-stdout-probe-password"

	spec := ReleaseSpec{
		Name:      "stdout",
		Namespace: "default",
		Chart:     chart,
		Set:       []string{"secret.password=" + password},
	}

	var (
		objs      []*unstructured.Unstructured
		renderErr error
	)

	out := captureStdout(t, func() {
		objs, renderErr = c.Render(ctx, spec, 30*time.Second)
	})

	if renderErr != nil {
		t.Fatalf("Render: %v", renderErr)
	}

	if len(out) != 0 {
		t.Fatalf("Render wrote %d bytes to the process stdout (want 0):\n%s", len(out), out)
	}

	// The redirect must only move nelm's printout, not thin the result: the
	// Secret still comes back, rendered with the value passed in.
	want := base64.StdEncoding.EncodeToString([]byte(password))

	for _, obj := range objs {
		if obj.GetKind() != "Secret" {
			continue
		}

		got, _, _ := unstructured.NestedString(obj.Object, "data", "password")
		if got != want {
			t.Fatalf("rendered Secret data.password = %q, want %q", got, want)
		}

		return
	}

	t.Fatalf("Render returned no Secret among %d objects", len(objs))
}
