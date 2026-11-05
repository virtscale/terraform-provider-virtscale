package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"gitlab.virtscale.nl/virtscale/virtscale-platform/terraform-provider-virtscale/internal/client"
)

var (
	_ resource.Resource                   = &InstanceFirewallResource{}
	_ resource.ResourceWithConfigure      = &InstanceFirewallResource{}
	_ resource.ResourceWithImportState    = &InstanceFirewallResource{}
	_ resource.ResourceWithValidateConfig = &InstanceFirewallResource{}
)

var errFirewallApplyFailed = errors.New("firewall entered error state")

// maxFirewallRules is how many rules the API takes for one instance.
const maxFirewallRules = 50

// InstanceFirewallResource implements virtscale_instance_firewall: the whole rule set of one instance. The API
// replaces the set as one, so the resource owns all of it: rules added elsewhere show as a difference and are removed
// by the next apply.
type InstanceFirewallResource struct {
	clientResource
}

// NewInstanceFirewallResource is the resource.Resource factory registered by the provider.
func NewInstanceFirewallResource() resource.Resource {
	return &InstanceFirewallResource{}
}

type instanceFirewallModel struct {
	ID         types.String `tfsdk:"id"`
	InstanceID types.String `tfsdk:"instance_id"`
	State      types.String `tfsdk:"state"`
	Rules      types.Set    `tfsdk:"rule"`
}

type firewallRuleModel struct {
	Direction   types.String `tfsdk:"direction"`
	Protocol    types.String `tfsdk:"protocol"`
	PortMin     types.Int64  `tfsdk:"port_min"`
	PortMax     types.Int64  `tfsdk:"port_max"`
	RemoteCIDR  types.String `tfsdk:"remote_cidr"`
	Description types.String `tfsdk:"description"`
}

var firewallRuleType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"direction": types.StringType, "protocol": types.StringType, "port_min": types.Int64Type,
	"port_max": types.Int64Type, "remote_cidr": types.StringType, "description": types.StringType,
}}

// Metadata sets the resource's type name to <provider type name>_instance_firewall.
func (r *InstanceFirewallResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_instance_firewall"
}

// Schema declares the virtscale_instance_firewall attributes: the instance, and the rules as a set of blocks.
func (r *InstanceFirewallResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "The firewall rules of one instance. The resource owns the whole rule set: the API replaces it as one " +
			"and applies it asynchronously, so apply waits until it is applied. Destroying the resource clears the " +
			"instance's rules. Needs an API key with the network:write scope.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed: true, Description: "The instance's id.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"instance_id": schema.StringAttribute{
				Required: true, Description: "The id of the instance. Changing it replaces the resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"state": schema.StringAttribute{
				Computed:    true,
				Description: "pending while the API applies a change, applied once it has, error when it could not.",
			},
		},
		Blocks: map[string]schema.Block{
			"rule": schema.SetNestedBlock{
				Description: "A rule. At most 50. The order does not matter.",
				NestedObject: schema.NestedBlockObject{Attributes: map[string]schema.Attribute{
					"direction": schema.StringAttribute{Required: true, Description: "ingress or egress."},
					"protocol":  schema.StringAttribute{Required: true, Description: "tcp, udp, icmp or any."},
					"port_min": schema.Int64Attribute{Optional: true,
						Description: "The first port, 1 to 65535. Required for tcp and udp, not allowed for icmp and any."},
					"port_max": schema.Int64Attribute{Optional: true,
						Description: "The last port, 1 to 65535, not below port_min. Required for tcp and udp, not allowed for icmp and any."},
					"remote_cidr": schema.StringAttribute{Required: true,
						Description: "The remote network, an IPv4 CIDR with no host bits set: 10.0.0.0/24, not 10.0.0.5/24."},
					"description": schema.StringAttribute{Optional: true,
						Description: "Free text, 1 to 255 characters; leave it out for none."},
				}},
			},
		},
	}
}

// ValidateConfig refuses at plan time what the API would refuse (422 invalid_rule), in its words. A rule that holds an
// unknown value cannot be judged yet.
func (r *InstanceFirewallResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var rules types.Set
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("rule"), &rules)...)
	if resp.Diagnostics.HasError() || rules.IsNull() || rules.IsUnknown() {
		return
	}
	if n := len(rules.Elements()); n > maxFirewallRules {
		resp.Diagnostics.AddAttributeError(path.Root("rule"), "Too many firewall rules", fmt.Sprintf("%d rules; an instance takes at most %d", n, maxFirewallRules))
	}
	var models []firewallRuleModel
	if diags := rules.ElementsAs(ctx, &models, false); diags.HasError() {
		return // an unknown element: judged when it is known
	}
	for _, m := range models {
		if sentence := firewallRuleRefusal(m); sentence != "" {
			resp.Diagnostics.AddAttributeError(path.Root("rule"), "Invalid firewall rule",
				fmt.Sprintf("%s %s %s: %s", m.Direction.ValueString(), m.Protocol.ValueString(), m.RemoteCIDR.ValueString(), sentence))
		}
	}
}

// firewallRuleRefusal is the sentence the API would refuse the rule with (domain/compute/edge.py _validate_rule), or "".
func firewallRuleRefusal(m firewallRuleModel) string {
	if m.Direction.IsUnknown() || m.Protocol.IsUnknown() || m.RemoteCIDR.IsUnknown() || m.PortMin.IsUnknown() || m.PortMax.IsUnknown() || m.Description.IsUnknown() {
		return ""
	}
	switch m.Direction.ValueString() {
	case "ingress", "egress":
	default:
		return fmt.Sprintf(`direction value must be one of: "ingress", "egress", got: %q`, m.Direction.ValueString())
	}
	protocol := m.Protocol.ValueString()
	switch protocol {
	case "tcp", "udp":
		if m.PortMin.IsNull() || m.PortMax.IsNull() {
			return protocol + " rules need a port range"
		}
		lo, hi := m.PortMin.ValueInt64(), m.PortMax.ValueInt64()
		if lo < 1 || lo > 65535 || hi < 1 || hi > 65535 {
			return "ports must be between 1 and 65535"
		}
		if lo > hi {
			return "port_min must not exceed port_max"
		}
	case "icmp", "any":
		if !m.PortMin.IsNull() || !m.PortMax.IsNull() {
			return protocol + " rules carry no port range"
		}
	default:
		return fmt.Sprintf(`protocol value must be one of: "tcp", "udp", "icmp", "any", got: %q`, protocol)
	}
	raw := m.RemoteCIDR.ValueString()
	ip, network, err := net.ParseCIDR(raw)
	switch {
	case err != nil || ip.To4() == nil || strings.Contains(raw, ":"):
		return fmt.Sprintf("%q is not an IPv4 CIDR", raw)
	case network.String() != raw:
		return fmt.Sprintf("%q is not a canonical CIDR: use %q", raw, network.String())
	}
	if !m.Description.IsNull() {
		if n := len([]rune(m.Description.ValueString())); n == 0 || n > 255 {
			return "description, when given, is 1 to 255 characters"
		}
	}
	return ""
}

// rulesToAPI turns the planned rule set into the PUT's body.
func rulesToAPI(ctx context.Context, set types.Set) ([]client.FirewallRuleIn, diag.Diagnostics) {
	var models []firewallRuleModel
	var diags diag.Diagnostics
	if !set.IsNull() && !set.IsUnknown() {
		diags = set.ElementsAs(ctx, &models, false)
	}
	out := make([]client.FirewallRuleIn, 0, len(models))
	for _, m := range models {
		rule := client.FirewallRuleIn{
			Direction: m.Direction.ValueString(), Protocol: m.Protocol.ValueString(),
			RemoteCIDR: m.RemoteCIDR.ValueString(), Description: m.Description.ValueString(),
		}
		if !m.PortMin.IsNull() {
			n := int(m.PortMin.ValueInt64())
			rule.PortMin = &n
		}
		if !m.PortMax.IsNull() {
			n := int(m.PortMax.ValueInt64())
			rule.PortMax = &n
		}
		out = append(out, rule)
	}
	return out, diags
}

// rulesFromAPI turns the confirmed rules into the state's set. The API's empty description is the block's absent one.
func rulesFromAPI(ctx context.Context, rules []client.FirewallRule) (types.Set, diag.Diagnostics) {
	models := make([]firewallRuleModel, 0, len(rules))
	for _, r := range rules {
		m := firewallRuleModel{
			Direction: types.StringValue(r.Direction), Protocol: types.StringValue(r.Protocol),
			PortMin: types.Int64Null(), PortMax: types.Int64Null(), RemoteCIDR: types.StringValue(r.RemoteCIDR),
			Description: types.StringNull(),
		}
		if r.PortMin != nil {
			m.PortMin = types.Int64Value(int64(*r.PortMin))
		}
		if r.PortMax != nil {
			m.PortMax = types.Int64Value(int64(*r.PortMax))
		}
		if r.Description != "" {
			m.Description = types.StringValue(r.Description)
		}
		models = append(models, m)
	}
	return types.SetValueFrom(ctx, firewallRuleType, models)
}

// apply replaces the instance's rule set and waits until the API says applied, then takes the confirmed rules into
// model. A set that ends in state error fails with the API's text; the model keeps the planned rules then, and the
// state is saved so that the next apply can try again.
func (r *InstanceFirewallResource) apply(ctx context.Context, model *instanceFirewallModel, rules []client.FirewallRuleIn, state *tfsdkState) {
	key, err := newIdempotencyKey()
	if err != nil {
		state.diags.AddError("Unable to Generate Idempotency Key", err.Error())
		return
	}
	id := model.InstanceID.ValueString()
	if _, err := r.client.SetFirewall(ctx, id, rules, key); err != nil {
		state.diags.AddError("Unable to Set Firewall", apiErrorDetail(err))
		return
	}
	model.ID = types.StringValue(id)
	model.State = types.StringValue("pending")
	state.save()

	waitCtx, cancel := context.WithTimeout(ctx, createTimeout)
	defer cancel()
	final, waitErr := r.client.WaitFirewall(waitCtx, id, pollInterval, func(f *client.Firewall) (bool, error) {
		if f == nil {
			return false, errors.New("the instance disappeared while its firewall was being changed")
		}
		switch f.State {
		case "applied":
			return true, nil
		case "error":
			return true, errFirewallApplyFailed
		}
		return false, nil
	})
	if errors.Is(waitErr, errFirewallApplyFailed) && final != nil {
		model.State = types.StringValue(final.State)
		state.save()
		reason := "the platform gave no reason"
		if final.Error != nil {
			reason = *final.Error
		}
		state.diags.AddError("Firewall Failed to Apply", "the rule set ended in state error: "+reason)
		return
	}
	if waitErr != nil {
		state.diags.AddError("Unable to Wait for the Firewall to Be Applied", waitErr.Error())
		return
	}
	set, diags := rulesFromAPI(ctx, final.Rules)
	state.diags.Append(diags...)
	model.Rules, model.State = set, types.StringValue(final.State)
	state.save()
}

// tfsdkState lets apply save the state of a create or an update response the same way.
type tfsdkState struct {
	diags diag.Diagnostics
	save  func()
}

// Create sets the rules and waits until they are applied.
func (r *InstanceFirewallResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan instanceFirewallModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	rules, diags := rulesToAPI(ctx, plan.Rules)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	st := &tfsdkState{}
	st.save = func() { st.diags.Append(resp.State.Set(ctx, &plan)...) }
	r.apply(ctx, &plan, rules, st)
	resp.Diagnostics.Append(st.diags...)
}

// Update replaces the rule set with the planned one.
func (r *InstanceFirewallResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan instanceFirewallModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	rules, diags := rulesToAPI(ctx, plan.Rules)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	st := &tfsdkState{}
	st.save = func() { st.diags.Append(resp.State.Set(ctx, &plan)...) }
	r.apply(ctx, &plan, rules, st)
	resp.Diagnostics.Append(st.diags...)
}

// Read refreshes the rules from the API. Only the instance 404 of the API's shared not_found helper removes the
// resource from state; any other 404 is an error.
func (r *InstanceFirewallResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state instanceFirewallModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	f, err := r.client.GetFirewall(ctx, state.InstanceID.ValueString())
	if err != nil {
		if client.IsNotFound(err, client.ResourceVm) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Unable to Read Firewall", apiErrorDetail(err))
		return
	}
	set, diags := rulesFromAPI(ctx, f.Rules)
	resp.Diagnostics.Append(diags...)
	state.ID, state.Rules, state.State = types.StringValue(f.VmID), set, types.StringValue(f.State)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Delete clears the instance's rules. An instance that is gone has no rules to clear.
func (r *InstanceFirewallResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state instanceFirewallModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	key, err := newIdempotencyKey()
	if err != nil {
		resp.Diagnostics.AddError("Unable to Generate Idempotency Key", err.Error())
		return
	}
	id := state.InstanceID.ValueString()
	if _, err := r.client.SetFirewall(ctx, id, nil, key); err != nil {
		if !client.IsNotFound(err, client.ResourceVm) {
			resp.Diagnostics.AddError("Unable to Clear Firewall", apiErrorDetail(err))
		}
		return
	}
	waitCtx, cancel := context.WithTimeout(ctx, deleteTimeout)
	defer cancel()
	if _, err := r.client.WaitFirewall(waitCtx, id, pollInterval, func(f *client.Firewall) (bool, error) {
		if f == nil || f.State == "applied" {
			return true, nil
		}
		if f.State == "error" {
			reason := "the platform gave no reason"
			if f.Error != nil {
				reason = *f.Error
			}
			return true, fmt.Errorf("the empty rule set ended in state error: %s", reason)
		}
		return false, nil
	}); err != nil {
		resp.Diagnostics.AddError("Unable to Clear Firewall", err.Error())
	}
}

// ImportState imports by the instance's id: the firewall is addressed through the instance.
func (r *InstanceFirewallResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("instance_id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
