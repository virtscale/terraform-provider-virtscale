package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	stdlog "log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestClient(t *testing.T, handler http.HandlerFunc, opts ...Option) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "test-key", "test-org", opts...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	// No test waits for a real backoff unless it is about waiting.
	c.retry.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return c, srv
}

func TestNew_RejectsEmptyKeyOrOrg(t *testing.T) {
	if _, err := New("https://api.virtscale.nl", "", "org"); err == nil {
		t.Fatal("New() with empty apiKey: want error, got nil")
	}
	if _, err := New("https://api.virtscale.nl", "key", ""); err == nil {
		t.Fatal("New() with empty org: want error, got nil")
	}
}

func TestNew_DefaultEndpointAndTrailingSlash(t *testing.T) {
	c, err := New("", "key", "org")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if c.endpoint != "https://api.virtscale.nl" {
		t.Errorf("default endpoint = %q, want https://api.virtscale.nl", c.endpoint)
	}

	c2, err := New("https://example.test/", "key", "org")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if c2.endpoint != "https://example.test" {
		t.Errorf("endpoint with trailing slash = %q, want https://example.test", c2.endpoint)
	}
}

func TestRequest_AuthAndUserAgentHeaders(t *testing.T) {
	var gotAuth, gotAccept, gotUA string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"vm-1","org_id":"o","region_id":"gro","name":"web-01","fqdn":null,"image":{"glance_id":"g1","name":"debian-12"},"vcpus":1,"memory_mb":1024,"disk_gb":10,"storage_pool_id":"pool1","network_profile_id":"np1","state":"running","desired_state":"running","error_class":null,"error_detail":null,"observed_status":null,"observed_addresses":null,"ssh_key_names":[],"protected":false,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`))
	}, WithUserAgentVersion("0.1.0"))

	_, err := c.GetVm(context.Background(), "vm-1")
	if err != nil {
		t.Fatalf("GetVm() error = %v", err)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer test-key")
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept header = %q, want application/json", gotAccept)
	}
	if gotUA != "terraform-provider-virtscale/0.1.0" {
		t.Errorf("User-Agent header = %q, want terraform-provider-virtscale/0.1.0", gotUA)
	}
}

func TestCreateVm_SendsIdempotencyKeyAndParsesResponse(t *testing.T) {
	var gotIdemp string
	var gotBody VmCreateRequest
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotIdemp = r.Header.Get("Idempotency-Key")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"vm":{"id":"vm-1","org_id":"o","region_id":"gro","name":"web-01","fqdn":null,"image":{"glance_id":"g1","name":"debian-12"},"vcpus":1,"memory_mb":1024,"disk_gb":10,"storage_pool_id":"pool1","network_profile_id":"np1","state":"pending","desired_state":"running","error_class":null,"error_detail":null,"observed_status":null,"observed_addresses":null,"ssh_key_names":[],"protected":false,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"},"operation":{"id":"op-1","type":"vm.create","state":"running","resource_type":"vm","resource_id":"vm-1","attempt":1,"max_attempts":3,"error_class":null,"error_detail":null,"created_at":"2026-01-01T00:00:00Z","finished_at":null}}`))
	})

	req := VmCreateRequest{
		Name:           "web-01",
		Image:          "img-1",
		VCPUs:          1,
		MemoryMB:       1024,
		DiskGB:         10,
		StoragePool:    "pool1",
		Region:         "gro",
		NetworkProfile: "np1",
	}
	vm, op, err := c.CreateVm(context.Background(), req, "idem-123")
	if err != nil {
		t.Fatalf("CreateVm() error = %v", err)
	}
	if gotIdemp != "idem-123" {
		t.Errorf("Idempotency-Key header = %q, want idem-123", gotIdemp)
	}
	if gotBody.Name != "web-01" || gotBody.Image != "img-1" {
		t.Errorf("request body = %+v, want name=web-01 image=img-1", gotBody)
	}
	if vm == nil || vm.ID != "vm-1" || vm.State != "pending" {
		t.Errorf("vm = %+v, want id=vm-1 state=pending", vm)
	}
	if op == nil || op.ID != "op-1" || op.State != "running" {
		t.Errorf("operation = %+v, want id=op-1 state=running", op)
	}
}

func TestCreateVm_NoIdempotencyKeyWhenEmpty(t *testing.T) {
	var sawHeader bool
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Idempotency-Key") != "" {
			sawHeader = true
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"vm":{"id":"vm-1","org_id":"o","region_id":"gro","name":"web-01","fqdn":null,"image":{"glance_id":"g1","name":"debian-12"},"vcpus":1,"memory_mb":1024,"disk_gb":10,"storage_pool_id":"pool1","network_profile_id":"np1","state":"pending","desired_state":"running","error_class":null,"error_detail":null,"observed_status":null,"observed_addresses":null,"ssh_key_names":[],"protected":false,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"},"operation":{"id":"op-1","type":"vm.create","state":"running","resource_type":"vm","resource_id":"vm-1","attempt":1,"max_attempts":3,"error_class":null,"error_detail":null,"created_at":"2026-01-01T00:00:00Z","finished_at":null}}`))
	})
	if _, _, err := c.CreateVm(context.Background(), VmCreateRequest{Name: "web-01"}, ""); err != nil {
		t.Fatalf("CreateVm() error = %v", err)
	}
	if sawHeader {
		t.Error("Idempotency-Key header sent when idempotencyKey was empty")
	}
}

func TestAPIError_ProblemJSON(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"type":"https://api.virtscale.nl/problems/name_taken","title":"Name already in use","status":409,"detail":"an instance named web-01 already exists","code":"name_taken","request_id":"req-abc123"}`))
	})

	_, _, err := c.CreateVm(context.Background(), VmCreateRequest{Name: "web-01"}, "")
	if err == nil {
		t.Fatal("CreateVm() error = nil, want APIError")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v (%T), want *APIError", err, err)
	}
	if apiErr.Status != 409 {
		t.Errorf("Status = %d, want 409", apiErr.Status)
	}
	if apiErr.Code != "name_taken" {
		t.Errorf("Code = %q, want name_taken", apiErr.Code)
	}
	if apiErr.Detail != "an instance named web-01 already exists" {
		t.Errorf("Detail = %q", apiErr.Detail)
	}
	if apiErr.RequestID != "req-abc123" {
		t.Errorf("RequestID = %q, want req-abc123", apiErr.RequestID)
	}
	want := "virtscale API 409 name_taken: an instance named web-01 already exists (request req-abc123)"
	if apiErr.Error() != want {
		t.Errorf("Error() = %q, want %q", apiErr.Error(), want)
	}
}

func TestAPIError_NonJSONBody(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html><body>Bad Gateway</body></html>"))
	})

	_, err := c.GetVm(context.Background(), "vm-1")
	if err == nil {
		t.Fatal("GetVm() error = nil, want APIError")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v (%T), want *APIError", err, err)
	}
	if apiErr.Status != 502 {
		t.Errorf("Status = %d, want 502", apiErr.Status)
	}
	if !strings.Contains(apiErr.Error(), "502") {
		t.Errorf("Error() = %q, want it to mention 502", apiErr.Error())
	}
}

// helperSentence is the sentence of the API's not_found helper (errors.py
// not_found(resource)): the detail of every resource 404. It is spelled out
// here, apart from the client's own, so that a wrong sentence in the client
// fails a test instead of being mirrored.
func helperSentence(resource string) string {
	return "no " + resource + " matching that identifier is visible to this organization"
}

// A 404 says "this resource is gone" only when its code is not_found and its
// detail is the helper's sentence for the resource the call asked about (V4
// L4). Everything else is an error that must reach the user: a mistyped
// endpoint answers not_found with the detail "Not Found", a proxy answers with
// a page, and the helper also words organization, compute service and region.
func TestIsNotFound_OnlyTheResourcesOwnSentenceCounts(t *testing.T) {
	notFound := func(code, detail string) error { return &APIError{Status: 404, Code: code, Detail: detail} }
	resources := []string{ResourceVm, ResourcePublicIp, ResourceSshKey}

	for _, resource := range resources {
		for _, other := range resources {
			err := notFound("not_found", helperSentence(other))
			if got, want := IsNotFound(err, resource), resource == other; got != want {
				t.Errorf("IsNotFound(the %s sentence, %s) = %v, want %v", other, resource, got, want)
			}
		}
		for name, err := range map[string]error{
			"an unknown route":                     notFound("not_found", "Not Found"),
			"the organization":                     notFound("not_found", helperSentence("organization")),
			"the compute service":                  notFound("not_found", helperSentence("compute service")),
			"a region":                             notFound("not_found", helperSentence("region")),
			"another kind of resource":             notFound("not_found", helperSentence("operation")),
			"the right sentence, another code":     notFound("vm_not_found", helperSentence(resource)),
			"the right sentence, no code":          notFound("", helperSentence(resource)),
			"the right sentence with more after":   notFound("not_found", helperSentence(resource)+" (id x)"),
			"a proxy's page":                       &APIError{Status: 404, Detail: "<html><body>Not Found</body></html>"},
			"the right sentence on another status": &APIError{Status: 409, Code: "not_found", Detail: helperSentence(resource)},
			"a plain error":                        errors.New("no " + resource + " matching that identifier is visible to this organization"),
			"nil":                                  nil,
		} {
			if IsNotFound(err, resource) {
				t.Errorf("IsNotFound(%s, %s) = true, want false", name, resource)
			}
		}
		if wrapped := fmt.Errorf("waiting: %w", notFound("not_found", helperSentence(resource))); !IsNotFound(wrapped, resource) {
			t.Errorf("IsNotFound(a wrapped %s 404) = false, want true", resource)
		}
	}
}

// The same rule through the client: what a handler answers for a read of one
// resource, and whether the call reports "gone".
func TestIsNotFound_ThroughTheClient(t *testing.T) {
	problem := func(code, title, detail string) (int, string) {
		return 404, `{"type":"https://docs.virtscale.nl/s3/problems/` + code + `","title":"` + title + `","status":404,"detail":"` + detail + `","instance":"/","code":"` + code + `","request_id":"req-404"}`
	}
	reads := []struct {
		name, resource string
		read           func(*Client) error
	}{
		{"GetVm", ResourceVm, func(c *Client) error { _, err := c.GetVm(context.Background(), "x"); return err }},
		{"GetSshKey", ResourceSshKey, func(c *Client) error { _, err := c.GetSshKey(context.Background(), "x"); return err }},
		{"DeleteVm", ResourceVm, func(c *Client) error { _, _, err := c.DeleteVm(context.Background(), "x"); return err }},
		{"DeleteSshKey", ResourceSshKey, func(c *Client) error { return c.DeleteSshKey(context.Background(), "x") }},
		{"ReleasePublicIp", ResourcePublicIp, func(c *Client) error { _, _, err := c.ReleasePublicIp(context.Background(), "x"); return err }},
	}
	type answer struct {
		name, contentType, body string
		gone                    bool
	}
	for _, rd := range reads {
		_, own := problem("not_found", "Resource not found", helperSentence(rd.resource))
		_, noRoute := problem("not_found", "Not Found", "Not Found")
		answers := []answer{
			{"its own sentence", "application/problem+json", own, true},
			{"an unknown route", "application/problem+json", noRoute, false},
			{"a proxy's page", "text/html", "<html><body>Not Found</body></html>", false},
			{"a bare JSON detail", "application/json", `{"detail":"Not Found"}`, false},
			{"an empty body", "text/plain", "", false},
		}
		for _, other := range []string{"organization", "compute service", "region", "operation"} {
			_, body := problem("not_found", "Resource not found", helperSentence(other))
			answers = append(answers, answer{"the sentence for " + other, "application/problem+json", body, false})
		}

		for _, tc := range answers {
			t.Run(rd.name+"/"+tc.name, func(t *testing.T) {
				c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", tc.contentType)
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(tc.body))
				})
				err := rd.read(c)
				if !is404(err) {
					t.Fatalf("err = %v, want the 404 as an *APIError", err)
				}
				if got := IsNotFound(err, rd.resource); got != tc.gone {
					t.Errorf("IsNotFound(%v, %s) = %v, want %v", err, rd.resource, got, tc.gone)
				}
			})
		}
	}
}

func TestGetPublicIp_AbsentIsNotFound(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[{"id":"ip-1","region_id":"gro","address":"1.2.3.4","vm_id":null,"state":"allocated","error":null,"created_at":"2026-01-01T00:00:00Z"}]}`))
	})

	_, err := c.GetPublicIp(context.Background(), "ip-does-not-exist")
	if err == nil {
		t.Fatal("GetPublicIp() error = nil, want not-found error")
	}
	if !IsNotFound(err, ResourcePublicIp) {
		t.Errorf("IsNotFound(%v, public_ip) = false, want true", err)
	}
	if IsNotFound(err, ResourceVm) {
		t.Errorf("IsNotFound(%v, vm) = true, want false: the absent address is not a vm", err)
	}

	ip, err := c.GetPublicIp(context.Background(), "ip-1")
	if err != nil {
		t.Fatalf("GetPublicIp() error = %v", err)
	}
	if ip.ID != "ip-1" || ip.Address == nil || *ip.Address != "1.2.3.4" {
		t.Errorf("ip = %+v, want id=ip-1 address=1.2.3.4", ip)
	}
}

func TestWaitVm_ReturnsWhenDoneAfterNPolls(t *testing.T) {
	var calls int
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		state := "pending"
		if calls >= 3 {
			state = "running"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"vm-1","org_id":"o","region_id":"gro","name":"web-01","fqdn":null,"image":{"glance_id":"g1","name":"debian-12"},"vcpus":1,"memory_mb":1024,"disk_gb":10,"storage_pool_id":"pool1","network_profile_id":"np1","state":"` + state + `","desired_state":"running","error_class":null,"error_detail":null,"observed_status":null,"observed_addresses":null,"ssh_key_names":[],"protected":false,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	vm, err := c.WaitVm(ctx, "vm-1", 5*time.Millisecond, func(vm *Vm) (bool, error) {
		return vm != nil && vm.State == "running", nil
	})
	if err != nil {
		t.Fatalf("WaitVm() error = %v", err)
	}
	if vm == nil || vm.State != "running" {
		t.Errorf("vm = %+v, want state=running", vm)
	}
	if calls < 3 {
		t.Errorf("calls = %d, want >= 3", calls)
	}
}

func TestWaitVm_DeadlineReturnsErrorWithLastState(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"vm-1","org_id":"o","region_id":"gro","name":"web-01","fqdn":null,"image":{"glance_id":"g1","name":"debian-12"},"vcpus":1,"memory_mb":1024,"disk_gb":10,"storage_pool_id":"pool1","network_profile_id":"np1","state":"pending","desired_state":"running","error_class":null,"error_detail":null,"observed_status":null,"observed_addresses":null,"ssh_key_names":[],"protected":false,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	_, err := c.WaitVm(ctx, "vm-1", 5*time.Millisecond, func(vm *Vm) (bool, error) {
		return false, nil
	})
	if err == nil {
		t.Fatal("WaitVm() error = nil, want deadline error")
	}
	if !strings.Contains(err.Error(), "pending") {
		t.Errorf("err = %q, want it to mention last state %q", err.Error(), "pending")
	}
}

func TestWaitVm_404PassedAsNilVm(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"type":"https://docs.virtscale.nl/s3/problems/not_found","title":"Resource not found","status":404,"code":"not_found","detail":"` + helperSentence("vm") + `","request_id":"req-9"}`))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var sawNil bool
	vm, err := c.WaitVm(ctx, "vm-1", 5*time.Millisecond, func(vm *Vm) (bool, error) {
		if vm == nil {
			sawNil = true
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("WaitVm() error = %v", err)
	}
	if vm != nil {
		t.Errorf("vm = %+v, want nil", vm)
	}
	if !sawNil {
		t.Error("done() was never called with vm == nil on 404")
	}
}

// A wait that polls a vm ends on "gone" only when the 404 is about the vm. A
// mistyped endpoint (an unknown route) is the caller's error, returned at once:
// done is never asked, and nothing reports a deletion nobody saw.
func TestWaitVm_AnUnknownRouteIsNotADeletedVm(t *testing.T) {
	c, sc, _ := newScriptedClient(t, problemStep(404, "not_found"))
	// problemStep words the detail "detail of not_found": not the vm sentence.

	asked := 0
	vm, err := c.WaitVm(context.Background(), "vm-1", time.Millisecond, func(v *Vm) (bool, error) {
		asked++
		return v == nil, nil
	})
	if vm != nil || err == nil || !is404(err) {
		t.Fatalf("WaitVm() = %v, %v; want no vm and the 404 as an error", vm, err)
	}
	if asked != 0 || sc.count() != 1 {
		t.Errorf("done asked %d times over %d calls, want 0 and 1: a 404 that is not about the vm ends the wait at once", asked, sc.count())
	}
}

func TestWaitPublicIp_AnAbsentAddressIsPassedAsNil(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"next_cursor":null}`))
	})

	var sawNil bool
	ip, err := c.WaitPublicIp(context.Background(), "ip-1", time.Millisecond, func(p *PublicIp) (bool, error) {
		sawNil = p == nil
		return p == nil, nil
	})
	if err != nil || ip != nil || !sawNil {
		t.Errorf("WaitPublicIp() = %v, %v (done saw nil: %v), want no address, no error, and done told it is absent", ip, err, sawNil)
	}
}

func TestWaitPublicIp_AnUnknownRouteIsNotAReleasedAddress(t *testing.T) {
	c, sc, _ := newScriptedClient(t, problemStep(404, "not_found"))

	asked := 0
	ip, err := c.WaitPublicIp(context.Background(), "ip-1", time.Millisecond, func(p *PublicIp) (bool, error) {
		asked++
		return p == nil, nil
	})
	if ip != nil || err == nil || !is404(err) {
		t.Fatalf("WaitPublicIp() = %v, %v; want no address and the 404 as an error", ip, err)
	}
	if asked != 0 || sc.count() != 1 {
		t.Errorf("done asked %d times over %d calls, want 0 and 1", asked, sc.count())
	}
}

// is404 reports whether err is an *APIError with status 404, whatever it says.
func is404(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}

func TestAPIError_FieldErrorsAreKeptAndShown(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"https://docs.virtscale.nl/s3/problems/invalid_request","title":"Invalid request","status":400,"detail":"the request body or parameters failed validation","instance":"/v1/orgs/x/compute/vms","code":"invalid_request","request_id":"req-9","errors":[{"field":"vcpus","message":"Input should be greater than or equal to 1"},{"field":"name","message":"Field required"}]}`))
	})

	_, _, err := c.CreateVm(context.Background(), VmCreateRequest{Name: "web-01"}, "")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v (%T), want *APIError", err, err)
	}
	if len(apiErr.Errors) != 2 || apiErr.Errors[0] != (FieldError{Field: "vcpus", Message: "Input should be greater than or equal to 1"}) ||
		apiErr.Errors[1] != (FieldError{Field: "name", Message: "Field required"}) {
		t.Errorf("Errors = %+v, want the two field errors in order", apiErr.Errors)
	}
	want := "virtscale API 400 invalid_request: the request body or parameters failed validation: " +
		"vcpus: Input should be greater than or equal to 1; name: Field required (request req-9)"
	if apiErr.Error() != want {
		t.Errorf("Error() = %q\nwant      %q", apiErr.Error(), want)
	}
}

// An errors[] entry may name no field (a rule about the body as a whole): the
// message is shown without a "field:" prefix.
func TestAPIError_FieldErrorWithoutAField(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"t","title":"Invalid request","status":400,"detail":"the request body or parameters failed validation","code":"invalid_request","request_id":"req-3","errors":[{"field":"","message":"JSON decode error"}]}`))
	})

	_, _, err := c.CreateVm(context.Background(), VmCreateRequest{Name: "web-01"}, "")
	want := "virtscale API 400 invalid_request: the request body or parameters failed validation: JSON decode error (request req-3)"
	if err == nil || err.Error() != want {
		t.Errorf("Error() = %v\nwant        %q", err, want)
	}
}

// The API answers 404 not_found for a missing vm and for an organization the
// key cannot see (deps.py, errors.not_found); only the detail differs. Reading
// the second as "the resource is gone" would drop real resources from state.
func TestIsNotFound_AnUnseenOrganizationIsNotAMissingResource(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		detail := "no vm matching that identifier is visible to this organization"
		if strings.Contains(r.URL.Path, "/orgs/other-org/") {
			detail = "no organization matching that identifier is visible to this organization"
		}
		_, _ = w.Write([]byte(`{"type":"https://docs.virtscale.nl/s3/problems/not_found","title":"Resource not found","status":404,"detail":"` + detail + `","instance":"` + r.URL.Path + `","code":"not_found","request_id":"req-1"}`))
	})

	_, err := c.GetVm(context.Background(), "vm-1")
	if !IsNotFound(err, ResourceVm) || IsOrganizationNotVisible(err) {
		t.Errorf("missing vm: IsNotFound=%v IsOrganizationNotVisible=%v, want true/false", IsNotFound(err, ResourceVm), IsOrganizationNotVisible(err))
	}

	other, err := New(c.endpoint, "test-key", "other-org")
	if err != nil {
		t.Fatal(err)
	}
	_, err = other.GetVm(context.Background(), "vm-1")
	if IsNotFound(err, ResourceVm) || !IsOrganizationNotVisible(err) {
		t.Errorf("unseen organization: IsNotFound=%v IsOrganizationNotVisible=%v, want false/true", IsNotFound(err, ResourceVm), IsOrganizationNotVisible(err))
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 404 {
		t.Errorf("the error must still be the *APIError (404), got %v", err)
	}
}

func TestGetPublicIp_AbsentUsesTheAPIsNotFoundCode(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"next_cursor":null}`))
	})
	_, err := c.GetPublicIp(context.Background(), "ip-9")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 404 || apiErr.Code != "not_found" {
		t.Errorf("err = %+v, want a 404 with the API's own code not_found", apiErr)
	}
	if apiErr != nil && apiErr.Detail != helperSentence("public_ip") {
		t.Errorf("Detail = %q, want the helper's sentence %q", apiErr.Detail, helperSentence("public_ip"))
	}
}

// --- retries (the AWS SDK's standard mode, adapted to this API) ------------

const (
	testVmJSON        = `{"id":"vm-1","org_id":"o","region_id":"gro","name":"web-01","fqdn":null,"image":{"glance_id":"g1","name":"debian-12"},"vcpus":1,"memory_mb":1024,"disk_gb":10,"storage_pool_id":"pool1","network_profile_id":"np1","state":"running","desired_state":"running","error_class":null,"error_detail":null,"observed_status":null,"observed_addresses":null,"ssh_key_names":[],"protected":false,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`
	testOperationJSON = `{"id":"op-1","type":"vm.create","state":"pending","resource_type":"vm","resource_id":"vm-1","attempt":0,"max_attempts":3,"error_class":null,"error_detail":null,"created_at":"2026-01-01T00:00:00Z","finished_at":null}`
	testIPJSON        = `{"id":"ip-1","region_id":"gro","address":null,"vm_id":null,"state":"pending","error":null,"created_at":"2026-01-01T00:00:00Z"}`
)

const testSshKeyJSON = `{"id":"key-1","name":"laptop","key_type":"ssh-ed25519","fingerprint":"SHA256:abc","public_key":"ssh-ed25519 AAAA","created_at":"2026-01-01T00:00:00Z"}`

var (
	testVmAcceptedJSON = `{"vm":` + testVmJSON + `,"operation":` + testOperationJSON + `}`
	testIPAcceptedJSON = `{"public_ip":` + testIPJSON + `,"operation":` + testOperationJSON + `}`
)

type recordedCall struct {
	method, path, key, body string
}

// scripted serves its steps in order, one per request; the last step repeats.
// It records every request, so a test can say how many calls were made and what
// each carried.
type scripted struct {
	mu    sync.Mutex
	calls []recordedCall
	steps []func(http.ResponseWriter, *http.Request)
}

func (s *scripted) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	i := len(s.calls)
	s.calls = append(s.calls, recordedCall{method: r.Method, path: r.URL.Path, key: r.Header.Get("Idempotency-Key"), body: string(body)})
	step := s.steps[min(i, len(s.steps)-1)]
	s.mu.Unlock()
	step(w, r)
}

func (s *scripted) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *scripted) call(i int) recordedCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[i]
}

// sleepLog records the waits a client asked for instead of waiting.
type sleepLog struct {
	mu sync.Mutex
	d  []time.Duration
}

func (l *sleepLog) sleep(ctx context.Context, d time.Duration) error {
	l.mu.Lock()
	l.d = append(l.d, d)
	l.mu.Unlock()
	return ctx.Err()
}

func (l *sleepLog) all() []time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]time.Duration(nil), l.d...)
}

func jsonStep(status int, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// problemStep is an API problem+json answer; headers are name/value pairs.
func problemStep(status int, code string, headers ...string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Set(headers[i], headers[i+1])
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"type":"https://docs.virtscale.nl/s3/problems/` + code + `","title":"` + http.StatusText(status) + `","status":` + itoa(status) + `,"detail":"detail of ` + code + `","code":"` + code + `","request_id":"req-` + code + `"}`))
	}
}

// htmlStep is what a proxy in front of the API answers.
func htmlStep(status int) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(status)
		_, _ = w.Write([]byte("<html><body>" + http.StatusText(status) + "</body></html>"))
	}
}

// dropStep closes the connection without answering: a transport error.
func dropStep(w http.ResponseWriter, _ *http.Request) {
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close()
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// noContentStep is the 204 of a delete that has nothing to say.
func noContentStep(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }

// newScriptedClient returns a client whose waits are recorded, not slept, and
// which never reuses a connection (so a dropped one is a visible transport
// error, not a silent net/http resend).
func newScriptedClient(t *testing.T, steps ...func(http.ResponseWriter, *http.Request)) (*Client, *scripted, *sleepLog) {
	t.Helper()
	sc := &scripted{steps: steps}
	log := &sleepLog{}
	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	c, _ := newTestClient(t, sc.handler, WithHTTPClient(hc))
	c.retry.sleep = log.sleep
	return c, sc, log
}

// op is one client call under test, with the verb it sends.
type op struct {
	name   string
	method string
	keyed  bool // carries an Idempotency-Key
	ok     func(http.ResponseWriter, *http.Request)
	run    func(c *Client) error
}

func testOps() []op {
	return []op{
		{"GetVm", http.MethodGet, false, jsonStep(200, testVmJSON), func(c *Client) error { _, err := c.GetVm(context.Background(), "vm-1"); return err }},
		{"ListImages", http.MethodGet, false, jsonStep(200, `{"items":[],"next_cursor":null}`), func(c *Client) error { _, err := c.ListImages(context.Background(), "gro"); return err }},
		{"DeleteVm", http.MethodDelete, false, jsonStep(202, testVmAcceptedJSON), func(c *Client) error { _, _, err := c.DeleteVm(context.Background(), "vm-1"); return err }},
		{"ReleasePublicIp", http.MethodDelete, false, jsonStep(202, testIPAcceptedJSON), func(c *Client) error { _, _, err := c.ReleasePublicIp(context.Background(), "ip-1"); return err }},
		{"CreateVm (keyed)", http.MethodPost, true, jsonStep(202, testVmAcceptedJSON), func(c *Client) error {
			_, _, err := c.CreateVm(context.Background(), VmCreateRequest{Name: "web-01", Image: "i", VCPUs: 1, MemoryMB: 1024, DiskGB: 10, StoragePool: "p", Region: "gro", NetworkProfile: "n"}, "key-1")
			return err
		}},
		{"CreateVm (no key)", http.MethodPost, false, jsonStep(202, testVmAcceptedJSON), func(c *Client) error {
			_, _, err := c.CreateVm(context.Background(), VmCreateRequest{Name: "web-01"}, "")
			return err
		}},
		{"AllocatePublicIp", http.MethodPost, false, jsonStep(202, testIPAcceptedJSON), func(c *Client) error { _, _, err := c.AllocatePublicIp(context.Background(), "gro"); return err }},
		{"AssociatePublicIp", http.MethodPost, false, jsonStep(202, testIPAcceptedJSON), func(c *Client) error {
			_, _, err := c.AssociatePublicIp(context.Background(), "ip-1", "vm-1")
			return err
		}},
		{"DisassociatePublicIp", http.MethodPost, false, jsonStep(202, testIPAcceptedJSON), func(c *Client) error { _, _, err := c.DisassociatePublicIp(context.Background(), "ip-1"); return err }},
		{"GetSshKey", http.MethodGet, false, jsonStep(200, testSshKeyJSON), func(c *Client) error { _, err := c.GetSshKey(context.Background(), "key-1"); return err }},
		{"DeleteSshKey", http.MethodDelete, false, noContentStep, func(c *Client) error { return c.DeleteSshKey(context.Background(), "key-1") }},
		{"CreateSshKey (keyed)", http.MethodPost, true, jsonStep(201, testSshKeyJSON), func(c *Client) error {
			_, err := c.CreateSshKey(context.Background(), SshKeyCreateRequest{Name: "laptop", PublicKey: "ssh-ed25519 AAAA"}, "key-1")
			return err
		}},
	}
}

// repeatable is the rule for 502/503/504 and transport errors: only a request
// that is safe to send twice (a GET, a DELETE, or a POST carrying an
// Idempotency-Key).
func (o op) repeatable() bool {
	return o.method == http.MethodGet || o.method == http.MethodDelete || o.keyed
}

func asAPIError(t *testing.T, err error) *APIError {
	t.Helper()
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v (%T), want *APIError", err, err)
	}
	return apiErr
}

// 429 means the request was refused before it ran, so every method retries it,
// and the wait is exactly what Retry-After says.
func TestRetry_429IsRetriedForEveryMethodAndHonoursRetryAfter(t *testing.T) {
	for _, o := range testOps() {
		t.Run(o.name, func(t *testing.T) {
			c, sc, log := newScriptedClient(t, problemStep(429, "rate_limited", "Retry-After", "1"), o.ok)

			if err := o.run(c); err != nil {
				t.Fatalf("%s: err = %v, want success on the second call", o.name, err)
			}
			if sc.count() != 2 {
				t.Errorf("calls = %d, want 2", sc.count())
			}
			if got := log.all(); len(got) != 1 || got[0] != time.Second {
				t.Errorf("waits = %v, want [1s] (Retry-After: 1)", got)
			}
		})
	}
}

// A 503 whose code is not_ready says a feature flag is off; retrying cannot help.
func TestRetry_503NotReadyIsNeverRetried(t *testing.T) {
	for _, o := range testOps() {
		t.Run(o.name, func(t *testing.T) {
			c, sc, log := newScriptedClient(t, problemStep(503, "not_ready"), o.ok)

			apiErr := asAPIError(t, o.run(c))
			if apiErr.Status != 503 || apiErr.Code != "not_ready" {
				t.Errorf("err = %v, want the 503 not_ready", apiErr)
			}
			if sc.count() != 1 || len(log.all()) != 0 {
				t.Errorf("calls = %d, waits = %v, want 1 call and no wait", sc.count(), log.all())
			}
		})
	}
}

// 502, 503 and 504 may have been applied, so only a request that is safe to
// repeat is retried: a GET, a DELETE, a POST with an Idempotency-Key.
func TestRetry_GatewayErrorsOnlyForRepeatableRequests(t *testing.T) {
	failures := map[string]func(http.ResponseWriter, *http.Request){
		"502 from a proxy":              htmlStep(502),
		"504 from a proxy":              htmlStep(504),
		"503 dependency_unavailable":    problemStep(503, "dependency_unavailable"),
		"503 from a proxy (no problem)": htmlStep(503),
	}
	for failName, fail := range failures {
		for _, o := range testOps() {
			t.Run(failName+"/"+o.name, func(t *testing.T) {
				c, sc, _ := newScriptedClient(t, fail, o.ok)

				err := o.run(c)
				if o.repeatable() {
					if err != nil || sc.count() != 2 {
						t.Errorf("repeatable request: err = %v, calls = %d, want success on the second call", err, sc.count())
					}
					return
				}
				apiErr := asAPIError(t, err)
				if apiErr.Status < 502 || apiErr.Status > 504 || sc.count() != 1 {
					t.Errorf("unsafe request: err = %v, calls = %d, want the gateway error after exactly one call", err, sc.count())
				}
			})
		}
	}
}

func TestRetry_503WaitsWhatRetryAfterSays(t *testing.T) {
	c, sc, log := newScriptedClient(t, problemStep(503, "dependency_unavailable", "Retry-After", "5"), jsonStep(200, testVmJSON))

	if _, err := c.GetVm(context.Background(), "vm-1"); err != nil {
		t.Fatalf("GetVm() error = %v", err)
	}
	if got := log.all(); sc.count() != 2 || len(got) != 1 || got[0] != 5*time.Second {
		t.Errorf("calls = %d waits = %v, want 2 calls and [5s]", sc.count(), got)
	}
}

func TestRetry_TransportErrorsOnlyForRepeatableRequests(t *testing.T) {
	for _, o := range testOps() {
		t.Run(o.name, func(t *testing.T) {
			c, sc, _ := newScriptedClient(t, dropStep, o.ok)

			err := o.run(c)
			if o.repeatable() {
				if err != nil || sc.count() != 2 {
					t.Errorf("repeatable request: err = %v, calls = %d, want success on the second call", err, sc.count())
				}
				return
			}
			if err == nil || sc.count() != 1 {
				t.Errorf("unsafe request: err = %v, calls = %d, want the transport error after exactly one call", err, sc.count())
			}
		})
	}
}

// The body is rebuilt for every attempt and the Idempotency-Key is the same
// one: a drained reader on the second try would send an empty body, and a new
// key would build a second VM.
func TestRetry_EveryAttemptCarriesTheSameBodyAndKey(t *testing.T) {
	c, sc, _ := newScriptedClient(t, htmlStep(502), dropStep, problemStep(429, "rate_limited"), jsonStep(202, testVmAcceptedJSON))

	req := VmCreateRequest{Name: "web-01", Image: "img-1", VCPUs: 2, MemoryMB: 2048, DiskGB: 20, StoragePool: "pool1", Region: "gro", NetworkProfile: "np1"}
	if _, _, err := c.CreateVm(context.Background(), req, "key-77"); err != nil {
		t.Fatalf("CreateVm() error = %v", err)
	}
	if sc.count() != 4 {
		t.Fatalf("calls = %d, want 4", sc.count())
	}
	first := sc.call(0)
	if !strings.Contains(first.body, `"name":"web-01"`) || first.key != "key-77" {
		t.Fatalf("first call = %+v, want the request body and key-77", first)
	}
	for i := 1; i < 4; i++ {
		if got := sc.call(i); got.body != first.body || got.key != "key-77" || got.method != http.MethodPost {
			t.Errorf("attempt %d = %+v, want the same body and key as the first", i+1, got)
		}
	}
}

func TestRetry_GivesUpAfterFiveAttemptsWithTheAPIError(t *testing.T) {
	c, sc, log := newScriptedClient(t, problemStep(429, "rate_limited", "Retry-After", "1"))

	_, err := c.GetVm(context.Background(), "vm-1")
	apiErr := asAPIError(t, err)
	if apiErr.Status != 429 || apiErr.Code != "rate_limited" || apiErr.RequestID != "req-rate_limited" {
		t.Errorf("err = %v, want the API's own 429 rate_limited", apiErr)
	}
	if sc.count() != 5 {
		t.Errorf("calls = %d, want 5 (the maximum)", sc.count())
	}
	if got := log.all(); len(got) != 4 {
		t.Errorf("waits = %v, want 4 (one between each pair of attempts)", got)
	}
	if apiErr.Attempts != 5 || !strings.HasSuffix(apiErr.Error(), "; gave up after 5 attempts") {
		t.Errorf("Attempts = %d, Error() = %q, want it to say it gave up after 5 attempts", apiErr.Attempts, apiErr.Error())
	}
}

func TestRetry_AGatewayErrorThatNeverClearsIsReturnedAsIs(t *testing.T) {
	c, sc, _ := newScriptedClient(t, htmlStep(502))

	_, err := c.GetVm(context.Background(), "vm-1")
	apiErr := asAPIError(t, err)
	if apiErr.Status != 502 || sc.count() != 5 {
		t.Errorf("err = %v calls = %d, want the 502 after 5 calls", err, sc.count())
	}
}

func TestRetry_ATransportErrorThatNeverClearsSaysHowOftenItTried(t *testing.T) {
	c, sc, _ := newScriptedClient(t, dropStep)

	_, err := c.GetVm(context.Background(), "vm-1")
	if err == nil || sc.count() != 5 || !strings.Contains(err.Error(), "5 attempts") {
		t.Errorf("err = %v calls = %d, want a transport error mentioning 5 attempts after 5 calls", err, sc.count())
	}
}

// The wait is never allowed to run past the context deadline: the last error is
// returned at once instead. A real sleeper proves nothing slept.
func TestRetry_ContextDeadlineShorterThanRetryAfterReturnsBeforeTheDeadline(t *testing.T) {
	c, sc, _ := newScriptedClient(t, problemStep(429, "rate_limited", "Retry-After", "30"))
	c.retry.sleep = sleepContext

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.GetVm(ctx, "vm-1")
	elapsed := time.Since(start)

	apiErr := asAPIError(t, err)
	if apiErr.Status != 429 {
		t.Errorf("err = %v, want the 429 and not a context error", err)
	}
	if sc.count() != 1 || elapsed >= 300*time.Millisecond {
		t.Errorf("calls = %d, elapsed = %v, want 1 call and an immediate return (deadline 600ms, Retry-After 30s)", sc.count(), elapsed)
	}
}

func TestRetry_WaitsWhenTheDeadlineAllowsIt(t *testing.T) {
	c, sc, log := newScriptedClient(t, problemStep(429, "rate_limited", "Retry-After", "2"), jsonStep(200, testVmJSON))

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := c.GetVm(ctx, "vm-1"); err != nil {
		t.Fatalf("GetVm() error = %v", err)
	}
	if got := log.all(); sc.count() != 2 || len(got) != 1 || got[0] != 2*time.Second {
		t.Errorf("calls = %d waits = %v, want 2 calls and [2s]", sc.count(), got)
	}
}

// Cancelling the context (Ctrl-C) ends the wait at once.
func TestRetry_ContextCancelledWhileWaitingReturnsPromptly(t *testing.T) {
	c, sc, _ := newScriptedClient(t, problemStep(429, "rate_limited", "Retry-After", "20"))
	c.retry.sleep = sleepContext

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, err := c.GetVm(ctx, "vm-1")
	elapsed := time.Since(start)

	apiErr := asAPIError(t, err)
	if apiErr.Status != 429 || sc.count() != 1 {
		t.Errorf("err = %v calls = %d, want the 429 after one call", err, sc.count())
	}
	if elapsed > 2*time.Second {
		t.Errorf("elapsed = %v, want the wait to end when the context is cancelled", elapsed)
	}
}

func TestRetry_RetryAfterAsAnHTTPDate(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name, header string
		want         time.Duration
	}{
		{"in three seconds", now.Add(3 * time.Second).Format(http.TimeFormat), 3 * time.Second},
		{"already past", now.Add(-time.Hour).Format(http.TimeFormat), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, sc, log := newScriptedClient(t, problemStep(429, "rate_limited", "Retry-After", tc.header), jsonStep(200, testVmJSON))
			c.retry.now = func() time.Time { return now }

			if _, err := c.GetVm(context.Background(), "vm-1"); err != nil {
				t.Fatalf("GetVm() error = %v", err)
			}
			if got := log.all(); sc.count() != 2 || len(got) != 1 || got[0] != tc.want {
				t.Errorf("calls = %d waits = %v, want 2 calls and [%v]", sc.count(), got, tc.want)
			}
		})
	}
}

// Without a usable Retry-After the wait is exponential backoff with full
// jitter: a random draw from [0, min(cap, base*2^n)).
func TestRetry_BackoffIsExponentialFullJitterUpToTheCap(t *testing.T) {
	c, _, log := newScriptedClient(t, problemStep(503, "dependency_unavailable"))
	var ceilings []time.Duration
	c.retry.jitter = func(ceiling time.Duration) time.Duration {
		ceilings = append(ceilings, ceiling)
		return ceiling / 2
	}

	if _, err := c.GetVm(context.Background(), "vm-1"); err == nil {
		t.Fatal("GetVm() error = nil, want the 503 after the last attempt")
	}
	wantCeilings := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	if len(ceilings) != 4 {
		t.Fatalf("jitter ceilings = %v, want %v", ceilings, wantCeilings)
	}
	for i, w := range wantCeilings {
		if ceilings[i] != w {
			t.Errorf("ceiling of retry %d = %v, want %v", i+1, ceilings[i], w)
		}
	}
	for i, d := range log.all() {
		if d != wantCeilings[i]/2 {
			t.Errorf("wait %d = %v, want what the jitter returned (%v)", i+1, d, wantCeilings[i]/2)
		}
	}

	// The cap: base 10s doubles to 20s and stays there.
	c2, _, _ := newScriptedClient(t, problemStep(503, "dependency_unavailable"))
	c2.retry.baseDelay = 10 * time.Second
	var capped []time.Duration
	c2.retry.jitter = func(ceiling time.Duration) time.Duration { capped = append(capped, ceiling); return ceiling }
	_, _ = c2.GetVm(context.Background(), "vm-1")
	want := []time.Duration{10 * time.Second, 20 * time.Second, 20 * time.Second, 20 * time.Second}
	for i, w := range want {
		if i >= len(capped) || capped[i] != w {
			t.Fatalf("capped ceilings = %v, want %v", capped, want)
		}
	}
}

func TestRetry_DefaultJitterIsUniformInZeroToMax(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 500; i++ {
		d := defaultJitter(time.Second)
		if d < 0 || d >= time.Second {
			t.Fatalf("defaultJitter(1s) = %v, want a value in [0, 1s)", d)
		}
		seen[d] = true
	}
	if len(seen) < 100 {
		t.Errorf("only %d distinct values in 500 draws: the jitter is not random", len(seen))
	}
}

func TestRetry_RetryAfterBeyondTheMaximumIsNotWaitedFor(t *testing.T) {
	// 3600 is an hour; the other value overflows int64 and must saturate, not wrap into a short wait.
	for _, seconds := range []string{"3600", "61", "99999999999999999999"} {
		t.Run(seconds, func(t *testing.T) {
			c, sc, log := newScriptedClient(t, problemStep(429, "rate_limited", "Retry-After", seconds))

			_, err := c.GetVm(context.Background(), "vm-1")
			if apiErr := asAPIError(t, err); apiErr.Status != 429 || sc.count() != 1 || len(log.all()) != 0 {
				t.Errorf("err = %v calls = %d waits = %v, want the 429 at once: a server asking for %ss is not waited for", err, sc.count(), log.all(), seconds)
			}
		})
	}

	// The limit itself is still waited for.
	c, sc, log := newScriptedClient(t, problemStep(429, "rate_limited", "Retry-After", "60"), jsonStep(200, testVmJSON))
	if _, err := c.GetVm(context.Background(), "vm-1"); err != nil {
		t.Fatalf("GetVm() error = %v", err)
	}
	if got := log.all(); sc.count() != 2 || len(got) != 1 || got[0] != 60*time.Second {
		t.Errorf("Retry-After: 60 -> calls = %d waits = %v, want 2 calls and [1m0s]", sc.count(), got)
	}
}

func TestRetry_AnUnusableRetryAfterFallsBackToBackoff(t *testing.T) {
	for _, header := range []string{"soon", "-5", "1.5", ""} {
		t.Run("Retry-After: "+header, func(t *testing.T) {
			c, _, log := newScriptedClient(t, problemStep(429, "rate_limited", "Retry-After", header), jsonStep(200, testVmJSON))
			c.retry.jitter = func(ceiling time.Duration) time.Duration { return ceiling }

			if _, err := c.GetVm(context.Background(), "vm-1"); err != nil {
				t.Fatalf("GetVm() error = %v", err)
			}
			if got := log.all(); len(got) != 1 || got[0] != time.Second {
				t.Errorf("waits = %v, want [1s] (the first backoff ceiling)", got)
			}
		})
	}
}

// Everything else is the caller's to handle: a 4xx is a decision about the
// request, and a 500 or 501 says nothing about whether it ran.
func TestRetry_OtherStatusesAreNotRetried(t *testing.T) {
	for _, st := range []struct {
		status int
		code   string
	}{{400, "invalid_request"}, {401, "unauthorized"}, {403, "permission_denied"}, {404, "not_found"}, {409, "operation_in_flight"}, {422, "quota_exceeded"}, {500, "internal_error"}, {501, "not_enabled"}} {
		t.Run(st.code, func(t *testing.T) {
			c, sc, log := newScriptedClient(t, problemStep(st.status, st.code, "Retry-After", "1"), jsonStep(200, testVmJSON))

			apiErr := asAPIError(t, func() error { _, err := c.GetVm(context.Background(), "vm-1"); return err }())
			if apiErr.Status != st.status || sc.count() != 1 || len(log.all()) != 0 || apiErr.Attempts != 1 {
				t.Errorf("err = %v calls = %d waits = %v attempts = %d, want one call and no wait", apiErr, sc.count(), log.all(), apiErr.Attempts)
			}
		})
	}
}

// --- diagnostics: say what to change, keep what the API said ---------------

// failWith serves one problem+json answer with the given detail.
func failWith(t *testing.T, status int, code, detail string) *Client {
	t.Helper()
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"type":"https://docs.virtscale.nl/s3/problems/` + code + `","title":"x","status":` + itoa(status) + `,"detail":"` + detail + `","instance":"/","code":"` + code + `","request_id":"req-7"}`))
	})
	return c
}

// permission_denied names the permission it wants (permissions.py); the
// diagnostic adds the key scope that grants it: compute.* and operation.* come
// with compute:write (compute:read for the reads), edge.* and network.* with
// network:write (network:read for the reads).
func TestAPIError_PermissionDeniedNamesTheScopeThatGrantsIt(t *testing.T) {
	const compute, network = "compute", "network"
	cases := []struct {
		permission, family string
		read               bool
	}{
		{"compute.instance.create", compute, false},
		{"compute.instance.delete", compute, false},
		{"compute.instance.update", compute, false},
		{"compute.instance.read", compute, true},
		{"compute.instance.list", compute, true},
		{"operation.read", compute, true},
		{"operation.cancel", compute, false},
		{"edge.public_ip.allocate", network, false},
		{"edge.public_ip.release", network, false},
		{"edge.public_ip.read", network, true},
		{"network.firewall.update", network, false},
		{"network.read", network, true},
	}
	for _, tc := range cases {
		t.Run(tc.permission, func(t *testing.T) {
			detail := "this credential does not carry the '" + tc.permission + "' permission"
			c := failWith(t, 403, "permission_denied", detail)

			_, err := c.GetVm(context.Background(), "vm-1")
			want := "virtscale API 403 permission_denied: " + detail + " (request req-7); the API key needs the '" + tc.family + ":write' scope"
			if tc.read {
				want = "virtscale API 403 permission_denied: " + detail + " (request req-7); the API key needs the '" + tc.family + ":read' or '" + tc.family + ":write' scope"
			}
			if err == nil || err.Error() != want {
				t.Errorf("Error() = %v\nwant        %q", err, want)
			}
		})
	}
}

func TestAPIError_PermissionDeniedForAnotherFamilyOnlySendsYouToTheScopes(t *testing.T) {
	for _, perm := range []string{"iam.token.read", "storage.bucket.create", "billing.update"} {
		detail := "this credential does not carry the '" + perm + "' permission"
		c := failWith(t, 403, "permission_denied", detail)

		_, err := c.GetVm(context.Background(), "vm-1")
		want := "virtscale API 403 permission_denied: " + detail + " (request req-7); check the scopes of the API key (VIRTSCALE_API_KEY)"
		if err == nil || err.Error() != want {
			t.Errorf("%s: Error() = %v\nwant %q", perm, err, want)
		}
	}
}

func TestAPIError_UnauthorizedAsksYouToCheckTheKey(t *testing.T) {
	c := failWith(t, 401, "unauthorized", "the presented API token is not valid")

	_, err := c.GetVm(context.Background(), "vm-1")
	want := "virtscale API 401 unauthorized: the presented API token is not valid (request req-7); " +
		"check VIRTSCALE_API_KEY: the key may be wrong, revoked or expired"
	if err == nil || err.Error() != want {
		t.Errorf("Error() = %v\nwant        %q", err, want)
	}
}

func TestAPIError_SourceAddressNotAllowedSaysTheKeyIsRestricted(t *testing.T) {
	c := failWith(t, 403, "source_address_not_allowed", "this API token cannot be used from this network address")

	_, err := c.GetVm(context.Background(), "vm-1")
	want := "virtscale API 403 source_address_not_allowed: this API token cannot be used from this network address (request req-7); " +
		"the key's network limits do not include this address; run Terraform from an allowed address or widen the key's network limits"
	if err == nil || err.Error() != want {
		t.Errorf("Error() = %v\nwant        %q", err, want)
	}
}

func TestAPIError_UnseenOrganizationPointsAtTheOrganizationSetting(t *testing.T) {
	c := failWith(t, 404, "not_found", "no organization matching that identifier is visible to this organization")

	_, err := c.GetVm(context.Background(), "vm-1")
	want := "virtscale API 404 not_found: no organization matching that identifier is visible to this organization (request req-7); " +
		"check VIRTSCALE_ORGANIZATION: the organization id may be wrong, or VIRTSCALE_API_KEY belongs to another organization"
	if err == nil || err.Error() != want {
		t.Errorf("Error() = %v\nwant        %q", err, want)
	}
}

// Everything else says what the API said and nothing more.
func TestAPIError_OtherErrorsGetNoHint(t *testing.T) {
	cases := []struct {
		status       int
		code, detail string
	}{
		{404, "not_found", "no vm matching that identifier is visible to this organization"},
		{403, "forbidden", "this API token does not carry the 'write' scope"},
		{409, "name_taken", "a VM named 'web-01' already exists in gro"},
		{422, "quota_exceeded", "public IPs: 1 in use plus 1 requested exceeds the limit of 1 for this account"},
	}
	for _, tc := range cases {
		c := failWith(t, tc.status, tc.code, tc.detail)

		_, err := c.GetVm(context.Background(), "vm-1")
		want := "virtscale API " + itoa(tc.status) + " " + tc.code + ": " + tc.detail + " (request req-7)"
		if err == nil || err.Error() != want {
			t.Errorf("%d %s: Error() = %v\nwant %q", tc.status, tc.code, err, want)
		}
	}
}

// --- SSH keys (TF7 plan section 4) ---------------------------------------------

func TestCreateSshKey_SendsTheKeyAndTheIdempotencyKeyAndParsesTheAnswer(t *testing.T) {
	var gotMethod, gotPath, gotIdemp string
	var gotBody map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotIdemp = r.Method, r.URL.Path, r.Header.Get("Idempotency-Key")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Location", "/v1/orgs/test-org/compute/ssh-keys/key-1")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(testSshKeyJSON))
	})

	key, err := c.CreateSshKey(context.Background(), SshKeyCreateRequest{Name: "laptop", PublicKey: "ssh-ed25519 AAAA tf@host"}, "idem-9")
	if err != nil {
		t.Fatalf("CreateSshKey() error = %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/orgs/test-org/compute/ssh-keys" || gotIdemp != "idem-9" {
		t.Errorf("request = %s %s with Idempotency-Key %q, want POST /v1/orgs/test-org/compute/ssh-keys with idem-9", gotMethod, gotPath, gotIdemp)
	}
	if len(gotBody) != 2 || gotBody["name"] != "laptop" || gotBody["public_key"] != "ssh-ed25519 AAAA tf@host" {
		t.Errorf("body = %v, want exactly name and public_key", gotBody)
	}
	want := SshKey{ID: "key-1", Name: "laptop", KeyType: "ssh-ed25519", Fingerprint: "SHA256:abc", PublicKey: "ssh-ed25519 AAAA",
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	if key == nil || *key != want {
		t.Errorf("key = %+v, want %+v", key, want)
	}
}

func TestCreateSshKey_NoIdempotencyKeyWhenEmpty(t *testing.T) {
	sawHeader := false
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, sawHeader = r.Header["Idempotency-Key"]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(testSshKeyJSON))
	})
	if _, err := c.CreateSshKey(context.Background(), SshKeyCreateRequest{Name: "laptop", PublicKey: "ssh-ed25519 AAAA"}, ""); err != nil {
		t.Fatalf("CreateSshKey() error = %v", err)
	}
	if sawHeader {
		t.Error("Idempotency-Key header sent when idempotencyKey was empty")
	}
}

func TestGetAndDeleteSshKey_PathsAndAnswers(t *testing.T) {
	var gotMethod, gotPath string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.EscapedPath()
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(testSshKeyJSON))
	})

	key, err := c.GetSshKey(context.Background(), "key 1/x")
	if err != nil || key == nil || key.ID != "key-1" || key.Fingerprint != "SHA256:abc" {
		t.Fatalf("GetSshKey() = %+v, %v", key, err)
	}
	if gotMethod != http.MethodGet || gotPath != "/v1/orgs/test-org/compute/ssh-keys/key%201%2Fx" {
		t.Errorf("get sent %s %s, want GET with the id path-escaped", gotMethod, gotPath)
	}
	if err := c.DeleteSshKey(context.Background(), "key-1"); err != nil {
		t.Fatalf("DeleteSshKey() error = %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/v1/orgs/test-org/compute/ssh-keys/key-1" {
		t.Errorf("delete sent %s %s, want DELETE /v1/orgs/test-org/compute/ssh-keys/key-1", gotMethod, gotPath)
	}
}

// What the API refuses on an import comes back as the API said it, once: none
// of these is something repeating the request could change.
func TestCreateSshKey_RefusalsKeepTheAPIsWordsAndAreNotRetried(t *testing.T) {
	for _, tc := range []struct {
		status       int
		code, detail string
	}{
		{422, "invalid_public_key", "upload one public key in OpenSSH format: <type> <base64> [comment]"},
		{422, "invalid_name", "an SSH key name is 1 to 64 characters"},
		{409, "name_taken", "an SSH key named 'laptop' already exists in this organization"},
		{422, "quota_exceeded", "ssh_keys: 20 in use plus 1 requested exceeds the limit of 20 for this account"},
		{409, "compute_not_enabled", "the organization is not enrolled in the compute service"},
		{503, "not_ready", "SSH keys are not switched on yet"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			c := failWith(t, tc.status, tc.code, tc.detail)
			_, err := c.CreateSshKey(context.Background(), SshKeyCreateRequest{Name: "laptop", PublicKey: "x"}, "key-1")
			want := "virtscale API " + itoa(tc.status) + " " + tc.code + ": " + tc.detail + " (request req-7)"
			if err == nil || err.Error() != want {
				t.Errorf("Error() = %v\nwant        %q", err, want)
			}
		})
	}
}

// --- the API key never reaches an error text (V4 I1) ------------------------------

func TestAPIError_RawBodyNeverCarriesTheAPIKey(t *testing.T) {
	const key = "vsk_unit-test-4f9a7c1e2b8d"
	cases := []struct {
		name, contentType, body string
		status                  int
	}{
		{"echoed headers", "text/plain", "debug: Authorization: Bearer " + key, 500},
		{"a proxy's page", "text/html", "<html><body>upstream said: " + key + "</body></html>", 502},
		{"the key twice", "text/plain", key + " and again " + key, 500},
		{"the key split by the truncation", "text/plain", strings.Repeat("x", 195) + key, 500},
		{"a JSON error that quotes it", "application/problem+json",
			`{"type":"t","title":"bad ` + key + `","status":400,"detail":"Authorization: Bearer ` + key + `","code":"invalid_request","request_id":"req-1","errors":[{"field":"` + key + `","message":"nope ` + key + `"}]}`, 400},
	}
	for _, tc := range cases {
		for _, call := range []struct {
			name string
			run  func(c *Client) error
		}{
			{"GetVm", func(c *Client) error { _, err := c.GetVm(context.Background(), "vm-1"); return err }},
			{"keyed CreateVm", keyedCreate},
		} {
			t.Run(tc.name+"/"+call.name, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", tc.contentType)
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				}))
				t.Cleanup(srv.Close)
				c, err := New(srv.URL, key, "org-1")
				if err != nil {
					t.Fatal(err)
				}
				c.retry.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }

				err = call.run(c)
				if err == nil {
					t.Fatal("err = nil, want the API error")
				}
				msg := err.Error()
				// Not even the start of it: a key cut by the truncation would leave one.
				if strings.Contains(msg, key[:4]) {
					t.Errorf("the API key (or a piece of it) is in the error: %q", msg)
				}
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.Status != tc.status {
					t.Errorf("err = %v, want the *APIError with status %d", err, tc.status)
				}
				if apiErr != nil && (strings.Contains(apiErr.Detail, key) || strings.Contains(apiErr.Title, key)) {
					t.Errorf("the key is in the APIError fields: %+v", apiErr)
				}
			})
		}
	}

	t.Run("it is replaced, not dropped", func(t *testing.T) {
		c := func() *Client {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(500)
				_, _ = w.Write([]byte("debug: Authorization: Bearer " + key))
			}))
			t.Cleanup(srv.Close)
			c, _ := New(srv.URL, key, "org-1")
			return c
		}()
		_, err := c.GetVm(context.Background(), "vm-1")
		want := "virtscale API 500 error: debug: Authorization: Bearer [redacted]"
		if err == nil || err.Error() != want {
			t.Errorf("Error() = %v\nwant        %q", err, want)
		}
	})
}

// --- giving up on a create that may have been applied (V5 I5) -----------------------

const recoveryHint = "the first request may still have created the resource: " +
	"if the next apply reports name_taken, find it in the panel or with the API " +
	"and bring it under management with tofu import"

func keyedSshKeyCreate(c *Client) error {
	_, err := c.CreateSshKey(context.Background(), SshKeyCreateRequest{Name: "laptop", PublicKey: "ssh-ed25519 AAAA"}, "key-1")
	return err
}

// A create that carries an Idempotency-Key and gives up on an answer that
// leaves open that the API processed it (a lost answer, a gateway error, a
// duplicate that is still being processed) says so, and says how to find what
// it may have made. The API's own detail and request id stay in the message.
func TestRecoveryHint_AKeyedCreateThatGivesUpMayHaveCreatedTheResource(t *testing.T) {
	creates := map[string]func(*Client) error{"CreateVm": keyedCreate, "CreateSshKey": keyedSshKeyCreate}
	failures := []struct {
		name string
		step func(http.ResponseWriter, *http.Request)
		want string // what the message must still say before the hint
	}{
		{"502 from a proxy", htmlStep(502), "virtscale API 502 error: <html><body>Bad Gateway</body></html>; gave up after 5 attempts; "},
		{"504 from a proxy", htmlStep(504), "virtscale API 504 error: <html><body>Gateway Timeout</body></html>; gave up after 5 attempts; "},
		{"503 dependency_unavailable", problemStep(503, "dependency_unavailable"),
			"virtscale API 503 dependency_unavailable: detail of dependency_unavailable (request req-dependency_unavailable); gave up after 5 attempts; "},
		{"409 idempotency_in_progress", problemStep(409, "idempotency_in_progress"),
			"virtscale API 409 idempotency_in_progress: detail of idempotency_in_progress (request req-idempotency_in_progress); gave up after 5 attempts; "},
		{"a dropped connection", dropStep, ""},
	}
	for createName, create := range creates {
		for _, f := range failures {
			t.Run(createName+"/"+f.name, func(t *testing.T) {
				c, sc, _ := newScriptedClient(t, f.step)

				err := create(c)
				if err == nil || sc.count() != 5 {
					t.Fatalf("err = %v, calls = %d, want an error after 5 calls", err, sc.count())
				}
				msg := err.Error()
				if !strings.HasSuffix(msg, recoveryHint) {
					t.Errorf("the message does not end with the recovery hint:\n%s", msg)
				}
				if !strings.Contains(msg, f.want) {
					t.Errorf("the message lost what the API said:\n got %s\nwant it to contain %q", msg, f.want)
				}
				if strings.Count(msg, "gave up after 5 attempts") != 1 {
					t.Errorf("the message should say once that it gave up after 5 attempts:\n%s", msg)
				}
			})
		}
	}
}

// The hint is about the create that was lost, nothing else: a 429 was refused
// before it ran, not_ready and a refusal were decided, a read or a delete
// creates nothing, and a bad certificate never reached the API.
func TestRecoveryHint_OnlyWhenTheCreateMayHaveBeenApplied(t *testing.T) {
	cases := []struct {
		name  string
		run   func(c *Client) error
		steps []func(http.ResponseWriter, *http.Request)
	}{
		{"keyed create, 429 until it gives up", keyedCreate, []func(http.ResponseWriter, *http.Request){problemStep(429, "rate_limited")}},
		{"keyed create, 503 not_ready", keyedCreate, []func(http.ResponseWriter, *http.Request){problemStep(503, "not_ready")}},
		{"keyed create, 409 name_taken", keyedCreate, []func(http.ResponseWriter, *http.Request){problemStep(409, "name_taken")}},
		{"keyed create, 422", keyedCreate, []func(http.ResponseWriter, *http.Request){problemStep(422, "quota_exceeded")}},
		{"keyed create, plain 500", keyedCreate, []func(http.ResponseWriter, *http.Request){problemStep(500, "internal_error")}},
		{"keyed ssh key create, 429 until it gives up", keyedSshKeyCreate, []func(http.ResponseWriter, *http.Request){problemStep(429, "rate_limited")}},
		{"keyed ssh key create, 503 not_ready", keyedSshKeyCreate, []func(http.ResponseWriter, *http.Request){problemStep(503, "not_ready")}},
		{"a read that gives up", func(c *Client) error { _, err := c.GetVm(context.Background(), "vm-1"); return err }, []func(http.ResponseWriter, *http.Request){htmlStep(502)}},
		{"a delete that gives up", func(c *Client) error { _, _, err := c.DeleteVm(context.Background(), "vm-1"); return err }, []func(http.ResponseWriter, *http.Request){htmlStep(504)}},
		{"an ssh key delete that gives up", func(c *Client) error { return c.DeleteSshKey(context.Background(), "key-1") }, []func(http.ResponseWriter, *http.Request){htmlStep(502)}},
		{"an unkeyed create after a 502 (never retried)", func(c *Client) error { _, _, err := c.AllocatePublicIp(context.Background(), "gro"); return err }, []func(http.ResponseWriter, *http.Request){htmlStep(502)}},
		{"a read, dropped connections", func(c *Client) error { _, err := c.GetVm(context.Background(), "vm-1"); return err }, []func(http.ResponseWriter, *http.Request){dropStep}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _ := newScriptedClient(t, tc.steps...)
			err := tc.run(c)
			if err == nil {
				t.Fatal("err = nil, want an error")
			}
			if strings.Contains(err.Error(), "tofu import") || strings.Contains(err.Error(), "may still have created") {
				t.Errorf("a recovery hint where nothing may have been created:\n%s", err)
			}
			if apiErr := (*APIError)(nil); errors.As(err, &apiErr) && apiErr.MayHaveBeenApplied {
				t.Errorf("MayHaveBeenApplied is set on %v", err)
			}
		})
	}

	t.Run("a certificate that does not verify never reached the API", func(t *testing.T) {
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) }))
		srv.Config.ErrorLog = stdlog.New(io.Discard, "", 0)
		srv.StartTLS()
		t.Cleanup(srv.Close)
		c, err := New(srv.URL, "test-key", "org-1")
		if err != nil {
			t.Fatal(err)
		}
		c.retry.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }

		err = keyedCreate(c)
		if err == nil || strings.Contains(err.Error(), "tofu import") {
			t.Errorf("err = %v, want the certificate error without a recovery hint", err)
		}
	})
}

// The other ways to stop short: a wait that would run past the deadline, a
// Retry-After beyond the limit, and Ctrl-C. For a keyed create each leaves the
// first request possibly applied.
func TestRecoveryHint_AlsoWhenTheWaitCannotBeAfforded(t *testing.T) {
	t.Run("the deadline is shorter than the wait", func(t *testing.T) {
		c, sc, _ := newScriptedClient(t, problemStep(503, "dependency_unavailable", "Retry-After", "30"))
		c.retry.sleep = sleepContext
		ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
		defer cancel()

		_, err := c.CreateSshKey(ctx, SshKeyCreateRequest{Name: "laptop", PublicKey: "ssh-ed25519 AAAA"}, "key-1")
		if err == nil || sc.count() != 1 || !strings.HasSuffix(err.Error(), recoveryHint) {
			t.Errorf("err = %v after %d calls, want the 503 with the recovery hint after 1", err, sc.count())
		}
	})

	t.Run("the server asks for longer than anyone waits", func(t *testing.T) {
		c, sc, _ := newScriptedClient(t, problemStep(504, "upstream_timeout", "Retry-After", "3600"))

		err := keyedCreate(c)
		if err == nil || sc.count() != 1 || !strings.HasSuffix(err.Error(), recoveryHint) {
			t.Errorf("err = %v after %d calls, want the 504 with the recovery hint after 1", err, sc.count())
		}
	})

	t.Run("cancelled while waiting", func(t *testing.T) {
		c, sc, _ := newScriptedClient(t, problemStep(502, "bad_gateway", "Retry-After", "20"))
		c.retry.sleep = sleepContext
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(100*time.Millisecond, cancel)

		_, _, err := c.CreateVm(ctx, VmCreateRequest{Name: "web-01"}, "key-1")
		if err == nil || sc.count() != 1 || !strings.HasSuffix(err.Error(), recoveryHint) {
			t.Errorf("err = %v after %d calls, want the 502 with the recovery hint after 1", err, sc.count())
		}
	})

	t.Run("cancelled while the request is out", func(t *testing.T) {
		started := make(chan struct{})
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			// The server only watches for a closed connection once the body has
			// been read to its end.
			_, _ = io.Copy(io.Discard, r.Body)
			close(started)
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
		})
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			<-started
			cancel()
		}()

		_, _, err := c.CreateVm(ctx, VmCreateRequest{Name: "web-01"}, "key-1")
		if err == nil || !strings.HasSuffix(err.Error(), recoveryHint) {
			t.Errorf("err = %v, want the transport error with the recovery hint: the request was sent", err)
		}
	})
}
