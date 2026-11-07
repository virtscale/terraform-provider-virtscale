package provider

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// Acceptance tests of virtscale_vpc against the fake (TF_ACC=1). A VPC is built and deleted asynchronously, so the
// resource waits for status active, and on destroy until the VPC is gone.

func vpcConfig(fake *fakeAPI, name string, extra string) string {
	return testAccProviderBlock(fake) + fmt.Sprintf(`
data "virtscale_project" "default" {}

resource "virtscale_vpc" "test" {
  project_id = data.virtscale_project.default.id
  name       = %q
  region     = "gro"
%s}
`, name, extra)
}

func vpcInRegion(fake *fakeAPI, name, region string) string {
	return testAccProviderBlock(fake) + fmt.Sprintf("resource \"virtscale_vpc\" \"x\" {\n  project_id = %q\n  name       = %q\n  region     = %q\n}\n", fakeUUID(5, 1), name, region)
}

func checkVpcsGone(fake *fakeAPI) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if n := fake.VpcCount(); n != 0 {
			return fmt.Errorf("%d VPCs are left", n)
		}
		return nil
	}
}

func TestAccVpcResource_Lifecycle(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	var id string
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy: func(s *terraform.State) error {
			if err := checkVpcsGone(fake)(s); err != nil {
				return err
			}
			if keys := fake.ProjectIdempotencyKeys(); len(keys) != 2 { // the create, and the delete
				return fmt.Errorf("the create and the delete sent %d Idempotency-Keys, want 2", len(keys))
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: vpcConfig(fake, "net-a", `  description = "first"`+"\n"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("virtscale_vpc.test", "name", "net-a"),
					resource.TestCheckResourceAttr("virtscale_vpc.test", "region", "gro"),
					resource.TestCheckResourceAttr("virtscale_vpc.test", "description", "first"),
					resource.TestCheckResourceAttr("virtscale_vpc.test", "status", "active"),
					resource.TestCheckResourceAttr("virtscale_vpc.test", "prefix_length", "22"),
					resource.TestCheckResourceAttr("virtscale_vpc.test", "is_default", "false"),
					resource.TestCheckResourceAttr("virtscale_vpc.test", "internet_egress", "false"), // only the default VPC has a gateway
					resource.TestCheckResourceAttrSet("virtscale_vpc.test", "cidr"),
					resource.TestCheckResourceAttrSet("virtscale_vpc.test", "vrn"),
					func(s *terraform.State) error {
						id = s.RootModule().Resources["virtscale_vpc.test"].Primary.ID
						if v, ok := fake.Vpc(id); !ok || v.Status != "active" {
							return fmt.Errorf("the API holds %+v", v)
						}
						if keys := fake.ProjectIdempotencyKeys(); len(keys) != 1 {
							return fmt.Errorf("the create sent %d Idempotency-Keys, want 1", len(keys))
						}
						return nil
					},
				),
			},
			{ // Nothing to change: the second plan is empty.
				Config:   vpcConfig(fake, "net-a", `  description = "first"`+"\n"),
				PlanOnly: true,
			},
			{ // Imported as <project id>/<vpc id>.
				ResourceName:      "virtscale_vpc.test",
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateIdFunc: func(*terraform.State) (string, error) { return fakeUUID(5, 1) + "/" + id, nil },
			},
		},
	})
}

func TestAccVpcResource_PrefixLengthIsReadFromTheBlockAndForcesReplacement(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             checkVpcsGone(fake),
		Steps: []resource.TestStep{
			{Config: vpcConfig(fake, "net-b", "  prefix_length = 24\n"), Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("virtscale_vpc.test", "prefix_length", "24"),
				resource.TestMatchResourceAttr("virtscale_vpc.test", "cidr", regexpEnding("/24")),
			)},
			{
				Config: vpcConfig(fake, "net-b", "  prefix_length = 23\n"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction("virtscale_vpc.test", plancheck.ResourceActionDestroyBeforeCreate),
				}},
				Check: resource.TestCheckResourceAttr("virtscale_vpc.test", "prefix_length", "23"),
			},
		},
	})
}

func TestAccVpcResource_ABuildThatEndsInErrorFailsWithItsReasonAndCanBeDestroyed(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.FailVpcBuilds()
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             checkVpcsGone(fake),
		Steps: []resource.TestStep{{
			Config:      vpcConfig(fake, "net-c", ""),
			ExpectError: wrapSafeInOrder("VPC Failed to Build", "the network could not be built"),
		}},
	})
}

func TestAccVpcResource_RefusalsCarryTheAPIsCode(t *testing.T) {
	t.Run("not switched on", func(t *testing.T) {
		fake := newFakeAPI(t, "test-api-key", testOrgID)
		fake.SetVpcsOff(true)
		resource.Test(t, resource.TestCase{
			PreCheck:                 func() { testAccPreCheck(t) },
			ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
			Steps: []resource.TestStep{{Config: vpcConfig(fake, "net-d", ""),
				ExpectError: wrapSafeInOrder("503 not_ready", "VPCs are not switched on yet")}},
		})
	})
	t.Run("a region that is not available", func(t *testing.T) {
		fake := newFakeAPI(t, "test-api-key", testOrgID)
		resource.Test(t, resource.TestCase{
			PreCheck:                 func() { testAccPreCheck(t) },
			ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
			Steps: []resource.TestStep{{
				Config:      vpcInRegion(fake, "net-e", "ams"),
				ExpectError: wrapSafeInOrder("422 region_unavailable", "not available to you")}},
		})
	})
	t.Run("a key without the network scope", func(t *testing.T) {
		fake := newFakeAPI(t, "test-api-key", testOrgID)
		fake.AddKey("compute-only", "compute:write")
		resource.Test(t, resource.TestCase{
			PreCheck:                 func() { testAccPreCheck(t) },
			ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
			Steps: []resource.TestStep{{
				Config:      strings.Replace(vpcInRegion(fake, "net-f", "gro"), `"test-api-key"`, `"compute-only"`, 1),
				ExpectError: wrapSafeInOrder("403 permission_denied", "'network.vpc.create' permission", "needs the 'network:write' scope")}},
		})
	})
}

func TestAccVpcResource_DestroyWaitsUntilTheVpcIsGoneAndShowsARefusal(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	config := vpcConfig(fake, "net-g", "")
	var id string
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{Config: config, Check: func(s *terraform.State) error {
				id = s.RootModule().Resources["virtscale_vpc.test"].Primary.ID
				fake.MakeVpcNotEmpty(id, true)
				return nil
			}},
			{ // With subnets on it the delete is refused, and the API's code is shown.
				Config:      config,
				Destroy:     true,
				ExpectError: wrapSafeInOrder("409 vpc_not_empty", "subnets still hang off this VPC"),
			},
			{ // Once the subnets are gone the destroy goes through and waits for the VPC to disappear.
				PreConfig: func() { fake.MakeVpcNotEmpty(id, false) },
				Config:    config,
				Destroy:   true,
			},
		},
		CheckDestroy: checkVpcsGone(fake),
	})
}

func TestAccVpcResource_VanishedOutOfBandIsRecreated(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	config := vpcConfig(fake, "net-h", "")
	var id string
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             checkVpcsGone(fake),
		Steps: []resource.TestStep{
			{Config: config, Check: func(s *terraform.State) error {
				id = s.RootModule().Resources["virtscale_vpc.test"].Primary.ID
				return nil
			}},
			{
				PreConfig: func() { fake.VanishVpc(id) },
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction("virtscale_vpc.test", plancheck.ResourceActionCreate),
				}},
			},
		},
	})
}

func regexpEnding(suffix string) *regexp.Regexp {
	return regexp.MustCompile(regexp.QuoteMeta(suffix) + `$`)
}

// A 404 about the project is not a 404 about the VPC: the VPC stays in state, and the error is shown.
func TestAccVpcResource_AMissingProjectKeepsTheResourceInState(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	config := vpcConfig(fake, "net-i", "")
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             checkVpcsGone(fake),
		Steps: []resource.TestStep{
			{Config: config},
			{
				PreConfig:   func() { fake.HideProject(fakeUUID(5, 1), true) },
				Config:      config,
				PlanOnly:    true,
				ExpectError: wrapSafe("no project matching that identifier"),
			},
			{PreConfig: func() { fake.HideProject(fakeUUID(5, 1), false) }, Config: config, PlanOnly: true},
		},
	})
}
