package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
)

// durationValidator validates that a string attribute, if known and
// non-empty, parses as a Go duration (time.ParseDuration). Used for
// kube_request_timeout, which has no dedicated validator in
// terraform-plugin-framework-validators v0.19.0.
type durationValidator struct{}

var (
	_ validator.String = durationValidator{}
)

func (v durationValidator) Description(_ context.Context) string {
	return "must be a valid Go duration string (e.g. \"30s\", \"5m\")"
}

func (v durationValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v durationValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}

	value := req.ConfigValue.ValueString()
	if value == "" {
		return
	}

	if _, err := time.ParseDuration(value); err != nil {
		resp.Diagnostics.AddAttributeError(
			req.Path,
			"Invalid Duration",
			fmt.Sprintf("%q is not a valid Go duration string: %s", value, err),
		)
	}
}
