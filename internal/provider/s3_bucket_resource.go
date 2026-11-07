package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"gitlab.virtscale.nl/virtscale/virtscale-platform/terraform-provider-virtscale/internal/client"
)

var (
	_ resource.Resource                = &S3BucketResource{}
	_ resource.ResourceWithConfigure   = &S3BucketResource{}
	_ resource.ResourceWithImportState = &S3BucketResource{}
	_ resource.ResourceWithModifyPlan  = &S3BucketResource{}
)

var errBucketBuildFailed = errors.New("bucket entered a failed state")

// S3BucketResource implements virtscale_s3_bucket: an object-storage bucket and its own size and object ceilings.
type S3BucketResource struct {
	clientResource
}

// NewS3BucketResource is the resource.Resource factory registered by the provider.
func NewS3BucketResource() resource.Resource {
	return &S3BucketResource{}
}

type s3BucketModel struct {
	ID           types.String `tfsdk:"id"`
	Name         types.String `tfsdk:"name"`
	Versioning   types.Bool   `tfsdk:"versioning"`
	ForceDestroy types.Bool   `tfsdk:"force_destroy"`
	MaxSizeBytes types.Int64  `tfsdk:"max_size_bytes"`
	MaxObjects   types.Int64  `tfsdk:"max_objects"`
	State        types.String `tfsdk:"state"`
	CreatedAt    types.String `tfsdk:"created_at"`
}

// Metadata sets the resource's type name to <provider type name>_s3_bucket.
func (r *S3BucketResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_s3_bucket"
}

// bucketNameValidator is the API's bucket name rule (domain/naming.py), in the API's order and with its sentences: 3 to 63
// characters, no dots, no two hyphens in a row, lower-case letters, digits and hyphens starting and ending with a letter or
// digit, and not a name or prefix the platform keeps for itself.
type bucketNameValidator struct{}

var (
	bucketNameRE           = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
	reservedBucketPrefixes = []string{"virtscale-", "vs-", "s3-", "rgw-", "admin-", "internal-", "system-"}
	reservedBucketNames    = map[string]bool{"admin": true, "api": true, "www": true, "internal": true, "system": true, "backup": true, "billing": true, "metrics": true, "health": true}
)

func (bucketNameValidator) Description(context.Context) string {
	return "3 to 63 lower-case letters, digits and hyphens, starting and ending with a letter or digit, no consecutive hyphens, not a name or prefix the platform reserves"
}

func (v bucketNameValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (bucketNameValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	name := req.ConfigValue.ValueString()
	var problem string
	switch {
	case len(name) < 3 || len(name) > 63:
		problem = "bucket names must be between 3 and 63 characters"
	case strings.Contains(name, "."):
		problem = "dots are not permitted: they break virtual-hosted addressing under the platform's wildcard certificate"
	case strings.Contains(name, "--"):
		problem = "bucket names may not contain consecutive hyphens"
	case !bucketNameRE.MatchString(name):
		problem = "bucket names may contain only lowercase letters, digits and hyphens, and must start and end with a letter or digit"
	case reservedBucketNames[name]:
		problem = fmt.Sprintf("the bucket name '%s' is reserved by the platform", name)
	default:
		for _, prefix := range reservedBucketPrefixes {
			if strings.HasPrefix(name, prefix) {
				problem = fmt.Sprintf("bucket names starting with '%s' are reserved by the platform", prefix)
				break
			}
		}
	}
	if problem != "" {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid bucket name", fmt.Sprintf("%q is not valid: %s", name, problem))
	}
}

// atLeastOneValidator refuses an integer below one: the API takes no zero or negative ceiling.
type atLeastOneValidator struct{}

func (atLeastOneValidator) Description(context.Context) string { return "at least 1" }

func (v atLeastOneValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (atLeastOneValidator) ValidateInt64(_ context.Context, req validator.Int64Request, resp *validator.Int64Response) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if n := req.ConfigValue.ValueInt64(); n < 1 {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid ceiling", fmt.Sprintf("%d is not valid: use 1 or more, or leave the attribute out for no per-bucket cap", n))
	}
}

// Schema declares the virtscale_s3_bucket attributes.
func (r *S3BucketResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "An object-storage bucket. It is created, resized and deleted asynchronously: apply waits until the " +
			"bucket is active. Deleting a bucket is a soft delete: it is marked deleting_soft at once, and a worker cuts off " +
			"access to it afterwards (not instantly, and not guaranteed), its data is kept for a retention window (7 days) and " +
			"then purged (production's purge worker is on), and the organization's bucket slot is freed. Its name stays taken " +
			"until it is purged. Needs an API key with the storage:write (or write) scope.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed: true, Description: "The bucket's id (a UUID), assigned by the API.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required: true, Description: "The bucket name, unique within the organization: 3 to 63 lower-case letters, digits and " +
					"hyphens, starting and ending with a letter or digit. Changing it replaces the bucket.",
				Validators:    []validator.String{bucketNameValidator{}},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"versioning": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				Description: "Keep previous versions of overwritten and deleted objects. Fixed when the bucket is created: changing it is " +
					"refused at plan time, because replacing the bucket would soft-delete it and its name stays taken until it is purged.",
			},
			"force_destroy": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				Description: "Acknowledge, when the bucket is destroyed, that it may still hold objects (the API refuses a non-empty " +
					"bucket with 409 bucket_not_empty otherwise). The provider's own: not stored by the API, so import leaves it false.",
			},
			"max_size_bytes": schema.Int64Attribute{
				Optional: true, Description: "The most bytes this bucket may hold, inside the organization's ceiling. Leave it out for " +
					"no per-bucket cap (the organization's ceiling still applies). Changes in place.",
				Validators: []validator.Int64{atLeastOneValidator{}},
			},
			"max_objects": schema.Int64Attribute{
				Optional: true, Description: "The most objects this bucket may hold. Leave it out for no per-bucket cap. Changes in place.",
				Validators: []validator.Int64{atLeastOneValidator{}},
			},
			"state": schema.StringAttribute{
				Computed: true, Description: "requested, provisioning, active, provision_failed, suspended, degraded, or deleting_soft once deleted.",
			},
			"created_at": schema.StringAttribute{
				Computed: true, Description: "When the bucket was created, RFC 3339.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func nullableInt(p *int64) types.Int64 {
	if p == nil {
		return types.Int64Null()
	}
	return types.Int64Value(*p)
}

// applyBucket copies what the API says about the bucket into model. force_destroy is the provider's own and stays.
func applyBucket(model *s3BucketModel, b *client.Bucket) {
	model.ID = types.StringValue(b.ID)
	model.Name = types.StringValue(b.Name)
	model.Versioning = types.BoolValue(b.Versioning)
	model.MaxSizeBytes = nullableInt(b.Quota.MaxSizeBytes)
	model.MaxObjects = nullableInt(b.Quota.MaxObjects)
	model.State = types.StringValue(b.State)
	model.CreatedAt = types.StringValue(b.CreatedAt.Format(time.RFC3339))
	if model.ForceDestroy.IsNull() || model.ForceDestroy.IsUnknown() {
		model.ForceDestroy = types.BoolValue(false)
	}
}

// bucketGone is a state in which the bucket is deleted as far as a configuration is concerned.
func bucketGone(state string) bool {
	switch state {
	case "deleting_soft", "purging", "purged", "deleted":
		return true
	}
	return false
}

// apiConflict reports whether err is a 409 with the given code.
func apiConflict(err error, code string) bool {
	var apiErr *client.APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict && apiErr.Code == code
}

// quotaChange is the PUT that takes the bucket from have to want; changed is false when they are the same.
func quotaChange(haveSize, haveObjects, wantSize, wantObjects types.Int64) (client.BucketQuotaChange, bool) {
	var change client.BucketQuotaChange
	if !wantSize.Equal(haveSize) {
		change.SizeSet = true
		if !wantSize.IsNull() {
			n := wantSize.ValueInt64()
			change.Size = &n
		}
	}
	if !wantObjects.Equal(haveObjects) {
		change.ObjectsSet = true
		if !wantObjects.IsNull() {
			n := wantObjects.ValueInt64()
			change.Objects = &n
		}
	}
	return change, change.SizeSet || change.ObjectsSet
}

// ModifyPlan refuses a change of versioning: the API cannot change it, and the replacement a plan would otherwise make
// soft-deletes the bucket (cutting access to it) while its name stays taken for the retention window, so the new bucket
// could not be created under the same name.
func (r *S3BucketResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	var plan, state s3BucketModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	// A new name is a new bucket: the replacement the name makes can take any versioning.
	if resp.Diagnostics.HasError() || plan.Versioning.IsUnknown() || plan.Versioning.Equal(state.Versioning) || !plan.Name.Equal(state.Name) {
		return
	}
	resp.Diagnostics.AddAttributeError(path.Root("versioning"), "Versioning cannot be changed",
		"versioning is fixed when a bucket is created. Replacing the bucket would soft-delete it, and its name stays taken until it is "+
			"purged, so a bucket of the same name could not be created. Create a bucket under another name (and move the objects) instead.")
}

// inFlightRetries is how often a change of a bucket is repeated while the API says another operation runs for it: the
// executor makes the bucket active a moment before it marks the create done, and a ceiling that was just set is applied
// by an operation of its own.
const inFlightRetries = 12

// retryWhileInFlight repeats fn, waiting pollInterval between the tries, while it fails with 409 operation_in_flight.
func retryWhileInFlight(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 0; attempt <= inFlightRetries; attempt++ {
		if err = fn(); err == nil || !operationInFlight(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(pollInterval):
		}
	}
	return err
}

// Create asks for the bucket with a fresh Idempotency-Key, keeps what it knows in the state at once so that a failed
// build can still be destroyed, waits until the bucket is active, and then sets the ceilings that were configured.
func (r *S3BucketResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan s3BucketModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	key, err := newIdempotencyKey()
	if err != nil {
		resp.Diagnostics.AddError("Unable to Generate Idempotency Key", err.Error())
		return
	}
	b, err := r.client.CreateBucket(ctx, client.BucketCreateRequest{Name: plan.Name.ValueString(), Versioning: plan.Versioning.ValueBool()}, key)
	if err != nil {
		resp.Diagnostics.AddError("Unable to Create Bucket", apiErrorDetail(err))
		return
	}
	wantSize, wantObjects := plan.MaxSizeBytes, plan.MaxObjects
	applyBucket(&plan, b) // the state holds what the API holds (no ceilings yet); the configured ones follow
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	waitCtx, cancel := context.WithTimeout(ctx, createTimeout)
	defer cancel()
	final, waitErr := r.client.WaitBucket(waitCtx, b.ID, pollInterval, func(b *client.Bucket) (bool, error) {
		if b == nil {
			return false, errors.New("the bucket disappeared while it was being created")
		}
		switch b.State {
		case "active":
			return true, nil
		case "provision_failed", "degraded", "suspended":
			return true, errBucketBuildFailed
		}
		return false, nil
	})
	if errors.Is(waitErr, errBucketBuildFailed) && final != nil {
		resp.Diagnostics.AddError("Bucket Failed to Build", "the bucket ended in state "+final.State+
			". It is in the state, tainted by this error, and the next apply would replace it: the failed bucket's name stays taken "+
			"until it is purged, so give the bucket another name (or restore it in the panel and import it)")
		return
	}
	if waitErr != nil {
		resp.Diagnostics.AddError("Unable to Wait for Bucket to Become Active", waitErr.Error())
		return
	}
	applyBucket(&plan, final)

	if change, changed := quotaChange(types.Int64Null(), types.Int64Null(), wantSize, wantObjects); changed {
		quotaKey, err := newIdempotencyKey()
		if err != nil {
			resp.Diagnostics.AddError("Unable to Generate Idempotency Key", err.Error())
			return
		}
		if err := retryWhileInFlight(ctx, func() error {
			_, e := r.client.SetBucketQuota(ctx, b.ID, change, quotaKey)
			return e
		}); err != nil {
			// The bucket exists and has no ceilings: the state says so. Terraform taints a resource whose create failed, and a
			// replacement would meet the soft-deleted bucket's name.
			resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
			resp.Diagnostics.AddError("Unable to Set the Bucket's Ceilings", apiErrorDetail(err)+
				". The bucket was created and is in the state without ceilings, tainted by this error: its name would stay taken after a "+
				"replacement. Fix the ceilings in the configuration, run `tofu untaint` on the bucket, and apply again to set them")
			return
		}
		plan.MaxSizeBytes, plan.MaxObjects = wantSize, wantObjects
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read refreshes state from the API. The bucket 404, or a bucket that has been deleted, removes the resource from
// state; any other 404 is an error.
func (r *S3BucketResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state s3BucketModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	b, err := r.client.GetBucket(ctx, state.ID.ValueString())
	if err != nil {
		if client.IsNotFound(err, client.ResourceBucket) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Unable to Read Bucket", apiErrorDetail(err))
		return
	}
	if bucketGone(b.State) {
		resp.State.RemoveResource(ctx)
		return
	}
	applyBucket(&state, b)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update changes the ceilings in place, with one PUT of the dimensions that differ (a dimension that was removed from the
// configuration is cleared). force_destroy is the provider's own: changing it calls nothing.
func (r *S3BucketResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state s3BucketModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()
	if change, changed := quotaChange(state.MaxSizeBytes, state.MaxObjects, plan.MaxSizeBytes, plan.MaxObjects); changed {
		key, err := newIdempotencyKey()
		if err != nil {
			resp.Diagnostics.AddError("Unable to Generate Idempotency Key", err.Error())
			return
		}
		if err := retryWhileInFlight(ctx, func() error {
			_, e := r.client.SetBucketQuota(ctx, id, change, key)
			return e
		}); err != nil {
			resp.Diagnostics.AddError("Unable to Set the Bucket's Ceilings", apiErrorDetail(err))
			return
		}
	}
	b, err := r.client.GetBucket(ctx, id)
	if err != nil {
		resp.Diagnostics.AddError("Unable to Read Bucket", apiErrorDetail(err))
		return
	}
	wantSize, wantObjects := plan.MaxSizeBytes, plan.MaxObjects
	applyBucket(&plan, b)
	plan.MaxSizeBytes, plan.MaxObjects = wantSize, wantObjects // what was asked: the API holds it as the desired quota
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete soft-deletes the bucket. A bucket that holds objects is refused with 409 bucket_not_empty unless force_destroy
// acknowledges it. A bucket the API does not know any more is gone already, and a 409 bucket_already_deleting says it is
// already being deleted: the destroy waits until it reads gone.
func (r *S3BucketResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state s3BucketModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	key, err := newIdempotencyKey()
	if err != nil {
		resp.Diagnostics.AddError("Unable to Generate Idempotency Key", err.Error())
		return
	}
	err = retryWhileInFlight(ctx, func() error {
		_, e := r.client.DeleteBucket(ctx, state.ID.ValueString(), state.ForceDestroy.ValueBool(), key)
		return e
	})
	switch {
	case err == nil || client.IsNotFound(err, client.ResourceBucket):
		// gone already
	case apiConflict(err, "bucket_already_deleting"):
		// someone else deleted the bucket: wait until it reads gone
		waitCtx, cancel := context.WithTimeout(ctx, deleteTimeout)
		defer cancel()
		if _, waitErr := r.client.WaitBucket(waitCtx, state.ID.ValueString(), pollInterval, func(b *client.Bucket) (bool, error) {
			return b == nil || bucketGone(b.State), nil
		}); waitErr != nil {
			resp.Diagnostics.AddError("Unable to Wait for the Bucket to Be Deleted", waitErr.Error())
		}
	default:
		detail := apiErrorDetail(err)
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Code == "bucket_not_empty" {
			detail += "; to soft-delete it with its objects (they are kept for the retention window) set force_destroy = true, run " +
				"`tofu apply` so that the setting is stored, and destroy again"
		}
		resp.Diagnostics.AddError("Unable to Delete Bucket", detail)
	}
}

// ImportState imports by id, passthrough.
func (r *S3BucketResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
