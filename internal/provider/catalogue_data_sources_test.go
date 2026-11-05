package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
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
		resource.TestCheckTypeSetElemAttr("data.virtscale_caller.me", "permissions.*", "network.firewall.update"),
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
		resource.TestCheckResourceAttr("data.virtscale_regions.all", "regions.#", "2"),
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
