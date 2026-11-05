package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"gitlab.virtscale.nl/virtscale/virtscale-platform/terraform-provider-virtscale/internal/client"
)

var (
	_ datasource.DataSource              = &CallerDataSource{}
	_ datasource.DataSourceWithConfigure = &CallerDataSource{}
)

// CallerDataSource implements the virtscale_caller data source: who the credential is (GET /v1/me).
type CallerDataSource struct {
	clientDataSource
}

// NewCallerDataSource is the datasource.DataSource factory registered by the provider.
func NewCallerDataSource() datasource.DataSource {
	return &CallerDataSource{}
}

type callerModel struct {
	ID                  types.String `tfsdk:"id"`
	Subject             types.String `tfsdk:"subject"`
	Realm               types.String `tfsdk:"realm"`
	Credential          types.String `tfsdk:"credential"`
	PermissionsEnforced types.Bool   `tfsdk:"permissions_enforced"`
	OrganizationID      types.String `tfsdk:"organization_id"`
	Role                types.String `tfsdk:"role"`
	Permissions         types.Set    `tfsdk:"permissions"`
	Scopes              types.Set    `tfsdk:"scopes"`
	APIKeyID            types.String `tfsdk:"api_key_id"`
	ProjectID           types.String `tfsdk:"project_id"`
	ExpiresAt           types.String `tfsdk:"expires_at"`
}

// Metadata sets the data source's type name to <provider type name>_caller.
func (d *CallerDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_caller"
}

// Schema declares the virtscale_caller attributes; all of them are computed.
func (d *CallerDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	str := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, Description: desc}
	}
	resp.Schema = schema.Schema{
		Description: "Who the API key (or other credential) is, from GET /v1/me: its organization, scopes and permissions. " +
			"It fails when the credential does not name the provider's organization.",
		Attributes: map[string]schema.Attribute{
			"id":                   str("The credential's subject."),
			"subject":              str("The credential's subject: api-token:<key id> for an API key."),
			"realm":                str("The identity realm of the credential."),
			"credential":           str("The kind of credential: api_token for an API key, oidc for a signed-in person."),
			"permissions_enforced": schema.BoolAttribute{Computed: true, Description: "Whether the deployment refuses a request that lacks a permission."},
			"organization_id":      str("The provider's organization, confirmed to be one the credential names."),
			"role":                 str("The caller's role in the organization; null for an API key."),
			"permissions":          schema.SetAttribute{Computed: true, ElementType: types.StringType, Description: "Every permission the credential holds in the organization."},
			"scopes":               schema.SetAttribute{Computed: true, ElementType: types.StringType, Description: "The key's scopes; empty when the credential is not an API key."},
			"api_key_id":           str("The key's id; null when the credential is not an API key."),
			"project_id":           str("Set when the key is restricted to one project; such a key is refused on the organization's routes."),
			"expires_at":           str("When the key expires, RFC 3339; null when it never does."),
		},
	}
}

// Read asks who the caller is and keeps what it holds in the provider's organization.
func (d *CallerDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	me, err := d.client.GetMe(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Unable to Read Caller", apiErrorDetail(err))
		return
	}
	org := d.client.Organization()
	var held *client.MeOrganization
	for i := range me.Organizations {
		if me.Organizations[i].OrgID == org {
			held = &me.Organizations[i]
		}
	}
	if held == nil {
		resp.Diagnostics.AddError("Unable to Read Caller", fmt.Sprintf(
			"the credential does not name the organization %s: check VIRTSCALE_ORGANIZATION, or that VIRTSCALE_API_KEY belongs to that organization", org))
		return
	}

	scopes := []string{}
	m := callerModel{
		ID: types.StringValue(me.Subject), Subject: types.StringValue(me.Subject), Realm: types.StringValue(me.Realm),
		Credential: types.StringValue(me.Credential), PermissionsEnforced: types.BoolValue(me.PermissionsEnforced),
		OrganizationID: types.StringValue(org), Role: nullable(held.Role),
		APIKeyID: types.StringNull(), ProjectID: types.StringNull(), ExpiresAt: types.StringNull(),
	}
	if me.APIKey != nil {
		scopes = me.APIKey.Scopes
		m.APIKeyID, m.ProjectID = types.StringValue(me.APIKey.ID), nullable(me.APIKey.ProjectID)
		if me.APIKey.ExpiresAt != nil {
			m.ExpiresAt = types.StringValue(me.APIKey.ExpiresAt.Format(time.RFC3339))
		}
	}
	var diags diag.Diagnostics
	m.Permissions, diags = stringSet(ctx, held.Permissions)
	resp.Diagnostics.Append(diags...)
	m.Scopes, diags = stringSet(ctx, scopes)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// nullable is a string that the API may leave null.
func nullable(s *string) types.String {
	if s == nil {
		return types.StringNull()
	}
	return types.StringValue(*s)
}

// stringSet is a set of strings; an empty or nil slice is an empty set, never null.
func stringSet(ctx context.Context, values []string) (types.Set, diag.Diagnostics) {
	if values == nil {
		values = []string{}
	}
	return types.SetValueFrom(ctx, types.StringType, values)
}
