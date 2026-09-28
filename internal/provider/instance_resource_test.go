package provider

// These are Terraform acceptance tests: they exercise the real provider
// binary against tofu, driven by terraform-plugin-testing. They only run
// when TF_ACC=1 is set (resource.Test skips otherwise) and require tofu on
// PATH. The full invocation this repo uses:
//
//	TF_ACC=1 \
//	TF_ACC_TERRAFORM_PATH=$(command -v tofu) \
//	TF_ACC_PROVIDER_NAMESPACE=virtscale \
//	TF_ACC_PROVIDER_HOST=registry.terraform.io \
//	go test ./...
//
// testAccPreCheck additionally skips with a clear message when tofu is
// missing from PATH, so a plain `go test` without the env above degrades
// gracefully instead of failing on an exec error.

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// TestMain shortens pollInterval (an unexported package var; production code
// never overrides it) so acceptance tests converge in milliseconds instead
// of waiting out the real 5s poll cadence.
func TestMain(m *testing.M) {
	pollInterval = 10 * time.Millisecond
	os.Exit(m.Run())
}

// testAccProtoV6ProviderFactories builds a fresh provider instance per test
// invocation (version "test").
func testAccProtoV6ProviderFactories() map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"virtscale": providerserver.NewProtocol6WithError(New("test")()),
	}
}

func testAccPreCheck(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tofu"); err != nil {
		t.Skip("tofu not found on PATH; skipping acceptance test")
	}
}

func testAccProviderBlock(fake *fakeAPI) string {
	return fmt.Sprintf(`
provider "virtscale" {
  api_key      = %q
  organization = %q
  endpoint     = %q
}
`, fake.apiKey, fake.org, fake.URL())
}

func testAccInstanceConfig(fake *fakeAPI, name string, vcpus int) string {
	return testAccInstanceConfigImage(fake, name, vcpus, "image-1")
}

// testAccInstanceConfigImage is testAccInstanceConfig with an explicit image
// id, so a test can exercise a specific image (e.g. Finding 1's "alias-"
// affordance) without duplicating the rest of the config.
func testAccInstanceConfigImage(fake *fakeAPI, name string, vcpus int, image string) string {
	return testAccProviderBlock(fake) + fmt.Sprintf(`
resource "virtscale_instance" "test" {
  name            = %q
  region          = "gro"
  image           = %q
  vcpus           = %d
  memory_mb       = 1024
  disk_gb         = 10
  storage_pool    = "pool-1"
  network_profile = "net-1"
}
`, name, image, vcpus)
}

func testAccCheckInstanceDestroy(fake *fakeAPI) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources["virtscale_instance.test"]
		if !ok {
			return fmt.Errorf("virtscale_instance.test not found in state")
		}
		state, ok := fake.VmState(rs.Primary.ID)
		if !ok {
			return fmt.Errorf("vm %s missing from fake API", rs.Primary.ID)
		}
		if state != "deleted" {
			return fmt.Errorf("expected vm %s to be deleted, got state %q", rs.Primary.ID, state)
		}
		return nil
	}
}

// TestAccInstanceResource_Lifecycle covers scenarios (1)-(4): create ->
// running with attributes set, ImportStateVerify, a vcpus change planning a
// replace and ending running again, then destroy leaving the fake's copy of
// the vm in state "deleted".
func TestAccInstanceResource_Lifecycle(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", "test-org")

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceConfig(fake, "lifecycle-1", 1),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("virtscale_instance.test", "state", "running"),
					resource.TestCheckResourceAttr("virtscale_instance.test", "name", "lifecycle-1"),
					resource.TestCheckResourceAttr("virtscale_instance.test", "vcpus", "1"),
					resource.TestCheckResourceAttrSet("virtscale_instance.test", "id"),
					resource.TestCheckResourceAttrSet("virtscale_instance.test", "created_at"),
					resource.TestCheckResourceAttr("virtscale_instance.test", "private_ipv4", "10.128.3.11"),
				),
			},
			{
				ResourceName:            "virtscale_instance.test",
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{},
			},
			{
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("virtscale_instance.test", plancheck.ResourceActionDestroyBeforeCreate),
					},
				},
				Config: testAccInstanceConfig(fake, "lifecycle-1", 2),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("virtscale_instance.test", "state", "running"),
					resource.TestCheckResourceAttr("virtscale_instance.test", "vcpus", "2"),
				),
			},
		},
	})
}

// TestAccInstanceResource_BuildFailure covers scenario (5): a name starting
// with fail- drives the fake to state error, and apply fails with a
// diagnostic naming the build failure.
func TestAccInstanceResource_BuildFailure(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", "test-org")

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config:      testAccInstanceConfig(fake, "fail-this-one", 1),
				ExpectError: regexp.MustCompile(`could not build`),
			},
		},
	})
}

// TestAccInstanceResource_MissingCredentials covers scenario (6): a provider
// with no api_key/organization and no matching env vars fails Configure with
// a diagnostic naming the environment variable.
func TestAccInstanceResource_MissingCredentials(t *testing.T) {
	t.Setenv("VIRTSCALE_API_KEY", "")
	t.Setenv("VIRTSCALE_ORGANIZATION", "")
	t.Setenv("VIRTSCALE_ENDPOINT", "")

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: `
provider "virtscale" {}

resource "virtscale_instance" "test" {
  name            = "no-creds"
  region          = "gro"
  image           = "image-1"
  vcpus           = 1
  memory_mb       = 1024
  disk_gb         = 10
  storage_pool    = "pool-1"
  network_profile = "net-1"
}
`,
				ExpectError: regexp.MustCompile(`VIRTSCALE_API_KEY`),
			},
		},
	})
}

// TestAccInstanceResource_InvalidAPIKey covers scenario (7): a 401 from the
// API surfaces the problem+json detail in the diagnostic.
func TestAccInstanceResource_InvalidAPIKey(t *testing.T) {
	fake := newFakeAPI(t, "correct-key", "test-org")

	badConfig := fmt.Sprintf(`
provider "virtscale" {
  api_key      = "wrong-key"
  organization = %q
  endpoint     = %q
}

resource "virtscale_instance" "test" {
  name            = "bad-key"
  region          = "gro"
  image           = "image-1"
  vcpus           = 1
  memory_mb       = 1024
  disk_gb         = 10
  storage_pool    = "pool-1"
  network_profile = "net-1"
}
`, fake.org, fake.URL())

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config:      badConfig,
				ExpectError: regexp.MustCompile(`the api key is invalid`),
			},
		},
	})
}

// TestAccInstanceResource_ImageDrift covers Finding 1: creating with the
// fake's "alias-" image affordance -- whose echoed glance_id diverges from
// the submitted image id -- must not crash the apply with "Provider produced
// inconsistent result", and the divergence must surface as drift (a
// non-empty plan, via image's RequiresReplace) on the very next plan rather
// than being silently absorbed.
func TestAccInstanceResource_ImageDrift(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", "test-org")

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{
				// The divergence is visible starting with the very first
				// post-apply refresh, so ExpectNonEmptyPlan belongs on this
				// step itself: without it, the test framework's own
				// consistency check (an implicit refresh + plan after every
				// apply, expected empty by default) would fail here instead
				// of exercising what this test is actually about.
				Config: testAccInstanceConfigImage(fake, "image-drift", 1, "alias-img"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("virtscale_instance.test", "image", "alias-img"),
					resource.TestCheckResourceAttr("virtscale_instance.test", "state", "running"),
				),
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

// TestAccInstanceResource_VanishedOutOfBand covers Finding 4: a vm removed
// out-of-band in a way that surfaces as a genuine 404 (the fake's Vanish
// affordance, distinct from the normal deleting -> deleted state machine)
// must be dropped from state by Read, so the next plan proposes a create
// rather than silently treating the resource as still present.
func TestAccInstanceResource_VanishedOutOfBand(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", "test-org")
	var instanceID string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceConfig(fake, "vanish-me", 1),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("virtscale_instance.test", "state", "running"),
					func(s *terraform.State) error {
						rs, ok := s.RootModule().Resources["virtscale_instance.test"]
						if !ok {
							return fmt.Errorf("virtscale_instance.test not found in state")
						}
						instanceID = rs.Primary.ID
						return nil
					},
				),
			},
			{
				// PreConfig, not this step's own Check: the framework's
				// implicit post-apply "plan must be empty" verification for
				// step 1 runs against the live fake, so vanishing the vm
				// inside step 1 would trip that check instead of this one.
				PreConfig:          func() { fake.Vanish(instanceID) },
				Config:             testAccInstanceConfig(fake, "vanish-me", 1),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}
