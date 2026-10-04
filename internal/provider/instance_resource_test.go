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
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
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

// wrapSafe matches phrase however tofu wraps its diagnostic: any run of spaces
// in phrase matches any whitespace, newlines included.
func wrapSafe(phrase string) *regexp.Regexp {
	words := strings.Fields(phrase)
	for i, w := range words {
		words[i] = regexp.QuoteMeta(w)
	}
	return regexp.MustCompile(strings.Join(words, `\s+`))
}

func testAccPreCheck(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tofu"); err != nil {
		t.Skip("tofu not found on PATH; skipping acceptance test")
	}
}

func testAccProviderBlock(fake *fakeAPI) string {
	return testAccProviderBlockWith(fake, fake.apiKey, fake.org)
}

// testAccProviderBlockWith configures the provider against fake with an
// explicit key and organization, for tests about the credentials themselves.
func testAccProviderBlockWith(fake *fakeAPI, key, org string) string {
	return fmt.Sprintf(`
provider "virtscale" {
  api_key      = %q
  organization = %q
  endpoint     = %q
}
`, key, org, fake.URL())
}

func testAccInstanceConfig(fake *fakeAPI, name string, vcpus int) string {
	return testAccInstanceConfigImage(fake, name, vcpus, "image-1")
}

// testAccInstanceBlock is the virtscale_instance resource alone, so a test can
// pair it with a provider block of its own.
func testAccInstanceBlock(name string) string {
	return fmt.Sprintf(`
resource "virtscale_instance" "test" {
  name            = %q
  region          = "gro"
  image           = "image-1"
  vcpus           = 1
  memory_mb       = 1024
  disk_gb         = 10
  storage_pool    = "pool-1"
  network_profile = "net-1"
}
`, name)
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
	fake := newFakeAPI(t, "test-api-key", testOrgID)

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
	fake := newFakeAPI(t, "test-api-key", testOrgID)

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
// API surfaces the problem+json detail in the diagnostic. The detail is the
// one the deployed API sends for every bad credential (deps.py, 94f5021f).
func TestAccInstanceResource_InvalidAPIKey(t *testing.T) {
	fake := newFakeAPI(t, "correct-key", testOrgID)

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
				ExpectError: wrapSafe("the presented API token is not valid"),
			},
			{
				// ...and says where to look: the key may be wrong, revoked or expired.
				Config:      badConfig,
				ExpectError: wrapSafe("check VIRTSCALE_API_KEY: the key may be wrong, revoked or expired"),
			},
		},
	})
}

// TestAccInstanceResource_ImageAliasNoPerpetualDiff covers Finding N1 (plan
// D4). The old TestAccInstanceResource_ImageDrift asserted the opposite of
// what this test asserts: it required ExpectNonEmptyPlan immediately after
// apply, treating the fake's "alias-" image affordance (whose echoed
// glance_id diverges from the submitted image id) as drift that forces a
// replace. That fix traded Finding 1's apply-time crash for a perpetual
// replace loop -- accepting the replace never converged, because the new
// create hit the same systematic divergence every time. Per D4, image is a
// create-time input that neither Create nor Read ever overwrites from the
// API -- there is no image-drift detection in v0 -- so the divergence no
// longer surfaces as drift at all. This is a two-cycle test: step 1 applies
// and must already converge with an empty plan (old assertion: non-empty;
// new assertion: empty from the very first apply); step 2 re-applies the
// identical config and asserts the plan is still empty, proving the replace
// loop cannot recur. image_name -- new in this fix -- carries what the API
// actually reports running, unaffected by any of this. Step 3 confirms
// import still works for this same divergent image: per D4, import is the
// one case where Read does seed image, from vm.Image.GlanceID rather than
// the originally configured value, since there is no prior state to
// preserve -- so image is expected to differ after import for this
// specific, aliased resource and is ignored here (only here;
// TestAccInstanceResource_Lifecycle keeps verifying image for the normal,
// non-diverging case).
func TestAccInstanceResource_ImageAliasNoPerpetualDiff(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceConfigImage(fake, "image-alias", 1, "alias-img"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("virtscale_instance.test", "image", "alias-img"),
					resource.TestCheckResourceAttr("virtscale_instance.test", "image_name", "alias-img"),
					resource.TestCheckResourceAttr("virtscale_instance.test", "state", "running"),
				),
			},
			{
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectEmptyPlan(),
					},
				},
				Config: testAccInstanceConfigImage(fake, "image-alias", 1, "alias-img"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("virtscale_instance.test", "image", "alias-img"),
					resource.TestCheckResourceAttr("virtscale_instance.test", "image_name", "alias-img"),
				),
			},
			{
				ResourceName:            "virtscale_instance.test",
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"image"},
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
	fake := newFakeAPI(t, "test-api-key", testOrgID)
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

// TestAccInstanceResource_WrongOrganizationKeepsTheResourceInState guards
// against state loss. The API answers 404 not_found both for a missing vm and
// for an organization the key cannot see; if Read took the second for the
// first, pointing the provider at the wrong organization would silently drop
// every live resource from state (orphaning running, billed VMs). Step 2
// refreshes against another organization and must fail; step 3 refreshes
// against the right one and must find the vm still tracked (an empty plan).
func TestAccInstanceResource_WrongOrganizationKeepsTheResourceInState(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceConfig(fake, "org-guard", 1),
				Check:  resource.TestCheckResourceAttr("virtscale_instance.test", "state", "running"),
			},
			{
				Config:   testAccProviderBlockWith(fake, fake.apiKey, testOtherOrg) + testAccInstanceBlock("org-guard"),
				PlanOnly: true,
				// The API's own words, then where to look.
				ExpectError: regexp.MustCompile(`(?s)no\s+organization\s+matching.*check\s+VIRTSCALE_ORGANIZATION`),
			},
			{
				Config:   testAccInstanceConfig(fake, "org-guard", 1),
				PlanOnly: true,
			},
		},
	})
}

// TestAccInstanceResource_MalformedOrganizationNamesTheField: the API wants a
// UUID in the path and answers 400 invalid_request with errors[]; the
// diagnostic must carry the field and the reason, not only "failed validation".
func TestAccInstanceResource_MalformedOrganizationNamesTheField(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config:      testAccProviderBlockWith(fake, fake.apiKey, "my-company") + testAccInstanceBlock("bad-org"),
				ExpectError: regexp.MustCompile(`org: Input should be a valid UUID`),
			},
		},
	})
}

// TestAccInstanceResource_NameMustMatchWhatTheAPIStores covers M1. The API
// canonicalises a name (lower-case, trimmed, a trailing .virtscale.nl
// stripped; naming.py) and answers with the canonical one, so a configured
// "Web-01" would come back as "web-01" and fail the apply with "inconsistent
// result" after the VM already exists. The documented rule is therefore
// enforced at plan time, before anything is created.
func TestAccInstanceResource_NameMustMatchWhatTheAPIStores(t *testing.T) {
	for _, name := range []string{"Web-01", "web_01", "-web", "web-", "web.virtscale.nl", " web", strings.Repeat("a", 64)} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeAPI(t, "test-api-key", testOrgID)

			resource.Test(t, resource.TestCase{
				PreCheck:                 func() { testAccPreCheck(t) },
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
				Steps: []resource.TestStep{
					{
						Config:      testAccInstanceConfig(fake, name, 1),
						ExpectError: regexp.MustCompile(`Invalid instance name`),
					},
				},
			})
			if fake.CreateCount() != 0 {
				t.Errorf("a name the API would rewrite must be refused at plan time; %d vm(s) were created", fake.CreateCount())
			}
		})
	}
}

// TestAccInstanceResource_NameAtTheLimitIsAccepted: 63 characters is a DNS
// label's maximum and the API stores it as given.
func TestAccInstanceResource_NameAtTheLimitIsAccepted(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	name := strings.Repeat("a", 63)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceConfig(fake, name, 1),
				Check:  resource.TestCheckResourceAttr("virtscale_instance.test", "name", name),
			},
		},
	})
}

// vmIDFromState captures the id of virtscale_instance.test into *dst.
func vmIDFromState(dst *string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources["virtscale_instance.test"]
		if !ok {
			return fmt.Errorf("virtscale_instance.test not found in state")
		}
		*dst = rs.Primary.ID
		return nil
	}
}

// TestAccInstanceResource_DestroyWhileADeleteIsAlreadyRunning covers M3. A
// second DELETE while the first is still being carried out is 409
// operation_in_flight (uq_operations_one_in_flight): what an interrupted
// destroy, or a retried DELETE whose first answer was lost, runs into. The
// vm is on its way out, so destroy has to wait for it, not fail.
func TestAccInstanceResource_DestroyWhileADeleteIsAlreadyRunning(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.SetDeleteSteps(4)
	var id string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceConfig(fake, "delete-twice", 1),
				Check:  vmIDFromState(&id),
			},
			{
				PreConfig: func() {
					if status, _ := deleteVmVia(t, fake, fake.apiKey, id); status != http.StatusAccepted {
						t.Errorf("out-of-band delete: status = %d, want 202", status)
					}
				},
				Config:  testAccInstanceConfig(fake, "delete-twice", 1),
				Destroy: true,
			},
		},
	})
}

// TestAccInstanceResource_ReplacingAProtectedVmKeepsTheRefusal: not every 409
// on a delete means "already going". A protected vm answers 409 vm_protected
// and is not being deleted; that refusal has to reach the user unchanged, and
// the replace has to go through once protection is lifted.
func TestAccInstanceResource_ReplacingAProtectedVmKeepsTheRefusal(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	var id string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceConfig(fake, "protected-vm", 1),
				Check:  vmIDFromState(&id),
			},
			{
				PreConfig:   func() { fake.Protect(id, true) },
				Config:      testAccInstanceConfig(fake, "protected-vm", 2),
				ExpectError: regexp.MustCompile(`vm_protected`),
			},
			{
				PreConfig: func() { fake.Protect(id, false) },
				Config:    testAccInstanceConfig(fake, "protected-vm", 2),
				Check:     resource.TestCheckResourceAttr("virtscale_instance.test", "vcpus", "2"),
			},
		},
	})
}

// TestAccInstanceResource_PrivateIPv4ReadsWhatTheContractPromises covers M5.
// observed_addresses is Nova's address map, stored verbatim; the contract
// promises "addr" and little else (OS-EXT-IPS:type is an extension, and the
// platform's own readers and fixtures accept entries with nothing but addr).
// private_ipv4 must come out for entries without the type key, and without
// the version key, and must not take an explicitly floating entry for the
// private address.
func TestAccInstanceResource_PrivateIPv4ReadsWhatTheContractPromises(t *testing.T) {
	for _, name := range []string{"notype-vm", "bare-vm"} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeAPI(t, "test-api-key", testOrgID)

			resource.Test(t, resource.TestCase{
				PreCheck:                 func() { testAccPreCheck(t) },
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
				CheckDestroy:             testAccCheckInstanceDestroy(fake),
				Steps: []resource.TestStep{
					{
						Config: testAccInstanceConfig(fake, name, 1),
						Check:  resource.TestCheckResourceAttr("virtscale_instance.test", "private_ipv4", "10.128.3.11"),
					},
				},
			})
		})
	}
}

func testAccInstanceConfigSSHKeys(fake *fakeAPI, name string, keys ...string) string {
	quoted := make([]string, len(keys))
	for i, k := range keys {
		quoted[i] = fmt.Sprintf("%q", k)
	}
	return testAccProviderBlock(fake) + fmt.Sprintf(`
resource "virtscale_instance" "test" {
  name            = %q
  region          = "gro"
  image           = "image-1"
  vcpus           = 1
  memory_mb       = 1024
  disk_gb         = 10
  storage_pool    = "pool-1"
  network_profile = "net-1"
  ssh_keys        = [%s]
}
`, name, strings.Join(quoted, ", "))
}

// TestAccInstanceResource_SSHKeyNameReachesTheAPI: ssh_keys carries Nova
// keypair NAMES (VmCreateRequest.ssh_keys); the API records them as
// ssh_key_names. (No customer project has a keypair yet, so against the real
// API a build with a name fails; see README. This pins the request shape.)
func TestAccInstanceResource_SSHKeyNameReachesTheAPI(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	var id string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceConfigSSHKeys(fake, "with-key", "deploy"),
				Check: resource.ComposeAggregateTestCheckFunc(
					vmIDFromState(&id),
					resource.TestCheckResourceAttr("virtscale_instance.test", "ssh_keys.#", "1"),
					resource.TestCheckResourceAttr("virtscale_instance.test", "ssh_keys.0", "deploy"),
					func(*terraform.State) error {
						if got := fake.SSHKeyNames(id); len(got) != 1 || got[0] != "deploy" {
							return fmt.Errorf("the API recorded ssh_key_names %v, want [deploy]", got)
						}
						return nil
					},
				),
			},
		},
	})
}

// TestAccInstanceResource_MoreThanOneSSHKeyIsRefusedByTheAPI: an instance is
// given exactly one keypair, so the API refuses more with 422
// too_many_ssh_keys before it builds anything. The refusal and its reason
// reach the user, and nothing is created.
func TestAccInstanceResource_MoreThanOneSSHKeyIsRefusedByTheAPI(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config:      testAccInstanceConfigSSHKeys(fake, "two-keys", "a", "b"),
				ExpectError: regexp.MustCompile(`too_many_ssh_keys`),
			},
		},
	})
	if fake.CreateCount() != 0 {
		t.Errorf("the API refused the request, yet %d vm(s) were created", fake.CreateCount())
	}
}

// TestAccInstanceResource_SurvivesThrottlingAndLostResponses runs a whole
// create and destroy through the failures the client retries: two 429s, a
// create whose 202 is lost after the API processed it (the retry reuses the
// Idempotency-Key and gets the stored answer, so exactly one VM exists), a 503
// on a poll, and a DELETE whose answer is lost (the retry meets 409
// operation_in_flight, which destroy takes as "already going"). Every Retry-After
// is 0 so the test does not wait.
func TestAccInstanceResource_SurvivesThrottlingAndLostResponses(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{
				PreConfig: func() {
					fake.FailNext(2, matchAny, respondThrottle("0"))
					fake.FailNextAfterHandling(1, matchMethod(http.MethodPost), respondGatewayRetryAfter(502, "0"))
					fake.FailNext(1, matchMethod(http.MethodGet), respondUnavailable("0"))
				},
				Config: testAccInstanceConfig(fake, "flaky-net", 1),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("virtscale_instance.test", "state", "running"),
					func(*terraform.State) error {
						if n := fake.CreateCount(); n != 1 {
							return fmt.Errorf("the retried create built %d vms, want exactly 1", n)
						}
						keys := fake.IdempotencyKeys()
						if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
							return fmt.Errorf("Idempotency-Keys of the create attempts that reached the API = %v, want the same key twice", keys)
						}
						if n := fake.FaultsServed(); n != 4 {
							return fmt.Errorf("faults served = %d, want 4 (2 throttles, 1 lost create, 1 poll)", n)
						}
						return nil
					},
				),
			},
			{
				PreConfig: func() {
					fake.FailNextAfterHandling(1, matchMethod(http.MethodDelete), respondGatewayRetryAfter(504, "0"))
				},
				Config:  testAccInstanceConfig(fake, "flaky-net", 1),
				Destroy: true,
			},
		},
	})
}

// --- diagnostics as the user sees them -------------------------------------

// testAccStackConfig is an instance, a public IP associated to it and the
// images catalogue, configured with the given API key: everything the provider
// can read or write.
func testAccStackConfig(fake *fakeAPI, key string) string {
	return testAccProviderBlockWith(fake, key, fake.org) + `
data "virtscale_images" "gro" {
  region = "gro"
}

resource "virtscale_instance" "test" {
  name            = "scoped"
  region          = "gro"
  image           = "image-1"
  vcpus           = 1
  memory_mb       = 1024
  disk_gb         = 10
  storage_pool    = "pool-1"
  network_profile = "net-1"
}

resource "virtscale_public_ip" "test" {
  region      = "gro"
  instance_id = virtscale_instance.test.id
}
`
}

// TestAccStack_KeyScopesMatchWhatTheREADMESays pins the README's "API key
// scopes" section against the deployed permission table (permissions.py,
// transcribed into the fake). The default test key holds exactly compute:write
// and network:write, and applies the whole stack. Plan-only use, which only
// reads, works with the two read halves. Without network:read the refresh of a
// public IP fails, and the message says which scope to add.
func TestAccStack_KeyScopesMatchWhatTheREADMESays(t *testing.T) {
	fake := newFakeAPI(t, "write-key", testOrgID)
	fake.AddKey("read-halves", "compute:read", "network:read")
	fake.AddKey("compute-read", "compute:read")

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{
				Config: testAccStackConfig(fake, "write-key"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("virtscale_instance.test", "state", "running"),
					resource.TestCheckResourceAttr("virtscale_public_ip.test", "state", "associated"),
					resource.TestCheckResourceAttr("data.virtscale_images.gro", "images.#", "2"),
				),
			},
			{
				// refresh + plan with the read halves: no change, no error.
				Config:   testAccStackConfig(fake, "read-halves"),
				PlanOnly: true,
			},
			{
				Config:      testAccStackConfig(fake, "compute-read"),
				PlanOnly:    true,
				ExpectError: wrapSafe("the API key needs the 'network:read' or 'network:write' scope"),
			},
			{
				// back to the key that can destroy what step 1 built
				Config:   testAccStackConfig(fake, "write-key"),
				PlanOnly: true,
			},
		},
	})
}

func TestAccInstanceResource_ReadOnlyKeyIsToldWhichScopeItLacks(t *testing.T) {
	fake := newFakeAPI(t, "write-key", testOrgID)
	fake.AddKey("read-halves", "compute:read", "network:read")

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config:      testAccProviderBlockWith(fake, "read-halves", fake.org) + testAccInstanceBlock("read-only"),
				ExpectError: wrapSafe("this credential does not carry the 'compute.instance.create' permission"),
			},
			{
				Config:      testAccProviderBlockWith(fake, "read-halves", fake.org) + testAccInstanceBlock("read-only"),
				ExpectError: wrapSafe("the API key needs the 'compute:write' scope"),
			},
		},
	})
	if fake.CreateCount() != 0 {
		t.Errorf("the API refused the request, yet %d vm(s) were created", fake.CreateCount())
	}
}

func TestAccInstanceResource_SourceAddressNotAllowedSaysTheKeyIsRestricted(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.DenySourceAddress()

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config:      testAccInstanceConfig(fake, "restricted", 1),
				ExpectError: wrapSafe("the key's network limits do not include this address"),
			},
		},
	})
}
