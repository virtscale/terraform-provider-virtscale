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
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
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

// wrapSafeInOrder matches the phrases one after the other, with anything in
// between, each as wrapSafe would match it.
func wrapSafeInOrder(phrases ...string) *regexp.Regexp {
	parts := make([]string, len(phrases))
	for i, phrase := range phrases {
		words := strings.Fields(phrase)
		for j, w := range words {
			words[j] = regexp.QuoteMeta(w)
		}
		parts[i] = strings.Join(words, `\s+`)
	}
	return regexp.MustCompile(`(?s)` + strings.Join(parts, `.*`))
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

// testAccInstanceConfigExtra is testAccInstanceConfig with extra attribute
// lines appended to the instance resource, for the attributes the other
// helpers do not set (hostname, for instance). extra may be "" for none.
func testAccInstanceConfigExtra(fake *fakeAPI, name string, vcpus int, extra string) string {
	return testAccProviderBlock(fake) + fmt.Sprintf(`
resource "virtscale_instance" "test" {
  name            = %q
  region          = "gro"
  image           = "image-1"
  vcpus           = %d
  memory_mb       = 1024
  disk_gb         = 10
  storage_pool    = "pool-1"
  network_profile = "net-1"
  %s
}
`, name, vcpus, extra)
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
					resource.TestCheckResourceAttr("virtscale_instance.test", "status", "running"),
					resource.TestCheckResourceAttr("virtscale_instance.test", "name", "lifecycle-1"),
					resource.TestCheckResourceAttr("virtscale_instance.test", "vcpus", "1"),
					resource.TestCheckResourceAttrSet("virtscale_instance.test", "id"),
					resource.TestCheckResourceAttrSet("virtscale_instance.test", "created_at"),
					resource.TestCheckResourceAttr("virtscale_instance.test", "private_ipv4", "10.128.3.11"),
					// A configuration that never mentions a hostname leaves it null, and
					// an ordinary instance has no instance_type.
					resource.TestCheckNoResourceAttr("virtscale_instance.test", "hostname"),
					resource.TestCheckNoResourceAttr("virtscale_instance.test", "instance_type"),
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
// image_id diverges from the submitted image id) as drift that forces a
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
// one case where Read does seed image, from vm.Image.ImageID rather than
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

// TestAccInstanceResource_HostnameIsSentOnCreateAndStable: a configured
// hostname is sent on create and stored by the API, comes back on every read,
// and is no drift: the same configuration plans empty, and an import reads the
// hostname and the status back.
func TestAccInstanceResource_HostnameIsSentOnCreateAndStable(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	config := testAccInstanceConfigExtra(fake, "hostname-vm", 1, `hostname = "web.example.com"`)
	var id string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					vmIDFromState(&id),
					resource.TestCheckResourceAttr("virtscale_instance.test", "hostname", "web.example.com"),
					resource.TestCheckResourceAttr("virtscale_instance.test", "status", "running"),
					func(*terraform.State) error {
						if got := fake.Hostname(id); got != "web.example.com" {
							return fmt.Errorf("the API stored hostname %q, want web.example.com", got)
						}
						if n := fake.CallCount("PATCH compute/instances/{vm_id}"); n != 0 {
							return fmt.Errorf("a create sent %d PATCH(es), want 0", n)
						}
						return nil
					},
				),
			},
			{
				Config:   config,
				PlanOnly: true,
			},
			{
				ResourceName:      "virtscale_instance.test",
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 {
						return fmt.Errorf("imported %d resources, want 1", len(states))
					}
					a := states[0].Attributes
					if a["hostname"] != "web.example.com" {
						return fmt.Errorf("imported hostname = %q, want web.example.com", a["hostname"])
					}
					if a["status"] != "running" {
						return fmt.Errorf("imported status = %q, want running", a["status"])
					}
					return nil
				},
			},
		},
	})
}

// decoyObservedAddress reads vm id from the API and returns the address in its
// Nova-shaped observed_addresses map: the decoy the provider must no longer
// read from.
func decoyObservedAddress(t *testing.T, fake *fakeAPI, id string) string {
	t.Helper()
	_, vm := getVmVia(t, fake, fake.apiKey, id)
	byNet, _ := vm["observed_addresses"].(map[string]any)
	entries, _ := byNet[fakeNetwork].([]any)
	for _, e := range entries {
		if m, ok := e.(map[string]any); ok {
			if addr, ok := m["addr"].(string); ok {
				return addr
			}
		}
	}
	return ""
}

// TestAccInstanceResource_PrivateIPv4IsThePrivateIP: private_ipv4 is the API's
// private_ip. The Nova-shaped observed_addresses decoy holds a different
// address, so a provider that still reads it reports the wrong value.
func TestAccInstanceResource_PrivateIPv4IsThePrivateIP(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	var id string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceConfig(fake, "private-ip-vm", 1),
				Check: resource.ComposeAggregateTestCheckFunc(
					vmIDFromState(&id),
					resource.TestCheckResourceAttr("virtscale_instance.test", "private_ipv4", "10.128.3.11"),
					func(*terraform.State) error {
						if decoy := decoyObservedAddress(t, fake, id); !strings.HasPrefix(decoy, "198.51.100.") {
							return fmt.Errorf("observed_addresses decoy = %q, want a 198.51.100.x address private_ipv4 must not be taken from", decoy)
						}
						return nil
					},
				),
			},
		},
	})
}

// TestAccInstanceResource_PrivateIPv4IsEmptyWhenTheAPISaysNull: a vm with no
// private address reports private_ipv4 as the empty string, and that empty
// value is not drift.
func TestAccInstanceResource_PrivateIPv4IsEmptyWhenTheAPISaysNull(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceConfig(fake, "noip-vm", 1),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("virtscale_instance.test", "private_ipv4", ""),
					resource.TestCheckResourceAttr("virtscale_instance.test", "state", "running"),
				),
			},
		},
	})
}

// TestAccInstanceResource_NoHostnameStaysNull: a configuration that never
// mentions a hostname leaves it null, and keeps planning nothing afterwards.
func TestAccInstanceResource_NoHostnameStaysNull(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	config := testAccInstanceConfig(fake, "no-hostname", 1)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check:  resource.TestCheckNoResourceAttr("virtscale_instance.test", "hostname"),
			},
			{
				Config:   config,
				PlanOnly: true,
			},
		},
	})
}

// TestAccInstanceResource_InstanceTypeAndStatusAreReadFromTheAPI: status is the
// API's status word; instance_type is what the API reports when it reports one.
func TestAccInstanceResource_InstanceTypeAndStatusAreReadFromTheAPI(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	config := testAccInstanceConfig(fake, "typed-vm", 1)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("virtscale_instance.test", "instance_type", "general.small"),
					resource.TestCheckResourceAttr("virtscale_instance.test", "status", "running"),
				),
			},
			{
				ResourceName:      "virtscale_instance.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

// TestAccInstanceResource_ChangingTheHostnameIsAnInPlaceUpdate: a hostname
// change is one PATCH carrying the generation read from a fresh GET and an
// Idempotency-Key -- never a replacement -- and the next plan has nothing left.
func TestAccInstanceResource_ChangingTheHostnameIsAnInPlaceUpdate(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	var id string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceConfigExtra(fake, "hostname-move", 1, `hostname = "a.example.com"`),
				Check:  vmIDFromState(&id),
			},
			{
				Config: testAccInstanceConfigExtra(fake, "hostname-move", 1, `hostname = "b.example.com"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("virtscale_instance.test", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("virtscale_instance.test", "hostname", "b.example.com"),
					func(s *terraform.State) error {
						rs := s.RootModule().Resources["virtscale_instance.test"]
						if rs == nil {
							return fmt.Errorf("virtscale_instance.test not found in state")
						}
						if rs.Primary.ID != id {
							return fmt.Errorf("the instance was replaced: id = %s, want %s unchanged", rs.Primary.ID, id)
						}
						if n := fake.CallCount("POST compute/instances"); n != 1 {
							return fmt.Errorf("POSTs = %d, want 1: a hostname change must not replace the instance", n)
						}
						calls := fake.PatchCalls()
						if len(calls) != 1 {
							return fmt.Errorf("PATCHes = %d (%+v), want exactly 1", len(calls), calls)
						}
						got := calls[0]
						if got.IfMatch != `"1"` {
							return fmt.Errorf("If-Match = %q, want the generation read before the patch, \"1\"", got.IfMatch)
						}
						if got.Body != `{"hostname":"b.example.com"}` {
							return fmt.Errorf("PATCH body = %q, want {\"hostname\":\"b.example.com\"}", got.Body)
						}
						if got.IdempotencyKey == "" {
							return fmt.Errorf("the PATCH carried no Idempotency-Key")
						}
						if g := fake.Generation(id); g != 2 {
							return fmt.Errorf("generation = %d, want 2 after one hostname change", g)
						}
						if h := fake.Hostname(id); h != "b.example.com" {
							return fmt.Errorf("stored hostname = %q, want b.example.com", h)
						}
						return nil
					},
				),
			},
			{
				Config: testAccInstanceConfigExtra(fake, "hostname-move", 1, `hostname = "b.example.com"`),
				Check: func(*terraform.State) error {
					if n := len(fake.PatchCalls()); n != 1 {
						return fmt.Errorf("the plan sent a PATCH (%d total), want only the one change", n)
					}
					return nil
				},
			},
		},
	})
}

// TestAccInstanceResource_TheGenerationComesFromAFreshRead: after an
// out-of-band hostname change, Update's If-Match carries the generation of a
// fresh read, not the one the state remembers (which would be 412).
func TestAccInstanceResource_TheGenerationComesFromAFreshRead(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	config := testAccInstanceConfigExtra(fake, "drift-back", 1, `hostname = "a.example.com"`)
	var id string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check:  vmIDFromState(&id),
			},
			{
				PreConfig: func() {
					drift := "drift.example.com"
					if status, _ := patchHostnameVia(t, fake, fake.apiKey, id, &drift); status != http.StatusOK {
						t.Errorf("out-of-band hostname patch: status = %d, want 200", status)
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("virtscale_instance.test", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("virtscale_instance.test", "hostname", "a.example.com"),
					func(*terraform.State) error {
						calls := fake.PatchCalls()
						if len(calls) == 0 {
							return fmt.Errorf("no PATCH reached the API")
						}
						if last := calls[len(calls)-1]; last.IfMatch != `"2"` {
							return fmt.Errorf("the last PATCH used If-Match %q, want the freshly read generation \"2\"", last.IfMatch)
						}
						return nil
					},
				),
			},
		},
	})
}

// TestAccInstanceResource_ATakenHostnameIsRefusedWithTheAPIsSentence: a
// hostname another instance of the organization holds is refused by the API,
// on create and on update, with its own sentence.
func TestAccInstanceResource_ATakenHostnameIsRefusedWithTheAPIsSentence(t *testing.T) {
	const sentence = "the hostname 'taken.example.com' is already in use by another instance of this organization"

	t.Run("on create", func(t *testing.T) {
		fake := newFakeAPI(t, "test-api-key", testOrgID)

		resource.Test(t, resource.TestCase{
			PreCheck:                 func() { testAccPreCheck(t) },
			ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
			Steps: []resource.TestStep{
				{
					PreConfig: func() {
						createVmVia(t, fake, fake.apiKey, vmWithHostname("holder", "taken.example.com"))
					},
					Config:      testAccInstanceConfigExtra(fake, "wants-taken", 1, `hostname = "taken.example.com"`),
					ExpectError: wrapSafe(sentence),
				},
			},
		})
		if fake.CreateCount() != 1 {
			t.Errorf("creates = %d, want 1 (only the holder)", fake.CreateCount())
		}
	})

	t.Run("on update", func(t *testing.T) {
		fake := newFakeAPI(t, "test-api-key", testOrgID)
		var id string

		resource.Test(t, resource.TestCase{
			PreCheck:                 func() { testAccPreCheck(t) },
			ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
			Steps: []resource.TestStep{
				{
					Config: testAccInstanceConfigExtra(fake, "moves-to-taken", 1, `hostname = "a.example.com"`),
					Check:  vmIDFromState(&id),
				},
				{
					PreConfig: func() {
						createVmVia(t, fake, fake.apiKey, vmWithHostname("holder", "taken.example.com"))
					},
					Config:      testAccInstanceConfigExtra(fake, "moves-to-taken", 1, `hostname = "taken.example.com"`),
					ExpectError: wrapSafe(sentence),
				},
			},
		})
		if fake.CreateCount() != 2 {
			t.Errorf("creates = %d, want 2 (the instance and the holder): no replacement must happen", fake.CreateCount())
		}
	})
}

// TestAccInstanceResource_ReplacingKeepsTheHostname: hostname survives a
// replacement (vcpus changed): the destroyed instance releases the name, so the
// replacement may take it again.
func TestAccInstanceResource_ReplacingKeepsTheHostname(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceConfigExtra(fake, "keep-hostname", 1, `hostname = "a.example.com"`),
				Check:  resource.TestCheckResourceAttr("virtscale_instance.test", "hostname", "a.example.com"),
			},
			{
				Config: testAccInstanceConfigExtra(fake, "keep-hostname", 2, `hostname = "a.example.com"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("virtscale_instance.test", plancheck.ResourceActionDestroyBeforeCreate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("virtscale_instance.test", "hostname", "a.example.com"),
					resource.TestCheckResourceAttr("virtscale_instance.test", "vcpus", "2"),
					func(*terraform.State) error {
						if n := fake.CallCount("PATCH compute/instances/{vm_id}"); n != 0 {
							return fmt.Errorf("a replacement sent %d PATCH(es), want 0", n)
						}
						return nil
					},
				),
			},
		},
	})
}

// TestAccInstanceResource_AnInvalidHostnameIsRefusedAtPlanTime: a hostname that
// does not match the API's rule, or is too long, is refused before anything is
// sent.
func TestAccInstanceResource_AnInvalidHostnameIsRefusedAtPlanTime(t *testing.T) {
	for _, hostname := range []string{"Web.Example.com", "nodots", "-bad.example.com", strings.Repeat("a", 254), "a..example.com"} {
		t.Run(hostname, func(t *testing.T) {
			fake := newFakeAPI(t, "test-api-key", testOrgID)

			resource.Test(t, resource.TestCase{
				PreCheck:                 func() { testAccPreCheck(t) },
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
				Steps: []resource.TestStep{
					{
						Config:      testAccInstanceConfigExtra(fake, "bad-hostname", 1, fmt.Sprintf("hostname = %q", hostname)),
						ExpectError: wrapSafe("Invalid hostname"),
					},
				},
			})
			if fake.CreateCount() != 0 {
				t.Errorf("an invalid hostname created %d vm(s)", fake.CreateCount())
			}
			if n := fake.CallCount("POST compute/instances"); n != 0 {
				t.Errorf("an invalid hostname sent %d create request(s), want 0", n)
			}
		})
	}
}

// TestAccInstanceResource_RemovingTheHostnameFromTheConfigurationKeepsIt: the
// attribute is Computed, so dropping the line keeps whatever the API holds and
// plans nothing.
func TestAccInstanceResource_RemovingTheHostnameFromTheConfigurationKeepsIt(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceConfigExtra(fake, "drop-hostname", 1, `hostname = "a.example.com"`),
				Check:  resource.TestCheckResourceAttr("virtscale_instance.test", "hostname", "a.example.com"),
			},
			{
				Config: testAccInstanceConfig(fake, "drop-hostname", 1),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("virtscale_instance.test", "hostname", "a.example.com"),
					func(*terraform.State) error {
						if n := fake.CallCount("PATCH compute/instances/{vm_id}"); n != 0 {
							return fmt.Errorf("dropping the hostname from the configuration sent %d PATCH(es), want 0", n)
						}
						return nil
					},
				),
			},
		},
	})
}

// TestAccInstanceResource_ImportOfAnInstanceWithAHostname: an instance made out
// of band with a hostname imports it (and its status), and both a configuration
// without the hostname and one with the same hostname plan nothing.
func TestAccInstanceResource_ImportOfAnInstanceWithAHostname(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	without := testAccInstanceConfig(fake, "imported-h", 1)
	with := testAccInstanceConfigExtra(fake, "imported-h", 1, `hostname = "imp.example.com"`)
	var id string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{
				PreConfig: func() {
					id = createVmVia(t, fake, fake.apiKey, vmWithHostname("imported-h", "imp.example.com")).get("vm")["id"].(string)
					runVm(t, fake, fake.apiKey, id)
				},
				Config:             without,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				ResourceName:       "virtscale_instance.test",
				ImportState:        true,
				ImportStatePersist: true,
				ImportStateIdFunc:  func(*terraform.State) (string, error) { return id, nil },
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 {
						return fmt.Errorf("imported %d resources, want 1", len(states))
					}
					a := states[0].Attributes
					if a["hostname"] != "imp.example.com" {
						return fmt.Errorf("imported hostname = %q, want imp.example.com", a["hostname"])
					}
					if a["status"] != "running" {
						return fmt.Errorf("imported status = %q, want running", a["status"])
					}
					return nil
				},
			},
			{
				Config: without,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				Config: with,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

// TestAccInstanceResource_SurvivesALostPatchAnswer: the PATCH is applied, its
// answer is lost, and the client repeats the keyed request; the API replays the
// stored answer, so the hostname is applied once and the apply succeeds.
func TestAccInstanceResource_SurvivesALostPatchAnswer(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	var id string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceConfigExtra(fake, "lost-patch", 1, `hostname = "a.example.com"`),
				Check:  vmIDFromState(&id),
			},
			{
				PreConfig: func() {
					fake.FailNextAfterHandling(1, matchRoute(http.MethodPatch, "/compute/instances/"), respondDrop)
				},
				Config: testAccInstanceConfigExtra(fake, "lost-patch", 1, `hostname = "b.example.com"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("virtscale_instance.test", "hostname", "b.example.com"),
					func(*terraform.State) error {
						if g := fake.Generation(id); g != 2 {
							return fmt.Errorf("generation = %d, want 2: the keyed retry must not apply twice", g)
						}
						calls := fake.PatchCalls()
						if len(calls) != 2 {
							return fmt.Errorf("PATCHes = %d (%+v), want 2 (the lost one and its retry)", len(calls), calls)
						}
						if calls[0].IdempotencyKey == "" || calls[0].IdempotencyKey != calls[1].IdempotencyKey {
							return fmt.Errorf("Idempotency-Keys = %q and %q, want the same non-empty key repeated", calls[0].IdempotencyKey, calls[1].IdempotencyKey)
						}
						if calls[0].IfMatch != calls[1].IfMatch {
							return fmt.Errorf("If-Match = %q and %q, want the same condition repeated", calls[0].IfMatch, calls[1].IfMatch)
						}
						return nil
					},
				),
			},
		},
	})
}

// TestAccInstanceResource_NeverCallsTheDeprecatedRoutes: a whole life -- create,
// import, hostname change, destroy -- reaches the renamed routes only, never
// /compute/vms.
func TestAccInstanceResource_NeverCallsTheDeprecatedRoutes(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy: func(s *terraform.State) error {
			if err := testAccCheckInstanceDestroy(fake)(s); err != nil {
				return err
			}
			paths := fake.RequestedPaths()
			if len(paths) == 0 {
				return fmt.Errorf("the fake saw no request at all")
			}
			for _, p := range paths {
				if strings.Contains(p, "/compute/vms") {
					return fmt.Errorf("the provider called the deprecated route %q", p)
				}
				if strings.Contains(p, "/compute/") &&
					!strings.Contains(p, "/compute/images") &&
					!strings.Contains(p, "/compute/public-ips") &&
					!strings.Contains(p, "/compute/ssh-keys") &&
					!strings.Contains(p, "/compute/instances") {
					return fmt.Errorf("the provider called an unexpected compute route %q", p)
				}
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceConfigExtra(fake, "no-legacy", 1, `hostname = "a.example.com"`),
				Check:  resource.TestCheckResourceAttr("virtscale_instance.test", "hostname", "a.example.com"),
			},
			{
				ResourceName:      "virtscale_instance.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
			{
				Config: testAccInstanceConfigExtra(fake, "no-legacy", 1, `hostname = "b.example.com"`),
				Check:  resource.TestCheckResourceAttr("virtscale_instance.test", "hostname", "b.example.com"),
			},
		},
	})
}

// testKeySpec is one virtscale_ssh_key of a test configuration.
type testKeySpec struct{ label, name, publicKey string }

var (
	keyAlpha = testKeySpec{"a", "alpha", testKeyEd25519}
	keyBeta  = testKeySpec{"b", "beta", testKeyOther}
)

// hclList renders names as an HCL list of strings.
func hclList(names ...string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%q", n)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// testAccKeysAndInstance is the provider, the given keys and an instance whose
// ssh_keys is the HCL expression sshKeysExpr (left out when it is empty).
func testAccKeysAndInstance(fake *fakeAPI, vmName, sshKeysExpr string, keys ...testKeySpec) string {
	var b strings.Builder
	b.WriteString(testAccProviderBlock(fake))
	for _, k := range keys {
		b.WriteString(testAccSshKeyBlock(k.label, k.name, fmt.Sprintf("%q", k.publicKey)))
	}
	sshKeysLine := ""
	if sshKeysExpr != "" {
		sshKeysLine = "ssh_keys        = " + sshKeysExpr
	}
	fmt.Fprintf(&b, `
resource "virtscale_instance" "test" {
  name            = %q
  region          = "gro"
  image           = "image-1"
  vcpus           = 1
  memory_mb       = 1024
  disk_gb         = 10
  storage_pool    = "pool-1"
  network_profile = "net-1"
  %s
}
`, vmName, sshKeysLine)
	return b.String()
}

// TestAccInstanceResource_SSHKeysComeFromTheKeystore: ssh_keys names keys that
// were imported into the organization (virtscale_ssh_key), in the order given;
// the API records the names it resolved. Importing the instance reads them back,
// so the imported state equals the applied one.
func TestAccInstanceResource_SSHKeysComeFromTheKeystore(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	config := testAccKeysAndInstance(fake, "with-keys", "[virtscale_ssh_key.b.name, virtscale_ssh_key.a.name]", keyAlpha, keyBeta)
	var id string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					vmIDFromState(&id),
					resource.TestCheckResourceAttr("virtscale_instance.test", "ssh_keys.#", "2"),
					resource.TestCheckResourceAttr("virtscale_instance.test", "ssh_keys.0", "beta"),
					resource.TestCheckResourceAttr("virtscale_instance.test", "ssh_keys.1", "alpha"),
					func(*terraform.State) error {
						if got := fake.SSHKeyNames(id); len(got) != 2 || got[0] != "beta" || got[1] != "alpha" {
							return fmt.Errorf("the API recorded ssh_key_names %v, want [beta alpha]", got)
						}
						return nil
					},
				),
			},
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				ResourceName:      "virtscale_instance.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

// TestAccInstanceResource_AnUnknownSSHKeyIsRefusedByTheAPI: the API resolves the
// names against the organization's keystore and refuses one that was never
// imported; its words reach the user and no instance is built.
func TestAccInstanceResource_AnUnknownSSHKeyIsRefusedByTheAPI(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config:      testAccKeysAndInstance(fake, "unknown-key", hclList("nope")),
				ExpectError: wrapSafe("ssh_key_not_found: no SSH key named 'nope' in this organization"),
			},
		},
	})
	if fake.CreateCount() != 0 {
		t.Errorf("the API refused the request, yet %d vm(s) were created", fake.CreateCount())
	}
}

// TestAccInstanceResource_MoreThanTenSSHKeysAreRefusedBeforeAnythingIsBuilt: at
// most 10 keys, checked at plan time with the API's own words (S7). Nothing is
// sent to the API.
func TestAccInstanceResource_MoreThanTenSSHKeysAreRefusedBeforeAnythingIsBuilt(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	names := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("key-%02d", i)
		}
		return out
	}

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				// Ten is allowed: the plan wants to create the instance.
				Config:             testAccKeysAndInstance(fake, "ten-keys", hclList(names(10)...)),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config:      testAccKeysAndInstance(fake, "eleven-keys", hclList(names(11)...)),
				ExpectError: wrapSafe("a VM can be given at most 10 SSH keys"),
			},
		},
	})
	if calls := fake.Calls(); len(calls) != 0 {
		t.Errorf("the plan-time checks must not need the API, yet it was called: %v", calls)
	}
}

// TestAccInstanceResource_ASSHKeyListedTwiceIsRefusedBeforeAnythingIsBuilt: no
// name twice, at plan time, with the API's own words (S8).
func TestAccInstanceResource_ASSHKeyListedTwiceIsRefusedBeforeAnythingIsBuilt(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config:      testAccKeysAndInstance(fake, "twice", hclList("alpha", "beta", "alpha")),
				ExpectError: wrapSafe("SSH key 'alpha' is listed more than once"),
			},
		},
	})
	if calls := fake.Calls(); len(calls) != 0 {
		t.Errorf("the plan-time checks must not need the API, yet it was called: %v", calls)
	}
}

// TestAccInstanceResource_ImportSeedsSSHKeysAndPlansNoReplacement covers M14.
// An instance made with keys, imported, reads its ssh_key_names back into
// ssh_keys, so the next plan has nothing to change. Before, the imported state
// had no ssh_keys, and the plan replaced the instance.
func TestAccInstanceResource_ImportSeedsSSHKeysAndPlansNoReplacement(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	config := testAccKeysAndInstance(fake, "imported", hclList("alpha", "beta"))
	var id string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{
				// An instance that already exists, made out of band with two keys; the
				// state does not know it. This step also fixes the configuration the
				// import below runs against.
				PreConfig: func() {
					importKeyVia(t, fake, "alpha", testKeyEd25519)
					importKeyVia(t, fake, "beta", testKeyOther)
					id = createVmVia(t, fake, fake.apiKey, vmWithKeys("imported", "alpha", "beta")).get("vm")["id"].(string)
					runVm(t, fake, fake.apiKey, id)
				},
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				ResourceName:       "virtscale_instance.test",
				ImportState:        true,
				ImportStatePersist: true,
				ImportStateIdFunc:  func(*terraform.State) (string, error) { return id, nil },
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 {
						return fmt.Errorf("imported %d resources, want 1", len(states))
					}
					a := states[0].Attributes
					if a["ssh_keys.#"] != "2" || a["ssh_keys.0"] != "alpha" || a["ssh_keys.1"] != "beta" {
						return fmt.Errorf("imported ssh_keys = %v, want [alpha beta] read back from ssh_key_names", a)
					}
					return nil
				},
			},
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

// TestAccInstanceResource_NoSSHKeysStayNull: an instance made without keys has
// no ssh_keys: the API returns an empty list, and Read keeps null, so a
// configuration that says nothing about keys has nothing to change.
func TestAccInstanceResource_NoSSHKeysStayNull(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceConfig(fake, "no-keys", 1),
				Check:  resource.TestCheckNoResourceAttr("virtscale_instance.test", "ssh_keys.#"),
			},
			{
				Config: testAccInstanceConfig(fake, "no-keys", 1),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				ResourceName:      "virtscale_instance.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

// TestAccInstanceResource_AnEmptySSHKeysListStaysEmpty: a configuration that
// says ssh_keys = [] and an API that returns no names agree: the state keeps the
// empty list, and the next plan has nothing to change.
func TestAccInstanceResource_AnEmptySSHKeysListStaysEmpty(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	config := testAccKeysAndInstance(fake, "empty-list", "[]")

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check:  resource.TestCheckResourceAttr("virtscale_instance.test", "ssh_keys.#", "0"),
			},
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

// TestAccInstanceResource_SSHKeysStayInTheOrderTheyAreConfiguredIn: a
// deployment that reports ssh_key_names in another order, the same names, is no
// change. A reordered list read back must not plan the instance's replacement.
func TestAccInstanceResource_SSHKeysStayInTheOrderTheyAreConfiguredIn(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	config := testAccKeysAndInstance(fake, "ordered", "[virtscale_ssh_key.b.name, virtscale_ssh_key.a.name]", keyAlpha, keyBeta)
	var id string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{Config: config, Check: vmIDFromState(&id)},
			{
				PreConfig: func() { fake.SetSSHKeyNames(id, "alpha", "beta") },
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckResourceAttr("virtscale_instance.test", "ssh_keys.0", "beta"),
			},
		},
	})
}

// TestAccInstanceResource_ChangingSSHKeysReplacesTheInstance: the keys are
// installed at first boot, so a different list is a different instance.
func TestAccInstanceResource_ChangingSSHKeysReplacesTheInstance(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{
				Config: testAccKeysAndInstance(fake, "changing-keys", "[virtscale_ssh_key.a.name]", keyAlpha, keyBeta),
				Check:  resource.TestCheckResourceAttr("virtscale_instance.test", "ssh_keys.#", "1"),
			},
			{
				Config: testAccKeysAndInstance(fake, "changing-keys", "[virtscale_ssh_key.a.name, virtscale_ssh_key.b.name]", keyAlpha, keyBeta),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction("virtscale_instance.test", plancheck.ResourceActionDestroyBeforeCreate)},
				},
				Check: resource.TestCheckResourceAttr("virtscale_instance.test", "ssh_keys.#", "2"),
			},
		},
	})
	if fake.CreateCount() != 2 {
		t.Errorf("creates = %d, want 2 (the original and its replacement)", fake.CreateCount())
	}
}

// TestAccInstanceResource_DeletingAKeyLaterDoesNotChangeTheInstance: keys are a
// launch-time snapshot. A key deleted afterwards is recreated by the next apply
// (it is gone from the keystore), and the instance that used it is untouched.
func TestAccInstanceResource_DeletingAKeyLaterDoesNotChangeTheInstance(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	config := testAccKeysAndInstance(fake, "keeps-keys", "[virtscale_ssh_key.a.name]", keyAlpha)
	var vmID, keyID string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					vmIDFromState(&vmID),
					func(s *terraform.State) error {
						keyID = s.RootModule().Resources["virtscale_ssh_key.a"].Primary.ID
						return nil
					},
				),
			},
			{
				PreConfig: func() {
					if status, _, body := fakeHTTP(t, fake, fake.apiKey, http.MethodDelete, sshKeysPath(fake.org)+"/"+keyID, ""); status != http.StatusNoContent {
						t.Errorf("out-of-band delete of the key: %d %s", status, body)
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("virtscale_ssh_key.a", plancheck.ResourceActionCreate),
						plancheck.ExpectResourceAction("virtscale_instance.test", plancheck.ResourceActionNoop),
					},
				},
				Check: func(*terraform.State) error {
					if got := fake.SSHKeyNames(vmID); len(got) != 1 || got[0] != "alpha" {
						return fmt.Errorf("the instance lists %v after its key was deleted, want [alpha] still", got)
					}
					return nil
				},
			},
		},
	})
	if fake.CreateCount() != 1 {
		t.Errorf("creates = %d, want 1: the instance must not be replaced", fake.CreateCount())
	}
}

// TestAccInstanceResource_SSHKeysNotSwitchedOnIsShownAndNotRetried: while the
// keystore is off, a non-empty ssh_keys is 503 not_ready, which says SSH keys
// are not switched on for this API yet. The request is sent once; nothing is built.
func TestAccInstanceResource_SSHKeysNotSwitchedOnIsShownAndNotRetried(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				PreConfig: func() {
					importKeyVia(t, fake, "alpha", testKeyEd25519)
					fake.SetSSHKeysOn(false)
				},
				Config:      testAccKeysAndInstance(fake, "keys-off", hclList("alpha")),
				ExpectError: wrapSafe("not_ready: SSH keys are not switched on yet"),
			},
		},
	})
	if n := fake.CallCount("POST compute/instances"); n != 1 {
		t.Errorf("the create was sent %d times, want exactly once: not_ready is never retried", n)
	}
	if fake.CreateCount() != 0 {
		t.Errorf("%d vm(s) were created", fake.CreateCount())
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

// testAccStackConfig is an SSH key, an instance that uses it, a public IP
// associated to the instance and the images catalogue, configured with the
// given API key: everything the provider can read or write.
func testAccStackConfig(fake *fakeAPI, key string) string {
	return testAccProviderBlockWith(fake, key, fake.org) + `
data "virtscale_images" "gro" {
  region = "gro"
}

resource "virtscale_ssh_key" "test" {
  name       = "scoped"
  public_key = "` + testKeyEd25519 + `"
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
  ssh_keys        = [virtscale_ssh_key.test.name]
}

resource "virtscale_public_ip" "test" {
  region      = "gro"
  instance_id = virtscale_instance.test.id
}
`
}

// TestAccStack_KeyScopesMatchWhatTheREADMESays pins the README's "API key
// scopes" section against the deployed permission table (permissions.py,
// transcribed into the fake, plus the four compute.ssh_key.* permissions of the
// TF7 plan). The default test key holds exactly compute:write and
// network:write, and applies the whole stack. Plan-only use, which only reads,
// works with the two read halves. Without network:read the refresh of a public
// IP fails, and the message says which scope to add.
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

// --- 404s: only "this resource is gone" is gone -----------------------------

// TestAccInstanceResource_ReadErrorsThatAreNotTheVmBeingGoneKeepTheResource: a
// 404 means "this vm is gone" only when it carries the vm sentence of the API's
// not_found helper. A mistyped endpoint, a proxy's page, and the sentence for
// the compute service, a region or another kind of resource are errors, and the
// instance stays in the state (V4 L4).
func TestAccInstanceResource_ReadErrorsThatAreNotTheVmBeingGoneKeepTheResource(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	config := testAccInstanceConfig(fake, "keep-me", 1)
	variants := notFoundVariants()

	steps := []resource.TestStep{{Config: config}}
	for _, name := range sortedVariantNames(variants) {
		respond := variants[name]
		steps = append(steps, resource.TestStep{
			PreConfig: func() {
				fake.ClearFaults()
				fake.FailNext(1000, matchRoute(http.MethodGet, "/compute/instances/"), respond)
			},
			Config:      config,
			PlanOnly:    true,
			ExpectError: wrapSafe("Unable to Read Instance"),
		})
	}
	steps = append(steps, resource.TestStep{
		PreConfig: fake.ClearFaults,
		Config:    config,
		PlanOnly:  true,
	})

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps:                    steps,
	})
}

// TestAccInstanceResource_DestroyDoesNotTakeAMistypedEndpointForADeletedVm: the
// wait after a DELETE ends when the vm reads as gone, which is the vm sentence
// and nothing else. If every read answers with an unknown route's 404, destroy
// fails and the instance stays tracked, instead of reporting a deletion nobody
// confirmed.
func TestAccInstanceResource_DestroyDoesNotTakeAMistypedEndpointForADeletedVm(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	var id string
	deleteSeen := false

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceConfig(fake, "wait-404", 1),
				Check:  vmIDFromState(&id),
			},
			{
				// The DELETE goes through; every read after it is the wrong 404.
				PreConfig: func() {
					fake.FailNext(1000, func(r *http.Request) bool {
						if r.Method == http.MethodDelete {
							deleteSeen = true
						}
						return deleteSeen && matchRoute(http.MethodGet, "/compute/instances/")(r)
					}, respondNoSuchRoute)
				},
				Config:      testAccProviderBlock(fake),
				ExpectError: wrapSafe("Unable to Wait for Instance Deletion"),
			},
			{
				PreConfig: fake.ClearFaults,
				Config:    testAccProviderBlock(fake),
			},
		},
	})
	if state, ok := fake.VmState(id); !ok || state != "deleted" {
		t.Errorf("the vm is %q (present %v), want deleted once destroy was allowed to finish", state, ok)
	}
}

// TestAccInstanceResource_ADeleteThatMeetsAConflictAndThenAnAbsentVmIsDone: the
// DELETE is answered 409 operation_in_flight and, by the time the vm is read
// back, it is gone (the vm sentence): the removal was already underway, so
// destroy is done.
func TestAccInstanceResource_ADeleteThatMeetsAConflictAndThenAnAbsentVmIsDone(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	var id string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceConfig(fake, "gone-by-then", 1),
				Check:  vmIDFromState(&id),
			},
			{
				PreConfig: func() {
					fake.FailNext(1, matchRoute(http.MethodDelete, "/compute/instances/"), func(rc *reqCtx) {
						fake.Vanish(id)
						rc.conflict("operation_in_flight", detailOperationInFlightVm)
					})
				},
				Config: testAccProviderBlock(fake),
			},
		},
	})
}

// --- an upstream that echoes the key, and a create that gives up -------------

// TestAccInstanceResource_AnUpstreamEchoingTheKeyDoesNotLeakItIntoTheError: a
// proxy's error page that prints the request's Authorization header reaches the
// user with the API key replaced (V4 I1).
func TestAccInstanceResource_AnUpstreamEchoingTheKeyDoesNotLeakItIntoTheError(t *testing.T) {
	fake := newFakeAPI(t, "sk-echoed-4f9a7c1e2b", testOrgID)
	config := testAccInstanceConfig(fake, "echo", 1)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{Config: config},
			{
				PreConfig: func() {
					fake.FailNext(1000, matchRoute(http.MethodGet, "/compute/instances/"), respondEchoingTheKey(500))
				},
				Config:      config,
				PlanOnly:    true,
				ExpectError: wrapSafe("debug: Authorization: Bearer [redacted]"),
			},
			{
				PreConfig: fake.ClearFaults,
				Config:    config,
				PlanOnly:  true,
			},
		},
	})
}

// TestAccInstanceResource_TheInFlightDuplicateIsWaitedFor: the retry of a lost
// create answer can meet 409 idempotency_in_progress while the first request is
// still inside its transaction (V4 L3, V5 I4). It waits and gets the stored
// answer: one instance, one Idempotency-Key, the apply succeeds.
func TestAccInstanceResource_TheInFlightDuplicateIsWaitedFor(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{
				PreConfig: func() {
					fake.FailNextAfterHandling(1, matchMethod(http.MethodPost), respondGatewayRetryAfter(502, "0"))
					fake.InProgressFor(2)
				},
				Config: testAccInstanceConfig(fake, "in-flight", 1),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("virtscale_instance.test", "state", "running"),
					func(*terraform.State) error {
						if n := fake.CreateCount(); n != 1 {
							return fmt.Errorf("the create built %d vms, want exactly 1", n)
						}
						keys := fake.IdempotencyKeys()
						if len(keys) != 4 || keys[0] == "" || keys[0] != keys[1] || keys[1] != keys[2] || keys[2] != keys[3] {
							return fmt.Errorf("Idempotency-Keys of the attempts = %v, want one key four times (lost answer, two races, replay)", keys)
						}
						return nil
					},
				),
			},
		},
	})
}

// TestAccInstanceResource_GivingUpSaysHowToRecover: when every attempt of the
// create is lost, the instance may exist anyway. The error keeps the API's own
// words and says what to do (name_taken on the next apply, then tofu import),
// and that recovery works: the next apply is name_taken, and importing the
// instance leaves nothing to change.
func TestAccInstanceResource_GivingUpSaysHowToRecover(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	config := testAccInstanceConfig(fake, "lost-create", 1)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{
				PreConfig: func() {
					fake.FailNextAfterHandling(5, matchMethod(http.MethodPost), respondGatewayRetryAfter(502, "0"))
				},
				Config: config,
				ExpectError: wrapSafeInOrder("virtscale API 502", "gave up after 5 attempts",
					"the first request may still have created the resource: "+
						"if the next apply reports name_taken, find it in the panel or with the API "+
						"and bring it under management with tofu import"),
			},
			{
				Config:      config,
				ExpectError: wrapSafe("name_taken"),
			},
			{
				ResourceName:       "virtscale_instance.test",
				ImportState:        true,
				ImportStatePersist: true,
				ImportStateIdFunc:  func(*terraform.State) (string, error) { return fake.VmIDByName("lost-create"), nil },
			},
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

// --- the plan-time checks on ssh_keys (S7, S8) -------------------------------------

func sshKeysListRequest(elements ...types.String) validator.ListRequest {
	values := make([]attr.Value, len(elements))
	for i, e := range elements {
		values[i] = e
	}
	return validator.ListRequest{
		Path:        path.Root("ssh_keys"),
		ConfigValue: types.ListValueMust(types.StringType, values),
	}
}

func keyNames(n ...string) []types.String {
	out := make([]types.String, len(n))
	for i, name := range n {
		out[i] = types.StringValue(name)
	}
	return out
}

func TestSSHKeyCountValidator(t *testing.T) {
	ctx := context.Background()
	many := func(n int) []types.String {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("k%d", i)
		}
		return keyNames(out...)
	}

	for _, tc := range []struct {
		name    string
		req     validator.ListRequest
		wantErr bool
	}{
		{"none", sshKeysListRequest(), false},
		{"one", sshKeysListRequest(many(1)...), false},
		{"ten", sshKeysListRequest(many(10)...), false},
		{"eleven", sshKeysListRequest(many(11)...), true},
		{"unknown elements still count", sshKeysListRequest(append(many(10), types.StringUnknown())...), true},
		{"a null list", validator.ListRequest{Path: path.Root("ssh_keys"), ConfigValue: types.ListNull(types.StringType)}, false},
		{"an unknown list", validator.ListRequest{Path: path.Root("ssh_keys"), ConfigValue: types.ListUnknown(types.StringType)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var resp validator.ListResponse
			sshKeyCountValidator{}.ValidateList(ctx, tc.req, &resp)
			if resp.Diagnostics.HasError() != tc.wantErr {
				t.Fatalf("diagnostics = %v, want an error: %v", resp.Diagnostics, tc.wantErr)
			}
			if tc.wantErr {
				d := resp.Diagnostics.Errors()[0]
				if d.Detail() != "a VM can be given at most 10 SSH keys" {
					t.Errorf("detail = %q, want the API's own sentence", d.Detail())
				}
			}
		})
	}
}

func TestSSHKeyUniqueValidator(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		req  validator.ListRequest
		want []string // the names reported, each once
	}{
		{"none", sshKeysListRequest(), nil},
		{"all different", sshKeysListRequest(keyNames("a", "b", "c")...), nil},
		{"one twice", sshKeysListRequest(keyNames("a", "b", "a")...), []string{"a"}},
		{"one three times is reported once", sshKeysListRequest(keyNames("a", "a", "a")...), []string{"a"}},
		{"two names twice, reported where the repeat is", sshKeysListRequest(keyNames("a", "b", "b", "a")...), []string{"b", "a"}},
		{"case differs", sshKeysListRequest(keyNames("a", "A")...), nil},
		{"unknown elements are not the same name", sshKeysListRequest(types.StringUnknown(), types.StringUnknown()), nil},
		{"null elements are not the same name", sshKeysListRequest(types.StringNull(), types.StringNull()), nil},
		{"a null list", validator.ListRequest{Path: path.Root("ssh_keys"), ConfigValue: types.ListNull(types.StringType)}, nil},
		{"an unknown list", validator.ListRequest{Path: path.Root("ssh_keys"), ConfigValue: types.ListUnknown(types.StringType)}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var resp validator.ListResponse
			sshKeyUniqueValidator{}.ValidateList(ctx, tc.req, &resp)
			var got []string
			for _, d := range resp.Diagnostics.Errors() {
				got = append(got, d.Detail())
			}
			var want []string
			for _, name := range tc.want {
				want = append(want, fmt.Sprintf("SSH key '%s' is listed more than once", name))
			}
			if strings.Join(got, "|") != strings.Join(want, "|") {
				t.Errorf("errors = %q, want %q", got, want)
			}
		})
	}
}
