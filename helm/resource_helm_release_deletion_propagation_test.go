// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package helm

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestHelmReleaseSchema_deletionPropagation(t *testing.T) {
	ctx := context.Background()
	resp := &resource.SchemaResponse{}
	NewHelmRelease().Schema(ctx, resource.SchemaRequest{}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %v", resp.Diagnostics)
	}

	attr, ok := resp.Schema.Attributes["deletion_propagation"].(schema.StringAttribute)
	if !ok {
		t.Fatalf("deletion_propagation is not a string attribute")
	}
	if !attr.Optional || attr.Computed || attr.Default != nil {
		t.Fatalf("deletion_propagation must be optional without default, got optional=%t computed=%t default=%v", attr.Optional, attr.Computed, attr.Default)
	}

	cases := map[string]bool{
		"foreground": true,
		"background": true,
		"orphan":     true,
		"Foreground": false,
		"invalid":    false,
		"":           false,
	}
	for value, valid := range cases {
		t.Run(value, func(t *testing.T) {
			req := validator.StringRequest{
				Path:        path.Root("deletion_propagation"),
				ConfigValue: types.StringValue(value),
			}
			var hasError bool
			for _, v := range attr.Validators {
				vresp := &validator.StringResponse{}
				v.ValidateString(ctx, req, vresp)
				hasError = hasError || vresp.Diagnostics.HasError()
			}
			if hasError == valid {
				t.Fatalf("value %q: valid=%t but validation error=%t", value, valid, hasError)
			}
		})
	}
}
