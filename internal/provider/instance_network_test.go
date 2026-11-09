package provider

// network_interface blocks of virtscale_instance for internal VMs (Track C, P3): a fixed private IP and MAC, secondary
// IPs for a VRRP pair and source_dest_check, like aws_instance's private_ip, secondary_private_ips and source_dest_check.
// Only an organization of kind internal may set them; entry i describes interface i of the network profile.

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

// infraProfile is AMS1's k8s-geneve profile for internal organizations: one filtered network interface.
const infraProfile = "ams-ssd-infra"

// plainDisk is the disk lines of an instance that uses neither a host nor a host-local volume.
const plainDisk = "disk_gb = 10\n  storage_pool = \"pool-1\""

// nicBlock is one network_interface block with the given lines.
func nicBlock(lines string) string {
	return "network_interface {\n    " + strings.ReplaceAll(lines, "\n", "\n    ") + "\n  }"
}

func nicVM(fake *fakeAPI, name, profile, blocks string) string {
	return internalVMOn(fake, name, profile, 1, 1024, plainDisk+"\n  "+blocks)
}

func TestInstanceSchema_NetworkInterface(t *testing.T) {
	var rs resource.SchemaResponse
	NewInstanceResource().Schema(context.Background(), resource.SchemaRequest{}, &rs)

	block, ok := rs.Schema.Blocks["network_interface"].(rschema.ListNestedBlock)
	if !ok {
		t.Fatalf("network_interface must be a list block, got %T", rs.Schema.Blocks["network_interface"])
	}
	for _, name := range []string{"private_ip", "mac_address", "secondary_private_ips"} {
		if a := block.NestedObject.Attributes[name]; a == nil || !a.IsOptional() || a.IsComputed() {
			t.Errorf("network_interface.%s must exist and be optional only (the API reads back what was asked): %+v", name, a)
		}
	}
	if _, isSet := block.NestedObject.Attributes["secondary_private_ips"].(rschema.SetAttribute); !isSet {
		t.Errorf("secondary_private_ips must be a set: the order of allowed addresses means nothing")
	}
	if a := block.NestedObject.Attributes["source_dest_check"]; a == nil || !a.IsOptional() || !a.IsComputed() {
		t.Errorf("network_interface.source_dest_check must exist and be optional with a default: %+v", a)
	}
}

// The block is sent as the API's network_interfaces (a MAC in lower case), read back into the block, and a change of it
// replaces the instance, as the API only sets it at create. A MAC in upper case is no difference (state keeps the
// spelling of the configuration); another case than state has is no replacement, and the order of the secondary
// addresses is no difference at all. Blocks taken out keep the
// instance (an update in place that forgets them), blocks added to an instance that has none replace it. An import
// reads the interfaces back from the API (the next test is about what follows it).
func TestAccInstanceResource_NetworkInterfaceIsSentReadBackAndReplaces(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.SetInternalOrg(true)
	nic := func(ip, mac, extra string, secondary ...string) string {
		quoted := make([]string, len(secondary))
		for i, s := range secondary {
			quoted[i] = fmt.Sprintf("%q", s)
		}
		return nicBlock(fmt.Sprintf("private_ip = %q\nmac_address = %q\nsecondary_private_ips = [%s]\n%s", ip, mac, strings.Join(quoted, ", "), extra))
	}
	cfg := func(blocks string) string { return nicVM(fake, "nic-01", infraProfile, blocks) }
	creates := func(n int) tfresource.TestCheckFunc {
		return func(*terraform.State) error {
			if got := fake.CreateCount(); got != n {
				return fmt.Errorf("creates = %d, want %d", got, n)
			}
			return nil
		}
	}
	const upper, lower = "FA:16:3E:AA:BB:CC", "fa:16:3e:aa:bb:cc"
	first := nic("10.20.30.40", upper, "", "10.20.30.41", "10.20.30.42")

	tfresource.Test(t, tfresource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []tfresource.TestStep{
			{
				Config: cfg(first),
				Check: tfresource.ComposeAggregateTestCheckFunc(
					tfresource.TestCheckResourceAttr(instanceRes, "network_interface.#", "1"),
					tfresource.TestCheckResourceAttr(instanceRes, "network_interface.0.private_ip", "10.20.30.40"),
					tfresource.TestCheckResourceAttr(instanceRes, "network_interface.0.mac_address", upper),
					tfresource.TestCheckResourceAttr(instanceRes, "network_interface.0.secondary_private_ips.#", "2"),
					tfresource.TestCheckTypeSetElemAttr(instanceRes, "network_interface.0.secondary_private_ips.*", "10.20.30.42"),
					tfresource.TestCheckResourceAttr(instanceRes, "network_interface.0.source_dest_check", "true"),
					creates(1),
					func(*terraform.State) error {
						want := `"network_interfaces":[{"private_ip":"10.20.30.40","mac_address":"` + lower + `","secondary_private_ips":["10.20.30.41","10.20.30.42"],"source_dest_check":true}]`
						if b := lastCreateBody(fake); !strings.Contains(b, want) {
							return fmt.Errorf("the create sent %s, want it to hold %s", b, want)
						}
						return nil
					},
				),
			},
			{Config: cfg(first), ConfigPlanChecks: planAction(plancheck.ExpectEmptyPlan())},
			{ // The MAC in the API's own case and the secondary addresses the other way round: not another interface, so no
				// replacement; an update in place that changes nothing, and then the plan is empty again.
				Config:           cfg(nic("10.20.30.40", lower, "", "10.20.30.42", "10.20.30.41")),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check:            tfresource.ComposeAggregateTestCheckFunc(tfresource.TestCheckResourceAttr(instanceRes, "network_interface.0.mac_address", lower), creates(1)),
			},
			{Config: cfg(nic("10.20.30.40", lower, "", "10.20.30.42", "10.20.30.41")), ConfigPlanChecks: planAction(plancheck.ExpectEmptyPlan())},
			{ // An import reads the interfaces back from the API, as the create asked for them (MAC in lower case).
				ResourceName:      instanceRes,
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if v := states[0].Attributes["network_interface.#"]; v != "1" {
						return fmt.Errorf("an imported instance has network_interface.# = %s, want 1", v)
					}
					return nil
				},
			},
			{
				Config:           cfg(nic("10.20.30.50", upper, "", "10.20.30.41", "10.20.30.42")),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionDestroyBeforeCreate),
				Check:            tfresource.ComposeAggregateTestCheckFunc(tfresource.TestCheckResourceAttr(instanceRes, "network_interface.0.private_ip", "10.20.30.50"), creates(2)),
			},
			{
				Config:           cfg(nic("10.20.30.50", upper, "", "10.20.30.41")),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionDestroyBeforeCreate),
				Check:            creates(3),
			},
			{ // An interface that is not filtered carries no firewall and takes no secondary address.
				Config:           cfg(nicBlock("private_ip = \"10.20.30.50\"\nsource_dest_check = false")),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionDestroyBeforeCreate),
				Check: tfresource.ComposeAggregateTestCheckFunc(
					tfresource.TestCheckResourceAttr(instanceRes, "network_interface.0.source_dest_check", "false"),
					tfresource.TestCheckResourceAttr(instanceRes, "network_interface.0.secondary_private_ips.#", "0"),
					tfresource.TestCheckNoResourceAttr(instanceRes, "network_interface.0.mac_address"),
					creates(4)),
			},
			{Config: cfg(nicBlock("private_ip = \"10.20.30.50\"\nsource_dest_check = false")), ConfigPlanChecks: planAction(plancheck.ExpectEmptyPlan())},
			{ // The blocks taken out: the instance keeps its interfaces, so no replacement; state forgets the block.
				Config:           internalVMOn(fake, "nic-01", infraProfile, 1, 1024, plainDisk),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check:            tfresource.ComposeAggregateTestCheckFunc(tfresource.TestCheckResourceAttr(instanceRes, "network_interface.#", "0"), creates(4)),
			},
			{Config: internalVMOn(fake, "nic-01", infraProfile, 1, 1024, plainDisk), ConfigPlanChecks: planAction(plancheck.ExpectEmptyPlan())},
			{ // Blocks added to an instance that has none: interfaces are set at create, so it is replaced.
				Config:           cfg(first),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionDestroyBeforeCreate),
				Check:            creates(5),
			},
		},
	})
}

// What the API refuses is shown with its own sentence and the next step: a customer organization is refused one way
// whatever it sends, an internal one is told which entry and which value is wrong. A secondary address on an interface
// that is not filtered, more than 8 of them and a MAC the API would store another way are refused at plan time.
func TestAccInstanceResource_NetworkInterfaceRefusalsAreExplained(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	const dual = "ams-dual" // the fake's profile with two interfaces, the second not filtered
	create := func(profile, blocks string) string { return nicVM(fake, "nic-refused", profile, blocks) }
	ipOnly := nicBlock(`private_ip = "10.20.30.40"`)

	tfresource.Test(t, tfresource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []tfresource.TestStep{
			{ // A customer organization.
				Config:      create(infraProfile, ipOnly),
				ExpectError: wrapSafeInOrder("network_interface_not_allowed", "internal organizations"),
			},
			{ // An internal organization: more entries than the profile has interfaces, a bad address, a repeated MAC.
				PreConfig:   func() { fake.SetInternalOrg(true) },
				Config:      create(infraProfile, ipOnly+"\n  "+ipOnly),
				ExpectError: wrapSafeInOrder("invalid_spec", "at most one entry per network interface"),
			},
			{
				Config:      create(infraProfile, nicBlock(`private_ip = "127.0.0.1"`)),
				ExpectError: wrapSafeInOrder("invalid_spec", "network_interfaces[0].private_ip"),
			},
			{
				Config:      create(dual, nicBlock("mac_address = \"fa:16:3e:aa:bb:cc\"")+"\n  "+nicBlock("mac_address = \"FA:16:3E:AA:BB:CC\"")),
				ExpectError: wrapSafeInOrder("invalid_spec", "the MAC address fa:16:3e:aa:bb:cc is used more than once"),
			},
			{ // Plan time: nothing is sent.
				Config:      create(infraProfile, nicBlock("secondary_private_ips = [\"10.20.30.41\"]\nsource_dest_check = false")),
				ExpectError: wrapSafeInOrder("secondary_private_ips", "source_dest_check"),
			},
			{
				Config:      create(infraProfile, nicBlock("secondary_private_ips = [\"10.0.0.1\", \"10.0.0.2\", \"10.0.0.3\", \"10.0.0.4\", \"10.0.0.5\", \"10.0.0.6\", \"10.0.0.7\", \"10.0.0.8\", \"10.0.0.9\"]")),
				ExpectError: wrapSafe("at most 8"),
			},
			{
				Config:      create(infraProfile, nicBlock(`mac_address = "fa-16-3e-aa-bb-cc"`)),
				ExpectError: wrapSafe("aa:bb:cc:dd:ee:ff"),
			},
		},
	})
	if n := fake.CreateCount(); n != 0 {
		t.Errorf("creates = %d, want 0: every create was refused", n)
	}
	if n := len(fake.Bodies(createRoute)); n != 4 {
		t.Errorf("the API got %d creates, want 4: the three plan-time refusals send nothing", n)
	}
}

// The fake answers network_interfaces as the API does (domain/compute/network_interfaces.py): who may set them, what an
// entry may say, what is stored (MAC in lower case with colons) and read back, and null when none were asked for.
func TestFakeAPI_NetworkInterfaces(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)
	create := func(name, profile, nics string) (int, map[string]any) {
		t.Helper()
		body := fmt.Sprintf(`{"name":%q,"image":"image-1","region":"ams","vcpus":1,"memory_mb":1024,"disk_gb":10,"storage_pool":"pool-1","network_profile":%q,"network_interfaces":%s}`, name, profile, nics)
		status, _, resp := fakeHTTP(t, f, "good-key", http.MethodPost, vmsPath(testOrgID), body)
		return status, decodeObject(t, resp)
	}
	const unproc = http.StatusUnprocessableEntity
	refused := func(what, name, profile, nics string, status int, code, detail string) {
		t.Helper()
		got, p := create(name, profile, nics)
		if got != status || p["code"] != code || !strings.Contains(fmt.Sprint(p["detail"]), detail) {
			t.Errorf("%s: %d %v %q, want %d %s holding %q", what, got, p["code"], p["detail"], status, code, detail)
		}
	}

	// A customer organization: one answer whatever it sends; an empty list asks for nothing.
	refused("a customer and an entry", "c1", infraProfile, `[{"private_ip":"10.20.30.40"}]`, unproc, "network_interface_not_allowed", "not available to this organization")
	refused("a customer and a bad entry", "c2", infraProfile, `[{"private_ip":"nonsense"}]`, unproc, "network_interface_not_allowed", "not available to this organization")
	if status, vm := create("c3", infraProfile, `[]`); status != http.StatusAccepted || vm["vm"].(map[string]any)["network_interfaces"] != nil {
		t.Errorf("an empty list from a customer: %d, network_interfaces %v, want 202 and null", status, vm["vm"])
	}

	f.SetInternalOrg(true)
	refused("too many entries", "i1", infraProfile, `[{},{}]`, unproc, "invalid_spec", "has 2 entries and this network profile has 1 network interface")
	refused("an address that is not one", "i2", infraProfile, `[{"private_ip":"nonsense"}]`, unproc, "invalid_spec", "network_interfaces[0].private_ip is not an IPv4 address")
	refused("a loopback address", "i3", infraProfile, `[{"private_ip":"127.0.0.1"}]`, unproc, "invalid_spec", "network_interfaces[0].private_ip")
	refused("a multicast MAC", "i4", infraProfile, `[{"mac_address":"01:00:5e:00:00:01"}]`, unproc, "invalid_spec", "network_interfaces[0].mac_address is not a unicast MAC address")
	refused("nine secondary addresses", "i5", infraProfile, `[{"secondary_private_ips":["10.0.0.1","10.0.0.2","10.0.0.3","10.0.0.4","10.0.0.5","10.0.0.6","10.0.0.7","10.0.0.8","10.0.0.9"]}]`, unproc, "invalid_spec", "at most 8")
	refused("a secondary address equal to the private one", "i6", infraProfile, `[{"private_ip":"10.0.0.1","secondary_private_ips":["10.0.0.1"]}]`, unproc, "invalid_spec", "repeats the private_ip")
	refused("a secondary address twice", "i7", infraProfile, `[{"secondary_private_ips":["10.0.0.2","10.0.0.2"]}]`, unproc, "invalid_spec", "more than once")
	refused("a secondary address without source_dest_check", "i8", infraProfile, `[{"secondary_private_ips":["10.0.0.2"],"source_dest_check":false}]`, unproc, "invalid_spec", "needs a filtered network interface")
	refused("a secondary address on an interface that has no port security", "i9", "ams-dual", `[{},{"secondary_private_ips":["10.0.0.2"]}]`, unproc, "invalid_spec", "needs a filtered network interface")
	refused("an address used on two entries", "i10", "ams-dual", `[{"private_ip":"10.0.0.2"},{"private_ip":"10.0.0.2"}]`, unproc, "invalid_spec", "the address 10.0.0.2 is used more than once")
	refused("a field the API does not have", "i11", infraProfile, `[{"private_ip":"10.0.0.2","vlan":3}]`, http.StatusBadRequest, "invalid_request", "")

	status, vm := create("i12", infraProfile, `[{"private_ip":"10.20.30.40","mac_address":"FA-16-3E-AA-BB-CC","secondary_private_ips":["10.20.30.41"]}]`)
	if status != http.StatusAccepted {
		t.Fatalf("a valid entry: %d %v", status, vm)
	}
	got := vm["vm"].(map[string]any)["network_interfaces"].([]any)
	want := map[string]any{"private_ip": "10.20.30.40", "mac_address": "fa:16:3e:aa:bb:cc", "secondary_private_ips": []any{"10.20.30.41"}, "source_dest_check": true}
	if len(got) != 1 || fmt.Sprint(got[0]) != fmt.Sprint(want) {
		t.Errorf("network_interfaces = %v, want [%v] (MAC in lower case with colons, source_dest_check true by default)", got, want)
	}
	if _, vm := getVmVia(t, f, "good-key", vm["vm"].(map[string]any)["id"].(string)); fmt.Sprint(vm["network_interfaces"]) != fmt.Sprint(got) {
		t.Errorf("a read answers %v, want what the create stored %v", vm["network_interfaces"], got)
	}
	if status, vm := create("i13", infraProfile, `null`); status != http.StatusAccepted || vm["vm"].(map[string]any)["network_interfaces"] != nil {
		t.Errorf("a null list: %d, network_interfaces %v, want 202 and null", status, vm["vm"])
	}
}

// out-of-band creates an instance the way the admin API or another tool would, with the given network_interfaces, and
// returns its id once it runs.
func createOutOfBand(t *testing.T, f *fakeAPI, name, nics string) string {
	t.Helper()
	body := fmt.Sprintf(`{"name":%q,"image":"image-1","region":"ams","vcpus":1,"memory_mb":1024,"disk_gb":10,"storage_pool":"pool-1","network_profile":%q,"network_interfaces":%s}`, name, infraProfile, nics)
	id := createVmVia(t, f, "test-api-key", body).get("vm")["id"].(string)
	runVm(t, f, "test-api-key", id)
	return id
}

// An instance created with interfaces and then imported has them in state, read back from the API, so a configuration
// with the same blocks plans no changes (it used to plan a replacement: state had no blocks to compare with). A
// configuration without blocks plans no replacement either: one update in place forgets the blocks, and after it
// Read does not bring them back, so the next plan is empty.
func TestAccInstanceResource_ImportedNetworkInterfacesPlanNoChanges(t *testing.T) {
	const nics = `[{"private_ip":"10.20.30.40","mac_address":"FA:16:3E:AA:BB:CC","secondary_private_ips":["10.20.30.41","10.20.30.42"]}]`
	for _, tc := range []struct {
		name        string
		blocks      string
		afterImport tfresource.ConfigPlanChecks
	}{
		{"with the same blocks", nicBlock("private_ip = \"10.20.30.40\"\nmac_address = \"fa:16:3e:aa:bb:cc\"\nsecondary_private_ips = [\"10.20.30.42\", \"10.20.30.41\"]"), planAction(plancheck.ExpectEmptyPlan())},
		{"without blocks", "", expectAction(plancheck.ResourceActionUpdate)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeAPI(t, "test-api-key", testOrgID)
			fake.SetInternalOrg(true)
			cfg := nicVM(fake, "imp-01", infraProfile, tc.blocks)
			var id string
			var creates int

			tfresource.Test(t, tfresource.TestCase{
				PreCheck:                 func() { testAccPreCheck(t) },
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
				CheckDestroy:             testAccCheckInstanceDestroy(fake),
				Steps: []tfresource.TestStep{
					{ // The instance exists, built outside Terraform; the plan would create another one.
						PreConfig:          func() { id = createOutOfBand(t, fake, "imp-01", nics); creates = fake.CreateCount() },
						Config:             cfg,
						PlanOnly:           true,
						ExpectNonEmptyPlan: true,
					},
					{ // Imported into the state the next steps use.
						ResourceName:       instanceRes,
						ImportState:        true,
						ImportStateIdFunc:  func(*terraform.State) (string, error) { return id, nil },
						ImportStatePersist: true,
					},
					{
						Config:           cfg,
						ConfigPlanChecks: tc.afterImport,
						Check: func(*terraform.State) error {
							if n := fake.CreateCount(); n != creates {
								return fmt.Errorf("creates = %d, want %d: the imported instance must not be replaced", n, creates)
							}
							return nil
						},
					},
					{Config: cfg, ConfigPlanChecks: planAction(plancheck.ExpectEmptyPlan())},
				},
			})
		})
	}
}
