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

// Tests for the claims of the project and VPC resources that the first verifier found untested (!10): what a PATCH
// carries, the Idempotency-Key of every write, plan-time validation, import ids, replacement, a delete that is refused,
// that ends in error or that meets a 409, and the state a failed wait leaves behind.

func projectWith(fake *fakeAPI, extra string) string {
	return testAccProviderBlockWith(fake, "writer", testOrgID) + "\nresource \"virtscale_project\" \"test\" {\n  name = \"web\"\n" + extra + "}\n"
}

func TestAccProjectResource_OnlyWhatChangedIsSentAndEveryWriteCarriesAKey(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.AddKey("writer", "write")
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy: func(*terraform.State) error {
			if n := len(fake.ProjectIdempotencyKeys()); n != 5 { // create, three PATCHes, delete
				return fmt.Errorf("the writes sent %d Idempotency-Keys, want 5", n)
			}
			return nil
		},
		Steps: []resource.TestStep{
			{Config: projectWith(fake, "  description = \"one\"\n  labels      = { team = \"a\", env = \"x\" }\n")},
			{ // a description alone
				Config: projectWith(fake, "  description = \"two\"\n  labels      = { team = \"a\", env = \"x\" }\n"),
				Check: func(*terraform.State) error {
					patches := fake.ProjectPatches()
					if len(patches) != 1 || !strings.HasSuffix(patches[0], ` {"description":"two"}`) {
						return fmt.Errorf("PATCHes = %q, want only the description", patches)
					}
					return nil
				},
			},
			{ // an empty label set clears the labels
				Config: projectWith(fake, "  description = \"two\"\n  labels      = {}\n"),
				Check: func(*terraform.State) error {
					patches := fake.ProjectPatches()
					if len(patches) != 2 || !strings.HasSuffix(patches[1], ` {"labels":{}}`) {
						return fmt.Errorf("PATCHes = %q, want only labels: {}", patches)
					}
					return nil
				},
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction("virtscale_project.test", plancheck.ResourceActionUpdate),
				}},
			},
			{ // the name alone, and labels set again
				Config: strings.Replace(projectWith(fake, "  description = \"two\"\n  labels      = { a = \"b\" }\n"), `"web"`, `"web-renamed"`, 1),
				Check: func(*terraform.State) error {
					patches := fake.ProjectPatches()
					if len(patches) != 3 || !strings.Contains(patches[2], `"name":"web-renamed"`) || !strings.Contains(patches[2], `"labels":{"a":"b"}`) || strings.Contains(patches[2], "description") {
						return fmt.Errorf("PATCHes = %q, want the name and the labels, not the description", patches)
					}
					return nil
				},
			},
		},
	})
}

func TestAccProjectResource_ARefusedDeleteIsShownAndAGoneProjectIsDone(t *testing.T) {
	for name, tc := range map[string]struct {
		respond func(*reqCtx)
		want    string
	}{
		"not empty": {func(rc *reqCtx) { rc.conflict("project_not_empty", "the project still holds a server") }, "409 project_not_empty"},
		"gone":      {respondNotFoundOf("project"), ""},
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeAPI(t, "test-api-key", testOrgID)
			fake.AddKey("writer", "write")
			config := projectWith(fake, "")
			steps := []resource.TestStep{{Config: config}}
			fake.FailNext(1, matchRoute(http.MethodDelete, "/projects/"), tc.respond)
			if tc.want != "" {
				steps = append(steps, resource.TestStep{Config: testAccProviderBlockWith(fake, "writer", testOrgID), ExpectError: wrapSafeInOrder(tc.want, "still holds a server")})
			} else {
				steps = append(steps, resource.TestStep{Config: testAccProviderBlockWith(fake, "writer", testOrgID)})
			}
			resource.Test(t, resource.TestCase{
				PreCheck:                 func() { testAccPreCheck(t) },
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
				Steps:                    steps,
			})
		})
	}
}

func TestAccProjectAndVpc_RulesTheAPIHasAreCheckedAtPlanTime(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.AddKey("writer", "write")
	long := strings.Repeat("x", 256)
	vpc := func(attrs string) string {
		return testAccProviderBlockWith(fake, "writer", testOrgID) + "\nresource \"virtscale_vpc\" \"x\" {\n  project_id = \"" + fakeUUID(5, 1) + "\"\n  name       = \"net\"\n  region     = \"gro\"\n" + attrs + "}\n"
	}
	cases := []struct{ name, config, want string }{
		{"a project name in capitals", strings.Replace(projectWith(fake, ""), `"web"`, `"Web_Tier"`, 1), "lower-case letters"},
		{"a project name that starts with a hyphen", strings.Replace(projectWith(fake, ""), `"web"`, `"-web"`, 1), "not starting or ending with a hyphen"},
		{"a project description that is too long", projectWith(fake, "  description = \""+long+"\"\n"), "the API takes at most 255"},
		{"a label key that starts with a hyphen", projectWith(fake, "  labels = { \"-bad\" = \"v\" }\n"), "Invalid label key"},
		{"a label value that is too long", projectWith(fake, "  labels = { k = \""+long+"\" }\n"), "longer than 255"},
		{"a vpc name in capitals", strings.Replace(vpc(""), `"net"`, `"Net"`, 1), "lower-case letters"},
		{"a prefix length of 19", vpc("  prefix_length = 19\n"), "from 20 to 24"},
		{"a prefix length of 25", vpc("  prefix_length = 25\n"), "from 20 to 24"},
		{"a vpc description that is too long", vpc("  description = \"" + long + "\"\n"), "the API takes at most 255"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resource.Test(t, resource.TestCase{
				PreCheck:                 func() { testAccPreCheck(t) },
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
				Steps:                    []resource.TestStep{{Config: tc.config, PlanOnly: true, ExpectError: wrapSafe(tc.want)}},
			})
		})
	}
	if n := fake.CallCount("POST projects") + fake.CallCount("POST projects/{project}/vpcs"); n != 0 {
		t.Errorf("%d creates reached the API; the refusals are plan-time", n)
	}
}

func TestAccVpcResource_ABadImportIdIsRefused(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	config := vpcConfig(fake, "net-j", "")
	steps := []resource.TestStep{{Config: config}}
	for _, id := range []string{"no-slash", "/only-vpc", "only-project/", "a/b/c"} {
		steps = append(steps, resource.TestStep{
			ResourceName:  "virtscale_vpc.test",
			ImportState:   true,
			ImportStateId: id,
			ExpectError:   wrapSafe("expected <project id>/<vpc id>"),
		})
	}
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             checkVpcsGone(fake),
		Steps:                    steps,
	})
}

// Every attribute of a VPC is fixed at creation: a new name, region, description or project is a new VPC.
func TestAccVpcResource_AChangeOfNameRegionProjectOrDescriptionReplacesTheVpc(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.AddKey("writer", "write") // creating the second project needs the write scope
	config := func(project, name, region, description string) string {
		return testAccProviderBlockWith(fake, "writer", testOrgID) + fmt.Sprintf(`
resource "virtscale_project" "other" {
  name = "other"
}

resource "virtscale_vpc" "test" {
  project_id  = %s
  name        = %q
  region      = %q
  description = %q
}
`, project, name, region, description)
	}
	def, other := `"`+fakeUUID(5, 1)+`"`, "virtscale_project.other.id"
	replaced := func(c string) resource.TestStep {
		return resource.TestStep{
			Config: c,
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction("virtscale_vpc.test", plancheck.ResourceActionDestroyBeforeCreate),
			}},
		}
	}
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             checkVpcsGone(fake),
		Steps: []resource.TestStep{
			{Config: config(def, "net", "gro", "")},
			replaced(config(def, "net-2", "gro", "")),
			replaced(config(def, "net-2", "fra", "")),
			replaced(config(def, "net-2", "fra", "now described")),
			replaced(config(other, "net-2", "fra", "now described")),
		},
	})
}

// The VPC exists as soon as the create is accepted, so a wait that fails for any reason must leave it in the state:
// otherwise it is built and billed and nothing destroys it.
func TestAccVpcResource_AFailedWaitKeepsTheVpcInState(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.FailNext(1, matchRoute(http.MethodGet, "/vpcs/"), func(rc *reqCtx) {
		rc.problem(http.StatusForbidden, "permission_denied", "Forbidden", "this credential does not carry the 'network.vpc.read' permission")
	})
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             checkVpcsGone(fake),
		Steps: []resource.TestStep{{
			Config:      vpcConfig(fake, "net-k", ""),
			ExpectError: wrapSafeInOrder("Unable to Wait for VPC to Become Active", "403 permission_denied"),
		}},
	})
}

// A 409 operation_in_flight on a delete means "already deleting" only when the VPC reads deleting; it is also the answer
// while another operation runs, and then it is shown.
func TestAccVpcResource_AnOperationInFlightOnDelete(t *testing.T) {
	t.Run("a delete another client started is waited for", func(t *testing.T) {
		fake := newFakeAPI(t, "test-api-key", testOrgID)
		config := vpcConfig(fake, "net-l", "")
		resource.Test(t, resource.TestCase{
			PreCheck:                 func() { testAccPreCheck(t) },
			ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
			Steps: []resource.TestStep{
				{Config: config},
				{
					PreConfig: func() { fake.StartVpcDelete(fakeUUID(6, 1)) },
					Config:    testAccProviderBlock(fake), // the VPC is no longer wanted
				},
			},
			CheckDestroy: checkVpcsGone(fake),
		})
	})
	t.Run("another operation is shown", func(t *testing.T) {
		fake := newFakeAPI(t, "test-api-key", testOrgID)
		config := vpcConfig(fake, "net-m", "")
		fake.FailNext(1, matchRoute(http.MethodDelete, "/vpcs/"), func(rc *reqCtx) {
			rc.conflict("operation_in_flight", "another operation is already running for this VPC")
		})
		resource.Test(t, resource.TestCase{
			PreCheck:                 func() { testAccPreCheck(t) },
			ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
			Steps: []resource.TestStep{
				{Config: config},
				{Config: testAccProviderBlock(fake), ExpectError: wrapSafeInOrder("Unable to Delete VPC", "409 operation_in_flight")},
			},
			CheckDestroy: checkVpcsGone(fake),
		})
	})
}

// A delete the platform could not carry out ends in status error: destroy says so with the platform's reason at once,
// instead of waiting out the timeout.
func TestAccVpcResource_ADeleteThatEndsInErrorFailsAtOnce(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	config := vpcConfig(fake, "net-n", "")
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             checkVpcsGone(fake),
		Steps: []resource.TestStep{
			{Config: config},
			{
				PreConfig:   func() { fake.FailNextVpcDelete(fakeUUID(6, 1)) },
				Config:      testAccProviderBlock(fake),
				ExpectError: wrapSafeInOrder("VPC Failed to Delete", "the network could not be removed"),
			},
		},
	})
}
