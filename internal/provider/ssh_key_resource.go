package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"gitlab.virtscale.nl/virtscale/virtscale-platform/terraform-provider-virtscale/internal/client"
)

// Ensure SshKeyResource satisfies the expected resource interfaces.
var (
	_ resource.Resource                = &SshKeyResource{}
	_ resource.ResourceWithConfigure   = &SshKeyResource{}
	_ resource.ResourceWithImportState = &SshKeyResource{}
)

// SshKeyResource implements the virtscale_ssh_key resource: a public key
// imported into the organization, which instances take by name.
type SshKeyResource struct {
	client *client.Client
}

// NewSshKeyResource is the resource.Resource factory registered by the
// provider.
func NewSshKeyResource() resource.Resource {
	return &SshKeyResource{}
}

// sshKeyResourceModel mirrors the virtscale_ssh_key schema.
type sshKeyResourceModel struct {
	ID          types.String   `tfsdk:"id"`
	Name        types.String   `tfsdk:"name"`
	PublicKey   PublicKeyValue `tfsdk:"public_key"`
	KeyType     types.String   `tfsdk:"key_type"`
	Fingerprint types.String   `tfsdk:"fingerprint"`
	CreatedAt   types.String   `tfsdk:"created_at"`
}

// Metadata sets the resource's type name to <provider type name>_ssh_key.
func (r *SshKeyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_ssh_key"
}

// Schema declares the virtscale_ssh_key attributes. The key is immutable on the
// API side: a different name, or a different key, is a different resource. Only
// a change of the public_key text that leaves the key alone is applied in place,
// and it never calls the API.
func (r *SshKeyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "An SSH public key imported into the organization, which instances take by name " +
			"(virtscale_instance.ssh_keys). Only the public key is stored. cloud-init installs it for the " +
			"image's default user when an instance first boots, and an instance keeps the keys it was created " +
			"with when a key is deleted or replaced later.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "The key's id (a UUID), assigned by the API.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Required: true,
				Description: "The key's name, unique within the organization and case-sensitive: 1 to 64 " +
					"characters from letters, digits, '.', '_', '@' and '-', starting with a letter or digit. " +
					"Instances name the key by this. Changing it replaces the key.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"public_key": schema.StringAttribute{
				CustomType: PublicKeyType{},
				Required:   true,
				Description: "The OpenSSH public key: one line, \"<type> <base64> [comment]\", for example " +
					"file(\"~/.ssh/id_ed25519.pub\"). Types: ssh-ed25519, ecdsa-sha2-nistp256, " +
					"ecdsa-sha2-nistp384, ecdsa-sha2-nistp521, and ssh-rsa of 2048 to 16384 bits. The API stores " +
					"the key without its comment. Two texts are the same key when the key type and the key " +
					"bytes are the same, so a comment, surrounding whitespace, a trailing newline or CRLF " +
					"never replace the resource. They do show up as an in-place change of public_key: editing " +
					"only the comment does, and so does the first plan after an import, where the state holds " +
					"the API's comment-less text and the configuration holds the file. Applying it stores your " +
					"text and makes no API call. A different key replaces the resource.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplaceIf(
						replaceWhenTheKeyChanges,
						"If the key itself changes, Terraform will destroy and recreate the resource. A change of "+
							"the text alone (a comment, whitespace, line endings) is applied in place.",
						"If the key itself changes, Terraform will destroy and recreate the resource. A change of "+
							"the text alone (a comment, whitespace, line endings) is applied in place.",
					),
				},
			},
			"key_type": schema.StringAttribute{
				Computed:    true,
				Description: "The key type the API recognised, for example ssh-ed25519.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"fingerprint": schema.StringAttribute{
				Computed: true,
				Description: "The key's SHA-256 fingerprint as ssh-keygen -lf prints it: SHA256: and the " +
					"unpadded base64 of the SHA-256 of the key.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"created_at": schema.StringAttribute{
				Computed:    true,
				Description: "When the key was imported, RFC 3339.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
		},
	}
}

// replaceWhenTheKeyChanges is the RequiresReplaceIf test of public_key. The
// plan modifier only asks when the text differs; this says whether the key does
// too. A value that is not known yet cannot be compared and may be another key.
func replaceWhenTheKeyChanges(_ context.Context, req planmodifier.StringRequest, resp *stringplanmodifier.RequiresReplaceIfFuncResponse) {
	if req.PlanValue.IsUnknown() {
		resp.RequiresReplace = true
		return
	}
	resp.RequiresReplace = !publicKeysEquivalent(req.PlanValue.ValueString(), req.StateValue.ValueString())
}

// Configure receives the shared *client.Client the provider built.
func (r *SshKeyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// Create imports the key with a fresh idempotency key, so a request repeated
// after a lost answer gets the first answer back and imports once. The state
// takes what the API says; the framework keeps the configured text of
// public_key when the API's canonical form is the same key.
func (r *SshKeyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan sshKeyResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	idempotencyKey, err := newIdempotencyKey()
	if err != nil {
		resp.Diagnostics.AddError("Unable to Generate Idempotency Key", err.Error())
		return
	}

	key, err := r.client.CreateSshKey(ctx, client.SshKeyCreateRequest{
		Name:      plan.Name.ValueString(),
		PublicKey: plan.PublicKey.ValueString(),
	}, idempotencyKey)
	if err != nil {
		resp.Diagnostics.AddError("Unable to Import SSH Key", apiErrorDetail(err))
		return
	}

	applySshKeyToModel(&plan, key)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read refreshes state from the API. Only the ssh_key 404 of the API's shared
// not_found helper removes the resource from state; any other 404 is an error.
func (r *SshKeyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state sshKeyResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	key, err := r.client.GetSshKey(ctx, state.ID.ValueString())
	if err != nil {
		if client.IsNotFound(err, client.ResourceSshKey) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Unable to Read SSH Key", apiErrorDetail(err))
		return
	}

	applySshKeyToModel(&state, key)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is reached only when the text of public_key changed and the key did
// not (a different name, or a different key, replaces the resource, and every
// other attribute is computed). It makes no API call: the API already holds the
// key, and the state takes the configuration's text, so that the plan is empty
// afterwards.
func (r *SshKeyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state sshKeyResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	state.PublicKey = plan.PublicKey
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Delete removes the key. Instances that were created with it keep it. The
// ssh_key 404 means the key is gone already, which is what destroy wanted; any
// other 404 is an error.
func (r *SshKeyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state sshKeyResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.client.DeleteSshKey(ctx, state.ID.ValueString()); err != nil && !client.IsNotFound(err, client.ResourceSshKey) {
		resp.Diagnostics.AddError("Unable to Delete SSH Key", apiErrorDetail(err))
	}
}

// ImportState imports by id, passthrough. Read then fills in the rest, with the
// API's canonical text of the key.
func (r *SshKeyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// applySshKeyToModel copies what the API says about the key into model.
func applySshKeyToModel(model *sshKeyResourceModel, key *client.SshKey) {
	model.ID = types.StringValue(key.ID)
	model.Name = types.StringValue(key.Name)
	model.PublicKey = NewPublicKeyValue(key.PublicKey)
	model.KeyType = types.StringValue(key.KeyType)
	model.Fingerprint = types.StringValue(key.Fingerprint)
	model.CreatedAt = types.StringValue(key.CreatedAt.Format(time.RFC3339))
}

// --- the public key, and when two texts are one key -------------------------

// parsePublicKey reads one OpenSSH public key line, "<type> <base64> [comment]",
// possibly surrounded by whitespace or ended by CRLF, and returns the key type
// and the decoded key bytes. ok is false for anything else: nothing, a type
// with no base64 after it, text that is not base64, more than one line, an
// options prefix such as command="...". The caller then compares the texts
// exactly, so a bad key still reaches the API and its 422 is shown.
//
// The bytes are what two texts of one key have in common. The base64 text is not
// (the last character may carry spare bits, which decode to nothing), and nor is
// the comment.
func parsePublicKey(text string) (keyType string, blob []byte, ok bool) {
	text = strings.TrimSpace(text)
	if text == "" || strings.ContainsAny(text, "\r\n") {
		return "", nil, false
	}
	fields := strings.Fields(text)
	if len(fields) < 2 {
		return "", nil, false
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil || len(blob) == 0 {
		return "", nil, false
	}
	return fields[0], blob, true
}

// publicKeysEquivalent reports whether two texts are the same key: the same
// key type and the same decoded bytes, whatever the comment, the whitespace, the
// line ending or the spare bits of the base64. When either text does not parse,
// only identical text is equal.
func publicKeysEquivalent(a, b string) bool {
	if a == b {
		return true
	}
	typeA, blobA, okA := parsePublicKey(a)
	typeB, blobB, okB := parsePublicKey(b)
	return okA && okB && typeA == typeB && bytes.Equal(blobA, blobB)
}

var (
	_ basetypes.StringTypable                    = PublicKeyType{}
	_ basetypes.StringValuableWithSemanticEquals = PublicKeyValue{}
)

// PublicKeyType is the type of virtscale_ssh_key.public_key: a string whose
// values know when two texts are the same key. The API answers with the key in
// canonical form, without the comment; the configuration holds what the user
// wrote. The framework asks the value, after Create and Read, whether the two
// are the same key, and keeps the configured text when they are.
type PublicKeyType struct {
	basetypes.StringType
}

// Equal reports whether o is the same type.
func (t PublicKeyType) Equal(o attr.Type) bool {
	other, ok := o.(PublicKeyType)
	if !ok {
		return false
	}
	return t.StringType.Equal(other.StringType)
}

// String names the type, for diagnostics.
func (t PublicKeyType) String() string {
	return "PublicKeyType"
}

// ValueFromString wraps a plain string value.
func (t PublicKeyType) ValueFromString(_ context.Context, in basetypes.StringValue) (basetypes.StringValuable, diag.Diagnostics) {
	return PublicKeyValue{StringValue: in}, nil
}

// ValueFromTerraform wraps a value that came from Terraform.
func (t PublicKeyType) ValueFromTerraform(ctx context.Context, in tftypes.Value) (attr.Value, error) {
	attrValue, err := t.StringType.ValueFromTerraform(ctx, in)
	if err != nil {
		return nil, err
	}
	stringValue, ok := attrValue.(basetypes.StringValue)
	if !ok {
		return nil, fmt.Errorf("unexpected value type of %T", attrValue)
	}
	stringValuable, diags := t.ValueFromString(ctx, stringValue)
	if diags.HasError() {
		return nil, fmt.Errorf("unexpected error converting StringValue to StringValuable: %v", diags)
	}
	return stringValuable, nil
}

// ValueType is the value this type holds.
func (t PublicKeyType) ValueType(_ context.Context) attr.Value {
	return PublicKeyValue{}
}

// PublicKeyValue is a public key as text. Its semantic equality is
// publicKeysEquivalent.
type PublicKeyValue struct {
	basetypes.StringValue
}

// NewPublicKeyValue is a known public key text.
func NewPublicKeyValue(text string) PublicKeyValue {
	return PublicKeyValue{StringValue: basetypes.NewStringValue(text)}
}

// NewPublicKeyNull is a null public key.
func NewPublicKeyNull() PublicKeyValue {
	return PublicKeyValue{StringValue: basetypes.NewStringNull()}
}

// Equal reports whether o is a PublicKeyValue with the same text. (Whether it is
// the same key is StringSemanticEquals.)
func (v PublicKeyValue) Equal(o attr.Value) bool {
	other, ok := o.(PublicKeyValue)
	if !ok {
		return false
	}
	return v.StringValue.Equal(other.StringValue)
}

// Type is PublicKeyType.
func (v PublicKeyValue) Type(_ context.Context) attr.Type {
	return PublicKeyType{}
}

// StringSemanticEquals reports whether newValuable is the same key as v: the
// same key type and the same decoded bytes, ignoring the comment, surrounding
// whitespace, line endings and the spare bits of the base64. When either text
// does not parse, only identical text is the same.
func (v PublicKeyValue) StringSemanticEquals(_ context.Context, newValuable basetypes.StringValuable) (bool, diag.Diagnostics) {
	var diags diag.Diagnostics

	newValue, ok := newValuable.(PublicKeyValue)
	if !ok {
		diags.AddError(
			"Semantic Equality Check Error",
			fmt.Sprintf("Expected value type %T but got value type %T. Please report this to the provider developers.", v, newValuable),
		)
		return false, diags
	}
	return publicKeysEquivalent(v.ValueString(), newValue.ValueString()), diags
}
