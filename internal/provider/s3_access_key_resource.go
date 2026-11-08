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

var (
	_ resource.Resource                = &S3AccessKeyResource{}
	_ resource.ResourceWithConfigure   = &S3AccessKeyResource{}
	_ resource.ResourceWithImportState = &S3AccessKeyResource{}
)

var errAccessKeyFailed = errors.New("access key entered a failed state")

// S3AccessKeyResource implements virtscale_s3_access_key: an S3 credential for the organization's storage.
type S3AccessKeyResource struct {
	clientResource
}

// NewS3AccessKeyResource is the resource.Resource factory registered by the provider.
func NewS3AccessKeyResource() resource.Resource {
	return &S3AccessKeyResource{}
}

type s3AccessKeyModel struct {
	ID              types.String `tfsdk:"id"`
	AccessKeyID     types.String `tfsdk:"access_key_id"`
	SecretAccessKey types.String `tfsdk:"secret_access_key"`
	State           types.String `tfsdk:"state"`
	CreatedAt       types.String `tfsdk:"created_at"`
}

// Metadata sets the resource's type name to <provider type name>_s3_access_key.
func (r *S3AccessKeyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_s3_access_key"
}

// Schema declares the virtscale_s3_access_key attributes. There is nothing to configure: a key is minted, and revoked
// when the resource is destroyed (replace_triggered_by rotates it).
func (r *S3AccessKeyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "An S3 access key for the organization's storage. The secret is returned exactly once, by the request " +
			"that mints the key, and nowhere else: the provider keeps it in the state, marked sensitive, so the state holds a " +
			"credential and must be protected (an encrypted backend, or OpenTofu state encryption). It cannot be read back, " +
			"so an imported key has none. Destroying the resource revokes the key. Note that the key is a key on the " +
			"organization's own storage owner: it can delete the organization's buckets and objects directly over S3, " +
			"bypassing the soft-delete window. An organization holds at most 5 active access keys: a sixth is refused " +
			"with 422 access_key_limit_reached until one is revoked. Needs an API key with the storage:write (or write) scope.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed: true, Description: "The key's id (a UUID), assigned by the API.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"access_key_id": schema.StringAttribute{
				Computed: true, Description: "The public half of the credential, to use as the S3 access key id.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"secret_access_key": schema.StringAttribute{
				Computed: true, Sensitive: true,
				Description:   "The secret half of the credential. Known only after the key is created here; null for an imported key.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"state": schema.StringAttribute{
				Computed:    true,
				Description: "requested, provisioning, active, revoking, revoked or degraded.",
			},
			"created_at": schema.StringAttribute{
				Computed: true, Description: "When the key was created, RFC 3339.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func applyAccessKey(model *s3AccessKeyModel, k *client.AccessKey) {
	model.ID = types.StringValue(k.ID)
	model.State = types.StringValue(k.State)
	model.CreatedAt = types.StringValue(k.CreatedAt.Format(time.RFC3339))
	model.AccessKeyID = types.StringNull()
	if k.RgwAccessKeyID != nil {
		model.AccessKeyID = types.StringValue(*k.RgwAccessKeyID)
	}
}

// Create mints the key with a fresh Idempotency-Key and keeps the secret from that one answer in the state at once. A
// repeated request meets the stored replay, which has no secret: the key exists, so it is kept (to be revoked), and the
// error says what happened. Then it waits until the key is active.
func (r *S3AccessKeyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan s3AccessKeyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	key, err := newIdempotencyKey()
	if err != nil {
		resp.Diagnostics.AddError("Unable to Generate Idempotency Key", err.Error())
		return
	}
	ak, secret, err := r.client.CreateAccessKey(ctx, key)
	if err != nil {
		resp.Diagnostics.AddError("Unable to Create Access Key", apiErrorDetail(err))
		return
	}
	applyAccessKey(&plan, ak)
	plan.SecretAccessKey = types.StringNull()
	if secret != nil {
		plan.SecretAccessKey = types.StringValue(*secret)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if secret == nil {
		resp.Diagnostics.AddError("Access Key Secret Not Returned", fmt.Sprintf(
			"the access key %s was created but its secret was not returned: the answer to a repeated request never holds it. "+
				"The key is kept in the state; run `tofu destroy` (or apply again, which replaces it) to revoke it and get a new one", ak.ID))
		return
	}

	waitCtx, cancel := context.WithTimeout(ctx, createTimeout)
	defer cancel()
	final, waitErr := r.client.WaitAccessKey(waitCtx, ak.ID, pollInterval, func(k *client.AccessKey) (bool, error) {
		if k == nil {
			return false, errors.New("the access key disappeared while it was being created")
		}
		switch k.State {
		case "active":
			return k.RgwAccessKeyID != nil, nil
		case "revoked", "revoking", "degraded":
			return true, errAccessKeyFailed
		}
		return false, nil
	})
	if errors.Is(waitErr, errAccessKeyFailed) && final != nil {
		resp.Diagnostics.AddError("Access Key Failed", "the access key ended in state "+final.State)
		return
	}
	if waitErr != nil {
		resp.Diagnostics.AddError("Unable to Wait for the Access Key to Become Active", waitErr.Error())
		return
	}
	applyAccessKey(&plan, final)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read refreshes state from the API, keeping the secret the state holds. A key that is not listed any more, or that is
// revoked, is removed from state.
func (r *S3AccessKeyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state s3AccessKeyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	k, err := r.client.GetAccessKey(ctx, state.ID.ValueString())
	if err != nil {
		if client.IsNotFound(err, client.ResourceAccessKey) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Unable to Read Access Key", apiErrorDetail(err))
		return
	}
	if k.State == "revoked" || k.State == "revoking" {
		resp.State.RemoveResource(ctx)
		return
	}
	applyAccessKey(&state, k)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is never called: there is no attribute to change. It refreshes the computed values, if the framework asks.
func (r *S3AccessKeyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan s3AccessKeyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete revokes the key and waits until it reads revoked. A key the API does not know any more, or that is already
// revoked, is done; a key somebody else is revoking (409 access_key_already_revoking) is waited for until it reads revoked.
func (r *S3AccessKeyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state s3AccessKeyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	key, err := newIdempotencyKey()
	if err != nil {
		resp.Diagnostics.AddError("Unable to Generate Idempotency Key", err.Error())
		return
	}
	id := state.ID.ValueString()
	if _, err := r.client.RevokeAccessKey(ctx, id, key); err != nil {
		switch {
		case client.IsNotFound(err, client.ResourceAccessKey):
			return // the key is not listed any more: revoked already
		case apiConflict(err, "access_key_already_revoked"):
			return // the key is revoked already
		case !apiConflict(err, "access_key_already_revoking"):
			resp.Diagnostics.AddError("Unable to Revoke Access Key", apiErrorDetail(err))
			return
		}
		// someone else is revoking the key: wait until it reads revoked
	}
	waitCtx, cancel := context.WithTimeout(ctx, deleteTimeout)
	defer cancel()
	if final, err := r.client.WaitAccessKey(waitCtx, id, pollInterval, func(k *client.AccessKey) (bool, error) {
		if k != nil && k.State == "degraded" {
			return true, errAccessKeyFailed
		}
		return k == nil || k.State == "revoked", nil
	}); errors.Is(err, errAccessKeyFailed) && final != nil {
		resp.Diagnostics.AddError("Access Key Not Revoked", "revoking the access key failed: it ended in state degraded, so it may still work. Revoke it again in the panel or with the API")
	} else if err != nil {
		resp.Diagnostics.AddError("Unable to Wait for the Access Key to Be Revoked", err.Error())
	}
}

// ImportState imports by id, passthrough. The secret cannot be read back, so an imported key has none.
func (r *S3AccessKeyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
