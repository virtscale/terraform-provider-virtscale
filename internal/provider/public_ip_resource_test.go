package provider

// Acceptance tests for virtscale_public_ip and virtscale_images. They share
// the TF_ACC harness defined in instance_resource_test.go (TestMain,
// testAccProtoV6ProviderFactories, testAccPreCheck): only run with TF_ACC=1
// and tofu on PATH.

import (
	"fmt"
	"net/http"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

func testAccPublicIpInstanceAndIpConfig(fake *fakeAPI, withInstanceID bool) string {
	instanceIDLine := ""
	if withInstanceID {
		instanceIDLine = "instance_id = virtscale_instance.test.id"
	}

	return testAccProviderBlock(fake) + fmt.Sprintf(`
resource "virtscale_instance" "test" {
  name            = "pubip-owner"
  region          = "gro"
  image           = "image-1"
  vcpus           = 1
  memory_mb       = 1024
  disk_gb         = 10
  storage_pool    = "pool-1"
  network_profile = "net-1"
}

resource "virtscale_public_ip" "test" {
  region = "gro"
  %s
}
`, instanceIDLine)
}

func testAccCheckPublicIpDestroy(fake *fakeAPI) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		if count := fake.PublicIPCount(); count != 0 {
			return fmt.Errorf("expected 0 public ips left in the fake, got %d", count)
		}
		return nil
	}
}

// TestAccPublicIpResource_Lifecycle covers scenarios (1)-(4): allocate with
// an instance_id -> associated with address set, remove instance_id ->
// updated in place to allocated with instance_id null, ImportStateVerify,
// then destroy leaving no public IP in the fake.
func TestAccPublicIpResource_Lifecycle(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckPublicIpDestroy(fake),
		Steps: []resource.TestStep{
			{
				Config: testAccPublicIpInstanceAndIpConfig(fake, true),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("virtscale_public_ip.test", "state", "associated"),
					resource.TestCheckResourceAttrSet("virtscale_public_ip.test", "address"),
					resource.TestCheckResourceAttrPair("virtscale_public_ip.test", "instance_id", "virtscale_instance.test", "id"),
				),
			},
			{
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("virtscale_public_ip.test", plancheck.ResourceActionUpdate),
					},
				},
				Config: testAccPublicIpInstanceAndIpConfig(fake, false),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("virtscale_public_ip.test", "state", "allocated"),
					resource.TestCheckNoResourceAttr("virtscale_public_ip.test", "instance_id"),
					// The refresh before this step read the vm while the
					// address was attached: Nova lists it, as a floating entry,
					// ahead of the fixed one. It is not the private address.
					resource.TestCheckResourceAttr("virtscale_instance.test", "private_ipv4", "10.128.3.11"),
				),
			},
			{
				ResourceName:            "virtscale_public_ip.test",
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{},
			},
		},
	})
}

// TestAccPublicIpResource_QuotaExceeded covers scenario (5): a second
// allocation while the first is still live fails with quota_exceeded.
func TestAccPublicIpResource_QuotaExceeded(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)

	config := testAccProviderBlock(fake) + `
resource "virtscale_public_ip" "first" {
  region = "gro"
}

resource "virtscale_public_ip" "second" {
  region = "gro"

  depends_on = [virtscale_public_ip.first]
}
`

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config:      config,
				ExpectError: regexp.MustCompile(`quota_exceeded`),
			},
		},
	})
}

// TestAccPublicIpResource_AssociateFailsFast covers Finding 2: associating
// with an instance whose name starts "ipfail-" drives the fake's public IP
// to state error, and the associate wait must fail immediately with the
// API's own error field rather than polling out the full timeout.
func TestAccPublicIpResource_AssociateFailsFast(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)

	config := testAccProviderBlock(fake) + `
resource "virtscale_instance" "test" {
  name            = "ipfail-owner"
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

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config:      config,
				ExpectError: regexp.MustCompile(`could not bind`),
			},
		},
	})
}

// TestAccPublicIpResource_DisassociateFailsFast covers Finding N2:
// disassociating an instance whose name starts "ipdisfail-" drives the
// fake's public IP to state error, and the disassociate wait must fail
// immediately with the API's own error field rather than polling out the
// full timeout. This mirrors TestAccPublicIpResource_AssociateFailsFast, but
// exercises Update's disassociate path (triggered here by clearing
// instance_id) instead of Create's associate path -- disassociate's
// fail-fast handling was already in place (Finding 2), it simply had no fake
// affordance or test driving it into state error at all.
//
// Step 3 is an explicit destroy step, not left to the suite's own implicit
// post-test cleanup: once step 2's disassociate call has genuinely put the
// fake's public IP into state error, Delete's own guard
// (`state.State == "associated"`) is false, so destroy releases directly
// without re-attempting the doomed disassociate -- proving Finding N2's fix
// doesn't leave the resource permanently stuck. Without a real destroy
// exercising that path here, a regression that made Delete retry
// unconditionally would go uncaught.
func TestAccPublicIpResource_DisassociateFailsFast(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)

	withInstanceID := testAccProviderBlock(fake) + `
resource "virtscale_instance" "test" {
  name            = "ipdisfail-owner"
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

	withoutInstanceID := testAccProviderBlock(fake) + `
resource "virtscale_instance" "test" {
  name            = "ipdisfail-owner"
  region          = "gro"
  image           = "image-1"
  vcpus           = 1
  memory_mb       = 1024
  disk_gb         = 10
  storage_pool    = "pool-1"
  network_profile = "net-1"
}

resource "virtscale_public_ip" "test" {
  region = "gro"
}
`

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: withInstanceID,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("virtscale_public_ip.test", "state", "associated"),
				),
			},
			{
				Config:      withoutInstanceID,
				ExpectError: regexp.MustCompile(`could not release`),
			},
			{
				Config:  withoutInstanceID,
				Destroy: true,
			},
		},
	})
}

// TestAccPublicIpResource_DisassociateBeforeRelease covers Finding 3:
// destroying a virtscale_public_ip that is still associated -- with no
// intervening step that clears instance_id -- must disassociate before it
// releases. Asserted against the fake's own recorded call sequence, not just
// against the final released state, so a regression in the guard (e.g.
// deleting it) is caught even though the end state looks identical.
func TestAccPublicIpResource_DisassociateBeforeRelease(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	var ipID string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy: func(s *terraform.State) error {
			seq := fake.CallSequence(ipID)
			if len(seq) < 2 || seq[0] != "disassociate" || seq[len(seq)-1] != "release" {
				return fmt.Errorf("expected disassociate before release for %s, got sequence %v", ipID, seq)
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: testAccPublicIpInstanceAndIpConfig(fake, true),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("virtscale_public_ip.test", "state", "associated"),
					func(s *terraform.State) error {
						rs, ok := s.RootModule().Resources["virtscale_public_ip.test"]
						if !ok {
							return fmt.Errorf("virtscale_public_ip.test not found in state")
						}
						ipID = rs.Primary.ID
						return nil
					},
				),
			},
		},
	})
}

func testAccImagesDataSourceConfig(fake *fakeAPI) string {
	return testAccProviderBlock(fake) + `
data "virtscale_images" "test" {
  region = "gro"
}
`
}

// TestAccImagesDataSource covers scenario (6): the data source returns the
// fake's two gro images, sorted by name.
func TestAccImagesDataSource(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: testAccImagesDataSourceConfig(fake),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.virtscale_images.test", "id", "gro"),
					resource.TestCheckResourceAttr("data.virtscale_images.test", "images.#", "2"),
					resource.TestCheckResourceAttr("data.virtscale_images.test", "images.0.name", "Debian 12"),
					resource.TestCheckResourceAttr("data.virtscale_images.test", "images.0.os_distro", "debian"),
					resource.TestCheckResourceAttr("data.virtscale_images.test", "images.0.os_version", "12"),
					resource.TestCheckResourceAttr("data.virtscale_images.test", "images.1.name", "Ubuntu 22.04"),
				),
			},
		},
	})
}

// TestAccPublicIpResource_DestroyWhileAReleaseIsAlreadyRunning covers M3 for
// public IPs: releasing an address that is already being released is 409
// public_ip_not_releasable ("this public IP is releasing"). The release is
// underway, so destroy waits for the address to go instead of failing.
func TestAccPublicIpResource_DestroyWhileAReleaseIsAlreadyRunning(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.SetReleaseSteps(4)
	config := testAccProviderBlock(fake) + `
resource "virtscale_public_ip" "test" {
  region = "gro"
}
`
	var id string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckPublicIpDestroy(fake),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: func(s *terraform.State) error {
					rs, ok := s.RootModule().Resources["virtscale_public_ip.test"]
					if !ok {
						return fmt.Errorf("virtscale_public_ip.test not found in state")
					}
					id = rs.Primary.ID
					return nil
				},
			},
			{
				PreConfig: func() {
					if status, _ := ipAction(t, fake, id, "", ""); status != http.StatusAccepted {
						t.Errorf("out-of-band release: status = %d, want 202", status)
					}
				},
				Config:  config,
				Destroy: true,
			},
		},
	})
}

// TestAccPublicIpResource_SurvivesALostReleaseResponse: the release (a DELETE,
// safe to repeat) is processed but its answer is lost; the retry meets 409
// public_ip_not_releasable because the address is already going, which destroy
// takes as underway. The disassociate before it (an unkeyed POST) is not retried
// and does not need to be.
func TestAccPublicIpResource_SurvivesALostReleaseResponse(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.SetReleaseSteps(2)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckPublicIpDestroy(fake),
		Steps: []resource.TestStep{
			{
				Config: testAccPublicIpInstanceAndIpConfig(fake, true),
				Check:  resource.TestCheckResourceAttr("virtscale_public_ip.test", "state", "associated"),
			},
			{
				PreConfig: func() {
					fake.FailNextAfterHandling(1, matchMethod(http.MethodDelete), respondGatewayRetryAfter(504, "0"))
				},
				Config:  testAccPublicIpInstanceAndIpConfig(fake, true),
				Destroy: true,
			},
		},
	})
}

// TestAccPublicIpResource_AnUnkeyedAllocateIsNeverRepeated is the other side:
// allocating an address has no Idempotency-Key, so when the API processed it
// but the answer was lost, repeating it would take a second address. The client
// does not; the error reaches the user, and exactly one address exists.
func TestAccPublicIpResource_AnUnkeyedAllocateIsNeverRepeated(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.SetPublicIPQuota(5)
	fake.FailNextAfterHandling(1, matchMethod(http.MethodPost), respondGateway(502))

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: testAccProviderBlock(fake) + `
resource "virtscale_public_ip" "test" {
  region = "gro"
}
`,
				ExpectError: regexp.MustCompile(`Unable to Allocate Public IP`),
			},
		},
	})
	if n := fake.PublicIPCount(); n != 1 {
		t.Errorf("addresses after one lost allocate answer = %d, want exactly 1: a repeated allocate would have taken a second", n)
	}
}

// TestAccPublicIpResource_KeyWithoutNetworkWriteIsToldSo: public IPs are edge.*
// permissions, so compute:write alone cannot allocate one, and the message
// names the scope that can.
func TestAccPublicIpResource_KeyWithoutNetworkWriteIsToldSo(t *testing.T) {
	fake := newFakeAPI(t, "write-key", testOrgID)
	fake.AddKey("compute-only", "compute:write")

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: testAccProviderBlockWith(fake, "compute-only", fake.org) + `
resource "virtscale_public_ip" "test" {
  region = "gro"
}
`,
				ExpectError: wrapSafe("the API key needs the 'network:write' scope"),
			},
		},
	})
	if fake.PublicIPCount() != 0 {
		t.Errorf("the API refused the request, yet %d public ip(s) exist", fake.PublicIPCount())
	}
}

// --- 404s: only "this address is gone" is gone -------------------------------

const testAccBareIpConfigBlock = `
resource "virtscale_public_ip" "test" {
  region = "gro"
}
`

func testAccPublicIpIDFromState(dst *string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources["virtscale_public_ip.test"]
		if !ok {
			return fmt.Errorf("virtscale_public_ip.test not found in state")
		}
		*dst = rs.Primary.ID
		return nil
	}
}

// TestAccPublicIpResource_VanishedOutOfBand: an address released out of band is
// absent from the API's list, which reads as the public_ip 404 of the shared
// helper: Read drops it, and the next plan allocates one again.
func TestAccPublicIpResource_VanishedOutOfBand(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	config := testAccProviderBlock(fake) + testAccBareIpConfigBlock
	var first, second string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckPublicIpDestroy(fake),
		Steps: []resource.TestStep{
			{Config: config, Check: testAccPublicIpIDFromState(&first)},
			{
				PreConfig:          func() { fake.VanishPublicIP(first) },
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction("virtscale_public_ip.test", plancheck.ResourceActionCreate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccPublicIpIDFromState(&second),
					func(*terraform.State) error {
						if second == first {
							return fmt.Errorf("the address was not allocated again")
						}
						return nil
					},
				),
			},
		},
	})
}

// TestAccPublicIpResource_ReadErrorsThatAreNotTheAddressBeingGoneKeepTheResource:
// the list that Read searches can fail with a 404 that is not about the address
// (a mistyped endpoint, a proxy's page, the compute service); that is an error,
// and the address stays in the state (V4 L4).
func TestAccPublicIpResource_ReadErrorsThatAreNotTheAddressBeingGoneKeepTheResource(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	config := testAccProviderBlock(fake) + testAccBareIpConfigBlock
	variants := notFoundVariants()

	steps := []resource.TestStep{{Config: config}}
	for _, name := range sortedVariantNames(variants) {
		respond := variants[name]
		steps = append(steps, resource.TestStep{
			PreConfig: func() {
				fake.ClearFaults()
				fake.FailNext(1000, matchRoute(http.MethodGet, "/compute/public-ips"), respond)
			},
			Config:      config,
			PlanOnly:    true,
			ExpectError: wrapSafe("Unable to Read Public IP"),
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
		CheckDestroy:             testAccCheckPublicIpDestroy(fake),
		Steps:                    steps,
	})
}

// TestAccPublicIpResource_ReleaseDoesNotTakeAMistypedEndpointForAReleasedAddress:
// the wait after the release ends when the address is absent from the list.
// Reads that fail with an unknown route's 404 are an error: destroy fails and
// the address stays tracked, instead of reporting a release nobody confirmed.
func TestAccPublicIpResource_ReleaseDoesNotTakeAMistypedEndpointForAReleasedAddress(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	deleteSeen := false

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{Config: testAccProviderBlock(fake) + testAccBareIpConfigBlock},
			{
				PreConfig: func() {
					fake.FailNext(1000, func(r *http.Request) bool {
						if r.Method == http.MethodDelete {
							deleteSeen = true
						}
						return deleteSeen && matchRoute(http.MethodGet, "/compute/public-ips")(r)
					}, respondNoSuchRoute)
				},
				Config:      testAccProviderBlock(fake),
				ExpectError: wrapSafe("Unable to Wait for Public IP Release"),
			},
			{
				PreConfig: fake.ClearFaults,
				Config:    testAccProviderBlock(fake),
			},
		},
	})
}

// TestAccPublicIpResource_AReleaseThatMeetsAConflictAndThenAnAbsentAddressIsDone:
// the DELETE is answered 409 public_ip_not_releasable and the address is gone
// when it is read back: it was being released already, so destroy is done.
func TestAccPublicIpResource_AReleaseThatMeetsAConflictAndThenAnAbsentAddressIsDone(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	var id string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{Config: testAccProviderBlock(fake) + testAccBareIpConfigBlock, Check: testAccPublicIpIDFromState(&id)},
			{
				PreConfig: func() {
					fake.FailNext(1, matchRoute(http.MethodDelete, "/compute/public-ips/"), func(rc *reqCtx) {
						fake.VanishPublicIP(id)
						rc.conflict("public_ip_not_releasable", "this public IP is releasing, so it cannot be released")
					})
				},
				Config: testAccProviderBlock(fake),
			},
		},
	})
	if n := fake.PublicIPCount(); n != 0 {
		t.Errorf("%d public ip(s) left", n)
	}
}
