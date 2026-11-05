package provider

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// Acceptance tests of virtscale_s3_bucket against the fake (TF_ACC=1). A bucket is created, resized and deleted
// asynchronously; deleting is a soft delete with a retention window, so a destroyed bucket stays on the API as
// deleting_soft.

func bucketConfig(fake *fakeAPI, extra string) string {
	return testAccProviderBlock(fake) + `
resource "virtscale_s3_bucket" "test" {
  name = "tf-bucket-a"
` + extra + `}
`
}

func checkBucketsSoftDeleted(fake *fakeAPI) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "virtscale_s3_bucket" {
				continue
			}
			if b, ok := fake.Bucket(rs.Primary.ID); ok && b.State != "deleting_soft" {
				return fmt.Errorf("bucket %s is %s, want deleting_soft", rs.Primary.ID, b.State)
			}
		}
		return nil
	}
}

func TestAccS3BucketResource_Lifecycle(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.AddKey("storage", "storage:write")
	var id string
	provider := func(extra string) string {
		return testAccProviderBlockWith(fake, "storage", testOrgID) + `
resource "virtscale_s3_bucket" "test" {
  name = "tf-bucket-a"
` + extra + `}
`
	}
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             checkBucketsSoftDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: provider("  versioning = true\n  max_size_bytes = 1073741824\n  max_objects = 1000\n"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("virtscale_s3_bucket.test", "name", "tf-bucket-a"),
					resource.TestCheckResourceAttr("virtscale_s3_bucket.test", "versioning", "true"),
					resource.TestCheckResourceAttr("virtscale_s3_bucket.test", "max_size_bytes", "1073741824"),
					resource.TestCheckResourceAttr("virtscale_s3_bucket.test", "max_objects", "1000"),
					resource.TestCheckResourceAttr("virtscale_s3_bucket.test", "state", "active"),
					resource.TestCheckResourceAttrSet("virtscale_s3_bucket.test", "created_at"),
					func(s *terraform.State) error {
						id = s.RootModule().Resources["virtscale_s3_bucket.test"].Primary.ID
						b, ok := fake.Bucket(id)
						if !ok || b.Quota.MaxObjects == nil || *b.Quota.MaxObjects != 1000 {
							return fmt.Errorf("the API holds %+v", b)
						}
						if keys := fake.ProjectIdempotencyKeys(); len(keys) != 2 {
							return fmt.Errorf("the create and the quota sent %d Idempotency-Keys, want 2", len(keys))
						}
						return nil
					},
				),
			},
			{Config: provider("  versioning = true\n  max_size_bytes = 1073741824\n  max_objects = 1000\n"), PlanOnly: true},
			{ // The quota changes in place; a dimension that is removed from the configuration is cleared with a null.
				Config: provider("  versioning = true\n  max_size_bytes = 2147483648\n"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction("virtscale_s3_bucket.test", plancheck.ResourceActionUpdate),
				}},
				Check: func(*terraform.State) error {
					b, _ := fake.Bucket(id)
					if b.Quota.MaxSizeBytes == nil || *b.Quota.MaxSizeBytes != 2147483648 || b.Quota.MaxObjects != nil {
						return fmt.Errorf("the API holds %+v", b.Quota)
					}
					return nil
				},
			},
			{ // Versioning cannot be changed after creation: a new bucket.
				Config: provider("  versioning = false\n  max_size_bytes = 2147483648\n"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction("virtscale_s3_bucket.test", plancheck.ResourceActionReplace),
				}},
				Check: resource.TestCheckResourceAttr("virtscale_s3_bucket.test", "versioning", "false"),
			},
			{
				ResourceName:            "virtscale_s3_bucket.test",
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"force_destroy"},
			},
		},
	})
}

func TestAccS3BucketResource_ANonEmptyBucketNeedsForceDestroy(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.AddKey("storage", "storage:write")
	config := func(force string) string {
		return testAccProviderBlockWith(fake, "storage", testOrgID) + `
resource "virtscale_s3_bucket" "test" {
  name          = "tf-bucket-b"
  force_destroy = ` + force + `
}
`
	}
	var id string
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             checkBucketsSoftDeleted(fake),
		Steps: []resource.TestStep{
			{Config: config("false"), Check: func(s *terraform.State) error {
				id = s.RootModule().Resources["virtscale_s3_bucket.test"].Primary.ID
				fake.MakeBucketNonEmpty(id, true)
				return nil
			}},
			{Config: config("false"), Destroy: true, ExpectError: wrapSafeInOrder("409 bucket_not_empty", "force=true")},
			{ // force_destroy is the provider's own: changing it calls no API.
				Config: config("true"),
				Check: func(*terraform.State) error {
					if len(fake.BucketDeletes()) != 1 {
						return fmt.Errorf("deletes = %q", fake.BucketDeletes())
					}
					return nil
				},
			},
			{Config: config("true"), Destroy: true},
			{Config: testAccProviderBlockWith(fake, "storage", testOrgID), Check: func(*terraform.State) error {
				if d := fake.BucketDeletes(); len(d) != 2 || d[0] != "tf-bucket-b force=false" || d[1] != "tf-bucket-b force=true" {
					return fmt.Errorf("deletes = %q, want one without and one with force", d)
				}
				return nil
			}},
		},
	})
}

func TestAccS3BucketResource_NamesTheAPIWouldRefuseAreRefusedAtPlanTime(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	for name, want := range map[string]string{
		"UPPER":       "lowercase letters",
		"ab":          "3 to 63",
		"has--double": "consecutive hyphens",
		"10.0.0.1":    "lowercase letters",
		"-lead":       "start and end",
	} {
		t.Run(name, func(t *testing.T) {
			resource.Test(t, resource.TestCase{
				PreCheck:                 func() { testAccPreCheck(t) },
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
				Steps: []resource.TestStep{{
					Config:      testAccProviderBlock(fake) + fmt.Sprintf("resource \"virtscale_s3_bucket\" \"x\" {\n  name = %q\n}\n", name),
					PlanOnly:    true,
					ExpectError: wrapSafe(want),
				}},
			})
		})
	}
}

func TestAccS3BucketResource_AProvisioningFailureIsReportedAndTheBucketCanBeDestroyed(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.AddKey("storage", "storage:write")
	fake.FailBucketBuilds()
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             checkBucketsSoftDeleted(fake),
		Steps: []resource.TestStep{{
			Config:      testAccProviderBlockWith(fake, "storage", testOrgID) + "resource \"virtscale_s3_bucket\" \"x\" {\n  name = \"tf-bucket-c\"\n}\n",
			ExpectError: wrapSafeInOrder("Bucket Failed to Build", "provision_failed"),
		}},
	})
}

func TestAccS3BucketResource_RefusalsCarryTheAPIsCode(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.AddKey("storage", "storage:write")
	fake.SetMaxBuckets(1)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             checkBucketsSoftDeleted(fake),
		Steps: []resource.TestStep{{
			Config: testAccProviderBlockWith(fake, "storage", testOrgID) + `
resource "virtscale_s3_bucket" "one" { name = "tf-bucket-d" }
resource "virtscale_s3_bucket" "two" {
  name       = "tf-bucket-e"
  depends_on = [virtscale_s3_bucket.one]
}
`,
			ExpectError: wrapSafeInOrder("422 bucket_limit_reached", "already has 1 of its 1 buckets"),
		}},
	})
}

func TestAccS3BucketResource_AKeyWithoutTheStorageScopeIsToldSo(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID) // compute:write and network:write
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{{
			Config:      bucketConfig(fake, ""),
			ExpectError: wrapSafeInOrder("403 permission_denied", "'storage.bucket.create' permission", "needs the 'storage:write' scope"),
		}},
	})
}

func TestAccS3BucketResource_GoneOrSoftDeletedOutOfBandIsRecreated(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.AddKey("storage", "storage:write")
	config := testAccProviderBlockWith(fake, "storage", testOrgID) + "resource \"virtscale_s3_bucket\" \"x\" {\n  name = \"tf-bucket-f\"\n}\n"
	var id string
	for name, gone := range map[string]func(){
		"vanished":     func() { fake.VanishBucket(id) },
		"soft-deleted": func() { fake.ForceSoftDeleteBucket(id) },
	} {
		t.Run(name, func(t *testing.T) {
			resource.Test(t, resource.TestCase{
				PreCheck:                 func() { testAccPreCheck(t) },
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
				Steps: []resource.TestStep{
					{Config: config, Check: func(s *terraform.State) error {
						id = s.RootModule().Resources["virtscale_s3_bucket.x"].Primary.ID
						return nil
					}},
					{
						PreConfig: gone,
						Config:    config,
						ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
							plancheck.ExpectResourceAction("virtscale_s3_bucket.x", plancheck.ResourceActionCreate),
						}},
					},
				},
			})
		})
	}
}

// A 404 about the organization is not a 404 about the bucket: it stays in state, and the error is shown.
func TestAccS3BucketResource_WrongOrganizationKeepsTheResourceInState(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.AddKey("storage", "storage:write")
	config := func(org string) string {
		return testAccProviderBlockWith(fake, "storage", org) + "resource \"virtscale_s3_bucket\" \"x\" {\n  name = \"tf-bucket-g\"\n}\n"
	}
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             checkBucketsSoftDeleted(fake),
		Steps: []resource.TestStep{
			{Config: config(testOrgID)},
			{Config: config(testOtherOrg), PlanOnly: true, ExpectError: wrapSafeInOrder("no organization matching", "check VIRTSCALE_ORGANIZATION")},
			{Config: config(testOrgID), PlanOnly: true},
		},
	})
}
