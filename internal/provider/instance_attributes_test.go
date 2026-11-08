package provider

// The attributes virtscale_instance gained for internal VMs (P1, like aws_instance): user_data, workload_type,
// disable_api_termination, a resize in place, and the computed values that stay known across an update.

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	tfresource "github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

const (
	resizeRoute     = "POST compute/instances/{vm_id}/resize"
	protectionRoute = "PUT compute/instances/{vm_id}/protection"
)

// attrConfig is the instance "test" with the given sizes (raw sizes, so a type never fills them in) and extra lines.
func attrConfig(fake *fakeAPI, name string, vcpus, memoryMB, diskGB int, extra string) string {
	return testAccProviderBlock(fake) + fmt.Sprintf(`
resource "virtscale_instance" "test" {
  name            = %q
  region          = "gro"
  image           = "image-1"
  vcpus           = %d
  memory_mb       = %d
  disk_gb         = %d
  storage_pool    = "pool-1"
  network_profile = "net-1"
  %s
}
`, name, vcpus, memoryMB, diskGB, extra)
}

func planAction(action plancheck.PlanCheck) tfresource.ConfigPlanChecks {
	return tfresource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{action}}
}

func expectAction(a plancheck.ResourceActionType) tfresource.ConfigPlanChecks {
	return planAction(plancheck.ExpectResourceAction(instanceRes, a))
}

// stateID is the id of the instance in the state a check sees.
func stateID(s *terraform.State) (string, error) {
	rs := s.RootModule().Resources[instanceRes]
	if rs == nil {
		return "", fmt.Errorf("%s not found in state", instanceRes)
	}
	return rs.Primary.ID, nil
}

// --- user_data ---------------------------------------------------------------------------------------------------

func TestInstanceSchema_UserDataIsSensitive(t *testing.T) {
	var rs resource.SchemaResponse
	NewInstanceResource().Schema(context.Background(), resource.SchemaRequest{}, &rs)
	a, ok := rs.Schema.Attributes["user_data"]
	if !ok || !a.IsSensitive() || !a.IsOptional() {
		t.Errorf("user_data must exist, be optional and be sensitive: %+v", a)
	}
}

// user_data is cloud-init text, sent as standard base64, kept in state as configured and never read back. Another
// value is another first boot, so it replaces the instance. An import has nothing to read it from and leaves it null.
func TestAccInstanceResource_UserDataIsSentAsBase64AndNeverReadBack(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	const text = "#cloud-config\npackages:\n  - htop\n"
	const other = "#cloud-config\npackages:\n  - tmux\n"
	cfg := func(ud string) string {
		return attrConfig(fake, "ud-vm", 1, 1024, 10, fmt.Sprintf("user_data = %q", ud))
	}
	sentIs := func(ud string, creates int) tfresource.TestCheckFunc {
		return func(s *terraform.State) error {
			id, err := stateID(s)
			if err != nil {
				return err
			}
			got, ok := fake.UserDataSent(id)
			if want := base64.StdEncoding.EncodeToString([]byte(ud)); !ok || got != want {
				return fmt.Errorf("the create sent user_data %q (sent: %v), want standard base64 %q", got, ok, want)
			}
			if n := fake.CreateCount(); n != creates {
				return fmt.Errorf("creates = %d, want %d", n, creates)
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
				Config: cfg(text),
				Check:  tfresource.ComposeAggregateTestCheckFunc(tfresource.TestCheckResourceAttr(instanceRes, "user_data", text), sentIs(text, 1)),
			},
			{ // A refresh keeps what was configured: Read never touches it.
				Config: cfg(text),
				Check:  tfresource.TestCheckResourceAttr(instanceRes, "user_data", text),
			},
			{
				ResourceName:            instanceRes,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"user_data"},
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if v := states[0].Attributes["user_data"]; v != "" {
						return fmt.Errorf("an imported instance has user_data %q, want it left null", v)
					}
					return nil
				},
			},
			{
				Config:           cfg(other),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionDestroyBeforeCreate),
				Check:            tfresource.ComposeAggregateTestCheckFunc(tfresource.TestCheckResourceAttr(instanceRes, "user_data", other), sentIs(other, 2)),
			},
		},
	})
}

// The API takes at most 32 KiB once decoded, an empty value is no payload: both are refused at plan time, before
// anything is sent. Exactly 32 KiB is fine.
func TestAccInstanceResource_UserDataSizeIsCheckedAtPlanTime(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	cfg := func(ud string) string {
		return attrConfig(fake, "ud-size", 1, 1024, 10, fmt.Sprintf("user_data = %q", ud))
	}

	tfresource.Test(t, tfresource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []tfresource.TestStep{
			{Config: cfg(strings.Repeat("a", 32*1024+1)), ExpectError: wrapSafe("32 KiB")},
			{Config: cfg(""), ExpectError: wrapSafe("at least 1")},
			{
				// CreateCount only counts creates the fake accepted, which would also hide a request the fake refused: the POSTs
				// that reached the route are what proves the plan-time validator, not the API, turned the two values away.
				PreConfig: func() {
					if n := fake.CallCount("POST compute/instances"); n != 0 {
						t.Fatalf("POST compute/instances calls = %d after the two refused values, want 0: they must be refused at plan time, not by the API", n)
					}
				},
				Config: cfg(strings.Repeat("a", 32*1024)),
				Check: func(s *terraform.State) error {
					if n := fake.CreateCount(); n != 1 {
						return fmt.Errorf("creates = %d, want 1: only the 32 KiB value was sent (the refused ones must not reach the API)", n)
					}
					if n := fake.CallCount("POST compute/instances"); n != 1 {
						return fmt.Errorf("POST compute/instances calls = %d, want 1: only the 32 KiB value was sent", n)
					}
					return nil
				},
			},
		},
	})
}

// A deployment with cloud-init switched off answers 503 not_ready: shown with the API's words and sent once.
func TestAccInstanceResource_UserDataSwitchedOffIsShownAndNotRetried(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.SetUserDataOff(true)

	tfresource.Test(t, tfresource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []tfresource.TestStep{{
			Config:      attrConfig(fake, "ud-off", 1, 1024, 10, `user_data = "#cloud-config"`),
			ExpectError: wrapSafe("not_ready"),
		}},
	})
	if n := fake.CallCount("POST compute/instances"); n != 1 {
		t.Errorf("creates sent = %d, want 1: a not_ready is never retried", n)
	}
}

// --- workload_type -----------------------------------------------------------------------------------------------

// workload_type is read from the API: "customer" when nothing is configured, and the same value configured is no change.
func TestAccInstanceResource_WorkloadTypeDefaultsToCustomerAndIsReadBack(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)

	tfresource.Test(t, tfresource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []tfresource.TestStep{
			{
				Config: attrConfig(fake, "wl-default", 1, 1024, 10, ""),
				Check:  tfresource.TestCheckResourceAttr(instanceRes, "workload_type", "customer"),
			},
			{ResourceName: instanceRes, ImportState: true, ImportStateVerify: true},
			{
				Config:           attrConfig(fake, "wl-default", 1, 1024, 10, `workload_type = "customer"`),
				ConfigPlanChecks: planAction(plancheck.ExpectEmptyPlan()),
			},
		},
	})
}

// "infra" selects VirtScale's own hardware: only an internal organization may ask for it, a customer organization is
// refused with the API's 422 workload_not_allowed. A change of the value replaces the instance.
func TestAccInstanceResource_InfraWorkloadIsForInternalOrganizations(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	infra := attrConfig(fake, "wl-infra", 1, 1024, 10, `workload_type = "infra"`)

	tfresource.Test(t, tfresource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []tfresource.TestStep{
			{Config: infra, ExpectError: wrapSafe("workload_not_allowed")},
			{
				PreConfig: func() { fake.SetInternalOrg(true) },
				Config:    infra,
				Check:     tfresource.TestCheckResourceAttr(instanceRes, "workload_type", "infra"),
			},
			{
				Config:           attrConfig(fake, "wl-infra", 1, 1024, 10, `workload_type = "customer"`),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionDestroyBeforeCreate),
				Check:            tfresource.TestCheckResourceAttr(instanceRes, "workload_type", "customer"),
			},
		},
	})
}

func TestAccInstanceResource_WorkloadTypeMustBeCustomerOrInfra(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)

	tfresource.Test(t, tfresource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []tfresource.TestStep{{
			Config:      attrConfig(fake, "wl-bad", 1, 1024, 10, `workload_type = "gold"`),
			ExpectError: wrapSafe(`value must be one of`),
		}},
	})
	if n := fake.CreateCount(); n != 0 {
		t.Errorf("creates = %d, want 0: the value is refused at plan time", n)
	}
}

// --- disable_api_termination -----------------------------------------------------------------------------------

// Set on create it protects the instance once it runs; a destroy is then refused with the way out and not lifted for
// the user; lifting the protection is an in-place update, and then the destroy works.
func TestAccInstanceResource_DisableApiTerminationProtectsAndRefusesTheDestroy(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	var id string

	tfresource.Test(t, tfresource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []tfresource.TestStep{
			{
				Config: attrConfig(fake, "protected", 1, 1024, 10, "disable_api_termination = true"),
				Check: tfresource.ComposeAggregateTestCheckFunc(
					vmIDFromState(&id),
					tfresource.TestCheckResourceAttr(instanceRes, "disable_api_termination", "true"),
					func(*terraform.State) error {
						if !fake.IsProtected(id) {
							return fmt.Errorf("the instance is not protected in the API")
						}
						if got := fake.Bodies(protectionRoute); len(got) != 1 || got[0] != `{"protected":true}` {
							return fmt.Errorf("protection calls = %q, want one {\"protected\":true}", got)
						}
						return nil
					},
				),
			},
			{ // The destroy is refused with the API's sentence and the way out; nothing is lifted for the user.
				Config:      attrConfig(fake, "protected", 1, 1024, 10, "disable_api_termination = true"),
				Destroy:     true,
				ExpectError: wrapSafeInOrder("vm_protected", "set disable_api_termination = false and apply first"),
			},
			{
				Config:           attrConfig(fake, "protected", 1, 1024, 10, "disable_api_termination = false"),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check: tfresource.ComposeAggregateTestCheckFunc(
					tfresource.TestCheckResourceAttr(instanceRes, "disable_api_termination", "false"),
					func(s *terraform.State) error {
						if now, _ := stateID(s); now != id {
							return fmt.Errorf("the instance was replaced: id = %s, want %s", now, id)
						}
						if fake.IsProtected(id) {
							return fmt.Errorf("the instance is still protected in the API")
						}
						if n := fake.CreateCount(); n != 1 {
							return fmt.Errorf("creates = %d, want 1", n)
						}
						if got := fake.Bodies(protectionRoute); len(got) != 2 || got[1] != `{"protected":false}` {
							return fmt.Errorf("protection calls = %q, want the second to be {\"protected\":false}", got)
						}
						return nil
					},
				),
			},
		},
	})
}

// Unset it is the API's value, false; it switches on in place; a change made outside Terraform is read and put back.
func TestAccInstanceResource_DisableApiTerminationDefaultsToFalseAndChangesInPlace(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	var id string
	on := attrConfig(fake, "toggle", 1, 1024, 10, "disable_api_termination = true")

	tfresource.Test(t, tfresource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []tfresource.TestStep{
			{
				Config: attrConfig(fake, "toggle", 1, 1024, 10, ""),
				Check: tfresource.ComposeAggregateTestCheckFunc(
					vmIDFromState(&id),
					tfresource.TestCheckResourceAttr(instanceRes, "disable_api_termination", "false"),
					func(*terraform.State) error {
						if n := fake.CallCount(protectionRoute); n != 0 {
							return fmt.Errorf("protection calls = %d, want 0 for an instance that asked for none", n)
						}
						return nil
					},
				),
			},
			{
				Config:           on,
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check: func(*terraform.State) error {
					if !fake.IsProtected(id) || fake.CreateCount() != 1 {
						return fmt.Errorf("protected = %v, creates = %d, want protected in place", fake.IsProtected(id), fake.CreateCount())
					}
					return nil
				},
			},
			{ // Lifted outside Terraform: the refresh reads it, and the apply puts it back.
				PreConfig:        func() { fake.Protect(id, false) },
				Config:           on,
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check: func(*terraform.State) error {
					if !fake.IsProtected(id) {
						return fmt.Errorf("the protection was not set again")
					}
					return nil
				},
			},
			{Config: attrConfig(fake, "toggle", 1, 1024, 10, "disable_api_termination = false")},
		},
	})
}

// When the protection cannot be set after the create (the key may not change it), the instance is built and in the
// state, unprotected, so a destroy or the next apply finds it; the error says what failed.
func TestAccInstanceResource_AFailedProtectionKeepsTheInstanceInState(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)

	tfresource.Test(t, tfresource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []tfresource.TestStep{
			{
				PreConfig: func() {
					fake.FailNext(1, matchRoute(http.MethodPut, "/protection"), func(rc *reqCtx) {
						rc.problem(http.StatusForbidden, "owner_required", "Forbidden", "only an owner may change the protection")
					})
				},
				Config:      attrConfig(fake, "half", 1, 1024, 10, "disable_api_termination = true"),
				ExpectError: wrapSafeInOrder("owner_required", "built and is running", "not protected"),
			},
			{ // The tainted instance is replaced and the second try protects it.
				Config: attrConfig(fake, "half", 1, 1024, 10, "disable_api_termination = true"),
				Check:  tfresource.TestCheckResourceAttr(instanceRes, "disable_api_termination", "true"),
			},
			{Config: attrConfig(fake, "half", 1, 1024, 10, "disable_api_termination = false")},
		},
	})
}

// --- resize in place -----------------------------------------------------------------------------------------------

// A change of vcpus or memory_mb is one keyed resize of the same instance, never a replacement, and apply waits until
// the instance reports the new size. disk_gb still replaces it. A hostname changed in the same apply goes through too.
func TestAccInstanceResource_VcpusAndMemoryChangeInPlace(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	var id string
	sameInstance := func(vcpus, memoryMB, resizes int) tfresource.TestCheckFunc {
		return func(s *terraform.State) error {
			now, _ := stateID(s)
			if now != id {
				return fmt.Errorf("the instance was replaced: id = %s, want %s", now, id)
			}
			if v, m := fake.VmShape(id); v != vcpus || m != memoryMB {
				return fmt.Errorf("the API reports %d vcpus and %d MB, want %d and %d", v, m, vcpus, memoryMB)
			}
			if n := len(fake.Bodies(resizeRoute)); n != resizes || fake.CreateCount() != 1 {
				return fmt.Errorf("resizes = %d (want %d), creates = %d (want 1)", n, resizes, fake.CreateCount())
			}
			return nil
		}
	}

	tfresource.Test(t, tfresource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []tfresource.TestStep{
			{Config: attrConfig(fake, "grow", 1, 1024, 10, ""), Check: vmIDFromState(&id)},
			{
				Config:           attrConfig(fake, "grow", 2, 2048, 10, ""),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check: tfresource.ComposeAggregateTestCheckFunc(
					sameInstance(2, 2048, 1),
					tfresource.TestCheckResourceAttr(instanceRes, "vcpus", "2"),
					tfresource.TestCheckResourceAttr(instanceRes, "memory_mb", "2048"),
					tfresource.TestCheckResourceAttr(instanceRes, "state", "running"),
					func(*terraform.State) error {
						if got := fake.Bodies(resizeRoute); got[0] != `{"vcpus":2,"memory_mb":2048}` {
							return fmt.Errorf("resize body = %s, want {\"vcpus\":2,\"memory_mb\":2048}", got[0])
						}
						return nil
					},
				),
			},
			{ // Memory alone.
				Config:           attrConfig(fake, "grow", 2, 4096, 10, ""),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check:            sameInstance(2, 4096, 2),
			},
			{ // A shrink, together with a hostname.
				Config:           attrConfig(fake, "grow", 1, 1024, 10, `hostname = "grow.example.com"`),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check: tfresource.ComposeAggregateTestCheckFunc(
					sameInstance(1, 1024, 3),
					tfresource.TestCheckResourceAttr(instanceRes, "hostname", "grow.example.com"),
				),
			},
			{ // The disk cannot be resized: a bigger disk is a new instance, and no resize is asked for.
				Config:           attrConfig(fake, "grow", 1, 1024, 20, `hostname = "grow.example.com"`),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionDestroyBeforeCreate),
				Check: func(*terraform.State) error {
					if n := len(fake.Bodies(resizeRoute)); n != 3 {
						return fmt.Errorf("resizes = %d, want still 3: a disk change replaces", n)
					}
					return nil
				},
			},
		},
	})
}

// A stopped instance is resized stopped and stays stopped.
func TestAccInstanceResource_AStoppedInstanceIsResizedAndStaysStopped(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	var id string

	tfresource.Test(t, tfresource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []tfresource.TestStep{
			{Config: attrConfig(fake, "napping", 1, 1024, 10, ""), Check: vmIDFromState(&id)},
			{
				PreConfig:        func() { fake.SetState(id, "stopped") },
				Config:           attrConfig(fake, "napping", 2, 1024, 10, ""),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check: tfresource.ComposeAggregateTestCheckFunc(
					tfresource.TestCheckResourceAttr(instanceRes, "state", "stopped"),
					tfresource.TestCheckResourceAttr(instanceRes, "vcpus", "2"),
				),
			},
		},
	})
}

// A resize the platform could not complete is reverted: the instance keeps its size, and apply fails with the
// operation's class and detail. The next apply, when it works, resizes.
func TestAccInstanceResource_AFailedResizeIsAnErrorAndKeepsTheSize(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	var id string

	tfresource.Test(t, tfresource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []tfresource.TestStep{
			{Config: attrConfig(fake, "revert", 1, 1024, 10, ""), Check: vmIDFromState(&id)},
			{
				PreConfig:   func() { fake.FailResizes("the compute service failed to resize this VM") },
				Config:      attrConfig(fake, "revert", 2, 1024, 10, ""),
				ExpectError: wrapSafeInOrder("resize_failed", "the compute service failed to resize this VM"),
			},
			{
				PreConfig: func() {
					if v, _ := fake.VmShape(id); v != 1 {
						t.Errorf("after the failed resize the instance has %d vcpus, want its old 1", v)
					}
					fake.FailResizes("")
				},
				Config: attrConfig(fake, "revert", 2, 1024, 10, ""),
				Check:  tfresource.TestCheckResourceAttr(instanceRes, "vcpus", "2"),
			},
		},
	})
}

// Where resizing is switched off for the organization the API answers 503 not_ready: the error says so, the request
// is sent once, and the instance is left as it was.
func TestAccInstanceResource_ResizeSwitchedOffIsShownAndNotRetried(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)

	tfresource.Test(t, tfresource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []tfresource.TestStep{
			{Config: attrConfig(fake, "off", 1, 1024, 10, "")},
			{
				PreConfig:   func() { fake.SetResizeOff(true) },
				Config:      attrConfig(fake, "off", 2, 1024, 10, ""),
				ExpectError: wrapSafe("not_ready"),
			},
			{PreConfig: func() { fake.SetResizeOff(false) }, Config: attrConfig(fake, "off", 1, 1024, 10, "")},
		},
	})
	if n := fake.CallCount(resizeRoute); n != 1 {
		t.Errorf("resize requests = %d, want 1: a not_ready is never retried", n)
	}
}

// --- computed values that stay known ---------------------------------------------------------------------------------

// An in-place hostname change plans image_name and private_ipv4 as the values they have, not as known after apply.
func TestAccInstanceResource_AnInPlaceChangeKeepsImageNameAndPrivateIPKnown(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)

	tfresource.Test(t, tfresource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckInstanceDestroy(fake),
		Steps: []tfresource.TestStep{
			{Config: attrConfig(fake, "known", 1, 1024, 10, `hostname = "a.example.com"`)},
			{
				Config: attrConfig(fake, "known", 1, 1024, 10, `hostname = "b.example.com"`),
				ConfigPlanChecks: tfresource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(instanceRes, plancheck.ResourceActionUpdate),
					plancheck.ExpectKnownValue(instanceRes, tfjsonpath.New("image_name"), knownvalue.StringExact("image-1")),
					plancheck.ExpectKnownValue(instanceRes, tfjsonpath.New("private_ipv4"), knownvalue.StringExact("10.128.3.11")),
				}},
			},
		},
	})
}

// --- the fake itself -----------------------------------------------------------------------------------------------

// The fake's resize, protection and operation routes follow the API: refusals in its order, 202 with an operation that
// finishes, 200 with the instance for the protection.
func TestFakeAPI_ResizeAndProtectionRoutes(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)
	id := createVmVia(t, f, "good-key", testValidVmJS).get("vm")["id"].(string)
	runVm(t, f, "good-key", id)
	base := vmsPath(testOrgID) + "/" + id

	for _, tc := range []struct {
		name, body, code string
		status           int
	}{
		{"the size it has", `{"vcpus":1,"memory_mb":1024}`, "no_change", http.StatusUnprocessableEntity},
		{"a vcpus count off the grid", `{"vcpus":3,"memory_mb":1024}`, "invalid_spec", http.StatusUnprocessableEntity},
		{"memory that is not whole gigabytes", `{"vcpus":2,"memory_mb":1500}`, "invalid_spec", http.StatusUnprocessableEntity},
		{"a missing size", `{"vcpus":2}`, "invalid_request", http.StatusBadRequest},
	} {
		status, _, resp := fakeHTTP(t, f, "good-key", http.MethodPost, base+"/resize", tc.body)
		if p := decodeObject(t, resp); status != tc.status || p["code"] != tc.code {
			t.Errorf("%s: %d %v, want %d %s", tc.name, status, p["code"], tc.status, tc.code)
		}
	}

	status, _, resp := fakeHTTP(t, f, "good-key", http.MethodPost, base+"/resize", `{"vcpus":2,"memory_mb":2048}`)
	if status != http.StatusAccepted {
		t.Fatalf("resize: %d (%s)", status, resp)
	}
	op := jsonObject(decodeObject(t, resp)).get("operation")
	if status, _, resp := fakeHTTP(t, f, "good-key", http.MethodPost, base+"/resize", `{"vcpus":4,"memory_mb":2048}`); status != http.StatusConflict {
		t.Errorf("a second resize while one runs: %d (%s), want 409 operation_in_flight", status, resp)
	}
	for i := 0; i < 2; i++ {
		getVmVia(t, f, "good-key", id)
	}
	_, vm := getVmVia(t, f, "good-key", id)
	if vm["vcpus"] != float64(2) || vm["memory_mb"] != float64(2048) || vm["state"] != "running" {
		t.Errorf("after the resize: %v vcpus, %v MB, state %v, want 2, 2048, running", vm["vcpus"], vm["memory_mb"], vm["state"])
	}
	status, _, resp = fakeHTTP(t, f, "good-key", http.MethodGet, "/v1/orgs/"+testOrgID+"/operations/"+op["id"].(string), "")
	if got := decodeObject(t, resp); status != http.StatusOK || got["state"] != "succeeded" || got["finished_at"] == nil {
		t.Errorf("the operation: %d %v, want 200 succeeded and finished", status, got)
	}

	status, _, resp = fakeHTTP(t, f, "good-key", http.MethodPut, base+"/protection", `{"protected":true}`)
	if got := decodeObject(t, resp); status != http.StatusOK || got["protected"] != true {
		t.Errorf("protection on: %d %v, want 200 and protected", status, got["protected"])
	}
	if status, p := deleteVmVia(t, f, "good-key", id); status != http.StatusConflict || p["code"] != "vm_protected" {
		t.Errorf("delete of a protected instance: %d %v, want 409 vm_protected", status, p["code"])
	}
	if status, _, _ := fakeHTTP(t, f, "good-key", http.MethodPut, base+"/protection", `{}`); status != http.StatusBadRequest {
		t.Errorf("protection without the flag: %d, want 400", status)
	}
}

// user_data is accepted as standard base64 (its presence is reported, never the value), and an internal organization
// may ask for infra.
func TestFakeAPI_UserDataAndInfraWorkload(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)
	body := func(extra string) string {
		return `{"name":"web-01","image":"i","vcpus":1,"memory_mb":1024,"disk_gb":10,"storage_pool":"p","region":"gro",` + extra + `}`
	}
	vm := createVmVia(t, f, "good-key", body(`"user_data":"I2Nsb3VkLWNvbmZpZw=="`)).get("vm")
	if vm["user_data_present"] != true {
		t.Errorf("user_data_present = %v, want true", vm["user_data_present"])
	}
	if got, _ := f.UserDataSent(vm["id"].(string)); got != "I2Nsb3VkLWNvbmZpZw==" {
		t.Errorf("user_data kept = %q", got)
	}
	if _, has := vm["user_data"]; has {
		t.Error("the answer holds user_data: the API never returns it")
	}

	f.SetUserDataOff(true)
	if status, _, _ := fakeHTTP(t, f, "good-key", http.MethodPost, vmsPath(testOrgID), strings.Replace(body(`"user_data":"I2Nsb3VkLWNvbmZpZw=="`), "web-01", "web-02", 1)); status != http.StatusServiceUnavailable {
		t.Errorf("user_data while it is off: %d, want 503", status)
	}

	f.SetInternalOrg(true)
	vm = createVmVia(t, f, "good-key", strings.Replace(body(`"workload_type":"infra"`), "web-01", "web-03", 1)).get("vm")
	if vm["workload_type"] != "infra" {
		t.Errorf("workload_type = %v, want infra for an internal organization", vm["workload_type"])
	}
}
