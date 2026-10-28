package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// hostnamePtr is the *string the hostname requests carry.
func hostnamePtr(s string) *string { return &s }

// --- routes: the instances resource, never the old vms route ---------------

// The customer API names the resource "instances" (/compute/instances). The
// old /compute/vms path is gone: every call must go to the new route, so a
// stale path shows up here instead of as a 404 in the field.
func TestVmRoutes_UseComputeInstances(t *testing.T) {
	c, sc, _ := newScriptedClient(t,
		jsonStep(http.StatusAccepted, testVmAcceptedJSON),
		jsonStep(http.StatusOK, testVmJSON),
		jsonStep(http.StatusAccepted, testVmAcceptedJSON),
		jsonStep(http.StatusOK, `{"items":[`+testVmJSON+`],"next_cursor":null}`),
	)

	if _, _, err := c.CreateVm(context.Background(), VmCreateRequest{Name: "web-01"}, ""); err != nil {
		t.Fatalf("CreateVm() error = %v", err)
	}
	if _, err := c.GetVm(context.Background(), "vm-1"); err != nil {
		t.Fatalf("GetVm() error = %v", err)
	}
	if _, _, err := c.DeleteVm(context.Background(), "vm-1"); err != nil {
		t.Fatalf("DeleteVm() error = %v", err)
	}
	if _, err := c.ListVms(context.Background()); err != nil {
		t.Fatalf("ListVms() error = %v", err)
	}

	want := []recordedCall{
		{method: http.MethodPost, path: "/v1/orgs/test-org/compute/instances"},
		{method: http.MethodGet, path: "/v1/orgs/test-org/compute/instances/vm-1"},
		{method: http.MethodDelete, path: "/v1/orgs/test-org/compute/instances/vm-1"},
		{method: http.MethodGet, path: "/v1/orgs/test-org/compute/instances"},
	}
	if sc.count() != len(want) {
		t.Fatalf("calls = %d, want %d", sc.count(), len(want))
	}
	for i, w := range want {
		got := sc.call(i)
		if got.method != w.method || got.path != w.path {
			t.Errorf("call %d = %s %s, want %s %s", i+1, got.method, got.path, w.method, w.path)
		}
		if strings.Contains(got.path, "/compute/vms") {
			t.Errorf("call %d went to the old route: %s %s", i+1, got.method, got.path)
		}
	}
}

// --- the decoded instance ------------------------------------------------

// The instance carries the provider-neutral fields; a missing optional one is
// a nil pointer rather than an empty string, so "no hostname" stays different
// from "the empty hostname".
func TestVm_DecodesTheNewFields(t *testing.T) {
	c, _, _ := newScriptedClient(t,
		jsonStep(http.StatusOK, testVmJSON),
		jsonStep(http.StatusOK, `{"id":"vm-2","hostname":null,"instance_type":null,"private_ip":null,"addresses":[],"generation":0}`),
	)

	vm, err := c.GetVm(context.Background(), "vm-1")
	if err != nil {
		t.Fatalf("GetVm() error = %v", err)
	}
	if vm == nil || vm.Hostname == nil || *vm.Hostname != "web-01.example.com" {
		t.Errorf("Hostname = %+v, want web-01.example.com", vm)
	}
	if vm == nil || vm.InstanceType == nil || *vm.InstanceType != "general.small" {
		t.Errorf("InstanceType = %+v, want general.small", vm)
	}
	if vm == nil || vm.Status != "running" {
		t.Errorf("Status = %q, want running", vm.Status)
	}
	if vm == nil || vm.PrivateIP == nil || *vm.PrivateIP != "10.128.3.11" {
		t.Errorf("PrivateIP = %+v, want 10.128.3.11", vm)
	}
	if vm == nil || len(vm.Addresses) != 1 || vm.Addresses[0] != "10.128.3.11" {
		t.Errorf("Addresses = %v, want [10.128.3.11]", vm.Addresses)
	}
	if vm == nil || vm.Generation != 3 {
		t.Errorf("Generation = %d, want 3", vm.Generation)
	}

	empty, err := c.GetVm(context.Background(), "vm-2")
	if err != nil {
		t.Fatalf("GetVm() error = %v", err)
	}
	if empty == nil {
		t.Fatal("GetVm() = nil, want the decoded instance")
	}
	if empty.Hostname != nil || empty.InstanceType != nil || empty.PrivateIP != nil {
		t.Errorf("Hostname/InstanceType/PrivateIP = %v/%v/%v, want nil for all three", empty.Hostname, empty.InstanceType, empty.PrivateIP)
	}
	if len(empty.Addresses) != 0 {
		t.Errorf("Addresses = %v, want none", empty.Addresses)
	}
}

// observed_status and observed_addresses are deprecated: the API still sends
// them, but nothing in Vm may read them. The decoys in testVmJSON make a field
// that still does visible in %+v.
func TestVm_DoesNotReadTheDeprecatedObservedFields(t *testing.T) {
	typ := reflect.TypeOf(Vm{})
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if strings.HasPrefix(name, "observed_") {
			t.Errorf("Vm.%s still carries the deprecated json key %q", field.Name, name)
		}
	}

	c, _, _ := newScriptedClient(t, jsonStep(http.StatusOK, testVmJSON))
	vm, err := c.GetVm(context.Background(), "vm-1")
	if err != nil {
		t.Fatalf("GetVm() error = %v", err)
	}
	if vm == nil {
		t.Fatal("GetVm() = nil, want the decoded instance")
	}
	rendered := fmt.Sprintf("%+v", *vm)
	if strings.Contains(rendered, "198.51.100.9") || strings.Contains(rendered, "ACTIVE") {
		t.Errorf("the deprecated observed_* decoys reached the decoded instance: %s", rendered)
	}
}

// A create has no hostname unless the caller sets one: the body omits the key
// rather than sending "" or null, which the API would take for a value.
func TestCreateVm_SendsTheHostnameOnlyWhenSet(t *testing.T) {
	var body map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body = nil
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(testVmAcceptedJSON))
	})

	if _, _, err := c.CreateVm(context.Background(), VmCreateRequest{Name: "web-01", Hostname: hostnamePtr("web.example.com")}, ""); err != nil {
		t.Fatalf("CreateVm() with a hostname: error = %v", err)
	}
	if got, ok := body["hostname"]; !ok || got != "web.example.com" {
		t.Errorf("body = %v, want hostname=web.example.com", body)
	}

	if _, _, err := c.CreateVm(context.Background(), VmCreateRequest{Name: "web-01"}, ""); err != nil {
		t.Fatalf("CreateVm() without a hostname: error = %v", err)
	}
	if _, ok := body["hostname"]; ok {
		t.Errorf("body = %v, want no hostname key when Hostname is nil", body)
	}
}

// --- UpdateVmHostname: the request ---------------------------------------

// The PATCH names the instance and sends exactly {"hostname": ...}; the
// precondition travels as If-Match, the generation in double quotes, and the
// idempotency key only when there is one.
func TestUpdateVmHostname_SendsPatchWithIfMatchAndKey(t *testing.T) {
	type sent struct {
		method, path, ifMatch, idempotencyKey, contentType, authorization, body string
		keyHeaderSent                                                           bool
	}
	cases := []struct {
		name           string
		hostname       *string
		idempotencyKey string
		wantBody       string
		wantKeyHeader  bool
	}{
		{"with a hostname", hostnamePtr("web.example.com"), "key-1", `{"hostname":"web.example.com"}`, true},
		{"with a nil hostname", nil, "key-1", `{"hostname":null}`, true},
		{"without an idempotency key", hostnamePtr("web.example.com"), "", `{"hostname":"web.example.com"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got sent
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				_, got.keyHeaderSent = r.Header["Idempotency-Key"]
				got.method = r.Method
				got.path = r.URL.Path
				got.ifMatch = r.Header.Get("If-Match")
				got.idempotencyKey = r.Header.Get("Idempotency-Key")
				got.contentType = r.Header.Get("Content-Type")
				got.authorization = r.Header.Get("Authorization")
				got.body = string(body)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(testVmJSON))
			})

			vm, err := c.UpdateVmHostname(context.Background(), "vm-1", 7, tc.hostname, tc.idempotencyKey)
			if err != nil {
				t.Fatalf("UpdateVmHostname() error = %v", err)
			}
			if got.method != http.MethodPatch {
				t.Errorf("method = %s, want PATCH", got.method)
			}
			if got.path != "/v1/orgs/test-org/compute/instances/vm-1" {
				t.Errorf("path = %s, want /v1/orgs/test-org/compute/instances/vm-1", got.path)
			}
			if got.ifMatch != `"7"` {
				t.Errorf("If-Match = %q, want %q (the generation in double quotes)", got.ifMatch, `"7"`)
			}
			if got.authorization != "Bearer test-key" {
				t.Errorf("Authorization = %q, want Bearer test-key", got.authorization)
			}
			if got.contentType != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got.contentType)
			}
			if got.body != tc.wantBody {
				t.Errorf("body = %s, want exactly %s", got.body, tc.wantBody)
			}
			if got.keyHeaderSent != tc.wantKeyHeader {
				t.Errorf("Idempotency-Key sent = %v, want %v", got.keyHeaderSent, tc.wantKeyHeader)
			}
			if tc.wantKeyHeader && got.idempotencyKey != tc.idempotencyKey {
				t.Errorf("Idempotency-Key = %q, want %q", got.idempotencyKey, tc.idempotencyKey)
			}
			if vm == nil || vm.ID != "vm-1" || vm.Generation != 3 || vm.Hostname == nil || *vm.Hostname != "web-01.example.com" {
				t.Errorf("vm = %+v, want the decoded instance from the 200 body", vm)
			}
		})
	}
}

// --- UpdateVmHostname: the refusals --------------------------------------

// The API's refusals are decisions about the request: repeated once, kept
// word for word, and nothing may have been applied.
func TestUpdateVmHostname_RefusalsKeepTheAPIsWordsAndAreNotRetried(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{
		{http.StatusConflict, "hostname_taken"},
		{http.StatusPreconditionFailed, "generation_mismatch"},
		{http.StatusPreconditionRequired, "precondition_required"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			c, sc, _ := newScriptedClient(t, problemStep(tc.status, tc.code))

			_, err := c.UpdateVmHostname(context.Background(), "vm-1", 3, hostnamePtr("web.example.com"), "key-1")
			apiErr := asAPIError(t, err)
			if apiErr.Status != tc.status || apiErr.Code != tc.code {
				t.Errorf("err = %v, want the %d %s", apiErr, tc.status, tc.code)
			}
			if !strings.Contains(err.Error(), "detail of "+tc.code) {
				t.Errorf("Error() = %q, want it to keep the API's detail", err.Error())
			}
			if sc.count() != 1 {
				t.Errorf("calls = %d, want 1", sc.count())
			}
			if apiErr.MayHaveBeenApplied {
				t.Errorf("MayHaveBeenApplied = true for a refusal: nothing may have been applied")
			}
		})
	}
}

// --- UpdateVmHostname: retries -------------------------------------------

// patchAttempt is one send of the hostname PATCH, as the server saw it.
type patchAttempt struct {
	method, path, body, ifMatch, key string
}

// A keyed PATCH is repeated on what may have been applied (a gateway error, a
// lost connection) and on a 429; every attempt carries the same body, If-Match
// and key, so the API replays the first answer instead of applying the change
// twice.
func TestUpdateVmHostname_AKeyedRequestIsRepeatedWithTheSameBodyIfMatchAndKey(t *testing.T) {
	var mu sync.Mutex
	var attempts []patchAttempt
	steps := []func(http.ResponseWriter, *http.Request){
		htmlStep(http.StatusBadGateway),
		dropStep,
		problemStep(http.StatusTooManyRequests, "rate_limited"),
		jsonStep(http.StatusOK, testVmJSON),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		i := len(attempts)
		attempts = append(attempts, patchAttempt{
			method:  r.Method,
			path:    r.URL.Path,
			body:    string(body),
			ifMatch: r.Header.Get("If-Match"),
			key:     r.Header.Get("Idempotency-Key"),
		})
		step := steps[min(i, len(steps)-1)]
		mu.Unlock()
		step(w, r)
	}))
	t.Cleanup(srv.Close)

	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	c, err := New(srv.URL, "test-key", "test-org", WithHTTPClient(hc))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	c.retry.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }

	vm, err := c.UpdateVmHostname(context.Background(), "vm-1", 7, hostnamePtr("web.example.com"), "key-1")
	if err != nil {
		t.Fatalf("UpdateVmHostname() error = %v", err)
	}
	if vm == nil || vm.Generation != 3 {
		t.Errorf("vm = %+v, want the decoded instance from the last answer", vm)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(attempts) != 4 {
		t.Fatalf("attempts = %d, want 4", len(attempts))
	}
	first := attempts[0]
	want := `{"hostname":"web.example.com"}`
	if first.method != http.MethodPatch || first.path != "/v1/orgs/test-org/compute/instances/vm-1" ||
		first.body != want || first.ifMatch != `"7"` || first.key != "key-1" {
		t.Fatalf("first attempt = %+v, want the PATCH with body %s, If-Match %q and key key-1", first, want, `"7"`)
	}
	for i, got := range attempts[1:] {
		if got != first {
			t.Errorf("attempt %d = %+v, want the same body, If-Match and key as the first", i+2, got)
		}
	}
}

// Without a key the PATCH cannot be repeated after a gateway error: the client
// cannot know whether the change ran, and sends exactly one request. A 429 was
// refused before it ran, so that one is retried for every method.
func TestUpdateVmHostname_AnUnkeyedRequestIsNotRepeatedAfterAGatewayError(t *testing.T) {
	c, sc, _ := newScriptedClient(t, htmlStep(http.StatusBadGateway), jsonStep(http.StatusOK, testVmJSON))

	_, err := c.UpdateVmHostname(context.Background(), "vm-1", 3, hostnamePtr("web.example.com"), "")
	apiErr := asAPIError(t, err)
	if apiErr.Status != http.StatusBadGateway {
		t.Errorf("err = %v, want the 502", apiErr)
	}
	if sc.count() != 1 {
		t.Errorf("calls = %d, want exactly 1: an unkeyed PATCH is not repeated after a gateway error", sc.count())
	}
	if apiErr.MayHaveBeenApplied {
		t.Errorf("MayHaveBeenApplied = true for an unkeyed PATCH")
	}
	if strings.Contains(err.Error(), "may still have been applied") {
		t.Errorf("err = %q, want no recovery hint without a key", err.Error())
	}

	c2, sc2, _ := newScriptedClient(t,
		problemStep(http.StatusTooManyRequests, "rate_limited", "Retry-After", "1"),
		jsonStep(http.StatusOK, testVmJSON),
	)
	if _, err := c2.UpdateVmHostname(context.Background(), "vm-1", 3, hostnamePtr("web.example.com"), ""); err != nil {
		t.Fatalf("UpdateVmHostname() error = %v, want success on the second call", err)
	}
	if sc2.count() != 2 {
		t.Errorf("calls = %d, want 2: a 429 is retried for every method", sc2.count())
	}
}

// hostnameHint is what a keyed PATCH that gave up on an answer that leaves
// open that it was applied says instead of the create hint. The sentence is
// pinned here, apart from the client's own, so a different wording fails.
const hostnameHint = "the first request may still have been applied: " +
	"run another plan to see the instance's current hostname"

// A keyed PATCH that gives up on a gateway error or a lost connection says the
// first request may still have been applied, and where to look. It must not
// borrow the create hint: nothing was created, so tofu import would send the
// user looking for a resource that does not exist.
func TestUpdateVmHostname_RecoveryHint(t *testing.T) {
	call := func(c *Client) error {
		_, err := c.UpdateVmHostname(context.Background(), "vm-1", 3, hostnamePtr("web.example.com"), "key-1")
		return err
	}

	for _, f := range []struct {
		name string
		step func(http.ResponseWriter, *http.Request)
	}{
		{"502 from a proxy", htmlStep(http.StatusBadGateway)},
		{"a dropped connection", dropStep},
	} {
		t.Run(f.name, func(t *testing.T) {
			c, sc, _ := newScriptedClient(t, f.step)

			err := call(c)
			if err == nil || sc.count() != 5 {
				t.Fatalf("err = %v, calls = %d, want an error after 5 calls", err, sc.count())
			}
			msg := err.Error()
			if !strings.HasSuffix(msg, hostnameHint) {
				t.Errorf("the message does not end with the hostname hint:\n%s", msg)
			}
			if strings.Count(msg, "gave up after 5 attempts") != 1 {
				t.Errorf("the message should say once that it gave up after 5 attempts:\n%s", msg)
			}
			if strings.Contains(msg, "tofu import") || strings.Contains(msg, "may still have created") {
				t.Errorf("the message carries the create hint:\n%s", msg)
			}
			// A transport error is not an *APIError, so only the message can
			// say it may have been applied; an API error says it in the field.
			if apiErr := (*APIError)(nil); errors.As(err, &apiErr) && !apiErr.MayHaveBeenApplied {
				t.Errorf("MayHaveBeenApplied = false, want true: the first request may still have been applied")
			}
		})
	}

	// The other outcomes leave nothing open: the request was refused before it
	// ran (429), the feature is off (503 not_ready), or the API decided against
	// it (409 hostname_taken, 412 generation_mismatch, and the 502 of an
	// unkeyed PATCH the client never repeats).
	for _, tc := range []struct {
		name      string
		key       string
		step      func(http.ResponseWriter, *http.Request)
		wantCalls int
	}{
		{"429 until it gives up", "key-1", problemStep(http.StatusTooManyRequests, "rate_limited", "Retry-After", "1"), 5},
		{"503 not_ready", "key-1", problemStep(http.StatusServiceUnavailable, "not_ready"), 1},
		{"409 hostname_taken", "key-1", problemStep(http.StatusConflict, "hostname_taken"), 1},
		{"412 generation_mismatch", "key-1", problemStep(http.StatusPreconditionFailed, "generation_mismatch"), 1},
		{"an unkeyed PATCH after a 502", "", htmlStep(http.StatusBadGateway), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, sc, _ := newScriptedClient(t, tc.step)

			_, err := c.UpdateVmHostname(context.Background(), "vm-1", 3, hostnamePtr("web.example.com"), tc.key)
			if err == nil {
				t.Fatal("err = nil, want an error")
			}
			if sc.count() != tc.wantCalls {
				t.Errorf("calls = %d, want %d", sc.count(), tc.wantCalls)
			}
			if strings.Contains(err.Error(), "may still have been applied") {
				t.Errorf("a recovery hint where nothing may have been applied:\n%s", err)
			}
			if apiErr := (*APIError)(nil); errors.As(err, &apiErr) && apiErr.MayHaveBeenApplied {
				t.Errorf("MayHaveBeenApplied is set on %v", err)
			}
		})
	}
}
