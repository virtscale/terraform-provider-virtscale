package client

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// A create carries user_data and workload_type only when the caller set them: an empty value is left out of the body,
// so a customer organization that asks for nothing sends nothing the API could refuse.
func TestCreateVm_SendsUserDataAndWorkloadTypeOnlyWhenSet(t *testing.T) {
	var body string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(testVmAcceptedJSON))
	})

	if _, _, err := c.CreateVm(context.Background(), VmCreateRequest{Name: "web-01", UserData: "I2Nsb3VkLWNvbmZpZwo=", WorkloadType: "infra"}, ""); err != nil {
		t.Fatalf("CreateVm() error = %v", err)
	}
	for _, want := range []string{`"user_data":"I2Nsb3VkLWNvbmZpZwo="`, `"workload_type":"infra"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body = %s, want it to hold %s", body, want)
		}
	}

	if _, _, err := c.CreateVm(context.Background(), VmCreateRequest{Name: "web-01"}, ""); err != nil {
		t.Fatalf("CreateVm() without them: error = %v", err)
	}
	for _, key := range []string{"user_data", "workload_type"} {
		if strings.Contains(body, key) {
			t.Errorf("body = %s, want no %s key when it is not set", body, key)
		}
	}
}

// The resize names the instance and sends exactly the two sizes the API takes; it carries an Idempotency-Key, so a lost
// answer may be asked again, and it answers 202 with the instance and the operation.
func TestResizeVm_SendsTheTwoSizesWithAKey(t *testing.T) {
	var method, path, key, body string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		method, path, key, body = r.Method, r.URL.Path, r.Header.Get("Idempotency-Key"), string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(testVmAcceptedJSON))
	})

	vm, op, err := c.ResizeVm(context.Background(), "vm-1", 2, 2048, "key-1")
	if err != nil {
		t.Fatalf("ResizeVm() error = %v", err)
	}
	if method != http.MethodPost || path != "/v1/orgs/test-org/compute/instances/vm-1/resize" {
		t.Errorf("request = %s %s, want POST /v1/orgs/test-org/compute/instances/vm-1/resize", method, path)
	}
	if body != `{"vcpus":2,"memory_mb":2048}` {
		t.Errorf("body = %s, want exactly {\"vcpus\":2,\"memory_mb\":2048}", body)
	}
	if key != "key-1" {
		t.Errorf("Idempotency-Key = %q, want key-1", key)
	}
	if vm == nil || vm.ID != "vm-1" || op == nil || op.ID == "" {
		t.Errorf("answer = %+v / %+v, want the instance and the operation of the 202", vm, op)
	}
}

// A resize that is refused keeps the API's words, and a 503 not_ready (resizing is off for the organization) is sent
// once, not retried.
func TestResizeVm_NotReadyIsNotRetried(t *testing.T) {
	calls := 0
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":503,"code":"not_ready","title":"Service unavailable","detail":"resizing is not switched on"}`))
	})

	_, _, err := c.ResizeVm(context.Background(), "vm-1", 2, 2048, "key-1")
	if err == nil || !strings.Contains(err.Error(), "not_ready") || !strings.Contains(err.Error(), "resizing is not switched on") {
		t.Errorf("error = %v, want the API's not_ready and its detail", err)
	}
	if calls != 1 {
		t.Errorf("requests = %d, want 1: a not_ready is never retried", calls)
	}
}

// The protection is a PUT of {"protected": bool}, keyed so that a lost answer may be asked again, and it answers 200
// with the instance.
func TestSetVmProtection_SendsAPut(t *testing.T) {
	for _, on := range []bool{true, false} {
		var method, path, key, body string
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			method, path, key, body = r.Method, r.URL.Path, r.Header.Get("Idempotency-Key"), string(b)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(testVmJSON))
		})

		vm, err := c.SetVmProtection(context.Background(), "vm-1", on, "key-2")
		if err != nil {
			t.Fatalf("SetVmProtection(%v) error = %v", on, err)
		}
		want := `{"protected":false}`
		if on {
			want = `{"protected":true}`
		}
		if method != http.MethodPut || path != "/v1/orgs/test-org/compute/instances/vm-1/protection" || body != want || key != "key-2" {
			t.Errorf("SetVmProtection(%v): %s %s body %s key %q, want PUT .../vm-1/protection %s key-2", on, method, path, body, key, want)
		}
		if vm == nil || vm.ID != "vm-1" {
			t.Errorf("vm = %+v, want the instance of the 200", vm)
		}
	}
}

// VmOut's protected, workload_type and user_data_present are decoded.
func TestVm_DecodesProtectionWorkloadTypeAndUserDataPresence(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"vm-1","protected":true,"workload_type":"infra","user_data_present":true}`))
	})
	vm, err := c.GetVm(context.Background(), "vm-1")
	if err != nil {
		t.Fatalf("GetVm() error = %v", err)
	}
	if !vm.Protected || vm.WorkloadType != "infra" || !vm.UserDataPresent {
		t.Errorf("vm = %+v, want protected, workload_type infra and user_data_present", vm)
	}
}

// A destroy that meets 409 vm_protected keeps the API's sentence and says what to change in the configuration.
func TestAPIError_ProtectedInstanceSaysHowToLiftTheProtection(t *testing.T) {
	err := (&APIError{Status: http.StatusConflict, Code: "vm_protected", Detail: "this VM is protected against deletion"}).Error()
	for _, want := range []string{"this VM is protected against deletion", "set disable_api_termination = false and apply first"} {
		if !strings.Contains(err, want) {
			t.Errorf("error = %q, want it to say %q", err, want)
		}
	}
}
