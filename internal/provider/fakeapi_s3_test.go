package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// The S3 surface (routes/buckets.py, routes/access_keys.py of s3-control-plane 87607618): buckets are created, resized and
// soft-deleted asynchronously (202 and an operation, the bucket row says where it stands), an access key is minted
// synchronously (201) and its secret is in that one answer, never again. Shapes are BucketOut, BucketAccepted,
// AccessKeyOut, AccessKeyCreated and AccessKeyAccepted of customer-v1.json.

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
	stepsLeft int
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
	bucketNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
	ipLikeRE     = regexp.MustCompile(`^\d{1,3}(\.\d{1,3}){3}$`)
)

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
	if !bucketNameRE.MatchString(name) || ipLikeRE.MatchString(name) || regexp.MustCompile(`--`).MatchString(name) {
		rc.unprocessable("bucket_name_invalid", "a bucket name is 3 to 63 lowercase letters, digits and single hyphens, starting and ending with a letter or digit")
		return
	}
	limit := s.maxBuckets
	if limit == 0 {
		limit = 3
	}
	live := 0
	for _, x := range s.buckets {
		if x.State != "deleting_soft" && x.State != "deleted" {
			live++
			if x.Name == name {
				rc.conflict("name_taken", fmt.Sprintf("a bucket named '%s' already exists in this organization", name))
				return
			}
		}
	}
	if live >= limit {
		rc.unprocessable("bucket_limit_reached", fmt.Sprintf("this organization already has %d of its %d buckets", live, limit))
		return
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

// putBucketQuota is PUT .../quota: an omitted dimension is left alone, an explicit null clears it; the answer is 202.
func (f *fakeAPI) putBucketQuota(rc *reqCtx, id string) {
	b := newBodyCheck(rc.r.Body)
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
	if size != nil && *size > 1<<40 {
		rc.unprocessable("bucket_quota_exceeds_plan", "that quota is above your plan's ceiling")
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

// deleteBucket is a soft delete: the bucket enters deleting_soft with a retention window and stays readable; a
// non-empty one is 409 bucket_not_empty unless force=true.
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
	s.keySeq++
	id := fakeUUID(9, s.keySeq)
	now := time.Now().UTC()
	k := &fakeAccessKey{wireAccessKeyOut: wireAccessKeyOut{ID: id, State: "requested", CreatedAt: now, UpdatedAt: now}}
	s.keys[id] = k
	op := f.newOperation("access_key.create", "access_key", id, now)
	secret := s.secret
	if secret == "" {
		secret = fmt.Sprintf("fake-secret-%d", s.keySeq)
	}
	s.secret = ""
	created := wireAccessKeyCreated{AccessKey: k.wireAccessKeyOut, Operation: op, SecretAccessKey: &secret}
	location := f.operationLocation(op)
	// The stored replay has the secret stripped (domain/access_keys.py).
	replay := created
	replay.SecretAccessKey = nil
	f.idemKeep(storeKey, digest, http.StatusCreated, replay, location, "")
	rc.json(http.StatusCreated, created, "Location", location)
}

func advanceAccessKey(k *fakeAccessKey) {
	switch k.State {
	case "requested":
		k.State = "provisioning"
	case "provisioning":
		k.State = "active"
		id := "AKIA" + k.ID[len(k.ID)-8:]
		k.RgwAccessKeyID = &id
	case "revoking":
		if k.stepsLeft--; k.stepsLeft <= 0 {
			k.State = "revoked"
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
	if k.State != "revoked" && k.State != "revoking" {
		k.State, k.stepsLeft = "revoking", 2
	}
	op := f.newOperation("access_key.revoke", "access_key", id, time.Now().UTC())
	accepted := wireAccessKeyAccepted{AccessKey: k.wireAccessKeyOut, Operation: op}
	location := f.operationLocation(op)
	f.idemKeep(storeKey, digest, http.StatusAccepted, accepted, location, "")
	rc.json(http.StatusAccepted, accepted, "Location", location)
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
		b.State = "deleting_soft"
	}
}
