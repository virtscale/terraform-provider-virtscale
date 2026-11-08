package client

// What a create sends and a read returns for host placement (host_id), and the hints of its refusals (Track C, P1).

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// host_id is in the body only when the caller named a host: an organization that does not ask sends nothing the API
// could refuse (a customer organization is refused any value, 422 host_placement_not_allowed).
func TestCreateVm_SendsHostIDOnlyWhenSet(t *testing.T) {
	var body string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(testVmAcceptedJSON))
	})

	req := VmCreateRequest{Name: "dns-01", Region: "ams", DiskGB: 40, StoragePool: "ams1-local", HostID: "IKVM-AMS1-01"}
	if _, _, err := c.CreateVm(context.Background(), req, ""); err != nil {
		t.Fatalf("CreateVm() error = %v", err)
	}
	for _, want := range []string{`"host_id":"IKVM-AMS1-01"`, `"storage_pool":"ams1-local"`, `"disk_gb":40`} {
		if !strings.Contains(body, want) {
			t.Errorf("body = %s, want it to hold %s", body, want)
		}
	}

	if _, _, err := c.CreateVm(context.Background(), VmCreateRequest{Name: "web-01"}, ""); err != nil {
		t.Fatalf("CreateVm() without a host: error = %v", err)
	}
	if strings.Contains(body, "host_id") {
		t.Errorf("body = %s, want no host_id key when it is not set", body)
	}
}

// VmOut.host_id is the host the instance was placed on, or null: a string pointer, nil when the API sends null.
func TestVm_DecodesHostID(t *testing.T) {
	c, _, _ := newScriptedClient(t,
		jsonStep(http.StatusOK, `{"id":"vm-1","host_id":"IKVM-AMS1-01","addresses":[]}`),
		jsonStep(http.StatusOK, `{"id":"vm-2","host_id":null,"addresses":[]}`),
		jsonStep(http.StatusOK, `{"id":"vm-3","addresses":[]}`),
	)

	pinned, err := c.GetVm(context.Background(), "vm-1")
	if err != nil || pinned.HostID == nil || *pinned.HostID != "IKVM-AMS1-01" {
		t.Fatalf("GetVm() = %+v, %v, want host_id IKVM-AMS1-01", pinned, err)
	}
	for _, id := range []string{"vm-2", "vm-3"} {
		vm, err := c.GetVm(context.Background(), id)
		if err != nil || vm.HostID != nil {
			t.Errorf("GetVm(%s) = %+v, %v, want a nil HostID for null and for absent", id, vm, err)
		}
	}
}

// Each refusal keeps the API's own sentence and adds the next step; the codes are matched, whatever the status says.
func TestAPIError_HostRefusalsSayWhatToChange(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		detail string
		hint   string
	}{
		{422, "host_placement_not_allowed", "placing an instance on a named host is not available to this organization",
			"host_id, network_interface and internal instance types are for VirtScale's internal organizations"},
		{422, "host_unavailable", "the named host is not available for this instance",
			"check host_id: the host must exist, be in the instance's region and be in service"},
		{422, "host_required", "instances on this storage pool keep their disk on the host they run on; name that host with host_id",
			"set host_id to the host the volume lives on"},
		{409, "resize_not_available", "an instance placed on a named host is not resized in place; replace it",
			"an instance with host_id is replaced, not resized: the provider plans a replacement when host_id is set"},
	} {
		c := failWith(t, tc.status, tc.code, tc.detail)
		_, err := c.GetVm(context.Background(), "vm-1")
		want := "virtscale API " + strconv.Itoa(tc.status) + " " + tc.code + ": " + tc.detail + " (request req-7); " + tc.hint
		if err == nil || err.Error() != want {
			t.Errorf("%s: Error() = %v\nwant %q", tc.code, err, want)
		}
	}
}
