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
func TestAPIError_PlacementRefusalsSayWhatToChange(t *testing.T) {
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
		{422, "network_interface_not_allowed", "setting a private IP address, MAC address or secondary IP addresses on a network interface is not available to this organization",
			"host_id, network_interface and internal instance types are for VirtScale's internal organizations"},
		{409, "resize_not_available", "an instance placed on a named host is not resized in place; replace it",
			"an instance with host_id or of an internal instance type is replaced, not resized: the provider plans a replacement when host_id is set, and tofu apply -replace replaces any other"},
	} {
		c := failWith(t, tc.status, tc.code, tc.detail)
		_, err := c.GetVm(context.Background(), "vm-1")
		want := "virtscale API " + strconv.Itoa(tc.status) + " " + tc.code + ": " + tc.detail + " (request req-7); " + tc.hint
		if err == nil || err.Error() != want {
			t.Errorf("%s: Error() = %v\nwant %q", tc.code, err, want)
		}
	}
}

// network_interfaces is in the body only when the caller gave entries; entry i is interface i, an empty value is left
// out (the platform's choice) and source_dest_check is always said.
func TestCreateVm_SendsNetworkInterfacesOnlyWhenSet(t *testing.T) {
	var body string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(testVmAcceptedJSON))
	})

	req := VmCreateRequest{Name: "dns-01", NetworkInterfaces: []NetworkInterface{
		{PrivateIP: "10.20.30.40", MACAddress: "fa:16:3e:aa:bb:cc", SecondaryPrivateIPs: []string{"10.20.30.41"}, SourceDestCheck: true},
		{SourceDestCheck: false},
	}}
	if _, _, err := c.CreateVm(context.Background(), req, ""); err != nil {
		t.Fatalf("CreateVm() error = %v", err)
	}
	want := `"network_interfaces":[{"private_ip":"10.20.30.40","mac_address":"fa:16:3e:aa:bb:cc","secondary_private_ips":["10.20.30.41"],"source_dest_check":true},{"source_dest_check":false}]`
	if !strings.Contains(body, want) {
		t.Errorf("body = %s, want it to hold %s", body, want)
	}

	if _, _, err := c.CreateVm(context.Background(), VmCreateRequest{Name: "web-01"}, ""); err != nil {
		t.Fatalf("CreateVm() without interfaces: error = %v", err)
	}
	if strings.Contains(body, "network_interfaces") {
		t.Errorf("body = %s, want no network_interfaces key when there are none", body)
	}
}

// VmOut.network_interfaces is what the create asked for, one entry per interface, or null.
func TestVm_DecodesNetworkInterfaces(t *testing.T) {
	c, _, _ := newScriptedClient(t,
		jsonStep(http.StatusOK, `{"id":"vm-1","addresses":[],"network_interfaces":[{"private_ip":"10.20.30.40","mac_address":null,"secondary_private_ips":["10.20.30.41","10.20.30.42"],"source_dest_check":false}]}`),
		jsonStep(http.StatusOK, `{"id":"vm-2","addresses":[],"network_interfaces":null}`),
		jsonStep(http.StatusOK, `{"id":"vm-3","addresses":[]}`),
	)

	vm, err := c.GetVm(context.Background(), "vm-1")
	if err != nil || len(vm.NetworkInterfaces) != 1 {
		t.Fatalf("GetVm() = %+v, %v, want one interface", vm, err)
	}
	got := vm.NetworkInterfaces[0]
	if got.PrivateIP != "10.20.30.40" || got.MACAddress != "" || strings.Join(got.SecondaryPrivateIPs, ",") != "10.20.30.41,10.20.30.42" || got.SourceDestCheck {
		t.Errorf("interface = %+v, want 10.20.30.40, no MAC, two secondary addresses, source_dest_check false", got)
	}
	for _, id := range []string{"vm-2", "vm-3"} {
		vm, err := c.GetVm(context.Background(), id)
		if err != nil || len(vm.NetworkInterfaces) != 0 {
			t.Errorf("GetVm(%s) = %+v, %v, want no interfaces for null and for absent", id, vm, err)
		}
	}
}
