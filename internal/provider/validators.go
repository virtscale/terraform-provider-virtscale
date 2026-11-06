package provider

import (
	"context"
	"fmt"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
)

// regexValidator refuses, at plan time, a string the API would not take, with the rule in the API's words. Only a
// known, non-null value is checked.
type regexValidator struct {
	re      *regexp.Regexp
	summary string
	rule    string
}

func (v regexValidator) Description(context.Context) string { return v.rule }

func (v regexValidator) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (v regexValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if value := req.ConfigValue.ValueString(); !v.re.MatchString(value) {
		resp.Diagnostics.AddAttributeError(req.Path, v.summary, fmt.Sprintf("%q is not valid: %s", value, v.rule))
	}
}

// dnsLabel is the name rule of projects and VPCs (and instances): 1 to 63 lower-case letters, digits and hyphens, not
// starting or ending with a hyphen.
func dnsLabel(what string) regexValidator {
	return regexValidator{
		re:      regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`),
		summary: "Invalid " + what + " name",
		rule:    "use 1 to 63 lower-case letters, digits or hyphens, not starting or ending with a hyphen",
	}
}

// maxLengthValidator refuses a string longer than max characters.
type maxLengthValidator struct{ max int }

func (v maxLengthValidator) Description(context.Context) string {
	return fmt.Sprintf("at most %d characters", v.max)
}

func (v maxLengthValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v maxLengthValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if n := len([]rune(req.ConfigValue.ValueString())); n > v.max {
		resp.Diagnostics.AddAttributeError(req.Path, "Value too long", fmt.Sprintf("this is %d characters; the API takes at most %d", n, v.max))
	}
}

// labelsValidator is the API's rule for project labels: at most 64 pairs, each key 1 to 63 characters from letters,
// digits and _ . : / - starting with a letter or digit, each value at most 255 characters.
type labelsValidator struct{}

var labelKeyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,62}$`)

func (labelsValidator) Description(context.Context) string {
	return "at most 64 labels; keys of 1 to 63 characters from letters, digits and _ . : / - starting with a letter or digit; values of at most 255 characters"
}

func (v labelsValidator) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (v labelsValidator) ValidateMap(_ context.Context, req validator.MapRequest, resp *validator.MapResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	elements := req.ConfigValue.Elements()
	if len(elements) > 64 {
		resp.Diagnostics.AddAttributeError(req.Path, "Too many labels", fmt.Sprintf("%d labels; the API takes at most 64", len(elements)))
	}
	for key, value := range elements {
		if !labelKeyRE.MatchString(key) {
			resp.Diagnostics.AddAttributeError(req.Path, "Invalid label key", fmt.Sprintf("%q is not valid: %s", key, v.Description(context.Background())))
		}
		if s, ok := value.(interface{ ValueString() string }); ok && len([]rune(s.ValueString())) > 255 {
			resp.Diagnostics.AddAttributeError(req.Path, "Label value too long", fmt.Sprintf("the value of %q is longer than 255 characters", key))
		}
	}
}
