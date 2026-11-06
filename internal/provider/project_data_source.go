package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"gitlab.virtscale.nl/virtscale/virtscale-platform/terraform-provider-virtscale/internal/client"
)

var (
	_ datasource.DataSource              = &ProjectDataSource{}
	_ datasource.DataSourceWithConfigure = &ProjectDataSource{}
)

// ProjectDataSource implements the virtscale_project data source: a project by name, or the default one.
type ProjectDataSource struct {
	clientDataSource
}

// NewProjectDataSource is the datasource.DataSource factory registered by the provider.
func NewProjectDataSource() datasource.DataSource {
	return &ProjectDataSource{}
}

type projectDataSourceModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	Labels      types.Map    `tfsdk:"labels"`
	VRN         types.String `tfsdk:"vrn"`
	IsDefault   types.Bool   `tfsdk:"is_default"`
	CreatedAt   types.String `tfsdk:"created_at"`
}

// Metadata sets the data source's type name to <provider type name>_project.
func (d *ProjectDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_project"
}

// Schema declares the virtscale_project data source.
func (d *ProjectDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	str := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, Description: desc}
	}
	resp.Schema = schema.Schema{
		Description: "A project of the organization, by name; without a name, the organization's default project.",
		Attributes: map[string]schema.Attribute{
			"name":        schema.StringAttribute{Optional: true, Computed: true, Description: "The project's name; leave it out for the default project."},
			"id":          str("The project's id (a UUID), for the project_id of a VPC."),
			"description": str("The project's description."),
			"labels":      schema.MapAttribute{Computed: true, ElementType: types.StringType, Description: "The project's labels."},
			"vrn":         str("The project's VirtScale resource name."),
			"is_default":  schema.BoolAttribute{Computed: true, Description: "True for the organization's default project."},
			"created_at":  str("When the project was created, RFC 3339."),
		},
	}
}

// Read lists the projects and picks one.
func (d *ProjectDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m projectDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	projects, err := d.client.ListProjects(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Unable to List Projects", apiErrorDetail(err))
		return
	}
	var found *client.Project
	for i := range projects {
		if (m.Name.IsNull() && projects[i].IsDefault) || (!m.Name.IsNull() && projects[i].Name == m.Name.ValueString()) {
			found = &projects[i]
			break
		}
	}
	if found == nil {
		what := "the organization has no default project"
		if !m.Name.IsNull() {
			what = fmt.Sprintf("no project named %q", m.Name.ValueString())
		}
		resp.Diagnostics.AddError("Project Not Found", what)
		return
	}
	labels := found.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	var diags = resp.Diagnostics
	m.Labels, diags = types.MapValueFrom(ctx, types.StringType, labels)
	resp.Diagnostics.Append(diags...)
	m.ID, m.Name, m.Description = types.StringValue(found.ID), types.StringValue(found.Name), types.StringValue(found.Description)
	m.VRN, m.IsDefault, m.CreatedAt = types.StringValue(found.VRN), types.BoolValue(found.IsDefault), types.StringValue(found.CreatedAt.Format(time.RFC3339))
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
