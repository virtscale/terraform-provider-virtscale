package provider

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

// An instance's firewall (routes/compute.py, domain/compute/edge.py of s3-control-plane 87607618): GET shows the rules
// last confirmed and a state, PUT replaces the whole set and answers 202 with the instance and a firewall.apply
// operation. Shapes are FirewallOut, FirewallRuleOut and VmAccepted of customer-v1.json.

type wireFirewallRuleOut struct {
	ID          string `json:"id"`
	Direction   string `json:"direction"`
	Protocol    string `json:"protocol"`
	PortMin     *int   `json:"port_min"`
	PortMax     *int   `json:"port_max"`
	RemoteCIDR  string `json:"remote_cidr"`
	Description string `json:"description"`
}

type wireFirewallOut struct {
	VmID  string                `json:"vm_id"`
	State string                `json:"state"`
	Error *string               `json:"error"`
	Rules []wireFirewallRuleOut `json:"rules"`
}

type wireFirewallRuleIn struct {
	Direction   string `json:"direction"`
	Protocol    string `json:"protocol"`
	PortMin     *int   `json:"port_min"`
	PortMax     *int   `json:"port_max"`
	RemoteCIDR  string `json:"remote_cidr"`
	Description string `json:"description"`
}

// fakeFirewall is one instance's rule set: applied is what firewall.apply last wrote, pending what it is writing.
type fakeFirewall struct {
	applied, pending []wireFirewallRuleOut
	state            string
	err              *string
	stepsLeft        int
	fail             bool
	lag              int // reads after a PUT that still show the old set as applied, before the state flips to pending
}

type firewallState struct {
	byVm    map[string]*fakeFirewall
	ruleSeq int
	puts    []string // the rule set of every PUT, as JSON
	failNew bool
}

func (f *fakeAPI) fw(id string) *fakeFirewall {
	if f.fws.byVm == nil {
		f.fws.byVm = map[string]*fakeFirewall{}
	}
	fw, ok := f.fws.byVm[id]
	if !ok {
		fw = &fakeFirewall{state: "applied"}
		f.fws.byVm[id] = fw
	}
	return fw
}

// SetFirewallLag makes the next PUT of the instance show the old rule set as applied for n reads before the state flips
// to pending: an API whose state change is asynchronous. A client that stops at the first applied reads the old rules.
func (f *fakeAPI) SetFirewallLag(vmID string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fw(vmID).lag = n
}

// FailFirewallApply makes the next firewall.apply of the instance end in state error.
func (f *fakeAPI) FailFirewallApply(vmID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fw(vmID).fail = true
}

// FirewallRules is the applied rule set of an instance, as "direction protocol min-max cidr" lines.
func (f *fakeAPI) FirewallRules(vmID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []string{}
	for _, r := range f.fw(vmID).applied {
		out = append(out, fmt.Sprintf("%s %s %s-%s %s", r.Direction, r.Protocol, intOrDash(r.PortMin), intOrDash(r.PortMax), r.RemoteCIDR))
	}
	return out
}

// FirewallPuts is the number of PUTs the firewall routes have seen.
func (f *fakeAPI) FirewallPuts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.fws.puts)
}

func intOrDash(p *int) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprint(*p)
}

func (f *fakeAPI) advanceFirewall(fw *fakeFirewall) {
	if fw.state != "pending" {
		return
	}
	if fw.lag > 0 {
		fw.lag--
		return
	}
	if fw.stepsLeft--; fw.stepsLeft > 0 {
		return
	}
	if fw.fail {
		fw.fail = false
		msg := "the rules could not be applied to the network"
		fw.state, fw.err = "error", &msg
		return
	}
	fw.applied, fw.pending, fw.state, fw.err = fw.pending, nil, "applied", nil
}

func (f *fakeAPI) getFirewall(rc *reqCtx, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if vm, ok := f.vms[id]; !ok || vm.deletedAt != nil {
		rc.notFound("vm")
		return
	}
	fw := f.fw(id)
	f.advanceFirewall(fw)
	rules := fw.applied
	if rules == nil {
		rules = []wireFirewallRuleOut{}
	}
	state := fw.state
	if state == "pending" && fw.lag > 0 {
		state = "applied" // the change has not shown yet
	}
	rc.json(http.StatusOK, wireFirewallOut{VmID: id, State: state, Error: fw.err, Rules: rules})
}

// putFirewall replaces the rule set: the whole body is validated first and every rule that fails is named
// (422 invalid_rule with errors[]); the instance must exist; one apply at a time (409 operation_in_flight).
func (f *fakeAPI) putFirewall(rc *reqCtx, id string) {
	b := newBodyCheck(rc.r.Body)
	var in []wireFirewallRuleIn
	if raw, ok := b.present("rules", true); ok {
		if err := json.Unmarshal(raw, &in); err != nil {
			b.fail("rules", "Input should be a valid list")
		}
	}
	if errs := b.finish(); len(errs) > 0 {
		rc.invalid(errs...)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if vm, ok := f.vms[id]; !ok || vm.deletedAt != nil {
		rc.notFound("vm")
		return
	}
	payload, _ := json.Marshal(in)
	storeKey, digest, replied := f.idemClaim(rc, "PUT firewall", append([]byte(id), payload...))
	if replied {
		return
	}
	var refusals []wireFieldError
	var first string
	if len(in) > 50 {
		rc.unprocessable("invalid_rule", "a firewall holds at most 50 rules")
		return
	}
	rules := make([]wireFirewallRuleOut, 0, len(in))
	for i, r := range in {
		sentence := ""
		field := ""
		switch {
		case r.Direction != "ingress" && r.Direction != "egress":
			field, sentence = "direction", "direction must be one of: ingress, egress"
		case r.Protocol != "tcp" && r.Protocol != "udp" && r.Protocol != "icmp" && r.Protocol != "any":
			field, sentence = "protocol", "protocol must be one of: tcp, udp, icmp, any"
		case (r.Protocol == "icmp" || r.Protocol == "any") && (r.PortMin != nil || r.PortMax != nil):
			field, sentence = "port_min", r.Protocol+" rules carry no port range"
		case (r.Protocol == "tcp" || r.Protocol == "udp") && (r.PortMin == nil || r.PortMax == nil):
			field, sentence = "port_min", r.Protocol+" rules need a port range"
		case r.PortMin != nil && (*r.PortMin < 1 || *r.PortMin > 65535 || *r.PortMax < 1 || *r.PortMax > 65535):
			field, sentence = "port_min", "ports must be between 1 and 65535"
		case r.PortMin != nil && *r.PortMin > *r.PortMax:
			field, sentence = "port_min", "port_min must not exceed port_max"
		}
		cidr := r.RemoteCIDR
		if sentence == "" {
			if _, n, err := net.ParseCIDR(r.RemoteCIDR); err != nil || n.IP.To4() == nil {
				field, sentence = "remote_cidr", fmt.Sprintf("%q is not a valid IPv4 CIDR", r.RemoteCIDR)
			} else {
				cidr = n.String() // host bits masked
			}
		}
		if sentence == "" && len(r.Description) > 255 {
			field, sentence = "description", "description must be at most 255 characters"
		}
		if sentence != "" {
			if first == "" {
				first = fmt.Sprintf("rule %d: %s", i, sentence)
			}
			refusals = append(refusals, wireFieldError{Field: fmt.Sprintf("rules.%d.%s", i, field), Message: sentence})
			continue
		}
		f.fws.ruleSeq++
		rules = append(rules, wireFirewallRuleOut{ID: fakeUUID(7, f.fws.ruleSeq), Direction: r.Direction, Protocol: r.Protocol,
			PortMin: r.PortMin, PortMax: r.PortMax, RemoteCIDR: cidr, Description: r.Description})
	}
	if len(refusals) > 0 {
		rc.problem(http.StatusUnprocessableEntity, "invalid_rule", "Unprocessable", first, refusals...)
		return
	}
	fw := f.fw(id)
	if fw.state == "pending" {
		rc.conflict("operation_in_flight", "another operation is already running for this instance")
		return
	}
	f.fws.puts = append(f.fws.puts, string(payload))
	fw.pending, fw.state, fw.err, fw.stepsLeft = rules, "pending", nil, 2
	op := f.newOperation("firewall.apply", "vm", id, time.Now().UTC())
	accepted := vmAccepted{Vm: f.vms[id].wireVmOut, Operation: op}
	f.idemKeep(storeKey, digest, http.StatusAccepted, accepted, f.operationLocation(op), "")
	rc.json(http.StatusAccepted, accepted, "Location", f.operationLocation(op))
}

type vmAccepted struct {
	Vm        wireVmOut        `json:"vm"`
	Operation wireOperationRef `json:"operation"`
}
