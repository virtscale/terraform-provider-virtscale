package provider

import (
	"context"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"gitlab.virtscale.nl/virtscale/virtscale-platform/terraform-provider-virtscale/internal/client"
)

var (
	_ resource.Resource                = &ProjectResource{}
	_ resource.ResourceWithConfigure   = &ProjectResource{}
	_ resource.ResourceWithImportState = &ProjectResource{}
)

// ProjectResource implements virtscale_project: a project groups an organization's resources.
type ProjectResource struct {
	clientResource
}

// NewProjectResource is the resource.Resource factory registered by the provider.
func NewProjectResource() resource.Resource {
	return &ProjectResource{}
}

type projectResourceModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	Labels      types.Map    `tfsdk:"labels"`
	VRN         types.String `tfsdk:"vrn"`
	IsDefault   types.Bool   `tfsdk:"is_default"`
	CreatedAt   types.String `tfsdk:"created_at"`
	UpdatedAt   types.String `tfsdk:"updated_at"`
}

// Metadata sets the resource's type name to <provider type name>_project.
func (r *ProjectResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_project"
}

// Schema declares the virtscale_project attributes. Name, description and labels change in place.
func (r *ProjectResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A project: it groups an organization's servers, public IPs and networks. Every organization has a " +
			"default project, which cannot be deleted. Creating, renaming and deleting projects needs an API key with the " +
			"write scope (compute:write only reads them).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "The project's id (a UUID), assigned by the API.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required: true,
				Description: "1 to 63 lower-case letters, digits or hyphens, not starting or ending with a hyphen. Unique among " +
					"the organization's projects. Changing it renames the project in place.",
				Validators: []validator.String{dnsLabel("project")},
			},
			"description": schema.StringAttribute{
				Optional: true, Computed: true,
				Default:     stringdefault.StaticString(""),
				Description: "Free text, at most 255 characters. Empty by default.",
				Validators:  []validator.String{maxLengthValidator{255}},
			},
			"labels": schema.MapAttribute{
				Optional: true, Computed: true, ElementType: types.StringType,
				Default:     mapdefault.StaticValue(types.MapValueMust(types.StringType, map[string]attr.Value{})),
				Description: "Free key-value labels, at most 64. Setting them replaces the whole set.",
				Validators:  []validator.Map{labelsValidator{}},
			},
			"vrn": schema.StringAttribute{
				Computed:      true,
				Description:   "The project's VirtScale resource name: vrn:<organization>:project:<id>.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"is_default": schema.BoolAttribute{
				Computed:      true,
				Description:   "True for the organization's default project, where a server created without naming a project lands.",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"created_at": schema.StringAttribute{
				Computed:      true,
				Description:   "When the project was created, RFC 3339.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"updated_at": schema.StringAttribute{
				Computed:    true,
				Description: "When the project last changed, RFC 3339.",
			},
		},
	}
}

// applyProject copies what the API says about the project into model.
func applyProject(ctx context.Context, model *projectResourceModel, p *client.Project) diag.Diagnostics {
	labels := p.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	m, diags := types.MapValueFrom(ctx, types.StringType, labels)
	model.ID = types.StringValue(p.ID)
	model.Name = types.StringValue(p.Name)
	model.Description = types.StringValue(p.Description)
	model.Labels = m
	model.VRN = types.StringValue(p.VRN)
	model.IsDefault = types.BoolValue(p.IsDefault)
	model.CreatedAt = types.StringValue(p.CreatedAt.Format(time.RFC3339))
	model.UpdatedAt = types.StringValue(p.UpdatedAt.Format(time.RFC3339))
	return diags
}

func labelsOf(ctx context.Context, m types.Map) (map[string]string, diag.Diagnostics) {
	out := map[string]string{}
	if m.IsNull() || m.IsUnknown() {
		return out, nil
	}
	return out, m.ElementsAs(ctx, &out, false)
}

// Create sends the project with a fresh Idempotency-Key, so a request repeated after a lost answer gets the first
// answer back.
func (r *ProjectResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan projectResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	labels, diags := labelsOf(ctx, plan.Labels)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	key, err := newIdempotencyKey()
	if err != nil {
		resp.Diagnostics.AddError("Unable to Generate Idempotency Key", err.Error())
		return
	}
	p, err := r.client.CreateProject(ctx, client.ProjectCreateRequest{
		Name: plan.Name.ValueString(), Description: plan.Description.ValueString(), Labels: labels,
	}, key)
	if err != nil {
		resp.Diagnostics.AddError("Unable to Create Project", apiErrorDetail(err))
		return
	}
	resp.Diagnostics.Append(applyProject(ctx, &plan, p)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read refreshes state from the API. Only the project 404 of the API's shared not_found helper removes the resource
// from state; any other 404 is an error.
func (r *ProjectResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state projectResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	p, err := r.client.GetProject(ctx, state.ID.ValueString())
	if err != nil {
		if client.IsNotFound(err, client.ResourceProject) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Unable to Read Project", apiErrorDetail(err))
		return
	}
	resp.Diagnostics.Append(applyProject(ctx, &state, p)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update sends the fields that changed in one PATCH, under the generation of a fresh read (If-Match) and a fresh
// Idempotency-Key. When nothing changed, the computed values the plan left unknown are resolved from a read.
func (r *ProjectResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state projectResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	current, err := r.client.GetProject(ctx, state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unable to Update Project", apiErrorDetail(err))
		return
	}

	var change client.ProjectUpdateRequest
	changed := false
	if !plan.Name.Equal(state.Name) {
		name := plan.Name.ValueString()
		change.Name, changed = &name, true
	}
	if !plan.Description.Equal(state.Description) {
		description := plan.Description.ValueString()
		change.Description, changed = &description, true
	}
	if !plan.Labels.Equal(state.Labels) {
		labels, diags := labelsOf(ctx, plan.Labels)
		resp.Diagnostics.Append(diags...)
		change.Labels, changed = &labels, true
	}
	if resp.Diagnostics.HasError() {
		return
	}
	if changed {
		key, err := newIdempotencyKey()
		if err != nil {
			resp.Diagnostics.AddError("Unable to Generate Idempotency Key", err.Error())
			return
		}
		if current, err = r.client.UpdateProject(ctx, current.ID, current.Generation, change, key); err != nil {
			resp.Diagnostics.AddError("Unable to Update Project", apiErrorDetail(err))
			return
		}
	}
	resp.Diagnostics.Append(applyProject(ctx, &plan, current)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete removes the project. The API refuses the default project (409 default_project) and one that still holds
// something (409 project_not_empty), and its code is shown. The project 404 means it is gone already.
func (r *ProjectResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state projectResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	key, err := newIdempotencyKey()
	if err != nil {
		resp.Diagnostics.AddError("Unable to Generate Idempotency Key", err.Error())
		return
	}
	if err := r.client.DeleteProject(ctx, state.ID.ValueString(), key); err != nil && !client.IsNotFound(err, client.ResourceProject) {
		resp.Diagnostics.AddError("Unable to Delete Project", apiErrorDetail(err))
	}
}

// ImportState imports by id, passthrough.
func (r *ProjectResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
