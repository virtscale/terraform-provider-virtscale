package provider

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// Acceptance tests of virtscale_s3_access_key against the fake (TF_ACC=1). The secret is in the answer that mints the
// key and nowhere else: the resource keeps it in the state, marked sensitive, and cannot get it back on import.

const canarySecret = "canary-s3-secret-0001"

func accessKeyConfig(fake *fakeAPI, key string) string {
	return testAccProviderBlockWith(fake, key, testOrgID) + "resource \"virtscale_s3_access_key\" \"test\" {}\n"
}

func checkAccessKeysRevoked(fake *fakeAPI) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if n := fake.ActiveAccessKeys(); n != 0 {
			return fmt.Errorf("%d access keys are still not revoked", n)
		}
		return nil
	}
}

func TestAccS3AccessKeyResource_Lifecycle(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.AddKey("storage", "storage:write")
	fake.SetNextSecret(canarySecret)
	var id string
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             checkAccessKeysRevoked(fake),
		Steps: []resource.TestStep{
			{
				Config: accessKeyConfig(fake, "storage"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("virtscale_s3_access_key.test", "secret_access_key", canarySecret),
					resource.TestCheckResourceAttr("virtscale_s3_access_key.test", "state", "active"),
					resource.TestCheckResourceAttrSet("virtscale_s3_access_key.test", "access_key_id"),
					resource.TestCheckResourceAttrSet("virtscale_s3_access_key.test", "created_at"),
					func(s *terraform.State) error {
						id = s.RootModule().Resources["virtscale_s3_access_key.test"].Primary.ID
						if keys := fake.ProjectIdempotencyKeys(); len(keys) != 1 {
							return fmt.Errorf("the create sent %d Idempotency-Keys, want 1", len(keys))
						}
						return nil
					},
				),
			},
			{ // The secret stays in the state; a refresh does not lose it.
				Config:   accessKeyConfig(fake, "storage"),
				PlanOnly: true,
			},
			{ // Import fills what the API still knows; the secret is not among it.
				ResourceName:            "virtscale_s3_access_key.test",
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"secret_access_key"},
			},
			{ // Without the resource the key is revoked, and the destroy waited until it was.
				Config: testAccProviderBlockWith(fake, "storage", testOrgID),
				Check: func(*terraform.State) error {
					if k, ok := fake.AccessKey(id); !ok || k.State != "revoked" {
						return fmt.Errorf("the key is %+v, want revoked", k)
					}
					return nil
				},
			},
		},
	})
}

// A create that is repeated after a lost answer meets the stored replay, which has no secret: the key exists, so it
// is kept in the state (to be revoked) and the error says what happened.
func TestAccS3AccessKeyResource_ALostAnswerLeavesNoSecretAndSaysSo(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.AddKey("storage", "storage:write")
	fake.FailNextAfterHandling(1, matchRoute("POST", "/s3/access-keys"), respondGateway(502))
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             checkAccessKeysRevoked(fake),
		Steps: []resource.TestStep{{
			Config:      accessKeyConfig(fake, "storage"),
			ExpectError: wrapSafeInOrder("Access Key Secret Not Returned", "was created but its secret was not returned", "destroy"),
		}},
	})
}

func TestAccS3AccessKeyResource_RevokedOutOfBandIsRecreated(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.AddKey("storage", "storage:write")
	var id string
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             checkAccessKeysRevoked(fake),
		Steps: []resource.TestStep{
			{Config: accessKeyConfig(fake, "storage"), Check: func(s *terraform.State) error {
				id = s.RootModule().Resources["virtscale_s3_access_key.test"].Primary.ID
				return nil
			}},
			{
				PreConfig: func() { fake.ForceRevokeAccessKey(id) },
				Config:    accessKeyConfig(fake, "storage"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction("virtscale_s3_access_key.test", plancheck.ResourceActionCreate),
				}},
			},
		},
	})
}

func TestAccS3AccessKeyResource_AKeyWithoutTheStorageScopeIsToldSo(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{{
			Config:      accessKeyConfig(fake, "test-api-key"),
			ExpectError: wrapSafeInOrder("403 permission_denied", "'storage.access_key.create' permission", "needs the 'storage:write' scope"),
		}},
	})
}
