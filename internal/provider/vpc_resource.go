package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"gitlab.virtscale.nl/virtscale/virtscale-platform/terraform-provider-virtscale/internal/client"
)

var (
	_ resource.Resource                = &VpcResource{}
	_ resource.ResourceWithConfigure   = &VpcResource{}
	_ resource.ResourceWithImportState = &VpcResource{}
)

var errVpcBuildFailed = errors.New("vpc entered error status")

// VpcResource implements virtscale_vpc: a private address space in one project and one region. A VPC has no PATCH:
// everything about it is fixed when it is created, so every change replaces it.
type VpcResource struct {
	clientResource
}

// NewVpcResource is the resource.Resource factory registered by the provider.
func NewVpcResource() resource.Resource {
	return &VpcResource{}
}

type vpcResourceModel struct {
	ID             types.String `tfsdk:"id"`
	ProjectID      types.String `tfsdk:"project_id"`
	Name           types.String `tfsdk:"name"`
	Region         types.String `tfsdk:"region"`
	Description    types.String `tfsdk:"description"`
	PrefixLength   types.Int64  `tfsdk:"prefix_length"`
	VRN            types.String `tfsdk:"vrn"`
	CIDR           types.String `tfsdk:"cidr"`
	IsDefault      types.Bool   `tfsdk:"is_default"`
	Status         types.String `tfsdk:"status"`
	InternetEgress types.Bool   `tfsdk:"internet_egress"`
	CreatedAt      types.String `tfsdk:"created_at"`
}

// Metadata sets the resource's type name to <provider type name>_vpc.
func (r *VpcResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_vpc"
}

// Schema declares the virtscale_vpc attributes.
func (r *VpcResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		Description: "A VPC: a private address space in one project and one region. It is built and deleted " +
			"asynchronously; apply waits until it is active, and destroy until it is gone. A VPC cannot be changed " +
			"after it is created, so a change to any attribute replaces it. Needs an API key with the network:write scope " +
			"(and compute:read to find the project, for example with the virtscale_project data source).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed: true, Description: "The VPC's id (a UUID), assigned by the API.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"project_id": schema.StringAttribute{
				Required: true, Description: "The id of the project the VPC lives in (a virtscale_project, or the default project).",
				PlanModifiers: replace,
			},
			"name": schema.StringAttribute{
				Required: true, Description: "1 to 63 lower-case letters, digits or hyphens. Unique among the project's VPCs in one region.",
				Validators: []validator.String{dnsLabel("VPC")}, PlanModifiers: replace,
			},
			"region": schema.StringAttribute{
				Required: true, Description: "The region id, from virtscale_regions.", PlanModifiers: replace,
			},
			"description": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				Description: "Free text, at most 255 characters.", Validators: []validator.String{maxLengthValidator{255}},
				PlanModifiers: replace,
			},
			"prefix_length": schema.Int64Attribute{
				Optional: true, Computed: true,
				Description: "How large the private address block is, as a prefix length from 20 to 24; read back from the block " +
					"the platform gave it. Leave it out for the address plan's default.",
				Validators: []validator.Int64{prefixLengthValidator{}},
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.UseStateForUnknown(),
					int64planmodifier.RequiresReplaceIfConfigured(),
				},
			},
			"vrn": schema.StringAttribute{
				Computed: true, Description: "The VPC's VirtScale resource name.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"cidr": schema.StringAttribute{
				Computed: true, Description: "The private address block the platform gave it, in CIDR form.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"is_default": schema.BoolAttribute{
				Computed: true, Description: "True for the VPC the project was set up with, which cannot be deleted.",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"status": schema.StringAttribute{
				Computed:    true,
				Description: "What the platform last confirmed: pending, creating, active, deleting, deleted or error.",
			},
			"internet_egress": schema.BoolAttribute{
				Computed: true, Description: "Whether the VPC's traffic may reach the internet without its own public IP.",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"created_at": schema.StringAttribute{
				Computed: true, Description: "When the VPC was created, RFC 3339.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

// prefixLengthValidator refuses a prefix length outside 20 to 24, as the API does.
type prefixLengthValidator struct{}

func (prefixLengthValidator) Description(context.Context) string {
	return "a prefix length from 20 to 24"
}

func (v prefixLengthValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (prefixLengthValidator) ValidateInt64(_ context.Context, req validator.Int64Request, resp *validator.Int64Response) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if n := req.ConfigValue.ValueInt64(); n < 20 || n > 24 {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid prefix length", fmt.Sprintf("%d is not valid: use a prefix length from 20 to 24", n))
	}
}

// applyVpc copies what the API says about the VPC into model. The project is not part of the answer: it stays what
// the model holds.
func applyVpc(model *vpcResourceModel, v *client.Vpc) {
	model.ID = types.StringValue(v.ID)
	model.Name = types.StringValue(v.Name)
	model.Region = types.StringValue(v.Region)
	model.Description = types.StringValue(v.Description)
	model.VRN = types.StringValue(v.VRN)
	model.CIDR = types.StringValue(v.CIDR)
	model.IsDefault = types.BoolValue(v.IsDefault)
	model.Status = types.StringValue(v.Status)
	model.InternetEgress = types.BoolValue(v.InternetEgress)
	model.CreatedAt = types.StringValue(v.CreatedAt.Format(time.RFC3339))
	model.PrefixLength = types.Int64Null()
	if _, block, err := net.ParseCIDR(v.CIDR); err == nil {
		ones, _ := block.Mask.Size()
		model.PrefixLength = types.Int64Value(int64(ones))
	}
}

// Create asks for the VPC with a fresh Idempotency-Key, keeps what it knows in the state at once so that a failed build
// can still be destroyed, then waits until the VPC is active. A build that ends in status error fails with the
// platform's reason.
func (r *VpcResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan vpcResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	key, err := newIdempotencyKey()
	if err != nil {
		resp.Diagnostics.AddError("Unable to Generate Idempotency Key", err.Error())
		return
	}
	create := client.VpcCreateRequest{Name: plan.Name.ValueString(), Region: plan.Region.ValueString(), Description: plan.Description.ValueString()}
	if !plan.PrefixLength.IsNull() && !plan.PrefixLength.IsUnknown() {
		n := int(plan.PrefixLength.ValueInt64())
		create.PrefixLength = &n
	}
	project := plan.ProjectID.ValueString()
	v, _, err := r.client.CreateVpc(ctx, project, create, key)
	if err != nil {
		resp.Diagnostics.AddError("Unable to Create VPC", apiErrorDetail(err))
		return
	}
	applyVpc(&plan, v)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	waitCtx, cancel := context.WithTimeout(ctx, createTimeout)
	defer cancel()
	final, waitErr := r.client.WaitVpc(waitCtx, project, v.ID, pollInterval, func(v *client.Vpc) (bool, error) {
		if v == nil {
			return false, errors.New("the VPC disappeared while it was being built")
		}
		switch v.Status {
		case "active":
			return true, nil
		case "error":
			return true, errVpcBuildFailed
		}
		return false, nil
	})
	if errors.Is(waitErr, errVpcBuildFailed) && final != nil {
		applyVpc(&plan, final)
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		reason := "the platform gave no reason"
		if final.StatusReason != nil {
			reason = *final.StatusReason
		}
		resp.Diagnostics.AddError("VPC Failed to Build", "the VPC ended in status error: "+reason)
		return
	}
	if waitErr != nil {
		resp.Diagnostics.AddError("Unable to Wait for VPC to Become Active", waitErr.Error())
		return
	}
	applyVpc(&plan, final)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read refreshes state from the API. The vpc 404, or status deleted, removes the resource from state; any other 404
// (the project, the organization) is an error.
func (r *VpcResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state vpcResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	v, err := r.client.GetVpc(ctx, state.ProjectID.ValueString(), state.ID.ValueString())
	if err != nil {
		if client.IsNotFound(err, client.ResourceVpc) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Unable to Read VPC", apiErrorDetail(err))
		return
	}
	if v.Status == "deleted" {
		resp.State.RemoveResource(ctx)
		return
	}
	applyVpc(&state, v)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is reached only to resolve computed values: every attribute that can change replaces the VPC.
func (r *VpcResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan vpcResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	v, err := r.client.GetVpc(ctx, plan.ProjectID.ValueString(), plan.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unable to Read VPC", apiErrorDetail(err))
		return
	}
	applyVpc(&plan, v)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete asks for the VPC to be removed and waits until it is gone. The VPC stays visible (status deleting) until the
// cloud has let go of it. A VPC that is gone already is what destroy wanted; a 409 operation_in_flight is believed to
// mean "a delete is already running" only after the VPC is read back (the API also answers it while a create runs); a
// VPC that still holds subnets is refused with the API's code. A delete that ends in status error fails at once with the
// platform's reason instead of waiting out the timeout.
func (r *VpcResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state vpcResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	key, err := newIdempotencyKey()
	if err != nil {
		resp.Diagnostics.AddError("Unable to Generate Idempotency Key", err.Error())
		return
	}
	project, id := state.ProjectID.ValueString(), state.ID.ValueString()
	if _, err := r.client.DeleteVpc(ctx, project, id, key); err != nil && !client.IsNotFound(err, client.ResourceVpc) && !r.deleteAlreadyRunning(ctx, project, id, err) {
		resp.Diagnostics.AddError("Unable to Delete VPC", apiErrorDetail(err))
		return
	}
	waitCtx, cancel := context.WithTimeout(ctx, deleteTimeout)
	defer cancel()
	if final, err := r.client.WaitVpc(waitCtx, project, id, pollInterval, func(v *client.Vpc) (bool, error) {
		if v != nil && v.Status == "error" {
			return true, errVpcBuildFailed
		}
		return v == nil || v.Status == "deleted", nil
	}); errors.Is(err, errVpcBuildFailed) && final != nil {
		reason := "the platform gave no reason"
		if final.StatusReason != nil {
			reason = *final.StatusReason
		}
		resp.Diagnostics.AddError("VPC Failed to Delete", "the VPC ended in status error: "+reason)
	} else if err != nil {
		resp.Diagnostics.AddError("Unable to Wait for VPC Deletion", err.Error())
	}
}

// deleteAlreadyRunning reports whether err, from a DELETE, only says that a delete of the VPC is already underway:
// 409 operation_in_flight, and the VPC reads deleting (or is gone). The same 409 is the answer while another operation,
// a create for instance, runs, and then it is a refusal to show.
func (r *VpcResource) deleteAlreadyRunning(ctx context.Context, project, id string, err error) bool {
	if !operationInFlight(err) {
		return false
	}
	v, getErr := r.client.GetVpc(ctx, project, id)
	if client.IsNotFound(getErr, client.ResourceVpc) {
		return true
	}
	return getErr == nil && (v.Status == "deleting" || v.Status == "deleted")
}

// operationInFlight reports whether err is the 409 that says another operation already runs for the resource.
func operationInFlight(err error) bool {
	var apiErr *client.APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict && apiErr.Code == "operation_in_flight"
}

// ImportState imports "<project id>/<vpc id>": a VPC is addressed through its project.
func (r *VpcResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	project, id, ok := strings.Cut(req.ID, "/")
	if !ok || project == "" || id == "" || strings.Contains(id, "/") {
		resp.Diagnostics.AddError("Invalid Import ID", fmt.Sprintf("expected <project id>/<vpc id>, got %q", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("project_id"), project)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id)...)
}
