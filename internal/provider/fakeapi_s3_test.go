package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The S3 surface (routes/buckets.py, routes/access_keys.py of s3-control-plane 87607618): buckets are created, resized and
// soft-deleted asynchronously (202 and an operation, the bucket row says where it stands), an access key is minted
// synchronously (201, born active with its key id, and an operation that is already succeeded) and its secret is in that one
// answer, never again. Shapes are BucketOut, BucketAccepted, AccessKeyOut, AccessKeyCreated and AccessKeyAccepted of
// customer-v1.json. The refusals follow domain/buckets.py and domain/access_keys.py in code, sentence and order (the 409s of
// a repeated or ill-timed delete and revoke, the slot and retention ceilings before the name rules); what is not modelled is
// the operation_in_flight gate, the billing and enrollment gates, and the 24-hour replay of a stored refusal.

type wireBucketQuota struct {
	MaxObjects   *int64 `json:"max_objects"`
	MaxSizeBytes *int64 `json:"max_size_bytes"`
}

type wireBucketOut struct {
	ID                 string           `json:"id"`
	Name               string           `json:"name"`
	State              string           `json:"state"`
	Versioning         bool             `json:"versioning"`
	Quota              wireBucketQuota  `json:"quota"`
	ObservedQuota      *wireBucketQuota `json:"observed_quota"`
	ObservedVersioning *bool            `json:"observed_versioning"`
	ObservedExists     *bool            `json:"observed_exists"`
	ObservedAt         *time.Time       `json:"observed_at"`
	OperationID        *string          `json:"operation_id"`
	PurgeAfter         *time.Time       `json:"purge_after"`
	SoftDeletedAt      *time.Time       `json:"soft_deleted_at"`
	CreatedAt          time.Time        `json:"created_at"`
	UpdatedAt          time.Time        `json:"updated_at"`
}

type wireBucketAccepted struct {
	Bucket    wireBucketOut    `json:"bucket"`
	Operation wireOperationRef `json:"operation"`
}

type wireAccessKeyOut struct {
	ID             string     `json:"id"`
	State          string     `json:"state"`
	RgwAccessKeyID *string    `json:"rgw_access_key_id"`
	OpenbaoPath    *string    `json:"openbao_path"`
	OperationID    *string    `json:"operation_id"`
	LastRotatedAt  *time.Time `json:"last_rotated_at"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

type wireAccessKeyCreated struct {
	AccessKey       wireAccessKeyOut `json:"access_key"`
	Operation       wireOperationRef `json:"operation"`
	SecretAccessKey *string          `json:"secret_access_key"`
}

type wireAccessKeyAccepted struct {
	AccessKey wireAccessKeyOut `json:"access_key"`
	Operation wireOperationRef `json:"operation"`
}

type fakeBucket struct {
	wireBucketOut
	nonEmpty  bool
	stepsLeft int
}

type fakeAccessKey struct {
	wireAccessKeyOut
	stepsLeft  int
	failRevoke bool // the next revoke ends in state degraded
}

type s3State struct {
	buckets    map[string]*fakeBucket
	bucketSeq  int
	keys       map[string]*fakeAccessKey
	keySeq     int
	failBucket bool     // new buckets end in provision_failed
	maxBuckets int      // 0 means the default of 3
	secret     string   // the secret the next minted key carries
	deletes    []string // "name force=<bool>" of every bucket DELETE
	quotaPuts  []string // the body of every quota PUT, as it arrived
}

func (f *fakeAPI) s3() *s3State {
	if f.s3s.buckets == nil {
		f.s3s.buckets, f.s3s.keys = map[string]*fakeBucket{}, map[string]*fakeAccessKey{}
	}
	return &f.s3s
}

// SetNextSecret is the secret the next minted access key carries.
func (f *fakeAPI) SetNextSecret(secret string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.s3().secret = secret
}

func (f *fakeAPI) FailBucketBuilds() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.s3().failBucket = true
}

func (f *fakeAPI) MakeBucketNonEmpty(id string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.s3().buckets[id]; ok {
		b.nonEmpty = on
	}
}

// Bucket returns the bucket, whatever its state.
func (f *fakeAPI) Bucket(id string) (wireBucketOut, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.s3().buckets[id]
	if !ok {
		return wireBucketOut{}, false
	}
	return b.wireBucketOut, true
}

func (f *fakeAPI) VanishBucket(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.s3().buckets, id)
}

// BucketDeletes is "name force=<bool>" of every bucket DELETE, in order.
func (f *fakeAPI) BucketDeletes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.s3().deletes...)
}

// AccessKey returns the key, whatever its state.
func (f *fakeAPI) AccessKey(id string) (wireAccessKeyOut, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.s3().keys[id]
	if !ok {
		return wireAccessKeyOut{}, false
	}
	return k.wireAccessKeyOut, true
}

func (f *fakeAPI) ActiveAccessKeys() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, k := range f.s3().keys {
		if k.State != "revoked" {
			n++
		}
	}
	return n
}

var (
	fakeBucketNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
	fakeIPLikeRE     = regexp.MustCompile(`^\d{1,3}(\.\d{1,3}){3}$`)
)

// fakeBucketNameProblem is domain/naming.py validate_bucket_name for a name of 3 to 63 characters (the request model
// has refused the others): the code and sentence of the first rule it breaks, or "". The API quotes with Python's repr, so
// with single quotes.
func fakeBucketNameProblem(name string) (code, detail string) {
	switch {
	case strings.Contains(name, "."):
		return "bucket_name_invalid", "dots are not permitted: they break virtual-hosted addressing under the platform's wildcard certificate"
	case strings.Contains(name, "--"):
		return "bucket_name_invalid", "bucket names may not contain consecutive hyphens"
	case !fakeBucketNameRE.MatchString(name):
		return "bucket_name_invalid", "bucket names may contain only lowercase letters, digits and hyphens, and must start and end with a letter or digit"
	}
	for _, reserved := range []string{"admin", "api", "www", "internal", "system", "backup", "billing", "metrics", "health"} {
		if name == reserved {
			return "bucket_name_reserved", fmt.Sprintf("the bucket name '%s' is reserved by the platform", name)
		}
	}
	for _, prefix := range []string{"virtscale-", "vs-", "s3-", "rgw-", "admin-", "internal-", "system-"} {
		if strings.HasPrefix(name, prefix) {
			return "bucket_name_reserved", fmt.Sprintf("bucket names starting with '%s' are reserved by the platform", prefix)
		}
	}
	return "", ""
}

func (f *fakeAPI) bucketAccepted(b *fakeBucket, typ string) wireBucketAccepted {
	op := f.newOperation(typ, "bucket", b.ID, time.Now().UTC())
	b.OperationID = &op.ID
	return wireBucketAccepted{Bucket: b.wireBucketOut, Operation: op}
}

func (f *fakeAPI) createBucket(rc *reqCtx) {
	b := newBodyCheck(rc.r.Body)
	name := b.str("name", true, 3, 63)
	versioning := false
	if raw, ok := b.present("versioning", false); ok {
		if json.Unmarshal(raw, &versioning) != nil {
			b.fail("versioning", "Input should be a valid boolean")
		}
	}
	if errs := b.finish(); len(errs) > 0 {
		rc.invalid(errs...)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.s3()
	storeKey, digest, replied := f.idemClaim(rc, "POST buckets", []byte(fmt.Sprint(name, versioning)))
	if replied {
		return
	}
	// create_bucket checks in this order: the slot limit, the retention ceiling, the name rules, and last the name being
	// taken (uq_buckets_org_id_name_live). A soft-deleted bucket holds no slot but still counts as a row, and keeps its
	// name; a purged or deleted one gives all three up. A purging bucket holds a slot (its erasure sentence is not modelled).
	limit := s.maxBuckets
	if limit == 0 {
		limit = 3
	}
	slots, rows, soft := 0, 0, 0
	var due *time.Time
	for _, x := range s.buckets {
		if x.State == "purged" || x.State == "deleted" {
			continue
		}
		rows++
		if x.State != "deleting_soft" {
			slots++
			continue
		}
		soft++
		if x.PurgeAfter != nil && (due == nil || x.PurgeAfter.Before(*due)) {
			due = x.PurgeAfter
		}
	}
	if slots >= limit {
		noun, deletes := "buckets", "one"
		if limit == 1 {
			noun = "bucket"
		}
		if over := slots - limit; over > 0 {
			deletes = fmt.Sprintf("%d of them", over+1)
		}
		rc.unprocessable("bucket_limit_reached", fmt.Sprintf("this organization already has %d of its %d %s; delete %s or upgrade the plan", slots, limit, noun, deletes))
		return
	}
	if ceiling := 2 * limit; rows >= ceiling { // the purge worker runs in production, so the sentence names the oldest erasure
		erasure := " (none of them currently carries a purge date)"
		if due != nil {
			erasure = ", the oldest eligible for erasure from " + due.UTC().Format("2006-01-02T15:04:05.000000-07:00")
		}
		rc.unprocessable("bucket_retention_limit_reached", fmt.Sprintf("this organization holds %d buckets against a ceiling of %d (%d of them soft-deleted but not yet erased%s); "+
			"restore one of those instead of creating a new bucket, or ask VirtScale support to raise this organization's bucket limit", rows, ceiling, soft, erasure))
		return
	}
	if code, detail := fakeBucketNameProblem(name); code != "" {
		rc.problem(http.StatusBadRequest, code, "Invalid request", detail)
		return
	}
	for _, x := range s.buckets {
		if x.State != "purged" && x.State != "deleted" && x.Name == name {
			rc.conflict("bucket_already_exists", fmt.Sprintf("this organization already has a bucket named '%s'", name))
			return
		}
	}
	s.bucketSeq++
	id := fakeUUID(8, s.bucketSeq)
	now := time.Now().UTC()
	bk := &fakeBucket{wireBucketOut: wireBucketOut{ID: id, Name: name, State: "requested", Versioning: versioning, CreatedAt: now, UpdatedAt: now}}
	if s.failBucket {
		bk.stepsLeft = -1
	}
	s.buckets[id] = bk
	accepted := f.bucketAccepted(bk, "bucket.create")
	location := f.operationLocation(accepted.Operation)
	f.idemKeep(storeKey, digest, http.StatusAccepted, accepted, location, "")
	rc.json(http.StatusAccepted, accepted, "Location", location)
}

// advanceBucket moves a bucket one step per read: requested, provisioning, then active (provision_failed for a
// failing build); a soft-deleted one stays visible with its retention window.
func advanceBucket(b *fakeBucket) {
	switch b.State {
	case "requested":
		b.State = "provisioning"
	case "provisioning":
		if b.stepsLeft < 0 {
			b.State = "provision_failed"
		} else {
			b.State = "active"
			t := true
			b.ObservedExists, b.ObservedVersioning = &t, &b.Versioning
			q := b.Quota
			b.ObservedQuota = &q
		}
	}
}

func (f *fakeAPI) getBucket(rc *reqCtx, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.s3().buckets[id]
	if !ok {
		rc.notFound("bucket")
		return
	}
	advanceBucket(b)
	rc.json(http.StatusOK, b.wireBucketOut)
}

func (f *fakeAPI) listBuckets(rc *reqCtx) {
	f.mu.Lock()
	defer f.mu.Unlock()
	items := []wireBucketOut{}
	for _, b := range f.s3().buckets {
		items = append(items, b.wireBucketOut)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	rc.json(http.StatusOK, wireList[wireBucketOut]{Items: items})
}

// putBucketQuota is PUT .../quota: an omitted dimension is left alone, an explicit null clears it; the answer is 202. Only
// an active or degraded bucket can be resized.
func (f *fakeAPI) putBucketQuota(rc *reqCtx, id string) {
	raw, _ := io.ReadAll(rc.r.Body)
	f.mu.Lock()
	f.s3().quotaPuts = append(f.s3().quotaPuts, string(bytes.TrimSpace(raw)))
	f.mu.Unlock()
	b := newBodyCheck(bytes.NewReader(raw))
	read := func(name string) (set bool, value *int64) {
		raw, ok := b.present(name, false)
		if !ok {
			return false, nil
		}
		if string(raw) == "null" {
			return true, nil
		}
		n, err := strconv.ParseInt(string(raw), 10, 64)
		if err != nil || n < 1 {
			b.fail(name, "Input should be a valid integer, greater than or equal to 1")
			return false, nil
		}
		return true, &n
	}
	sizeSet, size := read("max_size_bytes")
	objSet, objects := read("max_objects")
	if errs := b.finish(); len(errs) > 0 {
		rc.invalid(errs...)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	storeKey, digest, replied := f.idemClaim(rc, "PUT bucket quota", []byte(fmt.Sprint(id, sizeSet, size, objSet, objects)))
	if replied {
		return
	}
	bk, ok := f.s3().buckets[id]
	if !ok {
		rc.notFound("bucket")
		return
	}
	if bk.State != "active" && bk.State != "degraded" { // RESIZABLE_STATES, checked before the ceiling
		rc.conflict("bucket_not_resizable", fmt.Sprintf("a bucket in state '%s' cannot have its quota changed", bk.State))
		return
	}
	if size != nil && *size > 1<<40 {
		rc.unprocessable("bucket_quota_exceeds_plan", fmt.Sprintf("the requested max_size_bytes of %d is above this organization's ceiling of %d; upgrade the plan or request an override", *size, int64(1)<<40))
		return
	}
	if sizeSet {
		bk.Quota.MaxSizeBytes = size
	}
	if objSet {
		bk.Quota.MaxObjects = objects
	}
	bk.UpdatedAt = time.Now().UTC()
	q := bk.Quota
	bk.ObservedQuota = &q
	accepted := f.bucketAccepted(bk, "bucket.quota")
	location := f.operationLocation(accepted.Operation)
	f.idemKeep(storeKey, digest, http.StatusAccepted, accepted, location, "")
	rc.json(http.StatusAccepted, accepted, "Location", location)
}

// deleteBucket is a soft delete: the bucket enters deleting_soft with a retention window and stays readable. One that is
// already deleting_soft is 409 bucket_already_deleting, one that is still being built or is being erased is 409
// bucket_not_deletable, and a non-empty one is 409 bucket_not_empty unless force=true.
func (f *fakeAPI) deleteBucket(rc *reqCtx, id string) {
	force := rc.r.URL.Query().Get("force") == "true"
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.s3()
	storeKey, digest, replied := f.idemClaim(rc, "DELETE bucket", []byte(fmt.Sprint(id, force)))
	if replied {
		return
	}
	b, ok := s.buckets[id]
	if !ok {
		rc.notFound("bucket")
		return
	}
	s.deletes = append(s.deletes, fmt.Sprintf("%s force=%t", b.Name, force))
	switch b.State {
	case "active", "suspended", "provision_failed", "degraded":
	case "deleting_soft":
		rc.conflict("bucket_already_deleting", "this bucket is already scheduled for deletion; use restore to cancel")
		return
	default:
		rc.conflict("bucket_not_deletable", fmt.Sprintf("a bucket in state '%s' cannot be deleted", b.State))
		return
	}
	if b.nonEmpty && !force {
		rc.conflict("bucket_not_empty", "the bucket holds objects: pass force=true to soft-delete it anyway")
		return
	}
	now := time.Now().UTC()
	purge := now.Add(7 * 24 * time.Hour)
	b.State, b.SoftDeletedAt, b.PurgeAfter = "deleting_soft", &now, &purge
	accepted := f.bucketAccepted(b, "bucket.delete")
	location := f.operationLocation(accepted.Operation)
	f.idemKeep(storeKey, digest, http.StatusAccepted, accepted, location, "")
	rc.json(http.StatusAccepted, accepted, "Location", location)
}

func (f *fakeAPI) createAccessKey(rc *reqCtx) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.s3()
	storeKey, digest, replied := f.idemClaim(rc, "POST access-keys", nil)
	if replied {
		return
	}
	live := 0
	for _, k := range s.keys {
		if k.State != "revoked" {
			live++
		}
	}
	if live >= 5 {
		rc.unprocessable("access_key_limit_reached", fmt.Sprintf("this organization already holds %d of its 5 access keys; revoke one before creating another", live))
		return
	}
	s.keySeq++
	id := fakeUUID(9, s.keySeq)
	now := time.Now().UTC()
	rgw := "AKIA" + id[len(id)-8:]
	k := &fakeAccessKey{wireAccessKeyOut: wireAccessKeyOut{ID: id, State: "active", RgwAccessKeyID: &rgw, LastRotatedAt: &now, CreatedAt: now, UpdatedAt: now}}
	s.keys[id] = k
	op := f.newOperation("access_key.create", "access_key", id, now)
	op.State, op.FinishedAt = "succeeded", &now
	secret := s.secret
	if secret == "" {
		secret = fmt.Sprintf("fake-secret-%d", s.keySeq)
	}
	s.secret = ""
	created := wireAccessKeyCreated{AccessKey: k.wireAccessKeyOut, Operation: op, SecretAccessKey: &secret}
	// The stored replay has the secret stripped (domain/access_keys.py). The create sends no Location: its operation is done.
	replay := created
	replay.SecretAccessKey = nil
	f.idemKeep(storeKey, digest, http.StatusCreated, replay, "", "")
	rc.json(http.StatusCreated, created)
}

// advanceAccessKey moves a revoking key one step per read, to revoked (or degraded for a revoke that was made to fail). A
// key is born active: there is no create lifecycle to walk through.
func advanceAccessKey(k *fakeAccessKey) {
	if k.State == "revoking" {
		if k.stepsLeft--; k.stepsLeft <= 0 {
			k.State = "revoked"
			if k.failRevoke {
				k.State, k.failRevoke = "degraded", false
			}
		}
	}
}

// listAccessKeys lists every key, revoked ones included (they are history); each read moves the keys one step.
func (f *fakeAPI) listAccessKeys(rc *reqCtx) {
	f.mu.Lock()
	defer f.mu.Unlock()
	items := []wireAccessKeyOut{}
	for _, k := range f.s3().keys {
		advanceAccessKey(k)
		items = append(items, k.wireAccessKeyOut)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	rc.json(http.StatusOK, wireList[wireAccessKeyOut]{Items: items})
}

func (f *fakeAPI) revokeAccessKey(rc *reqCtx, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	storeKey, digest, replied := f.idemClaim(rc, "DELETE access-key", []byte(id))
	if replied {
		return
	}
	k, ok := f.s3().keys[id]
	if !ok {
		rc.notFound("access key")
		return
	}
	switch k.State {
	case "active", "degraded": // a failed revoke lands in degraded, and may be asked again
	case "revoked":
		rc.conflict("access_key_already_revoked", "this access key has already been revoked")
		return
	case "revoking":
		rc.conflict("access_key_already_revoking", "this access key is already being revoked; poll its operation")
		return
	default:
		rc.conflict("access_key_not_revocable", fmt.Sprintf("an access key in state '%s' cannot be revoked", k.State))
		return
	}
	k.State, k.stepsLeft = "revoking", 2
	op := f.newOperation("access_key.revoke", "access_key", id, time.Now().UTC())
	accepted := wireAccessKeyAccepted{AccessKey: k.wireAccessKeyOut, Operation: op}
	location := f.operationLocation(op)
	f.idemKeep(storeKey, digest, http.StatusAccepted, accepted, location, "")
	rc.json(http.StatusAccepted, accepted, "Location", location)
}

// QuotaPuts is the body of every quota PUT, in order.
func (f *fakeAPI) QuotaPuts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.s3().quotaPuts...)
}

// FailNextRevoke makes the next revoke of the key end in state degraded (and only that one).
func (f *fakeAPI) FailNextRevoke(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if k, ok := f.s3().keys[id]; ok {
		k.failRevoke = true
	}
}

// ForceAccessKeyState puts the key in a state at once.
func (f *fakeAPI) ForceAccessKeyState(id, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if k, ok := f.s3().keys[id]; ok {
		k.State = state
		if state == "revoking" {
			k.stepsLeft = 1000 // still being revoked after any number of reads
		}
	}
}

// SetMaxBuckets is the organization's bucket limit (the fake's default is 3).
func (f *fakeAPI) SetMaxBuckets(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.s3().maxBuckets = n
}

// ForceRevokeAccessKey is a revoke done elsewhere (the panel): the key reads revoked at once.
func (f *fakeAPI) ForceRevokeAccessKey(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if k, ok := f.s3().keys[id]; ok {
		k.State = "revoked"
	}
}

// ForceSoftDeleteBucket is a delete done elsewhere: the bucket reads deleting_soft at once.
func (f *fakeAPI) ForceSoftDeleteBucket(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.s3().buckets[id]; ok {
		now := time.Now().UTC()
		purge := now.Add(7 * 24 * time.Hour)
		b.State, b.SoftDeletedAt, b.PurgeAfter = "deleting_soft", &now, &purge
	}
}

// ForceBucketState puts the bucket in a state at once (the states the fake does not produce itself, such as purging).
func (f *fakeAPI) ForceBucketState(id, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.s3().buckets[id]; ok {
		b.State = state
	}
}

// StartRevokeElsewhere is a revoke another client began: the key is revoking, and reads revoked two list reads later.
func (f *fakeAPI) StartRevokeElsewhere(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if k, ok := f.s3().keys[id]; ok {
		k.State, k.stepsLeft = "revoking", 2
	}
}
