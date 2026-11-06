package provider

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// Tests for the claims of the S3 resources that the first verifier of !12 found untested or wrong: what a failed create
// leaves behind, which ceilings a PUT carries, the API's codes, a change that meets another operation, a revoke that fails.

func s3Fake(t *testing.T) (*fakeAPI, func(string) string) {
	t.Helper()
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.AddKey("storage", "storage:write")
	return fake, func(body string) string { return testAccProviderBlockWith(fake, "storage", testOrgID) + body }
}

func bucketBlock(name, extra string) string {
	return fmt.Sprintf("\nresource \"virtscale_s3_bucket\" \"b\" {\n  name = %q\n%s}\n", name, extra)
}

func replaced(addr string) resource.ConfigPlanChecks {
	return resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(addr, plancheck.ResourceActionDestroyBeforeCreate)}}
}

// A ceiling the API refuses leaves a bucket that exists: the state says so, with the ceilings the API holds (none), the
// error says how to go on, and a replacement (what a tainted resource gets) meets the soft-deleted bucket's name.
func TestAccS3BucketResource_ARefusedCeilingLeavesTheBucketInStateWithoutCeilings(t *testing.T) {
	fake, cfg := s3Fake(t)
	fake.FailNext(1, matchRoute(http.MethodPut, "/quota"), func(rc *reqCtx) {
		rc.unprocessable("bucket_quota_exceeds_plan", "that quota is above your plan's ceiling")
	})
	config := cfg(bucketBlock("tf-bucket-q", "  max_objects = 10\n"))
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{Config: config, ExpectError: wrapSafeInOrder("422 bucket_quota_exceeds_plan", "without ceilings", "tofu untaint")},
			{ // The bucket is in the state (tainted): a plan replaces it, and the replacement meets the retained name.
				Config:           config,
				ConfigPlanChecks: replaced("virtscale_s3_bucket.b"),
				ExpectError:      wrapSafeInOrder("409 bucket_already_exists"),
			},
		},
	})
}

func TestAccS3BucketResource_AFailedBuildLeavesTheBucketInState(t *testing.T) {
	fake, cfg := s3Fake(t)
	fake.FailBucketBuilds()
	config := cfg(bucketBlock("tf-bucket-r", ""))
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{Config: config, ExpectError: wrapSafeInOrder("Bucket Failed to Build", "provision_failed", "another name")},
			{Config: config, ConfigPlanChecks: replaced("virtscale_s3_bucket.b"), ExpectError: wrapSafe("409 bucket_already_exists")},
		},
	})
}

// Only the ceilings that changed are sent; a change of force_destroy alone sends nothing.
func TestAccS3BucketResource_OnlyTheCeilingsThatChangedAreSent(t *testing.T) {
	fake, cfg := s3Fake(t)
	puts := func(want ...string) resource.TestCheckFunc {
		return func(*terraform.State) error {
			got := fake.QuotaPuts()
			if len(got) != len(want) {
				return fmt.Errorf("quota PUTs = %q, want %q", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					return fmt.Errorf("quota PUT %d = %s, want %s", i, got[i], want[i])
				}
			}
			return nil
		}
	}
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{Config: cfg(bucketBlock("tf-bucket-s", "  max_objects = 10\n  max_size_bytes = 1000\n")),
				Check: puts(`{"max_objects":10,"max_size_bytes":1000}`)},
			{Config: cfg(bucketBlock("tf-bucket-s", "  max_objects = 20\n  max_size_bytes = 1000\n")),
				Check: puts(`{"max_objects":10,"max_size_bytes":1000}`, `{"max_objects":20}`)},
			{Config: cfg(bucketBlock("tf-bucket-s", "  max_objects = 20\n  max_size_bytes = 2000\n")),
				Check: puts(`{"max_objects":10,"max_size_bytes":1000}`, `{"max_objects":20}`, `{"max_size_bytes":2000}`)},
			{Config: cfg(bucketBlock("tf-bucket-s", "  max_objects = 20\n  max_size_bytes = 2000\n  force_destroy = true\n")), // nothing for the API
				Check: puts(`{"max_objects":10,"max_size_bytes":1000}`, `{"max_objects":20}`, `{"max_size_bytes":2000}`)},
			{Config: cfg(bucketBlock("tf-bucket-s", "  max_size_bytes = 2000\n  force_destroy = true\n")), // a ceiling removed is cleared
				Check: puts(`{"max_objects":10,"max_size_bytes":1000}`, `{"max_objects":20}`, `{"max_size_bytes":2000}`, `{"max_objects":null}`)},
		},
	})
}

func TestAccS3BucketResource_ARenameReplacesAndAVersioningChangeWithItIsAllowed(t *testing.T) {
	fake, cfg := s3Fake(t)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{Config: cfg(bucketBlock("tf-bucket-t", ""))},
			{Config: cfg(bucketBlock("tf-bucket-u", "")), ConfigPlanChecks: replaced("virtscale_s3_bucket.b")},
			// a new name is a new bucket, which can have any versioning
			{Config: cfg(bucketBlock("tf-bucket-v", "  versioning = true\n")), ConfigPlanChecks: replaced("virtscale_s3_bucket.b"),
				Check: resource.TestCheckResourceAttr("virtscale_s3_bucket.b", "versioning", "true")},
		},
	})
	if got := len(fake.BucketDeletes()); got < 2 {
		t.Errorf("the replacements soft-deleted %d buckets, want 2 (and the destroy a third)", got)
	}
}

func TestAccS3BucketResource_NamesAndCeilingsTheAPIRefusesAreRefusedAtPlanTime(t *testing.T) {
	fake, cfg := s3Fake(t)
	cases := map[string]struct{ config, want string }{
		"63 characters pass, 64 do not": {cfg(bucketBlock(strings.Repeat("a", 64), "")), "between 3 and 63 characters"},
		"a ceiling of zero":             {cfg(bucketBlock("tf-bucket-w", "  max_objects = 0\n")), "use 1 or more"},
		"a size ceiling below one":      {cfg(bucketBlock("tf-bucket-w", "  max_size_bytes = -5\n")), "use 1 or more"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			resource.Test(t, resource.TestCase{
				PreCheck:                 func() { testAccPreCheck(t) },
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
				Steps:                    []resource.TestStep{{Config: tc.config, PlanOnly: true, ExpectError: wrapSafe(tc.want)}},
			})
		})
	}
	// 63 characters are fine
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps:                    []resource.TestStep{{Config: cfg(bucketBlock(strings.Repeat("a", 63), ""))}},
	})
	_ = fake
}

// The API allows one operation per bucket: a ceiling set right after the bucket turned active, or a delete right after a
// ceiling, can meet 409 operation_in_flight, and is repeated.
func TestAccS3BucketResource_AChangeIsRepeatedWhileAnotherOperationRuns(t *testing.T) {
	fake, cfg := s3Fake(t)
	busy := func(rc *reqCtx) {
		rc.conflict("operation_in_flight", "another operation is already running for this bucket")
	}
	fake.FailNext(3, matchRoute(http.MethodPut, "/quota"), busy)
	fake.FailNext(2, matchRoute(http.MethodDelete, "/s3/buckets/"), busy)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             checkBucketsSoftDeleted(fake),
		Steps: []resource.TestStep{
			{Config: cfg(bucketBlock("tf-bucket-x", "  max_objects = 5\n")), Check: resource.TestCheckResourceAttr("virtscale_s3_bucket.b", "max_objects", "5")},
		},
	})
}

// A ceiling the API refuses on an update is reported and leaves the state as it was.
func TestAccS3BucketResource_AFailedCeilingUpdateIsReported(t *testing.T) {
	fake, cfg := s3Fake(t)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             checkBucketsSoftDeleted(fake),
		Steps: []resource.TestStep{
			{Config: cfg(bucketBlock("tf-bucket-y", "  max_objects = 5\n"))},
			{
				PreConfig: func() {
					fake.FailNext(1, matchRoute(http.MethodPut, "/quota"), func(rc *reqCtx) {
						rc.unprocessable("bucket_quota_exceeds_plan", "that quota is above your plan's ceiling")
					})
				},
				Config:      cfg(bucketBlock("tf-bucket-y", "  max_objects = 6\n")),
				ExpectError: wrapSafeInOrder("Unable to Set the Bucket's Ceilings", "422 bucket_quota_exceeds_plan"),
			},
			{Config: cfg(bucketBlock("tf-bucket-y", "  max_objects = 5\n")), PlanOnly: true}, // still what the API holds
		},
	})
}

// An imported bucket has no force_destroy, which the API does not store: it is false.
func TestAccS3BucketResource_AnImportedBucketHasForceDestroyFalse(t *testing.T) {
	_, cfg := s3Fake(t)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{Config: cfg(bucketBlock("tf-bucket-z", "  force_destroy = true\n"))},
			{
				ResourceName: "virtscale_s3_bucket.b",
				ImportState:  true,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 || states[0].Attributes["force_destroy"] != "false" {
						return fmt.Errorf("the imported state holds force_destroy %q, want false", states[0].Attributes["force_destroy"])
					}
					return nil
				},
			},
		},
	})
}

// --- access keys ---------------------------------------------------------------------------------------------------------

func TestAccS3AccessKeyResource_ALostAnswerKeepsTheKeyInState(t *testing.T) {
	fake, _ := s3Fake(t)
	fake.FailNextAfterHandling(1, matchRoute("POST", "/s3/access-keys"), respondGateway(502))
	config := accessKeyConfig(fake, "storage")
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             checkAccessKeysRevoked(fake),
		Steps: []resource.TestStep{
			{Config: config, ExpectError: wrapSafe("Access Key Secret Not Returned")},
			// The key is in the state, tainted: the next apply revokes it and mints another. (CheckDestroy alone would not show
			// a key missing from the state: it does not run when the state is empty.)
			{Config: config, ConfigPlanChecks: replaced("virtscale_s3_access_key.test")},
		},
	})
}

func TestAccS3AccessKeyResource_ARevokeThatFailsIsReportedAtOnce(t *testing.T) {
	fake, _ := s3Fake(t)
	config := accessKeyConfig(fake, "storage")
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             checkAccessKeysRevoked(fake),
		Steps: []resource.TestStep{
			{Config: config},
			{
				PreConfig:   func() { fake.FailNextRevoke(fakeUUID(9, 1)) },
				Config:      testAccProviderBlockWith(fake, "storage", testOrgID),
				ExpectError: wrapSafeInOrder("Access Key Not Revoked", "degraded"),
			},
		},
	})
}

func TestAccS3AccessKeyResource_ARefusedRevokeIsReportedAndAKeyThatIsBeingRevokedIsGone(t *testing.T) {
	fake, _ := s3Fake(t)
	config := accessKeyConfig(fake, "storage")
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             checkAccessKeysRevoked(fake),
		Steps: []resource.TestStep{
			{Config: config},
			{
				PreConfig: func() {
					fake.FailNext(1, matchRoute(http.MethodDelete, "/s3/access-keys/"), func(rc *reqCtx) {
						rc.problem(http.StatusForbidden, "permission_denied", "Forbidden", "this credential does not carry the 'storage.access_key.revoke' permission")
					})
				},
				Config:      testAccProviderBlockWith(fake, "storage", testOrgID),
				ExpectError: wrapSafeInOrder("Unable to Revoke Access Key", "403 permission_denied"),
			},
			{ // A key that is being revoked elsewhere is as good as gone: the plan creates another.
				PreConfig:        func() { fake.ForceAccessKeyState(fakeUUID(9, 1), "revoking") },
				Config:           config,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction("virtscale_s3_access_key.test", plancheck.ResourceActionCreate)}},
				Check: func(*terraform.State) error { // the other client's revoke completes
					fake.ForceAccessKeyState(fakeUUID(9, 1), "revoked")
					return nil
				},
			},
		},
	})
}

// An organization holds at most five access keys.
func TestAccS3AccessKeyResource_TheSixthKeyIsRefusedWithTheAPIsCode(t *testing.T) {
	fake, _ := s3Fake(t)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             checkAccessKeysRevoked(fake),
		Steps: []resource.TestStep{{
			Config:      testAccProviderBlockWith(fake, "storage", testOrgID) + "\nresource \"virtscale_s3_access_key\" \"k\" {\n  count = 6\n}\n",
			ExpectError: wrapSafeInOrder("422 access_key_limit_reached", "of its 5 access keys"),
		}},
	})
}
