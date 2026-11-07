package client

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A keyed create that gives up after a lost answer says what to do for that resource: a bucket is found by its name
// (the API's code is bucket_already_exists), and an access key has no name and no recoverable secret, so it is revoked.
func TestGiveUp_TheHintOfAKeyedCreateIsTheResourcesOwn(t *testing.T) {
	c := failWith(t, 502, "bad_gateway", "the upstream is down")
	c.retry.sleep = func(context.Context, time.Duration) error { return nil }

	_, err := c.CreateBucket(context.Background(), BucketCreateRequest{Name: "logs"}, "key-1")
	text := asAPIError(t, err).Error()
	if !strings.Contains(text, "bucket_already_exists") || strings.Contains(text, "name_taken") {
		t.Errorf("bucket: Error() = %q, want the bucket hint", text)
	}

	_, _, err = c.CreateAccessKey(context.Background(), "key-1")
	text = asAPIError(t, err).Error()
	if !strings.Contains(text, "revoke any you did not receive") || strings.Contains(text, "tofu import") || strings.Contains(text, "name_taken") {
		t.Errorf("access key: Error() = %q, want the revoke hint", text)
	}

	// and the generic one stays for a create that has no hint of its own
	_, _, err = c.CreateVm(context.Background(), VmCreateRequest{Name: "web"}, "key-1")
	if text := asAPIError(t, err).Error(); !strings.Contains(text, "name_taken") {
		t.Errorf("instance: Error() = %q, want the generic create hint", text)
	}
}

// A 2xx answer to a keyed create that cannot be decoded still means the API made the resource, and for an access key
// the secret was in that answer: the error says it may exist, and how to find and revoke it. A read that cannot be
// decoded made nothing, so it says nothing of the kind.
func TestDoJSON_ACreateAnswerThatCannotBeReadSaysTheResourceMayExist(t *testing.T) {
	cases := map[string]struct {
		status int
		create func(*Client) error
		want   []string
	}{
		"access key": {http.StatusCreated, func(c *Client) error { _, _, err := c.CreateAccessKey(context.Background(), "key-1"); return err },
			[]string{"may still have created an access key", "GET /v1/orgs/{org}/s3/access-keys", "DELETE /v1/orgs/{org}/s3/access-keys/{id}"}},
		"bucket": {http.StatusAccepted, func(c *Client) error {
			_, err := c.CreateBucket(context.Background(), BucketCreateRequest{Name: "logs"}, "key-1")
			return err
		}, []string{"may still have created the bucket", "bucket_already_exists"}},
		"instance": {http.StatusAccepted, func(c *Client) error {
			_, _, err := c.CreateVm(context.Background(), VmCreateRequest{Name: "web"}, "key-1")
			return err
		}, []string{"may still have created the resource", "name_taken"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"access_key": {"id": "k1"`)) // cut short
			})
			err := tc.create(c)
			if err == nil || !strings.Contains(err.Error(), "decode response body") {
				t.Fatalf("err = %v, want a decode error", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %q, want it to contain %q", err, want)
				}
			}
		})
	}

	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"items": [`)) })
	if _, err := c.ListAccessKeys(context.Background()); err == nil || strings.Contains(err.Error(), "created") {
		t.Errorf("a read that cannot be decoded: err = %v, want a decode error that speaks of no create", err)
	}
}
