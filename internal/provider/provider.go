// Package provider is the Terraform Plugin Framework implementation of the
// virtscale provider: configuration, and registration of its resources and
// data sources.
package provider

import (
	"context"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"gitlab.virtscale.nl/virtscale/virtscale-platform/terraform-provider-virtscale/internal/client"
)

// Ensure VirtscaleProvider satisfies the provider.Provider interface.
var _ provider.Provider = &VirtscaleProvider{}

// VirtscaleProvider is the virtscale Terraform provider.
type VirtscaleProvider struct {
	// version is set by main.go at build/release time; "dev" otherwise.
	version string
}

// VirtscaleProviderModel is the provider's configuration schema data.
type VirtscaleProviderModel struct {
	APIKey       types.String `tfsdk:"api_key"`
	Organization types.String `tfsdk:"organization"`
	Endpoint     types.String `tfsdk:"endpoint"`
}

// New returns a factory for the virtscale provider, for use with
// providerserver.Serve and with terraform-plugin-testing's
// ProtoV6ProviderFactories.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &VirtscaleProvider{version: version}
	}
}

// Metadata sets the provider's type name.
func (p *VirtscaleProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "virtscale"
	resp.Version = p.version
}

// Schema declares the provider's configuration attributes.
func (p *VirtscaleProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Interact with the VirtScale customer API (compute instances, and more) as Terraform resources.",
		Attributes: map[string]schema.Attribute{
			"api_key": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "The VirtScale API key used to authenticate. Defaults to the VIRTSCALE_API_KEY environment variable. A value set here takes precedence over the environment variable.",
			},
			"organization": schema.StringAttribute{
				Optional:    true,
				Description: "The id (a UUID) of the organization to operate in. Defaults to the VIRTSCALE_ORGANIZATION environment variable. A value set here takes precedence over the environment variable.",
			},
			"endpoint": schema.StringAttribute{
				Optional:    true,
				Description: "The base URL of the VirtScale API. Defaults to the VIRTSCALE_ENDPOINT environment variable, or https://api.virtscale.nl when neither is set. A value set here takes precedence over the environment variable.",
			},
		},
	}
}

// Configure builds the *client.Client shared by every resource and data
// source, from configuration attributes falling back to environment
// variables. Configuration wins over the environment. A missing api_key or
// organization (after both are checked) is reported as an attribute-path
// diagnostic naming the attribute and its environment variable -- never a
// panic.
func (p *VirtscaleProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var data VirtscaleProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	apiKey := data.APIKey.ValueString()
	if apiKey == "" {
		apiKey = os.Getenv("VIRTSCALE_API_KEY")
	}
	if apiKey == "" {
		resp.Diagnostics.AddAttributeError(
			path.Root("api_key"),
			"Missing API Key",
			"The provider requires an API key. Set the api_key attribute, or the VIRTSCALE_API_KEY environment variable.",
		)
	}

	organization := data.Organization.ValueString()
	if organization == "" {
		organization = os.Getenv("VIRTSCALE_ORGANIZATION")
	}
	if organization == "" {
		resp.Diagnostics.AddAttributeError(
			path.Root("organization"),
			"Missing Organization",
			"The provider requires an organization id. Set the organization attribute, or the VIRTSCALE_ORGANIZATION environment variable.",
		)
	}

	if resp.Diagnostics.HasError() {
		return
	}

	endpoint := data.Endpoint.ValueString()
	if endpoint == "" {
		endpoint = os.Getenv("VIRTSCALE_ENDPOINT")
	}
	if endpoint == "" {
		endpoint = client.DefaultEndpoint
	}

	c, err := client.New(endpoint, apiKey, organization, client.WithUserAgentVersion(p.version))
	if err != nil {
		resp.Diagnostics.AddError("Unable to Create VirtScale API Client", err.Error())
		return
	}

	resp.DataSourceData = c
	resp.ResourceData = c
}

// Resources registers the provider's managed resources.
func (p *VirtscaleProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewInstanceResource,
		NewPublicIpResource,
		NewSshKeyResource,
		NewProjectResource,
		NewVpcResource,
		NewInstanceFirewallResource,
		NewS3BucketResource,
		NewS3AccessKeyResource,
	}
}

// DataSources registers the provider's data sources.
func (p *VirtscaleProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewImagesDataSource,
		NewCallerDataSource,
		NewRegionsDataSource,
		NewInstanceTypesDataSource,
		NewPlansDataSource,
		NewComputeLimitsDataSource,
		NewProjectDataSource,
	}
}
