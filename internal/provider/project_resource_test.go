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

// Acceptance tests of virtscale_project and the virtscale_project data source against the fake (TF_ACC=1).

func projectConfig(fake *fakeAPI, key, name, description, labels string) string {
	return testAccProviderBlockWith(fake, key, testOrgID) + fmt.Sprintf(`
resource "virtscale_project" "test" {
  name        = %q
  description = %q
  labels      = %s
}
`, name, description, labels)
}

func checkProjectGone(fake *fakeAPI) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		for _, rs := range s.RootModule().Resources {
			if rs.Type == "virtscale_project" {
				if _, ok := fake.Project(rs.Primary.ID); ok {
					return fmt.Errorf("project %s still exists", rs.Primary.ID)
				}
			}
		}
		return nil
	}
}

func TestAccProjectResource_Lifecycle(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.AddKey("writer", "write")
	var id string
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             checkProjectGone(fake),
		Steps: []resource.TestStep{
			{
				Config: projectConfig(fake, "writer", "web", "the web tier", `{ team = "a", env = "test" }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("virtscale_project.test", "name", "web"),
					resource.TestCheckResourceAttr("virtscale_project.test", "description", "the web tier"),
					resource.TestCheckResourceAttr("virtscale_project.test", "labels.team", "a"),
					resource.TestCheckResourceAttr("virtscale_project.test", "labels.%", "2"),
					resource.TestCheckResourceAttr("virtscale_project.test", "is_default", "false"),
					resource.TestMatchResourceAttr("virtscale_project.test", "vrn", regexp.MustCompile(`^vrn:`+testOrgID+`:project:`)),
					resource.TestCheckResourceAttrSet("virtscale_project.test", "created_at"),
					func(s *terraform.State) error {
						id = s.RootModule().Resources["virtscale_project.test"].Primary.ID
						if keys := fake.ProjectIdempotencyKeys(); len(keys) != 1 {
							return fmt.Errorf("the create sent %d Idempotency-Keys, want 1", len(keys))
						}
						return nil
					},
				),
			},
			{ // The same configuration plans empty.
				Config:             projectConfig(fake, "writer", "web", "the web tier", `{ team = "a", env = "test" }`),
				PlanOnly:           true,
				ExpectNonEmptyPlan: false,
			},
			{ // Name, description and labels change in place, under the generation of a fresh read.
				Config: projectConfig(fake, "writer", "web2", "renamed", `{ team = "b" }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction("virtscale_project.test", plancheck.ResourceActionUpdate),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("virtscale_project.test", "name", "web2"),
					resource.TestCheckResourceAttr("virtscale_project.test", "description", "renamed"),
					resource.TestCheckResourceAttr("virtscale_project.test", "labels.%", "1"),
					func(s *terraform.State) error {
						p, ok := fake.Project(id)
						if !ok || p.Name != "web2" || p.Description != "renamed" || len(p.Labels) != 1 || p.Labels["team"] != "b" {
							return fmt.Errorf("the API holds %+v", p)
						}
						patches := fake.ProjectPatches()
						if len(patches) != 1 || !strings.HasPrefix(patches[0], id+` "1" `) {
							return fmt.Errorf("PATCHes = %q, want one with If-Match \"1\"", patches)
						}
						return nil
					},
				),
			},
			{
				ResourceName:      "virtscale_project.test",
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateIdFunc: func(*terraform.State) (string, error) { return id, nil },
			},
		},
	})
}

func TestAccProjectResource_OmittedDescriptionAndLabelsPlanEmpty(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.AddKey("writer", "write")
	config := testAccProviderBlockWith(fake, "writer", testOrgID) + `resource "virtscale_project" "test" { name = "bare" }`
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             checkProjectGone(fake),
		Steps: []resource.TestStep{
			{Config: config, Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("virtscale_project.test", "description", ""),
				resource.TestCheckResourceAttr("virtscale_project.test", "labels.%", "0"),
			)},
			{Config: config, PlanOnly: true},
		},
	})
}

func TestAccProjectResource_TheAPIsRefusalsAreShownWithTheirCode(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.AddKey("writer", "write")
	taken := testAccProviderBlockWith(fake, "writer", testOrgID) + `
resource "virtscale_project" "one" { name = "same" }
resource "virtscale_project" "two" {
  name       = "same"
  depends_on = [virtscale_project.one]
}
`
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{{
			Config:      taken,
			ExpectError: wrapSafeInOrder("409 name_taken", "a project named 'same' already exists"),
		}},
	})
}

// Creating, renaming and deleting a project need the legacy write scope: compute:write only reads projects.
func TestAccProjectResource_AKeyWithoutTheWriteScopeIsToldSo(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID) // compute:write and network:write
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{{
			Config:      projectConfig(fake, "test-api-key", "web", "", `{}`),
			ExpectError: wrapSafeInOrder("403 permission_denied", "'project.create' permission", "needs the 'write' scope"),
		}},
	})
}

func TestAccProjectResource_VanishedOutOfBandIsRecreated(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.AddKey("writer", "write")
	config := testAccProviderBlockWith(fake, "writer", testOrgID) + `resource "virtscale_project" "test" { name = "gone" }`
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             checkProjectGone(fake),
		Steps: []resource.TestStep{
			{Config: config},
			{
				PreConfig: func() { fake.VanishProject(fakeUUID(5, 2)) },
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction("virtscale_project.test", plancheck.ResourceActionCreate),
				}},
			},
		},
	})
}

func TestAccProjectDataSource_ByNameAndTheDefault(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.AddKey("writer", "write")
	config := testAccProviderBlockWith(fake, "writer", testOrgID) + `
resource "virtscale_project" "web" { name = "web" }
data "virtscale_project" "by_name" {
  name       = virtscale_project.web.name
}
data "virtscale_project" "default" {}
`
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{{
			Config: config,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttrPair("data.virtscale_project.by_name", "id", "virtscale_project.web", "id"),
				resource.TestCheckResourceAttr("data.virtscale_project.by_name", "is_default", "false"),
				resource.TestCheckResourceAttr("data.virtscale_project.default", "id", fakeUUID(5, 1)),
				resource.TestCheckResourceAttr("data.virtscale_project.default", "name", "default"),
				resource.TestCheckResourceAttr("data.virtscale_project.default", "is_default", "true"),
			),
		}},
	})
}

func TestAccProjectDataSource_UnknownName(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{{
			Config:      testAccProviderBlock(fake) + `data "virtscale_project" "x" { name = "nope" }`,
			ExpectError: wrapSafe(`no project named "nope"`),
		}},
	})
}

// The API answers 404 not_found both for a missing project and for an organization the key cannot see; Read must not
// take the second for the first, or pointing the provider at the wrong organization would drop every project from state.
func TestAccProjectResource_WrongOrganizationKeepsTheResourceInState(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.AddKey("writer", "write")
	config := func(org string) string {
		return testAccProviderBlockWith(fake, "writer", org) + `resource "virtscale_project" "test" { name = "org-guard" }`
	}
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             checkProjectGone(fake),
		Steps: []resource.TestStep{
			{Config: config(testOrgID)},
			{Config: config(testOtherOrg), PlanOnly: true, ExpectError: regexp.MustCompile(`(?s)no\s+organization\s+matching.*check\s+VIRTSCALE_ORGANIZATION`)},
			{Config: config(testOrgID), PlanOnly: true},
		},
	})
}
