package provider

// Host placement and a host-local root volume for internal VMs (Track C, P1): host_id and root_block_device on
// virtscale_instance, like aws_instance's host_id and root_block_device. Only an organization of kind internal may
// place an instance on a host; the fake API (SetInternalOrg) answers as the API does for either kind.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	tfresource "github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// createRoute is the key under which the fake records the bodies of instance creates (see fakeAPI.Bodies).
const createRoute = "POST compute/instances"

// lastCreateBody is the body of the newest create the fake received.
func lastCreateBody(f *fakeAPI) string {
	b := f.Bodies(createRoute)
	if len(b) == 0 {
		return ""
	}
	return b[len(b)-1]
}

// internalVM is an instance "test" in region ams (where the fake's hosts and the host-local pool ams1-local are), with
// the given sizes and lines; the disk and the host are the caller's to say.
func internalVM(fake *fakeAPI, name string, vcpus, memoryMB int, lines string) string {
	return testAccProviderBlock(fake) + fmt.Sprintf(`
resource "virtscale_instance" "test" {
  name            = %q
  region          = "ams"
  image           = "image-1"
  vcpus           = %d
  memory_mb       = %d
  network_profile = "net-1"
  %s
}
`, name, vcpus, memoryMB, lines)
}

// pinned is the lines of an instance on host with a plain disk.
func pinned(host string) string {
	return fmt.Sprintf("disk_gb = 10\n  storage_pool = \"pool-1\"\n  host_id = %q", host)
}

func TestInstanceSchema_HostIDAndRootBlockDevice(t *testing.T) {
	var rs resource.SchemaResponse
	NewInstanceResource().Schema(context.Background(), resource.SchemaRequest{}, &rs)

	host, ok := rs.Schema.Attributes["host_id"]
	if !ok || !host.IsOptional() || !host.IsComputed() {
		t.Errorf("host_id must exist and be optional and computed (read back from the API): %+v", host)
	}

	block, ok := rs.Schema.Blocks["root_block_device"].(rschema.ListNestedBlock)
	if !ok {
		t.Fatalf("root_block_device must be a list block (like aws_instance's, at most one), got %T", rs.Schema.Blocks["root_block_device"])
	}
	for _, name := range []string{"volume_type", "volume_size"} {
		if a := block.NestedObject.Attributes[name]; a == nil || !a.IsOptional() {
			t.Errorf("root_block_device.%s must exist and be optional: %+v", name, a)
		}
	}
	if a := block.NestedObject.Attributes["delete_on_termination"]; a == nil || !a.IsComputed() || a.IsOptional() {
		t.Errorf("root_block_device.delete_on_termination must be computed only: %+v", a)
	}
}

// host_id goes to the API as typed and comes back as the API spells the host (not case sensitive: the API stores
// IKVM-AMS1-01 for ikvm-ams1-01). State keeps what the configuration says, so there is no diff; a different case alone
// is not another host, so it does not replace; another host does, and so does nothing else: taking the line out keeps
// the host. An import reads the API's spelling.
func TestAccInstanceResource_HostIDIsSentReadBackAndReplaces(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.SetInternalOrg(true)
	cfg := func(host string) string { return internalVM(fake, "pin-01", 1, 1024, pinned(host)) }
	creates := func(n int) tfresource.TestCheckFunc {
		return func(*terraform.State) error {
			if got := fake.CreateCount(); got != n {
				return fmt.Errorf("creates = %d, want %d", got, n)
			}
			return nil
		}
	}

	tfresource.Test(t, tfresource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []tfresource.TestStep{
			{
				Config: cfg("ikvm-ams1-01"),
				Check: tfresource.ComposeAggregateTestCheckFunc(
					tfresource.TestCheckResourceAttr(instanceRes, "host_id", "ikvm-ams1-01"),
					creates(1),
					func(*terraform.State) error {
						if b := lastCreateBody(fake); !strings.Contains(b, `"host_id":"ikvm-ams1-01"`) {
							return fmt.Errorf("the create sent %s, want host_id as configured", b)
						}
						return nil
					},
				),
			},
			{Config: cfg("ikvm-ams1-01"), ConfigPlanChecks: planAction(plancheck.ExpectEmptyPlan())},
			{
				ResourceName:            instanceRes,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"host_id"},
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if got := states[0].Attributes["host_id"]; got != "IKVM-AMS1-01" {
						return fmt.Errorf("an imported instance has host_id %q, want the API's spelling IKVM-AMS1-01", got)
					}
					return nil
				},
			},
			{ // The same host in the API's own spelling: an update in place that changes nothing, never a replacement.
				Config:           cfg("IKVM-AMS1-01"),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check:            tfresource.ComposeAggregateTestCheckFunc(tfresource.TestCheckResourceAttr(instanceRes, "host_id", "IKVM-AMS1-01"), creates(1)),
			},
			{
				Config:           cfg("IKVM-AMS1-02"),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionDestroyBeforeCreate),
				Check:            tfresource.ComposeAggregateTestCheckFunc(tfresource.TestCheckResourceAttr(instanceRes, "host_id", "IKVM-AMS1-02"), creates(2)),
			},
			{ // Removing the line keeps the host, like every other optional value the API reports.
				Config:           internalVM(fake, "pin-01", 1, 1024, "disk_gb = 10\n  storage_pool = \"pool-1\""),
				ConfigPlanChecks: planAction(plancheck.ExpectEmptyPlan()),
			},
		},
	})
}

// What the API refuses is shown with its own sentence and the next step: a customer organization gets one answer
// whatever host it names, an internal one is told when the host or the pool's host is wrong.
func TestAccInstanceResource_HostRefusalsAreExplained(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	local := func(host string) string { // a disk on the host-local pool, with or without a host
		return "disk_gb = 40\n  storage_pool = \"ams1-local\"\n  " + host
	}

	tfresource.Test(t, tfresource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []tfresource.TestStep{
			{ // Not a host name: refused at plan time, before anything is sent.
				Config:      internalVM(fake, "refused", 1, 1024, pinned("not a host!")),
				ExpectError: wrapSafe("one plain host name"),
			},
			{ // A customer organization: refused, for a host that exists and for one that does not alike.
				Config:      internalVM(fake, "refused", 1, 1024, pinned("IKVM-AMS1-01")),
				ExpectError: wrapSafeInOrder("host_placement_not_allowed", "internal organizations"),
			},
			{
				Config:      internalVM(fake, "refused", 1, 1024, pinned("IKVM-NOWHERE-99")),
				ExpectError: wrapSafeInOrder("host_placement_not_allowed", "internal organizations"),
			},
			{ // An internal organization: a host nobody has, and a host of another region.
				PreConfig:   func() { fake.SetInternalOrg(true) },
				Config:      internalVM(fake, "refused", 1, 1024, pinned("IKVM-NOWHERE-99")),
				ExpectError: wrapSafeInOrder("host_unavailable", "check host_id"),
			},
			{
				Config:      internalVM(fake, "refused", 1, 1024, pinned("IKVM-GRO1-01")),
				ExpectError: wrapSafeInOrder("host_unavailable", "check host_id"),
			},
			{ // A host-local pool without its host.
				Config:      internalVM(fake, "refused", 1, 1024, local("")),
				ExpectError: wrapSafeInOrder("host_required", "set host_id"),
			},
		},
	})
	if n := fake.CreateCount(); n != 0 {
		t.Errorf("creates = %d, want 0: every create was refused", n)
	}
}

// The API never resizes an instance placed on a host in place (409 resize_not_available), so a different vcpus or
// memory_mb plans a replacement when host_id is set -- in the configuration or only in state, as after an import -- and
// no resize is asked for. Without a host the resize in place is what it was.
func TestAccInstanceResource_APinnedInstanceIsReplacedNotResized(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.SetInternalOrg(true)
	const disk = "disk_gb = 10\n  storage_pool = \"pool-1\""
	var id string
	replaced := func(creates int) tfresource.TestCheckFunc {
		return func(s *terraform.State) error {
			now, _ := stateID(s)
			if now == id {
				return fmt.Errorf("the instance was not replaced: id = %s", now)
			}
			id = now
			if n := len(fake.Bodies(resizeRoute)); n != 0 || fake.CreateCount() != creates {
				return fmt.Errorf("resizes = %d (want 0), creates = %d (want %d)", n, fake.CreateCount(), creates)
			}
			return nil
		}
	}

	tfresource.Test(t, tfresource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []tfresource.TestStep{
			{Config: internalVM(fake, "grow-pin", 1, 1024, pinned("IKVM-AMS1-01")), Check: vmIDFromState(&id)},
			{
				Config:           internalVM(fake, "grow-pin", 2, 1024, pinned("IKVM-AMS1-01")),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionDestroyBeforeCreate),
				Check:            replaced(2),
			},
			{
				Config:           internalVM(fake, "grow-pin", 2, 2048, pinned("IKVM-AMS1-01")),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionDestroyBeforeCreate),
				Check:            replaced(3),
			},
			{ // The host is only in state now (the line is gone): the instance is still pinned, so still replaced. The
				// replacement is built without a host, because the configuration names none.
				Config:           internalVM(fake, "grow-pin", 4, 2048, disk),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionDestroyBeforeCreate),
				Check: tfresource.ComposeAggregateTestCheckFunc(replaced(4),
					tfresource.TestCheckNoResourceAttr(instanceRes, "host_id")),
			},
			{ // Not pinned any more: a resize is in place again, one POST and the same instance.
				Config:           internalVM(fake, "grow-pin", 2, 2048, disk),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check: func(s *terraform.State) error {
					if now, _ := stateID(s); now != id || len(fake.Bodies(resizeRoute)) != 1 {
						return fmt.Errorf("id = %s (want %s), resizes = %d (want 1)", now, id, len(fake.Bodies(resizeRoute)))
					}
					return nil
				},
			},
		},
	})
}

// --- root_block_device -----------------------------------------------------------------------------------------

// The block is another spelling of storage_pool and disk_gb: it sends those two fields, reads them back into itself
// (and delete_on_termination, always true) and into the attributes, and every change of it replaces the instance.
func TestAccInstanceResource_RootBlockDeviceIsAnotherSpellingOfPoolAndDisk(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.SetInternalOrg(true)
	block := func(pool string, size int) string {
		return fmt.Sprintf("host_id = \"IKVM-AMS1-01\"\n  root_block_device {\n    volume_type = %q\n    volume_size = %d\n  }", pool, size)
	}
	attrs := func(pool string, size int) string {
		return fmt.Sprintf("host_id = \"IKVM-AMS1-01\"\n  storage_pool = %q\n  disk_gb = %d", pool, size)
	}
	vm := func(lines string) string { return internalVM(fake, "vol-01", 2, 2048, lines) }
	state := func(pool string, size int, creates int) tfresource.TestCheckFunc {
		return tfresource.ComposeAggregateTestCheckFunc(
			tfresource.TestCheckResourceAttr(instanceRes, "storage_pool", pool),
			tfresource.TestCheckResourceAttr(instanceRes, "disk_gb", fmt.Sprint(size)),
			func(*terraform.State) error {
				if got := fake.CreateCount(); got != creates {
					return fmt.Errorf("creates = %d, want %d", got, creates)
				}
				return nil
			})
	}
	inBlock := func(pool string, size int) tfresource.TestCheckFunc {
		return tfresource.ComposeAggregateTestCheckFunc(
			tfresource.TestCheckResourceAttr(instanceRes, "root_block_device.#", "1"),
			tfresource.TestCheckResourceAttr(instanceRes, "root_block_device.0.volume_type", pool),
			tfresource.TestCheckResourceAttr(instanceRes, "root_block_device.0.volume_size", fmt.Sprint(size)),
			tfresource.TestCheckResourceAttr(instanceRes, "root_block_device.0.delete_on_termination", "true"))
	}

	tfresource.Test(t, tfresource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []tfresource.TestStep{
			{
				Config: vm(block("ams1-local", 40)),
				Check: tfresource.ComposeAggregateTestCheckFunc(inBlock("ams1-local", 40), state("ams1-local", 40, 1),
					func(*terraform.State) error {
						b := lastCreateBody(fake)
						for _, want := range []string{`"storage_pool":"ams1-local"`, `"disk_gb":40`, `"vcpus":2`} {
							if !strings.Contains(b, want) {
								return fmt.Errorf("the create sent %s, want %s", b, want)
							}
						}
						for _, not := range []string{"instance_type", "root_block_device", "volume_"} {
							if strings.Contains(b, not) {
								return fmt.Errorf("the create sent %s, which must not hold %s", b, not)
							}
						}
						return nil
					}),
			},
			{Config: vm(block("ams1-local", 40)), ConfigPlanChecks: planAction(plancheck.ExpectEmptyPlan())},
			{
				Config:           vm(block("ams1-local", 50)),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionDestroyBeforeCreate),
				Check:            tfresource.ComposeAggregateTestCheckFunc(inBlock("ams1-local", 50), state("ams1-local", 50, 2)),
			},
			{
				Config:           vm(block("ams2-local", 50)),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionDestroyBeforeCreate),
				Check:            tfresource.ComposeAggregateTestCheckFunc(inBlock("ams2-local", 50), state("ams2-local", 50, 3)),
			},
			{ // The block taken out for the same two values as attributes: the disk is the same, so no replacement.
				Config:           vm(attrs("ams2-local", 50)),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check: tfresource.ComposeAggregateTestCheckFunc(state("ams2-local", 50, 3),
					tfresource.TestCheckResourceAttr(instanceRes, "root_block_device.#", "0")),
			},
			{ // ... and the other way round: the same disk written as the block adopts it, no replacement.
				Config:           vm(block("ams2-local", 50)),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check:            tfresource.ComposeAggregateTestCheckFunc(inBlock("ams2-local", 50), state("ams2-local", 50, 3)),
			},
			{Config: vm(block("ams2-local", 50)), ConfigPlanChecks: planAction(plancheck.ExpectEmptyPlan())},
		},
	})
}

// A volume_type alone goes with an instance type, as storage_pool does: the type sets the sizes, the block's
// volume_size is read back from them.
func TestAccInstanceResource_RootBlockDeviceVolumeTypeWithAnInstanceType(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	cfg := testAccInstanceBare(fake, "typed-vol", "gro", "instance_type = \"g1-small\"\n  root_block_device {\n    volume_type = \"gro-ceph\"\n  }")

	tfresource.Test(t, tfresource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []tfresource.TestStep{
			{
				Config: cfg,
				Check: tfresource.ComposeAggregateTestCheckFunc(
					tfresource.TestCheckResourceAttr(instanceRes, "storage_pool", "gro-ceph"),
					tfresource.TestCheckResourceAttr(instanceRes, "root_block_device.0.volume_type", "gro-ceph"),
					tfresource.TestCheckResourceAttr(instanceRes, "root_block_device.0.volume_size", "20"),
					func(*terraform.State) error {
						b := lastCreateBody(fake)
						if !strings.Contains(b, `"instance_type":"g1-small"`) || !strings.Contains(b, `"storage_pool":"gro-ceph"`) || strings.Contains(b, "disk_gb") {
							return fmt.Errorf("the create sent %s, want the type and the pool from the block, and no disk_gb", b)
						}
						return nil
					}),
			},
			{Config: cfg, ConfigPlanChecks: planAction(plancheck.ExpectEmptyPlan())},
			{ // An update in place plans the size the block leaves out as unknown, and reads it back: it is not lost.
				Config:           testAccInstanceBare(fake, "typed-vol", "gro", "instance_type = \"g1-small\"\n  hostname = \"typed-vol.example.com\"\n  root_block_device {\n    volume_type = \"gro-ceph\"\n  }"),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check:            tfresource.TestCheckResourceAttr(instanceRes, "root_block_device.0.volume_size", "20"),
			},
		},
	})
}

// The block does not go with what it replaces: storage_pool, disk_gb, or (for the size) an instance type; and at most
// one block. Each is refused at plan time, with nothing sent.
func TestAccInstanceResource_RootBlockDeviceConflictsAreRefusedAtPlanTime(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	for _, tc := range []struct {
		name, lines, want string
	}{
		{"storage_pool", "storage_pool = \"p\"\n  root_block_device {\n    volume_type = \"p\"\n    volume_size = 10\n  }\n  vcpus = 1\n  memory_mb = 1024", "root_block_device and storage_pool"},
		{"disk_gb", "disk_gb = 10\n  root_block_device {\n    volume_type = \"p\"\n    volume_size = 10\n  }\n  vcpus = 1\n  memory_mb = 1024", "root_block_device and disk_gb"},
		{"instance_type and volume_size", "instance_type = \"g1-small\"\n  root_block_device {\n    volume_size = 10\n  }", "volume_size and instance_type"},
		{"two blocks", "vcpus = 1\n  memory_mb = 1024\n  root_block_device {\n    volume_type = \"p\"\n    volume_size = 10\n  }\n  root_block_device {\n    volume_type = \"p\"\n    volume_size = 10\n  }", "at most 1"},
		{"a block without a size", "vcpus = 1\n  memory_mb = 1024\n  root_block_device {\n    volume_type = \"p\"\n  }", "Incomplete sizes"},
		{"a block without a pool", "vcpus = 1\n  memory_mb = 1024\n  root_block_device {\n    volume_size = 10\n  }", "Incomplete sizes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tfresource.Test(t, tfresource.TestCase{
				PreCheck:                 func() { testAccPreCheck(t) },
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
				Steps: []tfresource.TestStep{{
					Config:      testAccInstanceBare(fake, "conflict", "gro", tc.lines),
					ExpectError: wrapSafe(tc.want),
				}},
			})
		})
	}
	if n := fake.CreateCount(); n != 0 {
		t.Errorf("creates = %d, want 0: all of them are refused at plan time", n)
	}
}

// A configuration that uses neither new thing plans no difference, and neither does the instance it creates when it
// is imported: Read puts values in root_block_device only when the state already has the block, and host_id only as the
// API reports it (null for an instance on no named host).
func TestAccInstanceResource_ExistingConfigurationsSeeNoDifference(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	cfg := attrConfig(fake, "plain-01", 1, 1024, 10, "")

	tfresource.Test(t, tfresource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []tfresource.TestStep{
			{
				Config: cfg,
				Check: tfresource.ComposeAggregateTestCheckFunc(
					tfresource.TestCheckNoResourceAttr(instanceRes, "host_id"),
					tfresource.TestCheckResourceAttr(instanceRes, "root_block_device.#", "0")),
			},
			{Config: cfg, ConfigPlanChecks: planAction(plancheck.ExpectEmptyPlan())},
			{
				ResourceName:      instanceRes,
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					a := states[0].Attributes
					if v := a["root_block_device.#"]; v != "" && v != "0" {
						return fmt.Errorf("an imported instance has root_block_device.# = %s, want no block", v)
					}
					if v, has := a["host_id"]; has && v != "" {
						return fmt.Errorf("an instance on no named host has host_id %q, want null", v)
					}
					return nil
				},
			},
		},
	})
}

// --- the fake API itself ---------------------------------------------------------------------------------------

// The fake answers host_id as the API does (config/compute-platform.yaml: ams1-local, and its hosts; the refusals of
// domain/compute/vms.py): who may name a host, which host is usable, what a host-local pool needs, and that a pinned
// instance is not resized.
func TestFakeAPI_HostPlacementAndHostLocalPools(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)
	body := func(name, region, pool, extra string) string {
		return fmt.Sprintf(`{"name":%q,"image":"image-1","region":%q,"vcpus":1,"memory_mb":1024,"disk_gb":40,"storage_pool":%q%s}`, name, region, pool, extra)
	}
	refused := func(what, name, region, pool, extra string, status int, code string) {
		t.Helper()
		got, _, resp := fakeHTTP(t, f, "good-key", http.MethodPost, vmsPath(testOrgID), body(name, region, pool, extra))
		if p := decodeObject(t, resp); got != status || p["code"] != code {
			t.Errorf("%s: %d %v, want %d %s (%s)", what, got, p["code"], status, code, resp)
		}
	}
	const unproc = http.StatusUnprocessableEntity

	// A customer organization: one answer, whatever it names.
	refused("a customer and a real host", "c1", "ams", "ams1-local", `,"host_id":"IKVM-AMS1-01"`, unproc, "host_placement_not_allowed")
	refused("a customer and a host nobody has", "c2", "ams", "ams1-local", `,"host_id":"IKVM-NOWHERE-99"`, unproc, "host_placement_not_allowed")

	f.SetInternalOrg(true)
	refused("an unknown host", "i1", "ams", "ams1-local", `,"host_id":"IKVM-NOWHERE-99"`, unproc, "host_unavailable")
	refused("a host of another region", "i2", "gro", "pool-1", `,"host_id":"IKVM-AMS1-01"`, unproc, "host_unavailable")
	refused("a host-local pool without a host", "i3", "ams", "ams1-local", ``, unproc, "host_required")
	refused("a host that is not in the pool's list", "i4", "ams", "ams1-local", `,"host_id":"IKVM-GRO1-01"`, unproc, "host_unavailable")
	refused("a host name that is not a host name", "i5", "ams", "pool-1", `,"host_id":"not a host!"`, http.StatusBadRequest, "invalid_request")

	// The host is matched without regard to case and answered in the API's spelling; a pool that is not host-local takes
	// a host too.
	local := createVmVia(t, f, "good-key", body("i6", "ams", "ams1-local", `,"host_id":"ikvm-ams1-01"`)).get("vm")
	if local["host_id"] != "IKVM-AMS1-01" || local["storage_pool_id"] != "ams1-local" || local["disk_gb"] != float64(40) {
		t.Errorf("host-local create: host_id %v, pool %v, disk %v, want IKVM-AMS1-01, ams1-local, 40", local["host_id"], local["storage_pool_id"], local["disk_gb"])
	}
	other := createVmVia(t, f, "good-key", body("i7", "ams", "pool-1", `,"host_id":"IKVM-AMS1-02"`)).get("vm")
	plain := createVmVia(t, f, "good-key", body("i8", "gro", "pool-1", ``)).get("vm")
	if other["host_id"] != "IKVM-AMS1-02" || plain["host_id"] != nil {
		t.Errorf("host_id = %v and %v, want IKVM-AMS1-02 and null", other["host_id"], plain["host_id"])
	}

	// A pinned instance is not resized in place; one on no named host is.
	pinnedID, plainID := other["id"].(string), plain["id"].(string)
	runVm(t, f, "good-key", pinnedID)
	runVm(t, f, "good-key", plainID)
	status, _, resp := fakeHTTP(t, f, "good-key", http.MethodPost, vmsPath(testOrgID)+"/"+pinnedID+"/resize", `{"vcpus":2,"memory_mb":2048}`)
	if p := decodeObject(t, resp); status != http.StatusConflict || p["code"] != "resize_not_available" {
		t.Errorf("resize of a pinned instance: %d %v, want 409 resize_not_available (%s)", status, p["code"], resp)
	}
	if status, _, resp := fakeHTTP(t, f, "good-key", http.MethodPost, vmsPath(testOrgID)+"/"+plainID+"/resize", `{"vcpus":2,"memory_mb":2048}`); status != http.StatusAccepted {
		t.Errorf("resize of an instance on no named host: %d (%s), want 202", status, resp)
	}
}
