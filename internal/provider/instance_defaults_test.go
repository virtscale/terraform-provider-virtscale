package provider

// The create-without-ids behaviour of virtscale_instance: storage_pool, network_profile and the three sizes are
// optional, the API fills in what is left out (customer API compute.py _check_create_shape and domain/compute/vms.py
// create_vm_request), and the provider reads the chosen values back. The fake applies the same rules.

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

// testAccInstanceBare is a virtscale_instance named test in region with image-1 and only the given extra lines, so a
// test says exactly which of the optional attributes it sets.
func testAccInstanceBare(fake *fakeAPI, name, region, lines string) string {
	return testAccProviderBlock(fake) + fmt.Sprintf(`
resource "virtscale_instance" "test" {
  name   = %q
  region = %q
  image  = "image-1"
  %s
}
`, name, region, lines)
}

const instanceRes = "virtscale_instance.test"

// TestFakeAPI_CreateShapeAndDefaults pins the fake to the real create: which bodies it accepts, what a type fills in
// and the defaults for the pool and the profile.
func TestFakeAPI_CreateShapeAndDefaults(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)
	body := func(extra string) string {
		return `{"name":"shape","image":"image-1","region":"gro"` + extra + `}`
	}

	accepted := []struct {
		name, extra                     string
		vcpus, memoryMB, diskGB         float64
		pool, profile, instanceTypeWant string
	}{
		{"a type alone", `,"instance_type":"g1-small"`, 1, 2048, 20, "gro-ceph", "gro-tenant-default", "g1-small"},
		{"a type with its own pool and profile", `,"instance_type":"g1-medium","storage_pool":"p","network_profile":"n"`, 2, 4096, 40, "p", "n", "g1-medium"},
		{"sizes and a pool", `,"vcpus":3,"memory_mb":512,"disk_gb":7,"storage_pool":"p"`, 3, 512, 7, "p", "gro-tenant-default", ""},
	}
	for i, tc := range accepted {
		vm := createVmVia(t, f, "good-key", strings.Replace(body(tc.extra), `"shape"`, fmt.Sprintf(`"shape-%d"`, i), 1)).get("vm")
		if vm["vcpus"] != tc.vcpus || vm["memory_mb"] != tc.memoryMB || vm["disk_gb"] != tc.diskGB {
			t.Errorf("%s: sizes = %v/%v/%v, want %v/%v/%v", tc.name, vm["vcpus"], vm["memory_mb"], vm["disk_gb"], tc.vcpus, tc.memoryMB, tc.diskGB)
		}
		if vm["storage_pool_id"] != tc.pool || vm["network_profile_id"] != tc.profile {
			t.Errorf("%s: pool/profile = %v/%v, want %s/%s", tc.name, vm["storage_pool_id"], vm["network_profile_id"], tc.pool, tc.profile)
		}
		if tc.instanceTypeWant == "" {
			if vm["instance_type"] != nil {
				t.Errorf("%s: instance_type = %v, want null", tc.name, vm["instance_type"])
			}
		} else if vm["instance_type"] != tc.instanceTypeWant {
			t.Errorf("%s: instance_type = %v, want %s", tc.name, vm["instance_type"], tc.instanceTypeWant)
		}
	}

	refused := []struct{ name, extra, code, detail string }{
		{"a type and sizes", `,"instance_type":"g1-small","vcpus":1`, "invalid_spec",
			"name an instance type or give vcpus, memory_mb and disk_gb, not both"},
		{"nothing to size it", ``, "instance_type_required", "name an instance type: see GET /v1/orgs/{org}/instance-types"},
		{"a pool alone", `,"storage_pool":"p"`, "instance_type_required", "name an instance type"},
		{"some of the sizes", `,"vcpus":1,"memory_mb":512,"storage_pool":"p"`, "invalid_spec",
			"vcpus, memory_mb, disk_gb and a disk_type or storage_pool are required together"},
		{"sizes without a pool", `,"vcpus":1,"memory_mb":512,"disk_gb":5`, "invalid_spec",
			"vcpus, memory_mb, disk_gb and a disk_type or storage_pool are required together"},
		{"an unknown type", `,"instance_type":"nope"`, "instance_type_unavailable", "instance type 'nope' is not available in gro"},
		{"a coming_soon type", `,"instance_type":"c1-large"`, "instance_type_unavailable", "instance type 'c1-large' is not available in gro"},
	}
	for _, tc := range refused {
		status, _, resp := fakeHTTP(t, f, "good-key", http.MethodPost, vmsPath(testOrgID), body(tc.extra))
		got := decodeObject(t, resp)
		if status != http.StatusUnprocessableEntity || got["code"] != tc.code || !strings.Contains(fmt.Sprint(got["detail"]), tc.detail) {
			t.Errorf("%s: %d %v/%v, want 422 %s with %q", tc.name, status, got["code"], got["detail"], tc.code, tc.detail)
		}
	}
	if n := f.CreateCount(); n != len(accepted) {
		t.Errorf("creates = %d, want %d: a refused create must not make an instance", n, len(accepted))
	}
}

// A type alone is the whole first VM: no pool, no profile, no sizes. The API's choices are read back, the next plan is
// empty and an import finds the same values.
func TestAccInstanceDefaults_AnInstanceTypeAloneIsEnough(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	config := testAccInstanceBare(fake, "first-vm", "gro", `instance_type = "g1-small"`)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(instanceRes, "state", "running"),
					resource.TestCheckResourceAttr(instanceRes, "instance_type", "g1-small"),
					resource.TestCheckResourceAttr(instanceRes, "vcpus", "1"),
					resource.TestCheckResourceAttr(instanceRes, "memory_mb", "2048"),
					resource.TestCheckResourceAttr(instanceRes, "disk_gb", "20"),
					resource.TestCheckResourceAttr(instanceRes, "storage_pool", "gro-ceph"),
					resource.TestCheckResourceAttr(instanceRes, "network_profile", "gro-tenant-default"),
				),
			},
			{
				Config:           config,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
			},
			{ResourceName: instanceRes, ImportState: true, ImportStateVerify: true},
		},
	})
	if n := fake.CreateCount(); n != 1 {
		t.Errorf("creates = %d, want 1: reading the defaults back must not replace the instance", n)
	}
}

// Raw sizes still work, with the pool the API cannot guess; the profile is the API's default.
func TestAccInstanceDefaults_RawSizesNeedOnlyAPool(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	config := testAccInstanceBare(fake, "sized-vm", "gro", `
  vcpus        = 2
  memory_mb    = 1024
  disk_gb      = 10
  storage_pool = "pool-9"`)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(instanceRes, "storage_pool", "pool-9"),
					resource.TestCheckResourceAttr(instanceRes, "network_profile", "gro-tenant-default"),
					resource.TestCheckNoResourceAttr(instanceRes, "instance_type"),
				),
			},
			{Config: config, PlanOnly: true},
			{ResourceName: instanceRes, ImportState: true, ImportStateVerify: true},
		},
	})
}

// A pool or profile the user names is theirs: a different one replaces the instance. One they leave out is the API's:
// taking the line out again plans nothing.
func TestAccInstanceDefaults_ANamedPoolOrProfileStillReplaces(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	with := func(pool, profile string) string {
		return testAccInstanceBare(fake, "named-vm", "gro", fmt.Sprintf(`
  instance_type   = "g1-small"
  storage_pool    = %q
  network_profile = %q`, pool, profile))
	}
	replaced := resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
		plancheck.ExpectResourceAction(instanceRes, plancheck.ResourceActionDestroyBeforeCreate),
	}}

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{Config: with("pool-1", "net-1"), Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(instanceRes, "storage_pool", "pool-1"),
				resource.TestCheckResourceAttr(instanceRes, "network_profile", "net-1"))},
			{Config: with("pool-1", "net-1"), PlanOnly: true},
			{Config: with("pool-2", "net-1"), ConfigPlanChecks: replaced,
				Check: resource.TestCheckResourceAttr(instanceRes, "storage_pool", "pool-2")},
			{Config: with("pool-2", "net-2"), ConfigPlanChecks: replaced,
				Check: resource.TestCheckResourceAttr(instanceRes, "network_profile", "net-2")},
			{
				Config: testAccInstanceBare(fake, "named-vm", "gro", `instance_type = "g1-small"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectEmptyPlan(),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(instanceRes, "storage_pool", "pool-2"),
					resource.TestCheckResourceAttr(instanceRes, "network_profile", "net-2")),
			},
		},
	})
	if n := fake.CreateCount(); n != 3 {
		t.Errorf("creates = %d, want 3 (the first, and one replacement per changed id)", n)
	}
}

// A different type is a replacement, and the sizes of the new one are what ends up in state: the old ones must not be
// carried over into the plan.
func TestAccInstanceDefaults_ChangingTheTypeReplacesAndReadsTheNewSizes(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceBare(fake, "grow-vm", "gro", `instance_type = "g1-small"`),
				Check:  resource.TestCheckResourceAttr(instanceRes, "vcpus", "1"),
			},
			{
				Config: testAccInstanceBare(fake, "grow-vm", "gro", `instance_type = "g1-medium"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(instanceRes, plancheck.ResourceActionDestroyBeforeCreate),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(instanceRes, "instance_type", "g1-medium"),
					resource.TestCheckResourceAttr(instanceRes, "vcpus", "2"),
					resource.TestCheckResourceAttr(instanceRes, "memory_mb", "4096"),
					resource.TestCheckResourceAttr(instanceRes, "disk_gb", "40"),
				),
			},
		},
	})
}

// A type taken from the catalogue data source is unknown while the configuration is validated, and must pass.
func TestAccInstanceDefaults_ATypeFromTheCatalogueIsAccepted(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []resource.TestStep{{
			Config: testAccInstanceBare(fake, "catalogue-vm", "gro", `instance_type = data.virtscale_instance_types.gro.instance_types[0].id`) + `
data "virtscale_instance_types" "gro" {
  region = "gro"
}
`,
			Check: resource.TestCheckResourceAttr(instanceRes, "instance_type", "g1-small"),
		}},
	})
}

// The combinations the API refuses are refused at plan time with its sentences, before anything is built.
func TestAccInstanceDefaults_ImpossibleShapesAreRefusedAtPlanTime(t *testing.T) {
	cases := []struct{ name, lines, want string }{
		{"a type and a size", `
  instance_type = "g1-small"
  vcpus         = 1`, "name an instance type or give vcpus, memory_mb and disk_gb, not both"},
		{"nothing to size it", ``, "set instance_type (see the virtscale_instance_types data source), or vcpus, memory_mb, disk_gb and storage_pool"},
		{"a pool alone", `storage_pool = "pool-1"`, "set instance_type (see the virtscale_instance_types data source), or vcpus, memory_mb, disk_gb and storage_pool"},
		{"some of the sizes", `
  vcpus        = 1
  storage_pool = "pool-1"`, "vcpus, memory_mb, disk_gb and storage_pool are required together"},
		{"sizes without a pool", `
  vcpus     = 1
  memory_mb = 1024
  disk_gb   = 10`, "vcpus, memory_mb, disk_gb and storage_pool are required together"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeAPI(t, "test-api-key", testOrgID)
			resource.Test(t, resource.TestCase{
				PreCheck:                 func() { testAccPreCheck(t) },
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
				Steps: []resource.TestStep{{
					Config:      testAccInstanceBare(fake, "bad-shape", "gro", tc.lines),
					ExpectError: wrapSafe(tc.want),
				}},
			})
			if n := fake.CallCount("POST compute/instances"); n != 0 {
				t.Errorf("an impossible shape sent %d create request(s), want 0", n)
			}
		})
	}
}

// An empty string is not a choice: it would plan a create and then fail with "inconsistent result after apply", because
// the API answers with the value it picked. It is refused at plan time instead.
func TestAccInstanceDefaults_AnEmptyTypePoolOrProfileIsRefusedAtPlanTime(t *testing.T) {
	cases := []struct{ attr, lines string }{
		{"instance_type", `instance_type = ""`},
		{"storage_pool", "instance_type = \"g1-small\"\n  storage_pool  = \"\""},
		{"network_profile", "instance_type = \"g1-small\"\n  network_profile = \"\""},
	}
	for _, tc := range cases {
		t.Run(tc.attr, func(t *testing.T) {
			fake := newFakeAPI(t, "test-api-key", testOrgID)
			resource.Test(t, resource.TestCase{
				PreCheck:                 func() { testAccPreCheck(t) },
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
				Steps: []resource.TestStep{{
					Config:      testAccInstanceBare(fake, "empty-value", "gro", tc.lines),
					ExpectError: wrapSafe("Attribute " + tc.attr + " string length must be at least 1, got: 0"),
				}},
			})
			if n := fake.CallCount("POST compute/instances"); n != 0 {
				t.Errorf("an empty %s sent %d create request(s), want 0", tc.attr, n)
			}
		})
	}
}

// What the API decides stays the API's: a type it does not know, or does not sell in the region, is its 422.
func TestAccInstanceDefaults_TheAPIsRefusalsAreShown(t *testing.T) {
	cases := []struct{ name, region, instanceType, want string }{
		{"an unknown type", "gro", "no-such-type", "instance type 'no-such-type' is not available in gro"},
		{"a type not sold in the region", "fra", "g1-small", "instance type 'g1-small' is not available in fra"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeAPI(t, "test-api-key", testOrgID)
			resource.Test(t, resource.TestCase{
				PreCheck:                 func() { testAccPreCheck(t) },
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
				Steps: []resource.TestStep{{
					Config:      testAccInstanceBare(fake, "refused-vm", tc.region, fmt.Sprintf("instance_type = %q", tc.instanceType)),
					ExpectError: wrapSafeInOrder("instance_type_unavailable", tc.want),
				}},
			})
			if fake.CreateCount() != 0 {
				t.Errorf("a refused create made %d instance(s)", fake.CreateCount())
			}
		})
	}
}
