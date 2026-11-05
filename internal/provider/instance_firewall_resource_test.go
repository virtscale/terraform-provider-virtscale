package provider

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

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
