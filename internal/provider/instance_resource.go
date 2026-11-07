package provider

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
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
	_ resource.Resource                   = &InstanceResource{}
	_ resource.ResourceWithConfigure      = &InstanceResource{}
	_ resource.ResourceWithImportState    = &InstanceResource{}
	_ resource.ResourceWithValidateConfig = &InstanceResource{}
)

// pollInterval is how often Create/Delete poll the API while waiting for a
// vm to converge. It is a var, not a const, only so tests in this package
// can shorten it; production code never overrides it.
var pollInterval = 5 * time.Second

// How long Create/Update and Delete wait for the platform. Vars, like pollInterval, only so that tests in this package
// can shorten them; production code never overrides them.
var (
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

// hostnamePattern is the API's hostname rule: a dotted sequence of DNS labels,
// at least two of them, the last starting with a letter.
var hostnamePattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

// maxHostnameLen is the API's ceiling on a hostname, in characters.
const maxHostnameLen = 253

// hostnameValidator refuses, at plan time, a hostname the API would not store:
// one that does not match its pattern, or is longer than 253 characters. Only a
// known, non-null value is checked.
type hostnameValidator struct{}

func (hostnameValidator) Description(context.Context) string {
	return "a lower-case fully qualified name such as web.example.com, at most 253 characters"
}

func (v hostnameValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (hostnameValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	name := req.ConfigValue.ValueString()
	if len([]rune(name)) > maxHostnameLen || !hostnamePattern.MatchString(name) {
		resp.Diagnostics.AddAttributeError(
			req.Path,
			"Invalid hostname",
			fmt.Sprintf("%q is not a valid hostname: it must be a lower-case fully qualified name such as "+
				"web.example.com -- dot-separated labels of lower-case letters, digits and hyphens, each 1-63 "+
				"characters, not starting or ending with a hyphen, at least two labels, the last label starting "+
				"with a letter, at most %d characters in all.", name, maxHostnameLen),
		)
	}
}

// maxSSHKeys is how many keys the API gives one instance.
const maxSSHKeys = 10

// sshKeyCountValidator refuses, at plan time, more than 10 names in ssh_keys,
// with the API's own sentence (422 too_many_ssh_keys), before anything is sent.
type sshKeyCountValidator struct{}

func (sshKeyCountValidator) Description(context.Context) string {
	return fmt.Sprintf("at most %d SSH keys", maxSSHKeys)
}

func (v sshKeyCountValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (sshKeyCountValidator) ValidateList(_ context.Context, req validator.ListRequest, resp *validator.ListResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if len(req.ConfigValue.Elements()) > maxSSHKeys {
		resp.Diagnostics.AddAttributeError(
			req.Path,
			"Too many SSH keys",
			fmt.Sprintf("a VM can be given at most %d SSH keys", maxSSHKeys),
		)
	}
}

// sshKeyUniqueValidator refuses, at plan time, a name that is listed twice in
// ssh_keys, with the API's own sentence (422 duplicate_ssh_key). A name that is
// not known yet cannot be compared.
type sshKeyUniqueValidator struct{}

func (sshKeyUniqueValidator) Description(context.Context) string {
	return "no SSH key name listed more than once"
}

func (v sshKeyUniqueValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (sshKeyUniqueValidator) ValidateList(_ context.Context, req validator.ListRequest, resp *validator.ListResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	seen := map[string]bool{}
	reported := map[string]bool{}
	for _, element := range req.ConfigValue.Elements() {
		name, ok := element.(types.String)
		if !ok || name.IsNull() || name.IsUnknown() {
			continue
		}
		n := name.ValueString()
		if seen[n] && !reported[n] {
			reported[n] = true
			resp.Diagnostics.AddAttributeError(
				req.Path,
				"Duplicate SSH key",
				fmt.Sprintf("SSH key '%s' is listed more than once", n),
			)
		}
		seen[n] = true
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
	Status         types.String `tfsdk:"status"`
	Hostname       types.String `tfsdk:"hostname"`
	InstanceType   types.String `tfsdk:"instance_type"`
	FQDN           types.String `tfsdk:"fqdn"`
	PrivateIPv4    types.String `tfsdk:"private_ipv4"`
	CreatedAt      types.String `tfsdk:"created_at"`
}

// Metadata sets the resource's type name to <provider type name>_instance.
func (r *InstanceResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_instance"
}

// Schema declares the virtscale_instance attributes. Every required and
// optional attribute forces replacement, except hostname: it is the one
// in-place change the API offers.
//
// The sizes, storage_pool, network_profile and instance_type are optional because the API picks what a create leaves
// out (customer API compute.py _check_create_shape, domain/compute/vms.py create_vm_request): an instance type fills the
// three sizes and, unless a pool is named, the pool of its own; a profile left out is the first one the organization
// may use in the region. They are computed too, so the chosen values are read back into state, and keep their value
// across plans (UseStateForUnknown). They replace the instance only when the configuration sets a value that differs
// (RequiresReplaceIfConfigured): a value left out of the configuration is the API's to choose, not a change.
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
			"instance_type": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Description: "The catalogue instance type to create the instance from, for example g1-small (the " +
					"virtscale_instance_types data source lists them). It sets vcpus, memory_mb and disk_gb: name it " +
					"or give those three, not both. With a type, storage_pool and network_profile need not be set. " +
					"Read back from the API, so it is null for an instance the API reports no type for; an " +
					"instance that has one keeps it when the line is removed. A different type replaces the " +
					"instance. The platform may require a type, in which case it refuses vcpus, memory_mb and " +
					"disk_gb with 422 instance_type_required.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
					stringplanmodifier.RequiresReplaceIfConfigured(),
				},
			},
			"vcpus": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Description: "Number of virtual CPUs. Taken from instance_type when one is named; otherwise give " +
					"vcpus, memory_mb, disk_gb and storage_pool together.",
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.UseStateForUnknown(),
					int64planmodifier.RequiresReplaceIfConfigured(),
				},
			},
			"memory_mb": schema.Int64Attribute{
				Optional:    true,
				Computed:    true,
				Description: "Memory, in megabytes. Taken from instance_type when one is named.",
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.UseStateForUnknown(),
					int64planmodifier.RequiresReplaceIfConfigured(),
				},
			},
			"disk_gb": schema.Int64Attribute{
				Optional:    true,
				Computed:    true,
				Description: "Boot disk size, in gigabytes. Taken from instance_type when one is named.",
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.UseStateForUnknown(),
					int64planmodifier.RequiresReplaceIfConfigured(),
				},
			},
			"storage_pool": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Description: "The storage pool id to place the instance's disk on. Left out with instance_type, the " +
					"API uses the type's own pool; with vcpus, memory_mb and disk_gb it is required. State holds the " +
					"pool the instance is on. Naming a different pool replaces the instance; removing the line keeps " +
					"the pool.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
					stringplanmodifier.RequiresReplaceIfConfigured(),
				},
			},
			"network_profile": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Description: "The network profile id to attach the instance to. Left out, the API picks the first " +
					"profile your organization may use in the region on the hosts the instance is placed on. State " +
					"holds the profile the instance is on. Naming a different profile replaces the instance; " +
					"removing the line keeps the profile.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
					stringplanmodifier.RequiresReplaceIfConfigured(),
				},
			},
			"ssh_keys": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "Names of SSH keys imported into the organization (see virtscale_ssh_key), at " +
					"most 10, each listed once; no key material is stored here. cloud-init installs them for " +
					"the image's default user (for example debian or ubuntu) at first boot, and the instance " +
					"keeps them if a key is deleted or replaced later. The API refuses a name that was never " +
					"imported with 422 ssh_key_not_found, and a 503 not_ready means SSH keys are not switched " +
					"on for this API yet. Changing the list replaces the instance. Importing an instance reads " +
					"its key names back from the API.",
				Validators: []validator.List{
					sshKeyCountValidator{},
					sshKeyUniqueValidator{},
				},
				PlanModifiers: []planmodifier.List{
					listplanmodifier.RequiresReplace(),
				},
			},
			"state": schema.StringAttribute{
				Computed:    true,
				Description: "The instance's current lifecycle state, as reported by the API.",
			},
			"status": schema.StringAttribute{
				Computed:    true,
				Description: "The instance's status word, as the API reports it.",
			},
			"hostname": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Description: "The lower-case FQDN the instance is known by in your organization; unique among the " +
					"organization's live instances (a duplicate is 409 hostname_taken). It is a handle only: no DNS " +
					"record is created. Changing it updates the instance in place; it is read back from the API on " +
					"every refresh, so a change made elsewhere shows as a difference. Removing the attribute from " +
					"the configuration keeps the current hostname (this provider does not remove one). Import " +
					"fills it.",
				Validators: []validator.String{hostnameValidator{}},
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
				Computed: true,
				Description: "The instance's private IPv4 address, as reported by the platform; empty until it is " +
					"known. An associated public IP is not the private address.",
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

// ValidateConfig refuses, at plan time, the ways of sizing an instance the API refuses (compute.py
// _check_create_shape): an instance type together with any of the three sizes, none of the four, or sizes that are not
// all given with a storage pool. It judges only what is known: a value that is unknown now (a type taken from a data
// source) is left to the API, which stays the authority and answers every other refusal itself.
func (r *InstanceResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var c instanceResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &c)...)
	if resp.Diagnostics.HasError() {
		return
	}

	unknown := c.InstanceType.IsUnknown() || c.StoragePool.IsUnknown()
	sizes := 0
	for _, n := range []types.Int64{c.VCPUs, c.MemoryMB, c.DiskGB} {
		unknown = unknown || n.IsUnknown()
		if !n.IsNull() && !n.IsUnknown() {
			sizes++
		}
	}
	typed := !c.InstanceType.IsNull() && !c.InstanceType.IsUnknown()

	switch {
	case typed && sizes > 0:
		resp.Diagnostics.AddAttributeError(path.Root("instance_type"), "Instance type and sizes together",
			"name an instance type or give vcpus, memory_mb and disk_gb, not both: the type sets the three sizes")
	case typed || unknown:
	case sizes == 0:
		resp.Diagnostics.AddAttributeError(path.Root("instance_type"), "No instance type or sizes",
			"set instance_type (see the virtscale_instance_types data source), or vcpus, memory_mb, disk_gb and storage_pool")
	case sizes < 3 || c.StoragePool.IsNull():
		resp.Diagnostics.AddAttributeError(path.Root("vcpus"), "Incomplete sizes",
			"vcpus, memory_mb, disk_gb and storage_pool are required together when no instance_type is set")
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

	// hostname is sent only when the configuration gave one: an omitted
	// hostname is left out of the body entirely, so the instance has no hostname.
	var hostname *string
	if !plan.Hostname.IsNull() && !plan.Hostname.IsUnknown() {
		h := plan.Hostname.ValueString()
		hostname = &h
	}

	// The optional attributes the configuration left out are unknown in the plan, and read as "" or 0 here: the client
	// leaves those out of the body, and the API chooses them.
	vm, _, err := r.client.CreateVm(ctx, client.VmCreateRequest{
		Name:           plan.Name.ValueString(),
		Image:          plan.Image.ValueString(),
		InstanceType:   plan.InstanceType.ValueString(),
		VCPUs:          int(plan.VCPUs.ValueInt64()),
		MemoryMB:       int(plan.MemoryMB.ValueInt64()),
		DiskGB:         int(plan.DiskGB.ValueInt64()),
		StoragePool:    plan.StoragePool.ValueString(),
		Region:         plan.Region.ValueString(),
		NetworkProfile: plan.NetworkProfile.ValueString(),
		Hostname:       hostname,
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

// Read refreshes state from the API. The 404 that says the vm is gone, or state
// "deleted", removes the resource from state; any other 404 is an error.
func (r *InstanceResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state instanceResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	vm, err := r.client.GetVm(ctx, state.ID.ValueString())
	if err != nil {
		if client.IsNotFound(err, client.ResourceVm) {
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
	// image_id so ImportStateVerify has something to compare against.
	// There is no image-drift detection in v0 (see the image attribute's own
	// description); what the instance is actually running is reported
	// separately via image_name, which applyVmToModel always refreshes.
	if state.Image.IsNull() || state.Image.ValueString() == "" {
		state.Image = types.StringValue(vm.Image.ImageID)
	}

	applyVmToModel(&state, vm)

	// ssh_keys follows ssh_key_names, so that an imported instance has its keys
	// and does not plan a replacement for them (M14).
	sshKeys, diags := sshKeysFromAPI(ctx, state.SSHKeys, vm.SSHKeyNames)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	state.SSHKeys = sshKeys

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// sshKeysFromAPI is ssh_keys as Read stores it, given what the state holds and
// the names the API reports (VmOut.ssh_key_names, a launch-time snapshot).
//
//   - No names: null, so a configuration that does not mention keys has nothing
//     to change. A configured empty list stays an empty list.
//   - The names the state already holds, in whatever order the API gives them:
//     the state's list, so a deployment that sorts them does not make every plan
//     replace the instance.
//   - Anything else (an import, or a list that differs): what the API reports.
func sshKeysFromAPI(ctx context.Context, current types.List, names []string) (types.List, diag.Diagnostics) {
	if len(names) == 0 {
		if !current.IsNull() && !current.IsUnknown() && len(current.Elements()) == 0 {
			return current, nil
		}
		return types.ListNull(types.StringType), nil
	}

	if !current.IsNull() && !current.IsUnknown() {
		var held []string
		if diags := current.ElementsAs(ctx, &held, false); !diags.HasError() && sameNames(held, names) {
			return current, nil
		}
	}
	return types.ListValueFrom(ctx, types.StringType, names)
}

// sameNames reports whether a and b hold the same names, in any order.
func sameNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := map[string]int{}
	for _, n := range a {
		counts[n]++
	}
	for _, n := range b {
		counts[n]--
	}
	for _, c := range counts {
		if c != 0 {
			return false
		}
	}
	return true
}

// Update carries out the one in-place change the API offers: a hostname
// change. A configured, non-null hostname that differs from the state's is
// sent as a PATCH, with the generation of a fresh read as its precondition and
// a fresh Idempotency-Key. Everything else forces replacement, so a plan that
// reaches Update changes nothing but the hostname -- and when even that does
// not differ, the computed values the plan left unknown are resolved from a
// read instead.
func (r *InstanceResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior instanceResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if hostnameChanged(plan.Hostname, prior.Hostname) {
		// The generation the PATCH must carry is read fresh, not taken from
		// the state: the instance may have moved on since the last refresh,
		// and a stale If-Match would be refused with 412 generation_mismatch.
		vm, err := r.client.GetVm(ctx, prior.ID.ValueString())
		if err != nil {
			resp.Diagnostics.AddError("Unable to Update Instance", apiErrorDetail(err))
			return
		}

		idempotencyKey, err := newIdempotencyKey()
		if err != nil {
			resp.Diagnostics.AddError("Unable to Generate Idempotency Key", err.Error())
			return
		}

		hostname := plan.Hostname.ValueString()
		updated, err := r.client.UpdateVmHostname(ctx, vm.ID, vm.Generation, &hostname, idempotencyKey)
		if err != nil {
			resp.Diagnostics.AddError("Unable to Update Instance", apiErrorDetail(err))
			return
		}

		applyVmToModel(&plan, updated)
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return
	}

	// Nothing to change: refresh the computed values the plan left unknown so
	// that no unknown is written to state.
	vm, err := r.client.GetVm(ctx, prior.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unable to Update Instance", apiErrorDetail(err))
		return
	}
	applyVmToModel(&plan, vm)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// hostnameChanged reports whether planned differs from prior: a known,
// non-null planned hostname that the prior state does not already hold.
func hostnameChanged(planned, prior types.String) bool {
	if planned.IsNull() || planned.IsUnknown() {
		return false
	}
	if prior.IsNull() || prior.IsUnknown() {
		return true
	}
	return planned.ValueString() != prior.ValueString()
}

// Delete requests deletion and waits for the vm to report state deleted or
// disappear entirely. The 404 that says the vm is gone counts as done, and so
// does a 409 that turns out to mean the vm is already being deleted (see
// removalAlreadyUnderway). A 404 about anything else is an error.
func (r *InstanceResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state instanceResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	_, _, err := r.client.DeleteVm(ctx, state.ID.ValueString())
	if err != nil && !client.IsNotFound(err, client.ResourceVm) && !r.removalAlreadyUnderway(ctx, state.ID.ValueString(), err) {
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
	if client.IsNotFound(getErr, client.ResourceVm) {
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
	model.Status = types.StringValue(vm.Status)
	model.CreatedAt = types.StringValue(vm.CreatedAt.Format(time.RFC3339))

	if vm.FQDN != nil {
		model.FQDN = types.StringValue(*vm.FQDN)
	} else {
		model.FQDN = types.StringNull()
	}

	if vm.Hostname != nil {
		model.Hostname = types.StringValue(*vm.Hostname)
	} else {
		model.Hostname = types.StringNull()
	}

	if vm.InstanceType != nil {
		model.InstanceType = types.StringValue(*vm.InstanceType)
	} else {
		model.InstanceType = types.StringNull()
	}

	if vm.PrivateIP != nil {
		model.PrivateIPv4 = types.StringValue(*vm.PrivateIP)
	} else {
		model.PrivateIPv4 = types.StringValue("")
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
