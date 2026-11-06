package client

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// FirewallRule is one rule as the API confirms it (FirewallRuleOut). The platform's own id of the rule is not
// decoded: it is not part of what a customer manages.
type FirewallRule struct {
	Direction   string `json:"direction"`
	Protocol    string `json:"protocol"`
	PortMin     *int   `json:"port_min"`
	PortMax     *int   `json:"port_max"`
	RemoteCIDR  string `json:"remote_cidr"`
	Description string `json:"description"`
}

// Firewall is an instance's whole rule set (FirewallOut): the rows of the last PUT, shown at once, with the State of
// applying them: pending while the change is being applied, applied once it is, error when it could not be (Error says
// why). (The API's docstring says "never the mid-flight one"; its code and its own test show the new rows at once.)
type Firewall struct {
	VmID  string         `json:"vm_id"`
	State string         `json:"state"`
	Error *string        `json:"error"`
	Rules []FirewallRule `json:"rules"`
}

// FirewallRuleIn is one rule as a PUT sends it (FirewallRuleIn).
type FirewallRuleIn struct {
	Direction   string `json:"direction"`
	Protocol    string `json:"protocol"`
	PortMin     *int   `json:"port_min,omitempty"`
	PortMax     *int   `json:"port_max,omitempty"`
	RemoteCIDR  string `json:"remote_cidr"`
	Description string `json:"description"`
}

// GetFirewall sends GET /v1/orgs/{org}/compute/instances/{id}/firewall.
func (c *Client) GetFirewall(ctx context.Context, vmID string) (*Firewall, error) {
	var out Firewall
	if err := c.doJSON(ctx, http.MethodGet, c.orgPath("compute/instances/"+url.PathEscape(vmID)+"/firewall"), nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetFirewall sends PUT .../firewall, which replaces the whole rule set and answers 202 with a firewall.apply
// operation. The rules are required, so an empty set is sent as [] and clears the firewall. The Idempotency-Key makes
// the PUT safe to repeat after a lost answer.
func (c *Client) SetFirewall(ctx context.Context, vmID string, rules []FirewallRuleIn, idempotencyKey string) (*Operation, error) {
	if rules == nil {
		rules = []FirewallRuleIn{}
	}
	body := struct {
		Rules []FirewallRuleIn `json:"rules"`
	}{Rules: rules}
	var out vmAccepted
	if err := c.doJSON(ctx, http.MethodPut, c.orgPath("compute/instances/"+url.PathEscape(vmID)+"/firewall"), body, idempotencyKey, &out); err != nil {
		return nil, err
	}
	return &out.Operation, nil
}

// WaitFirewall polls GetFirewall until done reports true, with Poll's rules; the 404 that says the instance is gone is
// done(nil).
func (c *Client) WaitFirewall(ctx context.Context, vmID string, pollEvery time.Duration, done func(*Firewall) (bool, error)) (*Firewall, error) {
	return Poll(ctx, pollEvery, "the firewall of vm "+vmID,
		func(ctx context.Context) (*Firewall, error) { return c.GetFirewall(ctx, vmID) },
		func(err error) bool { return IsNotFound(err, ResourceVm) },
		done, func(f *Firewall) string { return "state " + f.State })
}
