package client

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	stdlog "log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func keyedCreate(c *Client) error {
	_, _, err := c.CreateVm(context.Background(), VmCreateRequest{
		Name: "web-01", Image: "i", VCPUs: 1, MemoryMB: 1024, DiskGB: 10,
		StoragePool: "p", Region: "gro", NetworkProfile: "n",
	}, "key-1")
	return err
}

// A keyed request whose first attempt is still being processed answers
// 409 idempotency_in_progress. The same key replays the first result once the
// API has stored it, so a keyed request waits and repeats; without that, a
// retried create could fail while its VM is being built, outside the state
// (V4 L3). No other request repeats a 409.
func TestRetry_IdempotencyInProgressIsRetriedOnlyForAKeyedRequest(t *testing.T) {
	for _, o := range testOps() {
		t.Run(o.name, func(t *testing.T) {
			c, sc, _ := newScriptedClient(t, problemStep(409, "idempotency_in_progress"), o.ok)

			err := o.run(c)
			if o.keyed {
				if err != nil || sc.count() != 2 {
					t.Errorf("keyed request: err = %v, calls = %d, want success on the second call", err, sc.count())
				}
				return
			}
			apiErr := asAPIError(t, err)
			if apiErr.Status != 409 || sc.count() != 1 {
				t.Errorf("unkeyed request: err = %v, calls = %d, want the 409 after exactly one call", err, sc.count())
			}
		})
	}
}

// Any other conflict is a decision about the request, keyed or not.
func TestRetry_OtherConflictsOnAKeyedCreateAreNotRetried(t *testing.T) {
	for _, code := range []string{"name_taken", "operation_in_flight", "compute_not_enabled"} {
		t.Run(code, func(t *testing.T) {
			c, sc, _ := newScriptedClient(t, problemStep(409, code), jsonStep(202, testVmAcceptedJSON))

			apiErr := asAPIError(t, keyedCreate(c))
			if apiErr.Code != code || sc.count() != 1 {
				t.Errorf("err = %v, calls = %d, want %s after exactly one call", apiErr, sc.count(), code)
			}
		})
	}
}

// A claim that never clears (its first request died holding the key) is
// returned after the usual five attempts, with the API's own error.
func TestRetry_IdempotencyInProgressThatNeverClearsGivesUpAfterFiveAttempts(t *testing.T) {
	c, sc, waits := newScriptedClient(t, problemStep(409, "idempotency_in_progress"))

	apiErr := asAPIError(t, keyedCreate(c))
	if apiErr.Code != "idempotency_in_progress" || sc.count() != 5 {
		t.Errorf("err = %v, calls = %d, want idempotency_in_progress after 5 calls", apiErr, sc.count())
	}
	if n := len(waits.all()); n != 4 {
		t.Errorf("waits = %d, want 4 (one between each pair of attempts)", n)
	}
}

// A certificate that does not verify cannot be fixed by trying again: the
// first transport error comes back with no wait and no second attempt (V4 L2).
func TestRetry_ACertificateErrorIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	srv.Config.ErrorLog = stdlog.New(io.Discard, "", 0) // the refused handshakes are expected
	srv.StartTLS()
	t.Cleanup(srv.Close)

	// The default HTTP client does not trust the test server's certificate.
	c, err := New(srv.URL, "test-key", "org-1")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	waits := &sleepLog{}
	c.retry.sleep = waits.sleep

	_, err = c.GetVm(context.Background(), "vm-1")
	var certErr *tls.CertificateVerificationError
	if !errors.As(err, &certErr) {
		t.Fatalf("err = %v, want a certificate verification error", err)
	}
	if n := len(waits.all()); n != 0 {
		t.Errorf("waits = %d, want none", n)
	}
	if strings.Contains(err.Error(), "gave up after") {
		t.Errorf("err = %q, want the first error as is, not a retried one", err)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("handler calls = %d, want 0 (the handshake fails first)", n)
	}
}

// An endpoint that is not an http(s) URL is a configuration error: no retry.
func TestRetry_AnEndpointWithAnUnsupportedSchemeIsNotRetried(t *testing.T) {
	c, err := New("ftp://example.invalid", "test-key", "org-1")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	waits := &sleepLog{}
	c.retry.sleep = waits.sleep

	_, err = c.GetVm(context.Background(), "vm-1")
	if err == nil || !strings.Contains(err.Error(), "unsupported protocol scheme") {
		t.Fatalf("err = %v, want an unsupported protocol scheme error", err)
	}
	if n := len(waits.all()); n != 0 {
		t.Errorf("waits = %d, want none", n)
	}
	if strings.Contains(err.Error(), "gave up after") {
		t.Errorf("err = %q, want the first error as is, not a retried one", err)
	}
}
