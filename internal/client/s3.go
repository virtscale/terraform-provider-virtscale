package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

// The resources a call can ask about (see ResourceVm). The API words an access key with a space.
const (
	ResourceBucket    = "bucket"
	ResourceAccessKey = "access key"
)

// BucketQuota is a bucket's own ceiling; nil in a dimension means this bucket is not individually capped (the
// organization's ceiling still applies).
type BucketQuota struct {
	MaxObjects   *int64 `json:"max_objects"`
	MaxSizeBytes *int64 `json:"max_size_bytes"`
}

// Bucket is the BucketOut representation returned by the s3/buckets endpoints. State is requested, provisioning,
// active, provision_failed, suspended, deleting_soft, purging, purged, deleted or degraded.
type Bucket struct {
	ID            string      `json:"id"`
	Name          string      `json:"name"`
	State         string      `json:"state"`
	Versioning    bool        `json:"versioning"`
	Quota         BucketQuota `json:"quota"`
	CreatedAt     time.Time   `json:"created_at"`
	UpdatedAt     time.Time   `json:"updated_at"`
	SoftDeletedAt *time.Time  `json:"soft_deleted_at"`
	PurgeAfter    *time.Time  `json:"purge_after"`
}

// BucketCreateRequest is the body of POST /v1/orgs/{org}/s3/buckets.
type BucketCreateRequest struct {
	Name       string `json:"name"`
	Versioning bool   `json:"versioning"`
}

// BucketQuotaChange is the body of PUT .../s3/buckets/{id}/quota: a dimension that is not set is left as it is, a set
// one with a nil value is cleared, and one with a value caps the bucket.
type BucketQuotaChange struct {
	SizeSet, ObjectsSet bool
	Size, Objects       *int64
}

// MarshalJSON leaves out the dimensions that are not set, and writes null for the ones that are cleared.
func (q BucketQuotaChange) MarshalJSON() ([]byte, error) {
	body := map[string]*int64{}
	if q.SizeSet {
		body["max_size_bytes"] = q.Size
	}
	if q.ObjectsSet {
		body["max_objects"] = q.Objects
	}
	return json.Marshal(body)
}

type bucketAccepted struct {
	Bucket    Bucket    `json:"bucket"`
	Operation Operation `json:"operation"`
}

// lostBucketHint is what the error of a keyed bucket create that gave up adds: the API's code for a name that is taken is
// bucket_already_exists, and a bucket that was made is imported by its id.
const lostBucketHint = "the first request may still have created the bucket: if the next apply reports " +
	"bucket_already_exists, find it in the panel or with the API and bring it under management with tofu import"

// lostAccessKeyHint is what the error of a keyed access key create that gave up adds. A key has no name to collide on, and
// its secret was in the answer that was lost, so an imported key would be useless: the key that may exist must be revoked.
const lostAccessKeyHint = "the first request may still have created an access key whose secret was lost with the answer: " +
	"list the organization's access keys in the panel or with the API (GET /v1/orgs/{org}/s3/access-keys) and revoke any you " +
	"did not receive (DELETE /v1/orgs/{org}/s3/access-keys/{id} revokes one by its id; an organization holds at most 5), then apply again"

// CreateBucket sends POST /v1/orgs/{org}/s3/buckets, which answers 202 with the bucket as it stands.
func (c *Client) CreateBucket(ctx context.Context, req BucketCreateRequest, idempotencyKey string) (*Bucket, error) {
	ctx = withLostCreateHint(ctx, lostBucketHint)
	var out bucketAccepted
	if err := c.doJSON(ctx, http.MethodPost, c.orgPath("s3/buckets"), req, idempotencyKey, &out); err != nil {
		return nil, err
	}
	return &out.Bucket, nil
}

// GetBucket sends GET /v1/orgs/{org}/s3/buckets/{id}.
func (c *Client) GetBucket(ctx context.Context, id string) (*Bucket, error) {
	var out Bucket
	if err := c.doJSON(ctx, http.MethodGet, c.orgPath("s3/buckets/"+url.PathEscape(id)), nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetBucketQuota sends PUT .../s3/buckets/{id}/quota, which answers 202 with the bucket.
func (c *Client) SetBucketQuota(ctx context.Context, id string, change BucketQuotaChange, idempotencyKey string) (*Bucket, error) {
	var out bucketAccepted
	if err := c.doJSON(ctx, http.MethodPut, c.orgPath("s3/buckets/"+url.PathEscape(id)+"/quota"), change, idempotencyKey, &out); err != nil {
		return nil, err
	}
	return &out.Bucket, nil
}

// DeleteBucket sends DELETE .../s3/buckets/{id}, a soft delete: the bucket enters deleting_soft and keeps its data for
// the retention window. A bucket that holds objects is 409 bucket_not_empty unless force acknowledges it.
func (c *Client) DeleteBucket(ctx context.Context, id string, force bool, idempotencyKey string) (*Bucket, error) {
	p := c.orgPath("s3/buckets/" + url.PathEscape(id))
	if force {
		p += "?force=true"
	}
	var out bucketAccepted
	if err := c.doJSON(ctx, http.MethodDelete, p, nil, idempotencyKey, &out); err != nil {
		return nil, err
	}
	return &out.Bucket, nil
}

// WaitBucket polls GetBucket until done reports true, with Poll's rules; the 404 that says the bucket is gone is
// done(nil).
func (c *Client) WaitBucket(ctx context.Context, id string, pollEvery time.Duration, done func(*Bucket) (bool, error)) (*Bucket, error) {
	return Poll(ctx, pollEvery, "bucket "+id,
		func(ctx context.Context) (*Bucket, error) { return c.GetBucket(ctx, id) },
		func(err error) bool { return IsNotFound(err, ResourceBucket) },
		done, func(b *Bucket) string { return "state " + b.State })
}

// AccessKey is the AccessKeyOut representation: a key's metadata, never its secret. State is requested,
// provisioning, active, revoking, revoked or degraded. RgwAccessKeyID is the public half of the credential: the API mints
// the key before it answers, so a key is created active with it set.
type AccessKey struct {
	ID             string    `json:"id"`
	State          string    `json:"state"`
	RgwAccessKeyID *string   `json:"rgw_access_key_id"`
	CreatedAt      time.Time `json:"created_at"`
}

type accessKeyCreated struct {
	AccessKey       AccessKey `json:"access_key"`
	SecretAccessKey *string   `json:"secret_access_key"`
}

type accessKeyAccepted struct {
	AccessKey AccessKey `json:"access_key"`
}

// CreateAccessKey sends POST /v1/orgs/{org}/s3/access-keys, which answers 201. The secret is in this answer and
// nowhere else, ever: it is nil when the answer is the stored replay of a repeated request (the replay has it
// stripped), and then it is lost for good.
func (c *Client) CreateAccessKey(ctx context.Context, idempotencyKey string) (*AccessKey, *string, error) {
	ctx = withLostCreateHint(ctx, lostAccessKeyHint)
	var out accessKeyCreated
	if err := c.doJSON(ctx, http.MethodPost, c.orgPath("s3/access-keys"), nil, idempotencyKey, &out); err != nil {
		return nil, nil, err
	}
	return &out.AccessKey, out.SecretAccessKey, nil
}

// ListAccessKeys sends GET /v1/orgs/{org}/s3/access-keys. Revoked keys are included: they are history.
func (c *Client) ListAccessKeys(ctx context.Context) ([]AccessKey, error) {
	var out listResponse[AccessKey]
	if err := c.doJSON(ctx, http.MethodGet, c.orgPath("s3/access-keys"), nil, "", &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// GetAccessKey has no single-item counterpart in the API; it lists and filters, returning an *APIError with Status 404
// (code not_found and the helper's sentence for an access key, like the API's own answer) when id is absent.
func (c *Client) GetAccessKey(ctx context.Context, id string) (*AccessKey, error) {
	keys, err := c.ListAccessKeys(ctx)
	if err != nil {
		return nil, err
	}
	for i := range keys {
		if keys[i].ID == id {
			return &keys[i], nil
		}
	}
	return nil, &APIError{Status: http.StatusNotFound, Code: "not_found", Detail: notFoundDetail(ResourceAccessKey)}
}

// RevokeAccessKey sends DELETE .../s3/access-keys/{id}, which answers 202: the key is still live in the object store
// until it reads revoked.
func (c *Client) RevokeAccessKey(ctx context.Context, id, idempotencyKey string) (*AccessKey, error) {
	var out accessKeyAccepted
	if err := c.doJSON(ctx, http.MethodDelete, c.orgPath("s3/access-keys/"+url.PathEscape(id)), nil, idempotencyKey, &out); err != nil {
		return nil, err
	}
	return &out.AccessKey, nil
}

// WaitAccessKey polls GetAccessKey until done reports true, with Poll's rules; a key that is not in the list is
// done(nil).
func (c *Client) WaitAccessKey(ctx context.Context, id string, pollEvery time.Duration, done func(*AccessKey) (bool, error)) (*AccessKey, error) {
	return Poll(ctx, pollEvery, "access key "+id,
		func(ctx context.Context) (*AccessKey, error) { return c.GetAccessKey(ctx, id) },
		func(err error) bool { return IsNotFound(err, ResourceAccessKey) },
		done, func(k *AccessKey) string { return "state " + k.State })
}
