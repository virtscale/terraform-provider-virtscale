package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// Two kinds of test of the S3 surface. The first group holds the fake to the answers of s3-control-plane (domain/buckets.py,
// domain/access_keys.py of 87607618, the code production runs): the codes, sentences and order of its refusals, and an access
// key that is born active. The second group proves what the resources do with the 409s of a delete or a revoke that comes
// after, or races with, the same act done elsewhere.

// s3HTTP is one request of a client that is not the provider's; the answer is decoded as an object.
func s3HTTP(t *testing.T, fake *fakeAPI, method, path, body string) (int, map[string]any) {
	t.Helper()
	status, _, raw := fakeHTTP(t, fake, "storage", method, "/v1/orgs/"+testOrgID+"/s3/"+path, body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return status, m
}

func wantS3Problem(t *testing.T, what string, status int, m map[string]any, wantStatus int, code, detail string) {
	t.Helper()
	if status != wantStatus || m["code"] != code || m["detail"] != detail {
		t.Errorf("%s: got %d %v %q, want %d %s %q", what, status, m["code"], m["detail"], wantStatus, code, detail)
	}
}

func wantHTTPStatus(t *testing.T, what string, got, want int) {
	t.Helper()
	if got != want {
		t.Errorf("%s: status %d, want %d", what, got, want)
	}
}

// newBucket creates a bucket over HTTP and returns its id (the bucket is still requested).
func newBucket(t *testing.T, fake *fakeAPI, name string) string {
	t.Helper()
	status, m := s3HTTP(t, fake, http.MethodPost, "buckets", fmt.Sprintf(`{"name":%q}`, name))
	if status != http.StatusAccepted {
		t.Fatalf("create %s: status %d: %v", name, status, m)
	}
	return m["bucket"].(map[string]any)["id"].(string)
}

// activate reads the bucket until the build is done.
func activate(t *testing.T, fake *fakeAPI, id string) {
	t.Helper()
	for i := 0; i < 2; i++ {
		s3HTTP(t, fake, http.MethodGet, "buckets/"+id, "")
	}
	if b, _ := fake.Bucket(id); b.State != "active" {
		t.Fatalf("bucket %s is %s, want active", id, b.State)
	}
}

func newAccessKey(t *testing.T, fake *fakeAPI) string {
	t.Helper()
	status, m := s3HTTP(t, fake, http.MethodPost, "access-keys", "")
	if status != http.StatusCreated {
		t.Fatalf("create access key: status %d: %v", status, m)
	}
	return m["access_key"].(map[string]any)["id"].(string)
}

// An access key is minted synchronously: record_access_key inserts the row active, with the key id the object store gave,
// and its operation is born succeeded (and stamped finished).
func TestFakeS3_AnAccessKeyIsActiveAtOnceAndItsOperationIsDone(t *testing.T) {
	fake, _ := s3Fake(t)
	status, m := s3HTTP(t, fake, http.MethodPost, "access-keys", "")
	key, _ := m["access_key"].(map[string]any)
	op, _ := m["operation"].(map[string]any)
	if status != http.StatusCreated || key["state"] != "active" || key["rgw_access_key_id"] == nil || m["secret_access_key"] == nil {
		t.Errorf("created access key: %d %v", status, m)
	}
	if op["state"] != "succeeded" || op["finished_at"] == nil {
		t.Errorf("the create's operation is %v, want succeeded and finished", op)
	}
}

// revoke_access_key: not found, then already revoked, already revoking, not in a state that can be revoked.
func TestFakeS3_ARevokeThatIsRepeatedOrIllTimedIsRefusedLikeTheAPI(t *testing.T) {
	fake, _ := s3Fake(t)
	id := newAccessKey(t, fake)
	path := "access-keys/" + id

	status, m := s3HTTP(t, fake, http.MethodDelete, path, "")
	if got := m["access_key"].(map[string]any)["state"]; status != http.StatusAccepted || got != "revoking" {
		t.Fatalf("the first revoke: %d, key %v", status, got)
	}
	status, m = s3HTTP(t, fake, http.MethodDelete, path, "")
	wantS3Problem(t, "a second revoke", status, m, http.StatusConflict, "access_key_already_revoking", "this access key is already being revoked; poll its operation")

	fake.ForceRevokeAccessKey(id)
	status, m = s3HTTP(t, fake, http.MethodDelete, path, "")
	wantS3Problem(t, "a revoke of a revoked key", status, m, http.StatusConflict, "access_key_already_revoked", "this access key has already been revoked")

	for _, state := range []string{"requested", "provisioning"} {
		fake.ForceAccessKeyState(id, state)
		status, m = s3HTTP(t, fake, http.MethodDelete, path, "")
		wantS3Problem(t, "a revoke of a "+state+" key", status, m, http.StatusConflict, "access_key_not_revocable", "an access key in state '"+state+"' cannot be revoked")
	}
	fake.ForceAccessKeyState(id, "degraded") // where a failed revoke lands: it may be revoked again
	status, _ = s3HTTP(t, fake, http.MethodDelete, path, "")
	wantHTTPStatus(t, "a revoke of a degraded key", status, http.StatusAccepted)
}

// delete_bucket: not found, already deleting_soft, a state that cannot be deleted, then the emptiness check.
func TestFakeS3_ADeleteThatIsRepeatedOrIllTimedIsRefusedLikeTheAPI(t *testing.T) {
	fake, _ := s3Fake(t)
	id := newBucket(t, fake, "tf-fid-del")
	path := "buckets/" + id

	status, m := s3HTTP(t, fake, http.MethodDelete, path, "")
	wantS3Problem(t, "a delete while requested", status, m, http.StatusConflict, "bucket_not_deletable", "a bucket in state 'requested' cannot be deleted")
	s3HTTP(t, fake, http.MethodGet, path, "")
	status, m = s3HTTP(t, fake, http.MethodDelete, path, "")
	wantS3Problem(t, "a delete while provisioning", status, m, http.StatusConflict, "bucket_not_deletable", "a bucket in state 'provisioning' cannot be deleted")

	activate(t, fake, id)
	status, _ = s3HTTP(t, fake, http.MethodDelete, path, "")
	wantHTTPStatus(t, "the delete of an active bucket", status, http.StatusAccepted)
	status, m = s3HTTP(t, fake, http.MethodDelete, path, "")
	wantS3Problem(t, "a second delete", status, m, http.StatusConflict, "bucket_already_deleting", "this bucket is already scheduled for deletion; use restore to cancel")

	fake.ForceBucketState(id, "purging")
	status, m = s3HTTP(t, fake, http.MethodDelete, path, "")
	wantS3Problem(t, "a delete while purging", status, m, http.StatusConflict, "bucket_not_deletable", "a bucket in state 'purging' cannot be deleted")

	for _, state := range []string{"active", "suspended", "provision_failed", "degraded"} {
		fake.ForceBucketState(id, state)
		status, _ = s3HTTP(t, fake, http.MethodDelete, path, "")
		wantHTTPStatus(t, "the delete of a "+state+" bucket", status, http.StatusAccepted)
	}
}

// delete_bucket refuses a non-empty bucket from the last metering sample, and the 409 quotes it: when, how many objects and
// bytes, why a bytes-only sample is not an empty bucket, and what a versioned (or never observed) bucket's count means.
// Sentences as in domain/buckets.py of 87607618 (the same on main, 40707934). The fake has one sample time, 2026-11-01T12:00:00+00:00.
func TestFakeS3_ANonEmptyBucketIsRefusedWithTheAPIsSentence(t *testing.T) {
	fake, _ := s3Fake(t)
	const ack = "; pass ?force=true to acknowledge and soft-delete it anyway -- that erases nothing and the retention window is unchanged"
	measured := func(found string) string {
		return "the last measurement of this bucket (2026-11-01T12:00:00+00:00) found " + found
	}
	refused := func(what, id, want string) {
		t.Helper()
		status, m := s3HTTP(t, fake, http.MethodDelete, "buckets/"+id, "")
		wantS3Problem(t, what, status, m, http.StatusConflict, "bucket_not_empty", want)
	}
	versioned := func(name string) string {
		status, m := s3HTTP(t, fake, http.MethodPost, "buckets", fmt.Sprintf(`{"name":%q,"versioning":true}`, name))
		if status != http.StatusAccepted {
			t.Fatalf("create %s: status %d: %v", name, status, m)
		}
		return m["bucket"].(map[string]any)["id"].(string)
	}

	id := newBucket(t, fake, "tf-fid-ne")
	activate(t, fake, id)
	fake.MakeBucketNonEmpty(id, true)
	refused("a bucket holding objects", id, measured("3 object(s) and 1536 byte(s)")+ack)

	fake.SetBucketSample(id, 0, 512) // incomplete multipart uploads: bytes without objects
	refused("a bucket holding bytes only", id, measured("0 object(s) and 512 byte(s) -- incomplete multipart uploads occupy storage without appearing as objects")+ack)

	vid := versioned("tf-fid-ver")
	activate(t, fake, vid)
	fake.MakeBucketNonEmpty(vid, true)
	refused("a versioned bucket", vid, measured("3 object(s) and 1536 byte(s) -- this bucket keeps previous versions, which the measurement counts "+
		"and a plain object listing does not show, so deleting every object you can see does not make it empty")+ack)

	nid := versioned("tf-fid-never") // versioning was asked for and never observed
	fake.ForceBucketState(nid, "active")
	fake.MakeBucketNonEmpty(nid, true)
	refused("a versioned bucket never observed", nid, measured("3 object(s) and 1536 byte(s) -- this bucket was created with versioning requested "+
		"and the object store has not been asked since; if versioning is on, the measurement counts previous versions that a plain object listing does not show")+ack)

	// No sample, or one that saw nothing, is no refusal; force=true clears one.
	fake.MakeBucketNonEmpty(id, false)
	status, _ := s3HTTP(t, fake, http.MethodDelete, "buckets/"+id, "")
	wantHTTPStatus(t, "the delete of a bucket with no sample", status, http.StatusAccepted)
	status, _ = s3HTTP(t, fake, http.MethodDelete, "buckets/"+vid+"?force=true", "")
	wantHTTPStatus(t, "the forced delete of a non-empty bucket", status, http.StatusAccepted)
}

// create_bucket checks in this order: the slot limit, the retention ceiling, the name rules, and last the name being taken.
func TestFakeS3_ACreateIsRefusedInTheAPIsOrderWithItsSentences(t *testing.T) {
	fake, _ := s3Fake(t)
	fake.SetMaxBuckets(1)
	create := func(name string) (int, map[string]any) {
		return s3HTTP(t, fake, http.MethodPost, "buckets", fmt.Sprintf(`{"name":%q}`, name))
	}
	limit := "this organization already has 1 of its 1 bucket; delete one or upgrade the plan" // one, not "1 buckets"

	a := newBucket(t, fake, "tf-fid-a")
	for _, name := range []string{"ADMIN", "tf-fid-a"} { // the slot limit comes before the name rules and before the name being taken
		status, m := create(name)
		wantS3Problem(t, "create "+name+" at the limit", status, m, http.StatusUnprocessableEntity, "bucket_limit_reached", limit)
	}

	fake.ForceSoftDeleteBucket(a) // a soft-deleted bucket holds no slot, but keeps its name
	status, m := create("tf-fid-a")
	wantS3Problem(t, "create a soft-deleted name", status, m, http.StatusConflict, "bucket_already_exists", "this organization already has a bucket named 'tf-fid-a'")
	status, m = create("admin")
	wantS3Problem(t, "create a reserved name", status, m, http.StatusBadRequest, "bucket_name_reserved", "the bucket name 'admin' is reserved by the platform")
	status, m = create("vs-logs")
	wantS3Problem(t, "create a reserved prefix", status, m, http.StatusBadRequest, "bucket_name_reserved", "bucket names starting with 'vs-' are reserved by the platform")

	b := newBucket(t, fake, "tf-fid-b")
	fake.ForceSoftDeleteBucket(b)                                    // two rows against a ceiling of 2 x 1, and no slot in use
	for _, name := range []string{"tf-fid-c", "ADMIN", "tf-fid-a"} { // the retention ceiling comes before the name rules and the name being taken
		status, m = create(name)
		if status != http.StatusUnprocessableEntity || m["code"] != "bucket_retention_limit_reached" {
			t.Errorf("create %s at the retention ceiling: %d %v %v", name, status, m["code"], m["detail"])
			continue
		}
		detail, _ := m["detail"].(string)
		if !strings.HasPrefix(detail, "this organization holds 2 buckets against a ceiling of 2 (2 of them soft-deleted but not yet erased, the oldest eligible for erasure from ") ||
			!strings.HasSuffix(detail, "); restore one of those instead of creating a new bucket, or ask VirtScale support to raise this organization's bucket limit") {
			t.Errorf("the retention sentence is %q", detail)
		}
	}
}

// With the default of three buckets the limit sentence is plural.
func TestFakeS3_TheLimitSentenceIsPluralFromTwoBuckets(t *testing.T) {
	fake, _ := s3Fake(t)
	for _, name := range []string{"tf-fid-p1", "tf-fid-p2", "tf-fid-p3"} {
		newBucket(t, fake, name)
	}
	status, m := s3HTTP(t, fake, http.MethodPost, "buckets", `{"name":"tf-fid-p4"}`)
	wantS3Problem(t, "the fourth bucket", status, m, http.StatusUnprocessableEntity, "bucket_limit_reached", "this organization already has 3 of its 3 buckets; delete one or upgrade the plan")
}

// set_bucket_quota: a bucket that is not active or degraded cannot be resized (before the ceiling is looked at).
func TestFakeS3_AQuotaOfABucketThatCannotBeResizedIsRefusedLikeTheAPI(t *testing.T) {
	fake, _ := s3Fake(t)
	id := newBucket(t, fake, "tf-fid-q")
	path := "buckets/" + id + "/quota"

	status, m := s3HTTP(t, fake, http.MethodPut, path, `{"max_size_bytes":2199023255552}`)
	wantS3Problem(t, "a quota while requested", status, m, http.StatusConflict, "bucket_not_resizable", "a bucket in state 'requested' cannot have its quota changed")

	activate(t, fake, id)
	status, m = s3HTTP(t, fake, http.MethodPut, path, `{"max_size_bytes":2199023255552}`)
	wantS3Problem(t, "a quota above the ceiling", status, m, http.StatusUnprocessableEntity, "bucket_quota_exceeds_plan",
		"the requested max_size_bytes of 2199023255552 is above this organization's ceiling of 1099511627776; upgrade the plan or request an override")

	fake.ForceBucketState(id, "degraded") // resizing a degraded bucket is allowed
	status, _ = s3HTTP(t, fake, http.MethodPut, path, `{"max_objects":5}`)
	wantHTTPStatus(t, "a quota of a degraded bucket", status, http.StatusAccepted)

	fake.ForceSoftDeleteBucket(id)
	status, m = s3HTTP(t, fake, http.MethodPut, path, `{"max_objects":5}`)
	wantS3Problem(t, "a quota while deleting_soft", status, m, http.StatusConflict, "bucket_not_resizable", "a bucket in state 'deleting_soft' cannot have its quota changed")
}

// --- what the resources do with the 409s of a delete or a revoke -------------------------------------------------------------

func captureID(addr string, into *string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		*into = s.RootModule().Resources[addr].Primary.ID
		return nil
	}
}

func faultServed(fake *fakeAPI, want int) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if got := fake.FaultsServed(); got != want {
			return fmt.Errorf("%d injected faults served, want %d", got, want)
		}
		return nil
	}
}

// A destroy that meets a key somebody revoked a moment before (409 access_key_already_revoked) is done: the key is gone.
func TestAccS3AccessKeyResource_ADestroyThatMeetsAnAlreadyRevokedKeyIsDone(t *testing.T) {
	fake, _ := s3Fake(t)
	var id string
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             checkAccessKeysRevoked(fake),
		Steps: []resource.TestStep{
			{Config: accessKeyConfig(fake, "storage"), Check: captureID("virtscale_s3_access_key.test", &id)},
			{
				PreConfig: func() { // revoked elsewhere between the provider's refresh and its revoke
					fake.FailNext(1, matchRoute(http.MethodDelete, "/s3/access-keys/"), func(rc *reqCtx) {
						fake.ForceRevokeAccessKey(id)
						fake.revokeAccessKey(rc, id)
					})
				},
				Config: testAccProviderBlockWith(fake, "storage", testOrgID),
				Check: resource.ComposeTestCheckFunc(faultServed(fake, 1), func(*terraform.State) error {
					if k, _ := fake.AccessKey(id); k.State != "revoked" {
						return fmt.Errorf("the key is %s, want revoked", k.State)
					}
					return nil
				}),
			},
		},
	})
}

// A destroy that meets a key somebody is revoking (409 access_key_already_revoking) waits until it reads revoked.
func TestAccS3AccessKeyResource_ADestroyThatMeetsAKeyBeingRevokedWaitsForIt(t *testing.T) {
	fake, _ := s3Fake(t)
	var id string
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             checkAccessKeysRevoked(fake),
		Steps: []resource.TestStep{
			{Config: accessKeyConfig(fake, "storage"), Check: captureID("virtscale_s3_access_key.test", &id)},
			{
				PreConfig: func() {
					fake.FailNext(1, matchRoute(http.MethodDelete, "/s3/access-keys/"), func(rc *reqCtx) {
						fake.StartRevokeElsewhere(id)
						fake.revokeAccessKey(rc, id)
					})
				},
				Config: testAccProviderBlockWith(fake, "storage", testOrgID),
				Check: resource.ComposeTestCheckFunc(faultServed(fake, 1), func(*terraform.State) error {
					if k, _ := fake.AccessKey(id); k.State != "revoked" {
						return fmt.Errorf("the key is %s: the destroy did not wait until it was revoked", k.State)
					}
					return nil
				}),
			},
		},
	})
}

// A key that cannot be revoked (409 access_key_not_revocable) is an error with the API's sentence, and stays in the state.
func TestAccS3AccessKeyResource_AKeyThatCannotBeRevokedIsAnErrorWithTheAPIsSentence(t *testing.T) {
	fake, _ := s3Fake(t)
	var id string
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             checkAccessKeysRevoked(fake),
		Steps: []resource.TestStep{
			{Config: accessKeyConfig(fake, "storage"), Check: captureID("virtscale_s3_access_key.test", &id)},
			{
				PreConfig: func() {
					fake.FailNext(1, matchRoute(http.MethodDelete, "/s3/access-keys/"), func(rc *reqCtx) {
						fake.ForceAccessKeyState(id, "provisioning")
						fake.revokeAccessKey(rc, id)
					})
				},
				Config:      testAccProviderBlockWith(fake, "storage", testOrgID),
				ExpectError: wrapSafeInOrder("Unable to Revoke Access Key", "409 access_key_not_revocable", "an access key in state 'provisioning' cannot be revoked"),
			},
			{ // once the key is active again the destroy goes through
				PreConfig: func() { fake.ForceAccessKeyState(id, "active") },
				Config:    testAccProviderBlockWith(fake, "storage", testOrgID),
				Check: func(*terraform.State) error {
					if k, _ := fake.AccessKey(id); k.State != "revoked" {
						return fmt.Errorf("the key is %s, want revoked", k.State)
					}
					return nil
				},
			},
		},
	})
}

// A destroy that meets a bucket somebody deleted a moment before (409 bucket_already_deleting) is done: the bucket is gone.
func TestAccS3BucketResource_ADestroyThatMeetsABucketAlreadyBeingDeletedIsDone(t *testing.T) {
	fake, cfg := s3Fake(t)
	var id string
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{Config: cfg(bucketBlock("tf-bucket-ad", "")), Check: captureID("virtscale_s3_bucket.b", &id)},
			{
				PreConfig: func() { // deleted elsewhere between the provider's refresh and its delete
					fake.FailNext(1, matchRoute(http.MethodDelete, "/s3/buckets/"), func(rc *reqCtx) {
						fake.ForceSoftDeleteBucket(id)
						fake.deleteBucket(rc, id)
					})
				},
				Config: cfg(""),
				Check: resource.ComposeTestCheckFunc(faultServed(fake, 1), func(*terraform.State) error {
					if b, _ := fake.Bucket(id); b.State != "deleting_soft" {
						return fmt.Errorf("the bucket is %s, want deleting_soft", b.State)
					}
					return nil
				}),
			},
		},
	})
}

// A bucket that cannot be deleted (409 bucket_not_deletable) is an error with the API's sentence, and stays in the state.
func TestAccS3BucketResource_ABucketThatCannotBeDeletedIsAnErrorWithTheAPIsSentence(t *testing.T) {
	fake, cfg := s3Fake(t)
	var id string
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{Config: cfg(bucketBlock("tf-bucket-nd", "")), Check: captureID("virtscale_s3_bucket.b", &id)},
			{
				PreConfig: func() {
					fake.FailNext(1, matchRoute(http.MethodDelete, "/s3/buckets/"), func(rc *reqCtx) {
						fake.ForceBucketState(id, "purging")
						fake.deleteBucket(rc, id)
					})
				},
				Config:      cfg(""),
				ExpectError: wrapSafeInOrder("Unable to Delete Bucket", "409 bucket_not_deletable", "a bucket in state 'purging' cannot be deleted"),
			},
			{ // once the bucket is active again the destroy goes through
				PreConfig: func() { fake.ForceBucketState(id, "active") },
				Config:    cfg(""),
				Check: func(*terraform.State) error {
					if b, _ := fake.Bucket(id); b.State != "deleting_soft" {
						return fmt.Errorf("the bucket is %s, want deleting_soft", b.State)
					}
					return nil
				},
			},
		},
	})
}
