package client

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// A definitive answer is not a retried one: a 429 that clears into a 422 returns the 422 as the API worded it, with no
// "gave up" suffix; the suffix is for the error that kept coming back.
func TestGiveUp_ADefinitiveAnswerAfterAThrottleHasNoGaveUpSuffix(t *testing.T) {
	c, sc, _ := newScriptedClient(t, problemStep(429, "rate_limited", "Retry-After", "1"), problemStep(422, "access_key_limit_reached"))

	_, err := c.GetVm(context.Background(), "vm-1")
	apiErr := asAPIError(t, err)
	if apiErr.Status != 422 || sc.count() != 2 {
		t.Fatalf("err = %v after %d calls, want the 422 after 2", err, sc.count())
	}
	if strings.Contains(err.Error(), "gave up") {
		t.Errorf("Error() = %q, want no gave-up suffix on a 422", err)
	}
}

func TestGiveUp_AThrottleThatNeverClearsStillSaysItGaveUp(t *testing.T) {
	c, _, _ := newScriptedClient(t, problemStep(429, "rate_limited", "Retry-After", "1"))

	_, err := c.GetVm(context.Background(), "vm-1")
	if !strings.HasSuffix(err.Error(), "; gave up after 5 attempts") {
		t.Errorf("Error() = %q, want the gave-up suffix after five 429s", err)
	}
}

func TestGiveUp_AGatewayErrorAfterAThrottleSaysItGaveUp(t *testing.T) {
	c, _, _ := newScriptedClient(t, problemStep(429, "rate_limited", "Retry-After", "1"), htmlStep(502), htmlStep(502), htmlStep(502), htmlStep(502))

	_, err := c.GetVm(context.Background(), "vm-1")
	if !strings.Contains(err.Error(), "gave up after 5 attempts") {
		t.Errorf("Error() = %q, want the gave-up suffix: the 502 was retried", err)
	}
}

// The suffix follows the real retry decision for the last attempt, not the status class: a 500 is never retried, and a 502
// on a request that cannot be repeated (a POST without an Idempotency-Key) is not either, so after a 429 they are answers.
func TestGiveUp_TheSuffixFollowsTheRetryDecisionOfTheLastAttempt(t *testing.T) {
	throttle := problemStep(429, "rate_limited", "Retry-After", "1")
	cases := []struct {
		name  string
		steps []func(http.ResponseWriter, *http.Request)
		run   func(c *Client) error
		calls int
		want  bool
	}{
		{"429 then 500 on a GET", []func(http.ResponseWriter, *http.Request){throttle, problemStep(500, "internal_error")}, getVm, 2, false},
		{"429 then 502 on a keyless POST", []func(http.ResponseWriter, *http.Request){throttle, htmlStep(502)}, createVmKeyless, 2, false},
		{"429 then 502 on a keyed POST, until the limit", []func(http.ResponseWriter, *http.Request){throttle, htmlStep(502)}, createVmKeyed, 5, true},
		{"429 then 502 on a GET, until the limit", []func(http.ResponseWriter, *http.Request){throttle, htmlStep(502)}, getVm, 5, true},
		{"429 five times", []func(http.ResponseWriter, *http.Request){throttle}, getVm, 5, true},
		{"429 then 503 not_ready", []func(http.ResponseWriter, *http.Request){throttle, problemStep(503, "not_ready")}, getVm, 2, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, sc, _ := newScriptedClient(t, tc.steps...)
			err := tc.run(c)
			apiErr := asAPIError(t, err)
			if sc.count() != tc.calls {
				t.Errorf("calls = %d, want %d", sc.count(), tc.calls)
			}
			if got := strings.Contains(apiErr.Error(), "gave up after"); got != tc.want {
				t.Errorf("Error() = %q; gave-up suffix = %v, want %v", apiErr, got, tc.want)
			}
		})
	}
}

func getVm(c *Client) error { _, err := c.GetVm(context.Background(), "vm-1"); return err }

func createVmKeyless(c *Client) error {
	_, _, err := c.CreateVm(context.Background(), VmCreateRequest{Name: "web-01"}, "")
	return err
}

func createVmKeyed(c *Client) error {
	_, _, err := c.CreateVm(context.Background(), VmCreateRequest{Name: "web-01"}, "key-1")
	return err
}

// A 404 that is not a VirtScale problem answer came from somewhere else: a mistyped endpoint. The error says where to look.
func TestAPIError_ANotFoundThatIsNotFromTheAPINamesTheEndpoint(t *testing.T) {
	const hint = "check VIRTSCALE_ENDPOINT (or the provider's endpoint): the request did not reach the VirtScale API, " +
		"whose address is https://api.virtscale.nl; unset it to use that"
	cases := []struct {
		name string
		step func(http.ResponseWriter, *http.Request)
		want string
	}{
		{"a JSON body that is no problem", jsonStep(404, `{"detail":"Not Found"}`), `virtscale API 404 error: {"detail":"Not Found"}; ` + hint},
		{"a page", htmlStep(404), "virtscale API 404 error: <html><body>Not Found</body></html>; " + hint},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _ := newScriptedClient(t, tc.step)
			_, err := c.GetVm(context.Background(), "vm-1")
			if err == nil || err.Error() != tc.want {
				t.Errorf("Error() = %v\nwant        %q", err, tc.want)
			}
		})
	}

	// The API's unknown route is a problem with the generic detail "Not Found": the same mistake, the same hint.
	c := failWith(t, 404, "not_found", "Not Found")
	_, err := c.GetVm(context.Background(), "vm-1")
	if want := "virtscale API 404 not_found: Not Found (request req-7); " + hint; err == nil || err.Error() != want {
		t.Errorf("Error() = %v\nwant        %q", err, want)
	}

	// A proper not_found of the API keeps its own text, with no hint.
	c = failWith(t, 404, "not_found", "no vm matching that identifier is visible to this organization")
	_, err = c.GetVm(context.Background(), "vm-1")
	if err == nil || strings.Contains(err.Error(), "VIRTSCALE_ENDPOINT") {
		t.Errorf("Error() = %v, want no endpoint hint on the API's own 404", err)
	}
}

// A Terraform user does not import keys with the API: the error says to manage the key as a resource.
func TestAPIError_SshKeyNotFoundPointsAtTheResource(t *testing.T) {
	c := failWith(t, 422, "ssh_key_not_found", "ssh key 'laptop' not found: import it with POST /v1/orgs/x/compute/ssh-keys")

	_, err := c.GetVm(context.Background(), "vm-1")
	want := "virtscale API 422 ssh_key_not_found: ssh key 'laptop' not found: import it with POST /v1/orgs/x/compute/ssh-keys (request req-7); " +
		"create the key with a virtscale_ssh_key resource and name it in ssh_keys"
	if err == nil || err.Error() != want {
		t.Errorf("Error() = %v\nwant        %q", err, want)
	}
}

func TestGetComputeLimits_ReadsTheGrid(t *testing.T) {
	var path string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"vcpus_steps":[1,2,4],"memory_gb":{"min":1,"max":128,"step":1},"disk_gb":{"min":10,"max":2000,"step":10}}`))
	})

	got, err := c.GetComputeLimits(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if path != "/v1/orgs/test-org/compute/limits" {
		t.Errorf("path = %q", path)
	}
	if len(got.VCPUsSteps) != 3 || got.VCPUsSteps[2] != 4 || got.MemoryGB != (Range{1, 128, 1}) || got.DiskGB != (Range{10, 2000, 10}) {
		t.Errorf("limits = %+v", got)
	}
}
