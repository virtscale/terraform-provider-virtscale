package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/resource"

	"gitlab.virtscale.nl/virtscale/virtscale-platform/terraform-provider-virtscale/internal/client"
)

// clientResource is what every resource shares: the *client.Client the provider built.
type clientResource struct {
	client *client.Client
}

// Configure receives the shared *client.Client the provider built.
func (r *clientResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	c, ok := req.ProviderData.(*client.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *client.Client, got: %T. Please report this issue.", req.ProviderData),
		)
		return
	}
	r.client = c
}
