package provider

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// Acceptance tests of virtscale_instance_firewall against the fake (TF_ACC=1). The resource owns the whole rule set
// of one instance: the API replaces it as one, and applies it asynchronously.

const (
	ruleSSH = `
  rule {
    direction   = "ingress"
    protocol    = "tcp"
    port_min    = 22
    port_max    = 22
    remote_cidr = "0.0.0.0/0"
    description = "ssh"
  }
`
	ruleHTTPS = `
  rule {
    direction   = "ingress"
    protocol    = "tcp"
    port_min    = 443
    port_max    = 443
    remote_cidr = "10.0.0.0/8"
  }
`
	rulePing = `
  rule {
    direction   = "ingress"
    protocol    = "icmp"
    remote_cidr = "0.0.0.0/0"
  }
`
	ruleHTTP = `
  rule {
    direction   = "ingress"
    protocol    = "tcp"
    port_min    = 80
    port_max    = 80
    remote_cidr = "0.0.0.0/0"
  }
`
	firstVm = "00000000-0000-4001-8000-000000000001" // fakeUUID(1, 1): the first instance a test creates
)

func firewallConfig(fake *fakeAPI, rules string) string {
	return testAccProviderBlock(fake) + testAccInstanceBlock("fw-vm") + `
resource "virtscale_instance_firewall" "test" {
  instance_id = virtscale_instance.test.id
` + rules + `}
`
}

func ruleAttrs(direction, protocol, portMin, portMax, cidr, description string) map[string]string {
	m := map[string]string{"direction": direction, "protocol": protocol, "remote_cidr": cidr}
	if portMin != "" {
		m["port_min"], m["port_max"] = portMin, portMax
	}
	if description != "" {
		m["description"] = description
	}
	return m
}

func TestAccInstanceFirewallResource_Lifecycle(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: firewallConfig(fake, ruleSSH+ruleHTTPS+rulePing),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair("virtscale_instance_firewall.test", "id", "virtscale_instance.test", "id"),
					resource.TestCheckResourceAttr("virtscale_instance_firewall.test", "state", "applied"),
					resource.TestCheckResourceAttr("virtscale_instance_firewall.test", "rule.#", "3"),
					resource.TestCheckTypeSetElemNestedAttrs("virtscale_instance_firewall.test", "rule.*", ruleAttrs("ingress", "tcp", "22", "22", "0.0.0.0/0", "ssh")),
					resource.TestCheckTypeSetElemNestedAttrs("virtscale_instance_firewall.test", "rule.*", ruleAttrs("ingress", "tcp", "443", "443", "10.0.0.0/8", "")),
					resource.TestCheckTypeSetElemNestedAttrs("virtscale_instance_firewall.test", "rule.*", ruleAttrs("ingress", "icmp", "", "", "0.0.0.0/0", "")),
					func(*terraform.State) error {
						if got := len(fake.FirewallRules(firstVm)); got != 3 {
							return fmt.Errorf("the API holds %d rules, want 3", got)
						}
						// The create returned only once the API said applied: it read the state more than once.
						if n := fake.CallCount("GET compute/instances/{vm_id}/firewall"); n < 2 {
							return fmt.Errorf("the firewall was read %d times, want at least 2 (pending, then applied)", n)
						}
						if keys := fake.ProjectIdempotencyKeys(); len(keys) != 1 {
							return fmt.Errorf("the PUT sent %d Idempotency-Keys, want 1", len(keys))
						}
						return nil
					},
				),
			},
			{ // The order of the rules is not part of the configuration: a set.
				Config:   firewallConfig(fake, rulePing+ruleSSH+ruleHTTPS),
				PlanOnly: true,
			},
			{ // One rule more is an update in place, and the API now holds four.
				Config: firewallConfig(fake, ruleSSH+ruleHTTPS+rulePing+ruleHTTP),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction("virtscale_instance_firewall.test", plancheck.ResourceActionUpdate),
				}},
				Check: func(*terraform.State) error {
					if got := len(fake.FirewallRules(firstVm)); got != 4 {
						return fmt.Errorf("the API holds %d rules, want 4", got)
					}
					return nil
				},
			},
			{ // Imported by the instance's id.
				ResourceName:      "virtscale_instance_firewall.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
			{ // Without the resource, the rules are cleared and the instance stays.
				Config: testAccProviderBlock(fake) + testAccInstanceBlock("fw-vm"),
				Check: func(*terraform.State) error {
					if rules := fake.FirewallRules(firstVm); len(rules) != 0 {
						return fmt.Errorf("the API still holds %q", rules)
					}
					return nil
				},
			},
		},
	})
}

func TestAccInstanceFirewallResource_ARuleSetThatEndsInErrorFailsWithTheAPIsText(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.FailFirewallApply(firstVm)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{{
			Config:      firewallConfig(fake, ruleSSH),
			ExpectError: wrapSafeInOrder("Firewall Failed to Apply", "the rules could not be applied to the network"),
		}},
	})
}

func TestAccInstanceFirewallResource_TheAPIsRefusalIsShownWithItsCodeAndFields(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.FailNext(1, matchRoute(http.MethodPut, "/firewall"), func(rc *reqCtx) {
		rc.problem(http.StatusUnprocessableEntity, "invalid_rule", "Unprocessable", "rule 0: ports must be between 1 and 65535",
			wireFieldError{Field: "rules.0.port_min", Message: "ports must be between 1 and 65535"})
	})
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{{
			Config:      firewallConfig(fake, ruleSSH),
			ExpectError: wrapSafeInOrder("422 invalid_rule", "rule 0: ports must be between 1 and 65535", "rules.0.port_min: ports must be between 1 and 65535"),
		}},
	})
}

// What can be known without asking the API is refused at plan time, with the API's own sentences.
func TestAccInstanceFirewallResource_RulesThatCannotBeRightAreRefusedAtPlanTime(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	rule := func(protocol, ports, cidr string) string {
		return fmt.Sprintf("\n  rule {\n    direction = \"ingress\"\n    protocol = %q\n%s    remote_cidr = %q\n  }\n", protocol, ports, cidr)
	}
	cases := []struct{ name, rules, want string }{
		{"tcp without ports", rule("tcp", "", "0.0.0.0/0"), "tcp rules need a port range"},
		{"icmp with ports", rule("icmp", "    port_min = 1\n    port_max = 2\n", "0.0.0.0/0"), "icmp rules carry no port range"},
		{"a reversed range", rule("tcp", "    port_min = 90\n    port_max = 80\n", "0.0.0.0/0"), "port_min must not exceed port_max"},
		{"a port out of range", rule("udp", "    port_min = 0\n    port_max = 80\n", "0.0.0.0/0"), "ports must be between 1 and 65535"},
		{"a port above the range", rule("udp", "    port_min = 80\n    port_max = 65536\n", "0.0.0.0/0"), "ports must be between 1 and 65535"},
		{"a cidr with host bits", rule("any", "", "10.0.0.5/24"), `use "10.0.0.0/24"`},
		{"an IPv6 cidr", rule("any", "", "::/0"), "is not an IPv4 CIDR"},
		{"another protocol", rule("sctp", "", "0.0.0.0/0"), "protocol value must be one of"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resource.Test(t, resource.TestCase{
				PreCheck:                 func() { testAccPreCheck(t) },
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
				Steps: []resource.TestStep{{
					Config:      firewallConfig(fake, tc.rules),
					PlanOnly:    true,
					ExpectError: wrapSafe(tc.want),
				}},
			})
		})
	}
	if n := fake.FirewallPuts(); n != 0 {
		t.Errorf("%d PUTs reached the API; the refusals are plan-time", n)
	}
}

func TestAccInstanceFirewallResource_AnInstanceThatIsGoneTakesTheFirewallWithIt(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	config := firewallConfig(fake, ruleSSH)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{Config: config},
			{
				PreConfig:          func() { fake.Vanish(firstVm) },
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

func TestAccInstanceFirewallResource_AKeyWithoutTheNetworkScopeIsToldSo(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.AddKey("compute-only", "compute:write")
	config := strings.Replace(firewallConfig(fake, ruleSSH), `"test-api-key"`, `"compute-only"`, 1)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{{
			Config:      config,
			ExpectError: wrapSafeInOrder("403 permission_denied", "'network.firewall.update' permission", "needs the 'network:write' scope"),
		}},
	})
}

// A 404 about the organization is not a 404 about the instance: the firewall stays in state, and the error is shown.
// The instance is made outside Terraform, so that only the firewall resource reads it.
func TestAccInstanceFirewallResource_WrongOrganizationKeepsTheResourceInState(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	vm := createVmVia(t, fake, "test-api-key", namedVmBody("seeded"))
	id, _ := vm.get("vm")["id"].(string)
	config := func(org string) string {
		return testAccProviderBlockWith(fake, "test-api-key", org) + fmt.Sprintf("resource \"virtscale_instance_firewall\" \"test\" {\n  instance_id = %q\n%s}\n", id, ruleSSH)
	}
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{Config: config(testOrgID)},
			{Config: config(testOtherOrg), PlanOnly: true, ExpectError: wrapSafeInOrder("no organization matching", "check VIRTSCALE_ORGANIZATION")},
			{Config: config(testOrgID), PlanOnly: true},
		},
	})
}

func firewallPath(id string) string { return vmsPath(testOrgID) + "/" + id + "/firewall" }

// Rules added or removed outside Terraform show up as a difference, and the next apply puts the configured set back.
func TestAccInstanceFirewallResource_DriftIsDetectedAndRepaired(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	config := firewallConfig(fake, ruleSSH+ruleHTTPS)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{Config: config},
			{ // someone sets another rule set in the panel
				PreConfig: func() {
					status, _, body := fakeHTTP(t, fake, "test-api-key", http.MethodPut, firewallPath(firstVm),
						`{"rules":[{"direction":"ingress","protocol":"icmp","remote_cidr":"0.0.0.0/0"}]}`)
					if status != http.StatusAccepted {
						t.Fatalf("out-of-band PUT: %d %s", status, body)
					}
				},
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config: config,
				Check: func(*terraform.State) error {
					if got := len(fake.FirewallRules(firstVm)); got != 2 {
						return fmt.Errorf("the API holds %d rules after the repair, want 2", got)
					}
					return nil
				},
			},
		},
	})
}

// When the API fails to apply a change, the rules it holds are not in force: the next plan differs from the
// configuration, and the next apply sets them again.
func TestAccInstanceFirewallResource_AFailedUpdateShowsInTheNextPlan(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{Config: firewallConfig(fake, ruleSSH)},
			{
				PreConfig:   func() { fake.FailFirewallApply(firstVm) },
				Config:      firewallConfig(fake, ruleSSH+rulePing),
				ExpectError: wrapSafeInOrder("Firewall Failed to Apply", "the rules could not be applied to the network"),
			},
			{Config: firewallConfig(fake, ruleSSH+rulePing), PlanOnly: true, ExpectNonEmptyPlan: true},
			{
				Config: firewallConfig(fake, ruleSSH+rulePing),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("virtscale_instance_firewall.test", "state", "applied"),
					resource.TestCheckResourceAttr("virtscale_instance_firewall.test", "rule.#", "2"),
				),
			},
		},
	})
}

func TestAccInstanceFirewallResource_MoreRulesThanTheAPITakesAreRefusedAtPlanTime(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	var rules strings.Builder
	for port := 1000; port < 1051; port++ { // 51 rules
		fmt.Fprintf(&rules, "  rule {\n    direction   = \"ingress\"\n    protocol    = \"tcp\"\n    port_min    = %d\n    port_max    = %d\n    remote_cidr = \"10.0.0.0/8\"\n  }\n", port, port)
	}
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{{
			Config:      firewallConfig(fake, rules.String()),
			PlanOnly:    true,
			ExpectError: wrapSafeInOrder("Too many firewall rules", "at most 50"),
		}},
	})
	if n := fake.FirewallPuts(); n != 0 {
		t.Errorf("%d PUTs reached the API; the refusal is plan-time", n)
	}
}

// The description of a rule is 1 to 80 characters (COMPUTE_FIREWALL_DESCRIPTION_MAX_LENGTH); 80 is accepted, 81 is refused
// before anything is sent.
func TestAccInstanceFirewallResource_ADescriptionOfEightyCharactersIsTheLimit(t *testing.T) {
	rule := func(description string) string {
		return fmt.Sprintf("  rule {\n    direction   = \"ingress\"\n    protocol    = \"icmp\"\n    remote_cidr = \"0.0.0.0/0\"\n    description = %q\n  }\n", description)
	}
	t.Run("80", func(t *testing.T) {
		fake := newFakeAPI(t, "test-api-key", testOrgID)
		resource.Test(t, resource.TestCase{
			PreCheck:                 func() { testAccPreCheck(t) },
			ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
			Steps:                    []resource.TestStep{{Config: firewallConfig(fake, rule(strings.Repeat("d", 80)))}},
		})
	})
	t.Run("81", func(t *testing.T) {
		fake := newFakeAPI(t, "test-api-key", testOrgID)
		resource.Test(t, resource.TestCase{
			PreCheck:                 func() { testAccPreCheck(t) },
			ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
			Steps: []resource.TestStep{{
				Config: firewallConfig(fake, rule(strings.Repeat("d", 81))), PlanOnly: true,
				ExpectError: wrapSafe("description must be at most 80 characters"),
			}},
		})
		if n := fake.FirewallPuts(); n != 0 {
			t.Errorf("%d PUTs reached the API; the refusal is plan-time", n)
		}
	})
	t.Run("empty", func(t *testing.T) {
		fake := newFakeAPI(t, "test-api-key", testOrgID)
		resource.Test(t, resource.TestCase{
			PreCheck:                 func() { testAccPreCheck(t) },
			ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
			Steps: []resource.TestStep{{
				Config: firewallConfig(fake, rule("")), PlanOnly: true,
				ExpectError: wrapSafe("description must be at most 80 characters (and not empty when given)"),
			}},
		})
	})
}

// A value that is not known at plan time (an id of a resource still to be made) cannot be judged yet; it is judged when
// it is known, and the rule is applied.
func TestAccInstanceFirewallResource_AValueThatIsKnownOnlyAfterApplyIsJudgedLater(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	rule := "  rule {\n    direction   = \"ingress\"\n    protocol    = \"icmp\"\n    remote_cidr = \"0.0.0.0/0\"\n    description = \"vm ${virtscale_instance.test.id}\"\n  }\n"
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{{
			Config: firewallConfig(fake, rule),
			Check: resource.TestMatchTypeSetElemNestedAttrs("virtscale_instance_firewall.test", "rule.*",
				map[string]*regexp.Regexp{"description": regexp.MustCompile(`^vm [0-9a-f-]{36}$`)}),
		}},
	})
}

// The PUT is accepted, so the rules are in place whatever happens to the wait: the state keeps the resource, and the
// destroy that follows clears them with a key of its own.
func TestAccInstanceFirewallResource_AFailedWaitKeepsTheResourceInStateAndEveryWriteCarriesAKey(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.FailNext(1, matchRoute(http.MethodGet, "/firewall"), func(rc *reqCtx) {
		rc.problem(http.StatusForbidden, "permission_denied", "Forbidden", "this credential does not carry the 'network.read' permission")
	})
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy: func(*terraform.State) error {
			if n := fake.FirewallPuts(); n != 2 { // the set, and the clear on destroy
				return fmt.Errorf("%d PUTs reached the API, want 2: the failed create must be cleared on destroy", n)
			}
			// Every PUT request carries a key of its own, the clears the API refused with a 409 (the change was still being
			// applied) as well as the one it took.
			keys := fake.ProjectIdempotencyKeys()
			if puts := fake.CallCount("PUT compute/instances/{vm_id}/firewall"); len(keys) != puts {
				return fmt.Errorf("the %d PUT requests sent %d Idempotency-Keys, want one each", puts, len(keys))
			}
			seen := map[string]bool{}
			for _, k := range keys {
				if seen[k] {
					return fmt.Errorf("an Idempotency-Key was sent twice")
				}
				seen[k] = true
			}
			return nil
		},
		Steps: []resource.TestStep{{
			Config:      firewallConfig(fake, ruleSSH),
			ExpectError: wrapSafeInOrder("Unable to Wait for the Firewall to Be Applied", "403 permission_denied"),
		}},
	})
}

// instance_id names the instance whose rules these are: another instance is another resource.
func TestAccInstanceFirewallResource_AnotherInstanceReplacesTheResource(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	config := func(target string) string {
		instance := func(label string) string {
			return strings.Replace(strings.Replace(testAccInstanceBlock("fw-"+label), `"virtscale_instance" "test"`, `"virtscale_instance" "`+label+`"`, 1), `"fw-`+label+`"`, `"fw-`+label+`"`, 1)
		}
		return testAccProviderBlock(fake) + instance("a") + instance("b") +
			fmt.Sprintf("\nresource \"virtscale_instance_firewall\" \"test\" {\n  instance_id = virtscale_instance.%s.id\n%s}\n", target, ruleSSH)
	}
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{Config: config("a")},
			{
				Config: config("b"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction("virtscale_instance_firewall.test", plancheck.ResourceActionDestroyBeforeCreate),
				}},
			},
		},
	})
}

// The limits themselves are accepted: 50 rules (the most the API takes) and the last port, 65535.
func TestAccInstanceFirewallResource_FiftyRulesAndTheLastPortAreAccepted(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	var rules strings.Builder
	for port := 1000; port < 1049; port++ { // 49 rules
		fmt.Fprintf(&rules, "  rule {\n    direction   = \"ingress\"\n    protocol    = \"tcp\"\n    port_min    = %d\n    port_max    = %d\n    remote_cidr = \"10.0.0.0/8\"\n  }\n", port, port)
	}
	rules.WriteString("  rule {\n    direction   = \"ingress\"\n    protocol    = \"udp\"\n    port_min    = 65535\n    port_max    = 65535\n    remote_cidr = \"10.0.0.0/8\"\n  }\n") // the 50th
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{{
			Config: firewallConfig(fake, rules.String()),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("virtscale_instance_firewall.test", "rule.#", "50"),
				resource.TestCheckTypeSetElemNestedAttrs("virtscale_instance_firewall.test", "rule.*", ruleAttrs("ingress", "udp", "65535", "65535", "10.0.0.0/8", "")),
				func(*terraform.State) error {
					if got := len(fake.FirewallRules(firstVm)); got != 50 {
						return fmt.Errorf("the API holds %d rules, want 50", got)
					}
					return nil
				},
			),
		}},
	})
}

// While a change is being applied the API already shows its rows: a plan in that window sees the same set, not a drift.
func TestAccInstanceFirewallResource_ARuleSetThatIsStillBeingAppliedIsNotDrift(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	config := firewallConfig(fake, ruleSSH)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{Config: config},
			{ // the same rule set is put again from the panel and stays pending for the next reads
				PreConfig: func() {
					fake.SlowFirewallApply(firstVm, 50)
					status, _, body := fakeHTTP(t, fake, "test-api-key", http.MethodPut, firewallPath(firstVm),
						`{"rules":[{"direction":"ingress","protocol":"tcp","port_min":22,"port_max":22,"remote_cidr":"0.0.0.0/0","description":"ssh"}]}`)
					if status != http.StatusAccepted {
						t.Fatalf("out-of-band PUT: %d %s", status, body)
					}
				},
				Config:   config,
				PlanOnly: true,
			},
		},
	})
}

// A change that never finishes (the worker gave up on a precondition and left the state pending; no operation runs any
// more) ends the create at its deadline, and the destroy asks for the empty rule set at once instead of waiting for a
// state that never leaves pending.
func TestAccInstanceFirewallResource_AChangeStuckInPendingEndsAtTheDeadlineAndCanBeDestroyed(t *testing.T) {
	shortTimeouts(t, time.Second, 5*time.Second)
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.StickFirewallPending(firstVm)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy: func(*terraform.State) error {
			if n := fake.FirewallPuts(); n != 2 { // the set, and the clear on destroy
				return fmt.Errorf("%d PUTs reached the API, want 2", n)
			}
			if rules := fake.FirewallRules(firstVm); len(rules) != 0 {
				return fmt.Errorf("the API still holds %q", rules)
			}
			return nil
		},
		Steps: []resource.TestStep{{
			Config:      firewallConfig(fake, ruleSSH),
			ExpectError: wrapSafeInOrder("Unable to Wait for the Firewall to Be Applied", "last seen: state pending"),
		}},
	})
}

// A change that is still being applied when the destroy comes (the create's wait ran out) makes the clearing PUT a 409
// operation_in_flight: the destroy asks again until the operation is through, then clears the rules.
func TestAccInstanceFirewallResource_ADestroyWaitsOutAChangeThatIsStillBeingApplied(t *testing.T) {
	shortTimeouts(t, 500*time.Millisecond, 20*time.Second)
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.SlowFirewallApply(firstVm, 120) // more reads than the create's 500 ms of polling can make
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy: func(*terraform.State) error {
			if n := fake.CallCount("PUT compute/instances/{vm_id}/firewall"); n < 3 {
				return fmt.Errorf("%d PUT requests reached the API, want the set, at least one refused clear and the clear", n)
			}
			if n := fake.FirewallPuts(); n != 2 {
				return fmt.Errorf("%d PUTs were taken, want 2 (the set, and the clear)", n)
			}
			if rules := fake.FirewallRules(firstVm); len(rules) != 0 {
				return fmt.Errorf("the API still holds %q", rules)
			}
			return nil
		},
		Steps: []resource.TestStep{{
			Config:      firewallConfig(fake, ruleSSH),
			ExpectError: wrapSafeInOrder("Unable to Wait for the Firewall to Be Applied", "last seen: state pending"),
		}},
	})
}

// A destroy whose empty rule set ends in state error is an error with the API's text, and the resource stays in state.
func TestAccInstanceFirewallResource_ADestroyThatEndsInErrorIsReported(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{Config: firewallConfig(fake, ruleSSH)},
			{
				PreConfig:   func() { fake.FailFirewallApply(firstVm) },
				Config:      testAccProviderBlock(fake) + testAccInstanceBlock("fw-vm"),
				ExpectError: wrapSafeInOrder("Unable to Clear Firewall", "the empty rule set ended in state error", "the rules could not be applied to the network"),
			},
		},
	})
}
