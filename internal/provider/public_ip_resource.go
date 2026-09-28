package provider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"gitlab.virtscale.nl/virtscale/virtscale-platform/terraform-provider-virtscale/internal/client"
)

// Ensure PublicIpResource satisfies the expected resource interfaces.
var (
	_ resource.Resource                = &PublicIpResource{}
	_ resource.ResourceWithConfigure   = &PublicIpResource{}
	_ resource.ResourceWithImportState = &PublicIpResource{}
)

// publicIpTimeout bounds every wait (allocate, associate, disassociate,
// release) this resource performs.
const publicIpTimeout = 10 * time.Minute

// errPublicIpFailed is the sentinel WaitPublicIp's done callback returns
// when the observed public IP reaches state "error" during allocation.
var errPublicIpFailed = errors.New("public ip entered error state")

// PublicIpResource implements the virtscale_public_ip resource.
type PublicIpResource struct {
	client *client.Client
}

// NewPublicIpResource is the resource.Resource factory registered by the
// provider.
func NewPublicIpResource() resource.Resource {
	return &PublicIpResource{}
}

// publicIpResourceModel mirrors the virtscale_public_ip schema.
type publicIpResourceModel struct {
	ID         types.String `tfsdk:"id"`
	Region     types.String `tfsdk:"region"`
	InstanceID types.String `tfsdk:"instance_id"`
	Address    types.String `tfsdk:"address"`
	State      types.String `tfsdk:"state"`
}

// Metadata sets the resource's type name to <provider type name>_public_ip.
func (r *PublicIpResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_public_ip"
}

// Schema declares the virtscale_public_ip attributes. Only region forces
// replacement; instance_id associates or disassociates the IP in place.
func (r *PublicIpResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A VirtScale public IP address. A customer organization's quota is one public IP.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "The public IP's id (a UUID), assigned by the API.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"region": schema.StringAttribute{
				Required:    true,
				Description: "The region id to allocate the public IP in.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"instance_id": schema.StringAttribute{
				Optional:    true,
				Description: "The id of a virtscale_instance to associate this public IP with. Changing this value (including removing it) associates or disassociates the IP in place; it never replaces the resource.",
			},
			"address": schema.StringAttribute{
				Computed:    true,
				Description: "The allocated public IPv4 address. Unset until the allocation converges.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"state": schema.StringAttribute{
				Computed: true,
				Description: "The public IP's current lifecycle state, as reported by the API. Not " +
					"stabilized with UseStateForUnknown: unlike the other computed attributes, " +
					"it changes as a side effect of instance_id (e.g. associated -> allocated on " +
					"disassociation), so it must be free to differ from its prior value on every plan.",
			},
		},
	}
}

// Configure receives the shared *client.Client the provider built.
func (r *PublicIpResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// Create allocates the public IP and waits for it to become allocated, then
// associates it with instance_id (when set) and waits for that to converge.
func (r *PublicIpResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan publicIpResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	ip, _, err := r.client.AllocatePublicIp(ctx, plan.Region.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unable to Allocate Public IP", apiErrorDetail(err))
		return
	}

	// Persist what we know right away: if the wait below fails, destroy
	// still has an id to work with. instance_id is left as configured; it
	// only becomes true once association (if any) below succeeds.
	applyPublicIpStatusToModel(&plan, ip)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	waitCtx, cancel := context.WithTimeout(ctx, publicIpTimeout)
	defer cancel()

	final, waitErr := r.client.WaitPublicIp(waitCtx, ip.ID, pollInterval, func(p *client.PublicIp) (bool, error) {
		if p == nil {
			return false, nil
		}
		switch p.State {
		case "allocated":
			return true, nil
		case "error":
			return true, errPublicIpFailed
		default:
			return false, nil
		}
	})

	if errors.Is(waitErr, errPublicIpFailed) && final != nil {
		applyPublicIpStatusToModel(&plan, final)
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		resp.Diagnostics.AddError(
			"Public IP Failed to Allocate",
			fmt.Sprintf("error=%s", ptrOrEmpty(final.Error)),
		)
		return
	}
	if waitErr != nil {
		resp.Diagnostics.AddError("Unable to Wait for Public IP to Allocate", waitErr.Error())
		return
	}

	applyPublicIpStatusToModel(&plan, final)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !plan.InstanceID.IsNull() && plan.InstanceID.ValueString() != "" {
		associated, err := r.associate(ctx, final.ID, plan.InstanceID.ValueString())
		if err != nil {
			resp.Diagnostics.AddError("Unable to Associate Public IP", err.Error())
			return
		}
		applyPublicIpToModel(&plan, associated)
	} else {
		plan.InstanceID = types.StringNull()
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read refreshes state from the API. A 404 removes the resource from state.
func (r *PublicIpResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state publicIpResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	ip, err := r.client.GetPublicIp(ctx, state.ID.ValueString())
	if err != nil {
		if client.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Unable to Read Public IP", apiErrorDetail(err))
		return
	}

	applyPublicIpToModel(&state, ip)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update reacts only to an instance_id change: disassociating the prior
// association (if any) and associating the new one (if set).
func (r *PublicIpResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan publicIpResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var state publicIpResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	oldInstanceID := state.InstanceID.ValueString()
	newInstanceID := plan.InstanceID.ValueString()

	if oldInstanceID != newInstanceID {
		if state.State.ValueString() == "associated" {
			disassociated, err := r.disassociate(ctx, state.ID.ValueString())
			if err != nil {
				resp.Diagnostics.AddError("Unable to Disassociate Public IP", err.Error())
				return
			}
			applyPublicIpStatusToModel(&plan, disassociated)
		}

		if !plan.InstanceID.IsNull() && newInstanceID != "" {
			associated, err := r.associate(ctx, state.ID.ValueString(), newInstanceID)
			if err != nil {
				resp.Diagnostics.AddError("Unable to Associate Public IP", err.Error())
				return
			}
			applyPublicIpToModel(&plan, associated)
		} else {
			plan.InstanceID = types.StringNull()
		}
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete disassociates the public IP first when associated, releases it,
// then waits for it to disappear (404).
func (r *PublicIpResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state publicIpResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if state.State.ValueString() == "associated" {
		if _, err := r.disassociate(ctx, state.ID.ValueString()); err != nil {
			resp.Diagnostics.AddError("Unable to Disassociate Public IP", err.Error())
			return
		}
	}

	_, _, err := r.client.ReleasePublicIp(ctx, state.ID.ValueString())
	if err != nil && !client.IsNotFound(err) {
		resp.Diagnostics.AddError("Unable to Release Public IP", apiErrorDetail(err))
		return
	}

	waitCtx, cancel := context.WithTimeout(ctx, publicIpTimeout)
	defer cancel()

	_, err = r.client.WaitPublicIp(waitCtx, state.ID.ValueString(), pollInterval, func(p *client.PublicIp) (bool, error) {
		return p == nil, nil
	})
	if err != nil {
		resp.Diagnostics.AddError("Unable to Wait for Public IP Release", err.Error())
	}
}

// ImportState imports by id, passthrough.
func (r *PublicIpResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// associate sends AssociatePublicIp and waits for state "associated",
// failing fast -- same message style as Create's allocate-wait -- if the API
// reports state "error" instead of polling out the full timeout.
func (r *PublicIpResource) associate(ctx context.Context, id, vmID string) (*client.PublicIp, error) {
	if _, _, err := r.client.AssociatePublicIp(ctx, id, vmID); err != nil {
		return nil, errors.New(apiErrorDetail(err))
	}

	waitCtx, cancel := context.WithTimeout(ctx, publicIpTimeout)
	defer cancel()

	final, waitErr := r.client.WaitPublicIp(waitCtx, id, pollInterval, func(p *client.PublicIp) (bool, error) {
		if p == nil {
			return false, nil
		}
		switch p.State {
		case "associated":
			return true, nil
		case "error":
			return true, errPublicIpFailed
		default:
			return false, nil
		}
	})
	if errors.Is(waitErr, errPublicIpFailed) && final != nil {
		return final, fmt.Errorf("error=%s", ptrOrEmpty(final.Error))
	}
	return final, waitErr
}

// disassociate sends DisassociatePublicIp and waits for state "allocated",
// failing fast -- same message style as Create's allocate-wait -- if the API
// reports state "error" instead of polling out the full timeout.
func (r *PublicIpResource) disassociate(ctx context.Context, id string) (*client.PublicIp, error) {
	if _, _, err := r.client.DisassociatePublicIp(ctx, id); err != nil {
		return nil, errors.New(apiErrorDetail(err))
	}

	waitCtx, cancel := context.WithTimeout(ctx, publicIpTimeout)
	defer cancel()

	final, waitErr := r.client.WaitPublicIp(waitCtx, id, pollInterval, func(p *client.PublicIp) (bool, error) {
		if p == nil {
			return false, nil
		}
		switch p.State {
		case "allocated":
			return true, nil
		case "error":
			return true, errPublicIpFailed
		default:
			return false, nil
		}
	})
	if errors.Is(waitErr, errPublicIpFailed) && final != nil {
		return final, fmt.Errorf("error=%s", ptrOrEmpty(final.Error))
	}
	return final, waitErr
}

// applyPublicIpStatusToModel copies id/region/address/state from ip into
// model, leaving instance_id untouched. Used mid-Create, before an
// association (if any) has been attempted, so the user's desired instance_id
// is never clobbered with the not-yet-true value observed from the API.
func applyPublicIpStatusToModel(model *publicIpResourceModel, ip *client.PublicIp) {
	model.ID = types.StringValue(ip.ID)
	model.Region = types.StringValue(ip.RegionID)
	model.State = types.StringValue(ip.State)

	if ip.Address != nil {
		model.Address = types.StringValue(*ip.Address)
	} else {
		model.Address = types.StringNull()
	}
}

// applyPublicIpToModel is applyPublicIpStatusToModel plus instance_id, set
// from vm_id (null when none). Used once the observed public IP fully
// reflects the desired association state.
func applyPublicIpToModel(model *publicIpResourceModel, ip *client.PublicIp) {
	applyPublicIpStatusToModel(model, ip)

	if ip.VmID != nil {
		model.InstanceID = types.StringValue(*ip.VmID)
	} else {
		model.InstanceID = types.StringNull()
	}
}
