package provider

// Acceptance tests for virtscale_public_ip and virtscale_images. They share
// the TF_ACC harness defined in instance_resource_test.go (TestMain,
// testAccProtoV6ProviderFactories, testAccPreCheck): only run with TF_ACC=1
// and tofu on PATH.

import (
	"fmt"
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
	fake := newFakeAPI(t, "test-api-key", "test-org")

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
	fake := newFakeAPI(t, "test-api-key", "test-org")

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
	fake := newFakeAPI(t, "test-api-key", "test-org")

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
