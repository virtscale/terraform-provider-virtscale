package provider

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"gitlab.virtscale.nl/virtscale/virtscale-platform/terraform-provider-virtscale/internal/client"
)

// Acceptance tests (TF_ACC=1, see instance_resource_test.go) of the read-only data sources: the caller
// (GET /v1/me), regions, instance types and plans.

func dataSourceTest(t *testing.T, fake *fakeAPI, config string, check resource.TestCheckFunc) {
	t.Helper()
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps:                    []resource.TestStep{{Config: testAccProviderBlock(fake) + config, Check: check}},
	})
}

func TestAccCallerDataSource_ReadsTheIdentityOfTheKey(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	dataSourceTest(t, fake, `data "virtscale_caller" "me" {}`, resource.ComposeAggregateTestCheckFunc(
		resource.TestCheckResourceAttr("data.virtscale_caller.me", "credential", "api_token"),
		resource.TestCheckResourceAttr("data.virtscale_caller.me", "realm", "customers"),
		resource.TestCheckResourceAttr("data.virtscale_caller.me", "subject", "api-token:"+fakeUUID(4, 1)),
		resource.TestCheckResourceAttr("data.virtscale_caller.me", "id", "api-token:"+fakeUUID(4, 1)),
		resource.TestCheckResourceAttr("data.virtscale_caller.me", "organization_id", testOrgID),
		resource.TestCheckResourceAttr("data.virtscale_caller.me", "permissions_enforced", "true"),
		resource.TestCheckResourceAttr("data.virtscale_caller.me", "api_key_id", fakeUUID(4, 1)),
		resource.TestCheckResourceAttr("data.virtscale_caller.me", "scopes.#", "2"),
		resource.TestCheckTypeSetElemAttr("data.virtscale_caller.me", "scopes.*", "compute:write"),
		resource.TestCheckTypeSetElemAttr("data.virtscale_caller.me", "scopes.*", "network:write"),
		resource.TestCheckTypeSetElemAttr("data.virtscale_caller.me", "permissions.*", "compute.instance.create"),
		resource.TestCheckTypeSetElemAttr("data.virtscale_caller.me", "permissions.*", "edge.public_ip.allocate"),
		// An organization-wide key with no expiry and no role has nothing to say about them.
		resource.TestCheckNoResourceAttr("data.virtscale_caller.me", "project_id"),
		resource.TestCheckNoResourceAttr("data.virtscale_caller.me", "expires_at"),
		resource.TestCheckNoResourceAttr("data.virtscale_caller.me", "role"),
	))
}

// A key of another organization is told up front, not by a 404 on the first resource.
func TestAccCallerDataSource_RefusesAnOrganizationTheKeyDoesNotName(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	const other = "11111111-2222-4333-8444-555555555555"
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{{
			Config:      testAccProviderBlockWith(fake, "test-api-key", other) + `data "virtscale_caller" "me" {}`,
			ExpectError: wrapSafe("does not name the organization " + other),
		}},
	})
}

func TestAccCallerDataSource_A401NamesTheKey(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{{
			Config:      testAccProviderBlockWith(fake, "wrong-key", testOrgID) + `data "virtscale_caller" "me" {}`,
			ExpectError: wrapSafeInOrder("401 unauthorized", "check VIRTSCALE_API_KEY"),
		}},
	})
}

func TestAccRegionsDataSource(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	dataSourceTest(t, fake, `data "virtscale_regions" "all" {}`, resource.ComposeAggregateTestCheckFunc(
		resource.TestCheckResourceAttr("data.virtscale_regions.all", "id", "regions"),
		resource.TestCheckResourceAttr("data.virtscale_regions.all", "regions.#", "3"),
		resource.TestCheckResourceAttr("data.virtscale_regions.all", "regions.0.id", "gro"),
		resource.TestCheckResourceAttr("data.virtscale_regions.all", "regions.0.name", "Groningen"),
		resource.TestCheckResourceAttr("data.virtscale_regions.all", "regions.0.datacenter", "GRO1"),
		resource.TestCheckResourceAttr("data.virtscale_regions.all", "regions.0.status", "active"),
		resource.TestCheckResourceAttr("data.virtscale_regions.all", "regions.0.access", "public"),
		resource.TestCheckResourceAttr("data.virtscale_regions.all", "regions.0.entitled", "true"),
		resource.TestCheckResourceAttr("data.virtscale_regions.all", "regions.0.default", "true"),
		resource.TestCheckResourceAttr("data.virtscale_regions.all", "regions.0.disk_types.#", "2"),
		resource.TestCheckResourceAttr("data.virtscale_regions.all", "regions.0.disk_types.0.disk_type", "ssd"),
		resource.TestCheckResourceAttr("data.virtscale_regions.all", "regions.0.disk_types.0.status", "active"),
		resource.TestCheckResourceAttr("data.virtscale_regions.all", "regions.1.id", "ams"),
		resource.TestCheckResourceAttr("data.virtscale_regions.all", "regions.1.entitled", "false"),
		resource.TestCheckResourceAttr("data.virtscale_regions.all", "regions.1.default", "false"),
		// Entitled and the default are two facts: Frankfurt is open to this organization and is not the default.
		resource.TestCheckResourceAttr("data.virtscale_regions.all", "regions.2.id", "fra"),
		resource.TestCheckResourceAttr("data.virtscale_regions.all", "regions.2.entitled", "true"),
		resource.TestCheckResourceAttr("data.virtscale_regions.all", "regions.2.default", "false"),
	))
}

func TestAccInstanceTypesDataSource(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	dataSourceTest(t, fake, `data "virtscale_instance_types" "all" {}`, resource.ComposeAggregateTestCheckFunc(
		resource.TestCheckResourceAttr("data.virtscale_instance_types.all", "id", "instance-types"),
		resource.TestCheckResourceAttr("data.virtscale_instance_types.all", "instance_types.#", "3"),
		resource.TestCheckResourceAttr("data.virtscale_instance_types.all", "instance_types.0.id", "g1-small"),
		resource.TestCheckResourceAttr("data.virtscale_instance_types.all", "instance_types.0.vrn", "vrn:virtscale:instance-type:g1-small"),
		resource.TestCheckResourceAttr("data.virtscale_instance_types.all", "instance_types.0.name", "General small"),
		resource.TestCheckResourceAttr("data.virtscale_instance_types.all", "instance_types.0.family", "general"),
		resource.TestCheckResourceAttr("data.virtscale_instance_types.all", "instance_types.0.cpu_kind", "shared"),
		resource.TestCheckResourceAttr("data.virtscale_instance_types.all", "instance_types.0.vcpus", "1"),
		resource.TestCheckResourceAttr("data.virtscale_instance_types.all", "instance_types.0.memory_mb", "2048"),
		resource.TestCheckResourceAttr("data.virtscale_instance_types.all", "instance_types.0.disk_gb", "20"),
		resource.TestCheckResourceAttr("data.virtscale_instance_types.all", "instance_types.0.status", "active"),
		resource.TestCheckResourceAttr("data.virtscale_instance_types.all", "instance_types.0.regions.#", "1"),
		resource.TestCheckResourceAttr("data.virtscale_instance_types.all", "instance_types.0.regions.0", "gro"),
		resource.TestCheckResourceAttr("data.virtscale_instance_types.all", "instance_types.2.status", "coming_soon"),
		resource.TestCheckResourceAttr("data.virtscale_instance_types.all", "instance_types.2.regions.#", "0"),
	))
}

func TestAccInstanceTypesDataSource_RegionFiltersWhatCanBeOrderedThere(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	dataSourceTest(t, fake, `data "virtscale_instance_types" "gro" { region = "gro" }`, resource.ComposeAggregateTestCheckFunc(
		resource.TestCheckResourceAttr("data.virtscale_instance_types.gro", "id", "instance-types"),
		resource.TestCheckResourceAttr("data.virtscale_instance_types.gro", "instance_types.#", "2"),
		resource.TestCheckResourceAttr("data.virtscale_instance_types.gro", "instance_types.1.id", "g1-medium"),
	))
	dataSourceTest(t, fake, `data "virtscale_instance_types" "none" { region = "ams" }`,
		resource.TestCheckResourceAttr("data.virtscale_instance_types.none", "instance_types.#", "0"))
}

func TestAccPlansDataSource(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	dataSourceTest(t, fake, `data "virtscale_plans" "all" {}`, resource.ComposeAggregateTestCheckFunc(
		resource.TestCheckResourceAttr("data.virtscale_plans.all", "id", "plans"),
		resource.TestCheckResourceAttr("data.virtscale_plans.all", "plans.#", "2"),
		resource.TestCheckResourceAttr("data.virtscale_plans.all", "plans.0.id", "managed-s"),
		resource.TestCheckResourceAttr("data.virtscale_plans.all", "plans.0.name", "Managed S"),
		resource.TestCheckResourceAttr("data.virtscale_plans.all", "plans.0.vcpus", "1"),
		resource.TestCheckResourceAttr("data.virtscale_plans.all", "plans.0.ram_mb", "2048"),
		resource.TestCheckResourceAttr("data.virtscale_plans.all", "plans.0.disk_gb", "40"),
		resource.TestCheckResourceAttr("data.virtscale_plans.all", "plans.0.monthly_price_cents", "1500"),
		resource.TestCheckResourceAttr("data.virtscale_plans.all", "plans.0.currency", "eur"),
		resource.TestCheckResourceAttr("data.virtscale_plans.all", "plans.0.status", "active"),
		resource.TestCheckResourceAttr("data.virtscale_plans.all", "plans.0.management_level", "full"),
		resource.TestCheckResourceAttr("data.virtscale_plans.all", "plans.0.backup_policy", "daily-7d"),
		resource.TestCheckNoResourceAttr("data.virtscale_plans.all", "plans.1.backup_policy"),
	))
}

func TestAccComputeLimitsDataSource(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	dataSourceTest(t, fake, `data "virtscale_compute_limits" "grid" {}`, resource.ComposeAggregateTestCheckFunc(
		resource.TestCheckResourceAttr("data.virtscale_compute_limits.grid", "id", "compute-limits"),
		resource.TestCheckResourceAttr("data.virtscale_compute_limits.grid", "vcpus_steps.#", "6"),
		resource.TestCheckResourceAttr("data.virtscale_compute_limits.grid", "vcpus_steps.0", "1"),
		resource.TestCheckResourceAttr("data.virtscale_compute_limits.grid", "vcpus_steps.5", "32"),
		resource.TestCheckResourceAttr("data.virtscale_compute_limits.grid", "memory_gb.min", "1"),
		resource.TestCheckResourceAttr("data.virtscale_compute_limits.grid", "memory_gb.max", "128"),
		resource.TestCheckResourceAttr("data.virtscale_compute_limits.grid", "memory_gb.step", "1"),
		resource.TestCheckResourceAttr("data.virtscale_compute_limits.grid", "disk_gb.min", "10"),
		resource.TestCheckResourceAttr("data.virtscale_compute_limits.grid", "disk_gb.max", "2000"),
		resource.TestCheckResourceAttr("data.virtscale_compute_limits.grid", "disk_gb.step", "10"),
	))
}

// A key without the read scope gets the API's own refusal and the scope to add, for each catalogue.
func TestAccCatalogueDataSources_AKeyWithoutTheScopeIsToldWhichToAdd(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.AddKey("network-only", "network:read")
	for _, ds := range []string{"virtscale_regions", "virtscale_instance_types", "virtscale_plans"} {
		resource.Test(t, resource.TestCase{
			PreCheck:                 func() { testAccPreCheck(t) },
			ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
			Steps: []resource.TestStep{{
				Config:      testAccProviderBlockWith(fake, "network-only", testOrgID) + `data "` + ds + `" "x" {}`,
				ExpectError: wrapSafeInOrder("403 permission_denied", "needs the 'compute:read' or 'compute:write' scope"),
			}},
		})
	}
}

// A key restricted to a project, with an expiry, says so; the organization is compared without regard to case.
func TestAccCallerDataSource_AKeyRestrictedToAProjectAndExpiring(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	expires := time.Date(2027, 3, 4, 5, 6, 7, 0, time.UTC)
	fake.AddRestrictedKey("scoped", fakeUUID(5, 1), expires, "compute:read")
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{{
			Config: testAccProviderBlockWith(fake, "scoped", strings.ToUpper(testOrgID)) + `data "virtscale_caller" "me" {}`,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("data.virtscale_caller.me", "project_id", fakeUUID(5, 1)),
				resource.TestCheckResourceAttr("data.virtscale_caller.me", "expires_at", "2027-03-04T05:06:07Z"),
				resource.TestCheckResourceAttr("data.virtscale_caller.me", "scopes.#", "1"),
			),
		}},
	})
}

// The fake checks what the provider's client exchanges with it against the contract; the checks are counted so that a
// fake that stopped checking would fail here.
func TestFakeAPI_ClientExchangesAreCheckedAgainstTheContract(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	c, err := client.New(fake.URL(), "test-api-key", testOrgID, client.WithUserAgentVersion("test"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListRegions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := fake.ContractChecked(); n != 1 {
		t.Errorf("checked %d exchanges after one client call, want 1", n)
	}
	// A request that is not the client's (the fake's own tests send wrong ones on purpose) is not checked.
	fakeHTTP(t, fake, "test-api-key", http.MethodGet, "/v1/orgs/"+testOrgID+"/compute/regions", "")
	if n := fake.ContractChecked(); n != 1 {
		t.Errorf("checked %d exchanges after a request that is not the client's, want still 1", n)
	}
}
