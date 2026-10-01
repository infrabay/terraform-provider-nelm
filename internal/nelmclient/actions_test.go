package nelmclient

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/werf/nelm/pkg/common"
	nelmlog "github.com/werf/nelm/pkg/log"
	"github.com/werf/nelm/pkg/plan"
	"github.com/werf/nelm/pkg/resource/spec"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestCaptureWarningsCtx_KeepsPlannedDiffsOutOfErrors is the regression test
// for Plan folding nelm's resource diffs into its error diagnostics:
// ReleasePlanInstall logs every planned change's unified diff at info level
// before it writes the plan artifact, and nelm diffs a Secret annotated
// werf.io/sensitive: "false" with its data. A plan that then failed (e.g.
// writing the artifact hit ENOSPC) carried that Secret data into the
// "nelm_release plan failed" diagnostic and CI logs. The diff is produced and
// logged exactly as nelm's logPlannedChanges does.
func TestCaptureWarningsCtx_KeepsPlannedDiffsOutOfErrors(t *testing.T) {
	ctx := context.Background()

	if err := Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	const (
		oldData = "T0xEUEFTUw==" // base64("OLDPASS")
		newData = "TkVXUEFTUw==" // base64("NEWPASS")
	)

	secret := func(data string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]interface{}{
				"name":        "db",
				"namespace":   "default",
				"annotations": map[string]interface{}{"werf.io/sensitive": "false"},
			},
			"data": map[string]interface{}{"password": data},
		}}
	}

	change := &plan.ResourceChange{
		Type:         "update",
		ResourceMeta: spec.NewResourceMetaFromUnstructured(secret(newData), "default", "templates/secret.yaml"),
		Before:       secret(oldData),
		After:        secret(newData),
	}

	uDiff, err := change.UDiff(common.ResourceDiffOptions{DiffContextLines: common.DefaultDiffContextLines})
	if err != nil {
		t.Fatalf("UDiff: %v", err)
	}

	if !strings.Contains(uDiff, newData) {
		t.Fatalf("precondition: nelm's diff of an opted-out Secret no longer shows its data, so this test proves nothing:\n%s", uDiff)
	}

	ctx, buf := captureWarningsCtx(ctx)

	if err := nelmlog.Default.InfoBlockErr(ctx, nelmlog.BlockOptions{BlockTitle: "Update Secret/db"}, func() error {
		nelmlog.Default.Info(ctx, "%s", uDiff)
		return nil
	}); err != nil {
		t.Fatalf("InfoBlockErr: %v", err)
	}

	nelmlog.Default.Warn(ctx, "Chart %q is deprecated", "app:1.0.0")

	got := tailErr(errors.New(`save install plan to "/tmp/plan.artifact": no space left on device`), buf).Error()

	for _, data := range []string{oldData, newData} {
		if strings.Contains(got, data) {
			t.Fatalf("Secret data %q from nelm's planned-change diff reached the error:\n%s", data, got)
		}
	}

	if !strings.Contains(got, `Chart "app:1.0.0" is deprecated`) {
		t.Fatalf("nelm's warnings must still be folded into the error:\n%s", got)
	}
}

// TestTailErr_FoldsABoundedTail checks tailErr folds only the end of a long
// nelm log (where nelm reports what failed), starting on a line boundary,
// and keeps the original error unwrappable.
func TestTailErr_FoldsABoundedTail(t *testing.T) {
	buf := &syncBuffer{}

	for i := 0; i < 5000; i++ {
		_, _ = fmt.Fprintf(buf, "nelm log line %05d\n", i)
	}

	base := errors.New("release install: boom")

	err := tailErr(base, buf)
	if !errors.Is(err, base) {
		t.Fatalf("tailErr must wrap the original error, got: %v", err)
	}

	got := err.Error()

	if len(got) > maxErrTail+256 {
		t.Fatalf("folded %d bytes, want at most about %d", len(got), maxErrTail)
	}

	if !strings.Contains(got, "nelm log line 04999\n") {
		t.Fatalf("the end of the log must be kept:\n%s", got)
	}

	if strings.Contains(got, "nelm log line 00000") {
		t.Fatal("the start of a long log must be dropped")
	}

	_, rest, found := strings.Cut(got, "earlier bytes omitted ...]\n")
	if !found {
		t.Fatalf("expected an omission marker:\n%.300s", got)
	}

	if !strings.HasPrefix(rest, "nelm log line ") {
		t.Fatalf("tail must start on a line boundary, starts with %.40q", rest)
	}

	short := &syncBuffer{}
	_, _ = short.Write([]byte("only line\n"))

	if got := tailErr(base, short).Error(); !strings.HasSuffix(got, "--- nelm output ---\nonly line\n") {
		t.Fatalf("a short log must be folded whole, got: %q", got)
	}
}
