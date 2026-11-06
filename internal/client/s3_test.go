package client

import (
	"context"
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
