package provider

// Live acceptance tests for the data sources and resources added with TF-001: the catalogues, projects, VPCs, an
// instance with its firewall, and S3 (a bucket and an access key). Each family has a proof body that runs against any
// provider block, a fake-backed twin ("AgainstTheFake") that proves the test code in CI without a real API, and a
// TestLive... entry point that talks to a real one. The entry points skip unless liveSshKeyEnv says all is set (see
// live_ssh_key_test.go), refuse the production API, and name everything they create tf-live-<random>.
//
// Optional settings of a live run (the staging values are the defaults): VIRTSCALE_LIVE_REGION,
// VIRTSCALE_LIVE_STORAGE_POOL and VIRTSCALE_LIVE_NETWORK_PROFILE.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"gitlab.virtscale.nl/virtscale/virtscale-platform/terraform-provider-virtscale/internal/client"
)

// liveParams are the values of the place a proof runs in.
type liveParams struct {
	region, storagePool, networkProfile string
}

// fakeParams are the fake API's own place.
func fakeParams() liveParams { return liveParams{"gro", "pool-1", "net-1"} }

// liveParamsFromEnv is the staging place unless the environment names another.
func liveParamsFromEnv() liveParams {
	get := func(name, def string) string {
		if v := os.Getenv(name); v != "" {
			return v
		}
		return def
	}
	return liveParams{
		region:         get("VIRTSCALE_LIVE_REGION", "stg"),
		storagePool:    get("VIRTSCALE_LIVE_STORAGE_POOL", "stg-local"),
		networkProfile: get("VIRTSCALE_LIVE_NETWORK_PROFILE", "stg-tenant"),
	}
}

// liveName is a throwaway name: tf-live- and 8 random lower-case hex characters, so that a sweep finds only what a
// run made.
func liveName(t *testing.T) string {
	t.Helper()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("generate a random name: %v", err)
	}
	return "tf-live-" + hex.EncodeToString(b[:])
}

// liveClient is the API client of the destroy checks.
func liveClient(endpoint, apiKey, org string) (*client.Client, error) {
	return client.New(endpoint, apiKey, org)
}

const liveProviderBlock = "\nprovider \"virtscale\" {\n}\n"

// liveCheckTimeout bounds one read of a destroy check.
const liveCheckTimeout = 60 * time.Second

// --- the catalogues ----------------------------------------------------------------------------------------------

func runCatalogueProof(t *testing.T, providerBlock, org string, p liveParams) {
	t.Helper()
	config := providerBlock + `
data "virtscale_caller" "me" {}
data "virtscale_regions" "all" {}
data "virtscale_instance_types" "all" {}
data "virtscale_plans" "all" {}
data "virtscale_project" "default" {}
`
	regionListed := func(s *terraform.State) error {
		attrs := s.RootModule().Resources["data.virtscale_regions.all"].Primary.Attributes
		for k, v := range attrs {
			if strings.HasPrefix(k, "regions.") && strings.HasSuffix(k, ".id") && v == p.region {
				return nil
			}
		}
		return fmt.Errorf("region %q is not in virtscale_regions", p.region)
	}
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{{
			Config: config,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("data.virtscale_caller.me", "organization_id", org),
				resource.TestCheckResourceAttr("data.virtscale_caller.me", "credential", "api_token"),
				resource.TestCheckResourceAttrSet("data.virtscale_caller.me", "api_key_id"),
				resource.TestCheckResourceAttrSet("data.virtscale_caller.me", "scopes.#"),
				resource.TestCheckResourceAttrSet("data.virtscale_caller.me", "permissions.#"),
				regionListed,
				resource.TestCheckResourceAttrSet("data.virtscale_instance_types.all", "id"),
				resource.TestCheckResourceAttrSet("data.virtscale_plans.all", "id"),
				resource.TestCheckResourceAttr("data.virtscale_project.default", "is_default", "true"),
			),
		}},
	})
}

func TestAccCatalogue_ProofAgainstTheFake(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.AddKey("writer", "write")
	runCatalogueProof(t, testAccProviderBlockWith(fake, "writer", testOrgID), testOrgID, fakeParams())
}

func TestLiveCatalogueProof(t *testing.T) {
	_, _, org := liveSshKeyEnv(t)
	runCatalogueProof(t, liveProviderBlock, org, liveParamsFromEnv())
}

// --- projects ------------------------------------------------------------------------------------------------------

// goneCheck reads one object and says why it is not gone, or nil when it is.
type goneCheck func(ctx context.Context, id string) error

func destroyCheckFor(resourceType string, gone goneCheck) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		found := 0
		for addr, rs := range s.RootModule().Resources {
			if rs.Type != resourceType {
				continue
			}
			found++
			ctx, cancel := context.WithTimeout(context.Background(), liveCheckTimeout)
			err := gone(ctx, rs.Primary.ID)
			cancel()
			if err != nil {
				return fmt.Errorf("%s after destroy: %v", addr, err)
			}
		}
		if found == 0 {
			return fmt.Errorf("no %s in the state to check", resourceType)
		}
		return nil
	}
}

func projectGone(c *client.Client) goneCheck {
	return func(ctx context.Context, id string) error {
		_, err := c.GetProject(ctx, id)
		if client.IsNotFound(err, client.ResourceProject) {
			return nil
		}
		if err == nil {
			return fmt.Errorf("the project is still there")
		}
		return fmt.Errorf("the read failed: %s", errKind(err))
	}
}

// errKind describes an error without the text of a response: its status and code, or its type.
func errKind(err error) string {
	if apiErr, ok := err.(*client.APIError); ok {
		return fmt.Sprintf("status %d (code %q)", apiErr.Status, apiErr.Code)
	}
	return fmt.Sprintf("%T", err)
}

func runProjectLifecycleProof(t *testing.T, providerBlock string, c *client.Client) {
	t.Helper()
	name := liveName(t)
	config := func(description, labels string) string {
		return providerBlock + fmt.Sprintf("\nresource \"virtscale_project\" \"p\" {\n  name        = %q\n  description = %q\n  labels      = %s\n}\n", name, description, labels)
	}
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             destroyCheckFor("virtscale_project", projectGone(c)),
		Steps: []resource.TestStep{
			{
				Config: config("created by a live test", `{ run = "live" }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("virtscale_project.p", "name", name),
					resource.TestCheckResourceAttr("virtscale_project.p", "is_default", "false"),
					resource.TestCheckResourceAttrSet("virtscale_project.p", "id"),
					resource.TestCheckResourceAttrSet("virtscale_project.p", "vrn"),
				),
			},
			{ // The same configuration again: an empty plan is the proof that nothing drifted.
				Config:           config("created by a live test", `{ run = "live" }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
			},
			{ // Description and labels change in place.
				Config: config("changed by a live test", `{ run = "live", step = "update" }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction("virtscale_project.p", plancheck.ResourceActionUpdate),
				}},
				Check: resource.TestCheckResourceAttr("virtscale_project.p", "labels.step", "update"),
			},
			{ResourceName: "virtscale_project.p", ImportState: true, ImportStateVerify: true},
		},
	})
}

func TestAccProjectResource_LiveProofAgainstTheFake(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.AddKey("writer", "write")
	c, _ := liveClient(fake.URL(), "writer", fake.org)
	runProjectLifecycleProof(t, testAccProviderBlockWith(fake, "writer", testOrgID), c)
}

func TestLiveProjectResource_LifecycleProof(t *testing.T) {
	endpoint, apiKey, org := liveSshKeyEnv(t)
	c, err := liveClient(endpoint, apiKey, org)
	if err != nil {
		t.Fatalf("build the API client: %v", err)
	}
	runProjectLifecycleProof(t, liveProviderBlock, c)
}

// --- VPCs ------------------------------------------------------------------------------------------------------------

func vpcGone(c *client.Client) goneCheck {
	return func(ctx context.Context, id string) error {
		// The state is gone when the check runs; the VPC is found through the projects it could be in.
		projects, err := c.ListProjects(ctx)
		if err != nil {
			return fmt.Errorf("list the projects: %s", errKind(err))
		}
		for _, p := range projects {
			v, err := c.GetVpc(ctx, p.ID, id)
			switch {
			case err == nil && v.Status != "deleted":
				return fmt.Errorf("the VPC is still there (status %s)", v.Status)
			case err != nil && !client.IsNotFound(err, client.ResourceVpc) && !client.IsNotFound(err, client.ResourceProject):
				return fmt.Errorf("the read failed: %s", errKind(err))
			}
		}
		return nil
	}
}

func runVpcLifecycleProof(t *testing.T, providerBlock string, p liveParams, c *client.Client) {
	t.Helper()
	name := liveName(t)
	config := providerBlock + fmt.Sprintf(`
data "virtscale_project" "default" {}

resource "virtscale_vpc" "v" {
  project_id  = data.virtscale_project.default.id
  name        = %q
  region      = %q
  description = "created by a live test"
}
`, name, p.region)
	var projectID string
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             destroyCheckFor("virtscale_vpc", vpcGone(c)),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("virtscale_vpc.v", "status", "active"),
					resource.TestCheckResourceAttr("virtscale_vpc.v", "region", p.region),
					resource.TestCheckResourceAttrSet("virtscale_vpc.v", "cidr"),
					resource.TestCheckResourceAttrSet("virtscale_vpc.v", "prefix_length"),
					func(s *terraform.State) error {
						projectID = s.RootModule().Resources["virtscale_vpc.v"].Primary.Attributes["project_id"]
						return nil
					},
				),
			},
			{Config: config, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}},
			{
				ResourceName:      "virtscale_vpc.v",
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateIdFunc: func(s *terraform.State) (string, error) {
					return projectID + "/" + s.RootModule().Resources["virtscale_vpc.v"].Primary.ID, nil
				},
			},
		},
	})
}

func TestAccVpcResource_LiveProofAgainstTheFake(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	c, _ := liveClient(fake.URL(), "test-api-key", fake.org)
	runVpcLifecycleProof(t, testAccProviderBlock(fake), fakeParams(), c)
}

func TestLiveVpcResource_LifecycleProof(t *testing.T) {
	endpoint, apiKey, org := liveSshKeyEnv(t)
	c, err := liveClient(endpoint, apiKey, org)
	if err != nil {
		t.Fatalf("build the API client: %v", err)
	}
	runVpcLifecycleProof(t, liveProviderBlock, liveParamsFromEnv(), c)
}

// --- an instance and its firewall ------------------------------------------------------------------------------

func instanceGone(c *client.Client) goneCheck {
	return func(ctx context.Context, id string) error {
		vm, err := c.GetVm(ctx, id)
		switch {
		case client.IsNotFound(err, client.ResourceVm):
			return nil
		case err != nil:
			return fmt.Errorf("the read failed: %s", errKind(err))
		case vm.State != "deleted":
			return fmt.Errorf("the instance is still there (state %s)", vm.State)
		}
		return nil
	}
}

func runInstanceFirewallProof(t *testing.T, providerBlock string, p liveParams, c *client.Client) {
	t.Helper()
	name := liveName(t)
	config := func(rules string) string {
		return providerBlock + fmt.Sprintf(`
data "virtscale_images" "r" {
  region = %q
}

resource "virtscale_instance" "i" {
  name            = %q
  region          = %q
  image           = [for i in data.virtscale_images.r.images : i.id if i.min_ram_mb <= 1024 && i.min_disk_gb <= 10][0]
  vcpus           = 1
  memory_mb       = 1024
  disk_gb         = 10
  storage_pool    = %q
  network_profile = %q
}

resource "virtscale_instance_firewall" "f" {
  instance_id = virtscale_instance.i.id
%s}
`, p.region, name, p.region, p.storagePool, p.networkProfile, rules)
	}
	ssh := "  rule {\n    direction   = \"ingress\"\n    protocol    = \"tcp\"\n    port_min    = 22\n    port_max    = 22\n    remote_cidr = \"192.0.2.0/24\"\n    description = \"live test\"\n  }\n"
	ping := "  rule {\n    direction   = \"ingress\"\n    protocol    = \"icmp\"\n    remote_cidr = \"192.0.2.0/24\"\n  }\n"
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             destroyCheckFor("virtscale_instance", instanceGone(c)),
		Steps: []resource.TestStep{
			{
				Config: config(ssh),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("virtscale_instance.i", "state", "running"),
					resource.TestCheckResourceAttr("virtscale_instance_firewall.f", "state", "applied"),
					resource.TestCheckResourceAttr("virtscale_instance_firewall.f", "rule.#", "1"),
				),
			},
			{Config: config(ssh), ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}},
			{ // One rule more is an update in place.
				Config: config(ssh + ping),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction("virtscale_instance_firewall.f", plancheck.ResourceActionUpdate),
				}},
				Check: resource.TestCheckResourceAttr("virtscale_instance_firewall.f", "rule.#", "2"),
			},
			{ResourceName: "virtscale_instance_firewall.f", ImportState: true, ImportStateVerify: true},
		},
	})
}

func TestAccInstanceFirewallResource_LiveProofAgainstTheFake(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	c, _ := liveClient(fake.URL(), "test-api-key", fake.org)
	runInstanceFirewallProof(t, testAccProviderBlock(fake), fakeParams(), c)
}

func TestLiveInstanceFirewallResource_LifecycleProof(t *testing.T) {
	endpoint, apiKey, org := liveSshKeyEnv(t)
	c, err := liveClient(endpoint, apiKey, org)
	if err != nil {
		t.Fatalf("build the API client: %v", err)
	}
	runInstanceFirewallProof(t, liveProviderBlock, liveParamsFromEnv(), c)
}

// --- S3: a bucket and an access key ----------------------------------------------------------------------------------

func bucketGoneOrSoftDeleted(c *client.Client) goneCheck {
	return func(ctx context.Context, id string) error {
		b, err := c.GetBucket(ctx, id)
		switch {
		case client.IsNotFound(err, client.ResourceBucket):
			return nil
		case err != nil:
			return fmt.Errorf("the read failed: %s", errKind(err))
		case !bucketGone(b.State):
			return fmt.Errorf("the bucket is still there (state %s)", b.State)
		}
		return nil
	}
}

func accessKeyRevoked(c *client.Client) goneCheck {
	return func(ctx context.Context, id string) error {
		k, err := c.GetAccessKey(ctx, id)
		switch {
		case client.IsNotFound(err, client.ResourceAccessKey):
			return nil
		case err != nil:
			return fmt.Errorf("the read failed: %s", errKind(err))
		case k.State != "revoked":
			return fmt.Errorf("the access key is not revoked (state %s)", k.State)
		}
		return nil
	}
}

func runS3Proof(t *testing.T, providerBlock string, c *client.Client) {
	t.Helper()
	name := liveName(t)
	config := func(maxObjects int) string {
		return providerBlock + fmt.Sprintf(`
resource "virtscale_s3_bucket" "b" {
  name        = %q
  max_objects = %d
}

resource "virtscale_s3_access_key" "k" {}
`, name, maxObjects)
	}
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			destroyCheckFor("virtscale_s3_bucket", bucketGoneOrSoftDeleted(c)),
			destroyCheckFor("virtscale_s3_access_key", accessKeyRevoked(c)),
		),
		Steps: []resource.TestStep{
			{
				Config: config(1000),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("virtscale_s3_bucket.b", "state", "active"),
					resource.TestCheckResourceAttr("virtscale_s3_bucket.b", "max_objects", "1000"),
					resource.TestCheckResourceAttr("virtscale_s3_access_key.k", "state", "active"),
					resource.TestCheckResourceAttrSet("virtscale_s3_access_key.k", "access_key_id"),
					// The secret is in the state and is not printed: only that it is there.
					resource.TestCheckResourceAttrSet("virtscale_s3_access_key.k", "secret_access_key"),
				),
			},
			{Config: config(1000), ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}},
			{
				Config: config(2000),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction("virtscale_s3_bucket.b", plancheck.ResourceActionUpdate),
					plancheck.ExpectResourceAction("virtscale_s3_access_key.k", plancheck.ResourceActionNoop),
				}},
				Check: resource.TestCheckResourceAttr("virtscale_s3_bucket.b", "max_objects", "2000"),
			},
			{ResourceName: "virtscale_s3_bucket.b", ImportState: true, ImportStateVerify: true, ImportStateVerifyIgnore: []string{"force_destroy"}},
			{ResourceName: "virtscale_s3_access_key.k", ImportState: true, ImportStateVerify: true, ImportStateVerifyIgnore: []string{"secret_access_key"}},
		},
	})
}

func TestAccS3Resources_LiveProofAgainstTheFake(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.AddKey("writer", "write")
	c, _ := liveClient(fake.URL(), "writer", fake.org)
	runS3Proof(t, testAccProviderBlockWith(fake, "writer", testOrgID), c)
}

func TestLiveS3Resources_LifecycleProof(t *testing.T) {
	endpoint, apiKey, org := liveSshKeyEnv(t)
	// The bucket this creates is soft-deleted afterwards and, while the platform's purge worker is off, never purged: it keeps
	// its name and one of the organization's retention rows for good. Staging has no object store behind the API either.
	if os.Getenv("VIRTSCALE_LIVE_S3") != "1" {
		t.Skip("the live S3 proof needs an object store behind the API and leaves a soft-deleted bucket that may never be purged: set VIRTSCALE_LIVE_S3=1 to run it")
	}
	c, err := liveClient(endpoint, apiKey, org)
	if err != nil {
		t.Fatalf("build the API client: %v", err)
	}
	runS3Proof(t, liveProviderBlock, c)
}
