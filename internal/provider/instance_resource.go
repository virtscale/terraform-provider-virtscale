package provider

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"gitlab.virtscale.nl/virtscale/virtscale-platform/terraform-provider-virtscale/internal/client"
)

// Ensure InstanceResource satisfies the expected resource interfaces.
var (
	_ resource.Resource                = &InstanceResource{}
	_ resource.ResourceWithConfigure   = &InstanceResource{}
	_ resource.ResourceWithImportState = &InstanceResource{}
)

// pollInterval is how often Create/Delete poll the API while waiting for a
// vm to converge. It is a var, not a const, only so tests in this package
// can shorten it; production code never overrides it.
var pollInterval = 5 * time.Second

const (
	createTimeout = 20 * time.Minute
	deleteTimeout = 10 * time.Minute
)

// errInstanceBuildFailed is the sentinel WaitVm's done callback returns when
// the observed vm reaches state "error" during create.
var errInstanceBuildFailed = errors.New("instance entered error state")

// vmNamePattern is the API's stored form of an instance name: a DNS label of
// 1-63 characters (compute/naming.py NAME_RE).
var vmNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// vmNameValidator refuses, at plan time, a name the API would not store as
// given. The API canonicalises names (trims, lower-cases, strips a trailing
// ".virtscale.nl") and answers with the canonical one; the provider copies
// that answer into state, so a name that is not already canonical would fail
// the apply with "inconsistent result" after the instance has been created.
type vmNameValidator struct{}

func (vmNameValidator) Description(context.Context) string {
	return "1-63 lowercase letters, digits or hyphens, starting and ending with a letter or digit"
}

func (v vmNameValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (vmNameValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if name := req.ConfigValue.ValueString(); !vmNamePattern.MatchString(name) {
		resp.Diagnostics.AddAttributeError(
			req.Path,
			"Invalid instance name",
			fmt.Sprintf("%q is not a valid instance name: use 1-63 lowercase letters, digits or hyphens, "+
				"starting and ending with a letter or digit. The API lower-cases names and strips a trailing "+
				"\".virtscale.nl\", so any other form would come back different from the configuration.", name),
		)
	}
}

// InstanceResource implements the virtscale_instance resource.
type InstanceResource struct {
	client *client.Client
}

// NewInstanceResource is the resource.Resource factory registered by the
// provider.
func NewInstanceResource() resource.Resource {
	return &InstanceResource{}
}

// instanceResourceModel mirrors the virtscale_instance schema.
type instanceResourceModel struct {
	ID             types.String `tfsdk:"id"`
	Name           types.String `tfsdk:"name"`
	Region         types.String `tfsdk:"region"`
	Image          types.String `tfsdk:"image"`
	ImageName      types.String `tfsdk:"image_name"`
	VCPUs          types.Int64  `tfsdk:"vcpus"`
	MemoryMB       types.Int64  `tfsdk:"memory_mb"`
	DiskGB         types.Int64  `tfsdk:"disk_gb"`
	StoragePool    types.String `tfsdk:"storage_pool"`
	NetworkProfile types.String `tfsdk:"network_profile"`
	SSHKeys        types.List   `tfsdk:"ssh_keys"`
	State          types.String `tfsdk:"state"`
	FQDN           types.String `tfsdk:"fqdn"`
	PrivateIPv4    types.String `tfsdk:"private_ipv4"`
	CreatedAt      types.String `tfsdk:"created_at"`
}

// Metadata sets the resource's type name to <provider type name>_instance.
func (r *InstanceResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_instance"
}

// Schema declares the virtscale_instance attributes. Every required and
// optional attribute forces replacement: in-place changes are out of scope
// for v0.
func (r *InstanceResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A VirtScale compute instance (VM).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "The instance's id (a UUID), assigned by the API.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Required: true,
				Description: "1-63 lowercase letters, digits or hyphens; must start and end alphanumeric. " +
					"Unique among live instances in the same region -- a deleted instance's name is reusable.",
				Validators: []validator.String{vmNameValidator{}},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"region": schema.StringAttribute{
				Required:    true,
				Description: "The region id to create the instance in.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"image": schema.StringAttribute{
				Required: true,
				Description: "The image id to boot from, as listed by the images catalogue. A " +
					"create-time input only: neither Create nor Read ever overwrites it from the " +
					"API, so it always reflects what was configured (import is the one exception, " +
					"seeding it once from the observed value since there is no prior state to " +
					"preserve). There is no image-drift detection in v0 -- see image_name for what " +
					"the instance is actually running.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"image_name": schema.StringAttribute{
				Computed: true,
				Description: "The human-readable name of the image the instance is currently " +
					"running, as reported by the API. Informational only -- it does not " +
					"participate in planning and never forces a replacement.",
			},
			"vcpus": schema.Int64Attribute{
				Required:    true,
				Description: "Number of virtual CPUs.",
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.RequiresReplace(),
				},
			},
			"memory_mb": schema.Int64Attribute{
				Required:    true,
				Description: "Memory, in megabytes.",
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.RequiresReplace(),
				},
			},
			"disk_gb": schema.Int64Attribute{
				Required:    true,
				Description: "Boot disk size, in gigabytes.",
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.RequiresReplace(),
				},
			},
			"storage_pool": schema.StringAttribute{
				Required:    true,
				Description: "The storage pool id to place the instance's disk on.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"network_profile": schema.StringAttribute{
				Required:    true,
				Description: "The network profile id to attach the instance to.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"ssh_keys": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "Names of your SSH keys to install on the instance. No key material is stored here.",
				PlanModifiers: []planmodifier.List{
					listplanmodifier.RequiresReplace(),
				},
			},
			"state": schema.StringAttribute{
				Computed:    true,
				Description: "The instance's current lifecycle state, as reported by the API.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"fqdn": schema.StringAttribute{
				Computed:    true,
				Description: "The instance's fully-qualified domain name. Null for a customer account.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"private_ipv4": schema.StringAttribute{
				Computed:    true,
				Description: "The instance's first observed private IPv4 address. Empty until one is observed.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"created_at": schema.StringAttribute{
				Computed:    true,
				Description: "When the instance was created, RFC 3339.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
		},
	}
}

// Configure receives the shared *client.Client the provider built.
func (r *InstanceResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// Create sends the create request with a fresh idempotency key, saves the
// id immediately so a failed wait still leaves destroy something to clean
// up, then waits for the vm to reach state running (or fail on state
// error).
func (r *InstanceResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan instanceResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var sshKeys []string
	if !plan.SSHKeys.IsNull() && !plan.SSHKeys.IsUnknown() {
		resp.Diagnostics.Append(plan.SSHKeys.ElementsAs(ctx, &sshKeys, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}

	idempotencyKey, err := newIdempotencyKey()
	if err != nil {
		resp.Diagnostics.AddError("Unable to Generate Idempotency Key", err.Error())
		return
	}

	vm, _, err := r.client.CreateVm(ctx, client.VmCreateRequest{
		Name:           plan.Name.ValueString(),
		Image:          plan.Image.ValueString(),
		VCPUs:          int(plan.VCPUs.ValueInt64()),
		MemoryMB:       int(plan.MemoryMB.ValueInt64()),
		DiskGB:         int(plan.DiskGB.ValueInt64()),
		StoragePool:    plan.StoragePool.ValueString(),
		Region:         plan.Region.ValueString(),
		NetworkProfile: plan.NetworkProfile.ValueString(),
		SSHKeys:        sshKeys,
	}, idempotencyKey)
	if err != nil {
		resp.Diagnostics.AddError("Unable to Create Instance", apiErrorDetail(err))
		return
	}

	// Persist everything we know right away: if the wait below fails,
	// destroy still has an id (and the rest) to work with.
	applyVmToModel(&plan, vm)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	waitCtx, cancel := context.WithTimeout(ctx, createTimeout)
	defer cancel()

	final, waitErr := r.client.WaitVm(waitCtx, vm.ID, pollInterval, func(v *client.Vm) (bool, error) {
		if v == nil {
			return false, nil
		}
		switch v.State {
		case "running":
			return true, nil
		case "error":
			return true, errInstanceBuildFailed
		default:
			return false, nil
		}
	})

	if errors.Is(waitErr, errInstanceBuildFailed) && final != nil {
		applyVmToModel(&plan, final)
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		resp.Diagnostics.AddError(
			"Instance Failed to Build",
			fmt.Sprintf("error_class=%s error_detail=%s", ptrOrEmpty(final.ErrorClass), ptrOrEmpty(final.ErrorDetail)),
		)
		return
	}
	if waitErr != nil {
		resp.Diagnostics.AddError("Unable to Wait for Instance to Become Running", waitErr.Error())
		return
	}

	applyVmToModel(&plan, final)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read refreshes state from the API. A 404 or state "deleted" removes the
// resource from state.
func (r *InstanceResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state instanceResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	vm, err := r.client.GetVm(ctx, state.ID.ValueString())
	if err != nil {
		if client.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Unable to Read Instance", apiErrorDetail(err))
		return
	}

	if vm.State == "deleted" {
		resp.State.RemoveResource(ctx)
		return
	}

	// image is a create-time input (plan D4): Read never overwrites it once
	// there is a prior value to preserve. The one exception is import, where
	// state.Image starts empty -- this seeds it once from the observed
	// glance_id so ImportStateVerify has something to compare against.
	// There is no image-drift detection in v0 (see the image attribute's own
	// description); what the instance is actually running is reported
	// separately via image_name, which applyVmToModel always refreshes.
	if state.Image.IsNull() || state.Image.ValueString() == "" {
		state.Image = types.StringValue(vm.Image.GlanceID)
	}

	applyVmToModel(&state, vm)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is never reached for a config change: every settable attribute
// forces replacement. It is implemented to copy plan into state, matching
// what the framework's Update contract expects.
func (r *InstanceResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan instanceResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete requests deletion and waits for the vm to report state deleted or
// disappear entirely. A 404 counts as already gone, and so does a 409 that
// turns out to mean the vm is already being deleted (see
// removalAlreadyUnderway).
func (r *InstanceResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state instanceResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	_, _, err := r.client.DeleteVm(ctx, state.ID.ValueString())
	if err != nil && !client.IsNotFound(err) && !r.removalAlreadyUnderway(ctx, state.ID.ValueString(), err) {
		resp.Diagnostics.AddError("Unable to Delete Instance", apiErrorDetail(err))
		return
	}

	waitCtx, cancel := context.WithTimeout(ctx, deleteTimeout)
	defer cancel()

	_, err = r.client.WaitVm(waitCtx, state.ID.ValueString(), pollInterval, func(v *client.Vm) (bool, error) {
		if v == nil {
			return true, nil
		}
		return v.State == "deleted", nil
	})
	if err != nil {
		resp.Diagnostics.AddError("Unable to Wait for Instance Deletion", err.Error())
	}
}

// removalAlreadyUnderway reports whether err, from a DELETE, only says that the
// vm is already on its way out. The API allows one operation per vm, so a
// second DELETE while the first runs is 409 operation_in_flight -- what an
// interrupted destroy meets on its next run, and what a DELETE retried after a
// lost response meets. Other 409s are real refusals (vm_protected,
// vm_action_not_allowed, an operation that is not a delete), so the vm is read
// back before the 409 is believed: it is underway only when it is gone, or the
// delete is recorded (desired_state deleted) or running (deleting).
func (r *InstanceResource) removalAlreadyUnderway(ctx context.Context, id string, err error) bool {
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict {
		return false
	}
	vm, getErr := r.client.GetVm(ctx, id)
	if client.IsNotFound(getErr) {
		return true
	}
	if getErr != nil {
		return false
	}
	return vm.DesiredState == "deleted" || vm.State == "deleting" || vm.State == "deleted"
}

// ImportState imports by id, passthrough.
func (r *InstanceResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// applyVmToModel copies observed API state into model. It never touches
// image: Create must leave image at the planned value (the framework's own
// consistency check aborts the apply with "Provider produced inconsistent
// result" if a Required, non-Computed attribute changes after apply), and
// per plan D4 Read never overwrites it either -- only Read's caller seeds it
// once, on import, when there is no prior value -- see Read. image_name is
// always refreshed here: it is purely informational and carries no
// consistency constraint of its own.
func applyVmToModel(model *instanceResourceModel, vm *client.Vm) {
	model.ID = types.StringValue(vm.ID)
	model.Name = types.StringValue(vm.Name)
	model.Region = types.StringValue(vm.RegionID)
	model.ImageName = types.StringValue(vm.Image.Name)
	model.VCPUs = types.Int64Value(int64(vm.VCPUs))
	model.MemoryMB = types.Int64Value(int64(vm.MemoryMB))
	model.DiskGB = types.Int64Value(int64(vm.DiskGB))
	model.StoragePool = types.StringValue(vm.StoragePoolID)
	model.NetworkProfile = types.StringValue(vm.NetworkProfileID)
	model.State = types.StringValue(vm.State)
	model.CreatedAt = types.StringValue(vm.CreatedAt.Format(time.RFC3339))

	if vm.FQDN != nil {
		model.FQDN = types.StringValue(*vm.FQDN)
	} else {
		model.FQDN = types.StringNull()
	}

	model.PrivateIPv4 = types.StringValue(firstIPv4(vm.ObservedAddresses))
}

// firstIPv4 returns the first fixed IPv4 address from a Nova-shaped
// observed_addresses map, iterating network names in sorted order for
// determinism. It returns "" when there is none.
func firstIPv4(observed map[string]any) string {
	if observed == nil {
		return ""
	}

	names := make([]string, 0, len(observed))
	for name := range observed {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		entries, ok := observed[name].([]any)
		if !ok {
			continue
		}
		for _, e := range entries {
			addr, ok := e.(map[string]any)
			if !ok {
				continue
			}

			if !isIPv4(addr["version"]) {
				continue
			}
			addrType, _ := addr["OS-EXT-IPS:type"].(string)
			if addrType != "fixed" {
				continue
			}
			if a, ok := addr["addr"].(string); ok {
				return a
			}
		}
	}
	return ""
}

func isIPv4(v any) bool {
	switch t := v.(type) {
	case float64:
		return t == 4
	case int:
		return t == 4
	case string:
		return t == "4"
	default:
		return false
	}
}

func ptrOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// apiErrorDetail renders a *client.APIError's own formatted message
// (including the problem+json detail and code) when err is one, and falls
// back to err.Error() otherwise.
func apiErrorDetail(err error) string {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Error()
	}
	return err.Error()
}

// newIdempotencyKey returns a fresh UUIDv4, generated from crypto/rand.
func newIdempotencyKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate idempotency key: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10xx
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
