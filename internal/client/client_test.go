package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestIsNotFound(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"type":"about:blank","title":"Not Found","status":404,"code":"vm_not_found","detail":"no such vm","request_id":"req-1"}`))
	})

	_, err := c.GetVm(context.Background(), "missing")
	if err == nil {
		t.Fatal("GetVm() error = nil, want error")
	}
	if !IsNotFound(err) {
		t.Errorf("IsNotFound(%v) = false, want true", err)
	}

	if IsNotFound(errors.New("some other error")) {
		t.Error("IsNotFound(plain error) = true, want false")
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
	if !IsNotFound(err) {
		t.Errorf("IsNotFound(%v) = false, want true", err)
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
		_, _ = w.Write([]byte(`{"type":"about:blank","title":"Not Found","status":404,"code":"vm_not_found","detail":"gone","request_id":"req-9"}`))
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
	if !IsNotFound(err) || IsOrganizationNotVisible(err) {
		t.Errorf("missing vm: IsNotFound=%v IsOrganizationNotVisible=%v, want true/false", IsNotFound(err), IsOrganizationNotVisible(err))
	}

	other, err := New(c.endpoint, "test-key", "other-org")
	if err != nil {
		t.Fatal(err)
	}
	_, err = other.GetVm(context.Background(), "vm-1")
	if IsNotFound(err) || !IsOrganizationNotVisible(err) {
		t.Errorf("unseen organization: IsNotFound=%v IsOrganizationNotVisible=%v, want false/true", IsNotFound(err), IsOrganizationNotVisible(err))
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
}
