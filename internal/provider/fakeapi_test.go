package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.virtscale.nl/virtscale/virtscale-platform/terraform-provider-virtscale/internal/client"
)

// fakeAPI is an in-process httptest fake of the VirtScale customer API,
// scoped to the compute/vms surface the provider and its resources need.
// It is deliberately structured so a later task can add compute/public-ips
// routes alongside createVm/getVm/deleteVm without reshaping this file.
type fakeAPI struct {
	apiKey string
	org    string
	srv    *httptest.Server

	mu          sync.Mutex
	vms         map[string]*client.Vm
	createCount int
	lastIdemKey string
	nextOctet   int
}

// newFakeAPI starts a fake API server scoped to apiKey/org and registers its
// shutdown with t.Cleanup.
func newFakeAPI(t *testing.T, apiKey, org string) *fakeAPI {
	t.Helper()

	f := &fakeAPI{
		apiKey:    apiKey,
		org:       org,
		vms:       make(map[string]*client.Vm),
		nextOctet: 10,
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

// URL returns the fake server's base URL, suitable for the provider's
// endpoint attribute.
func (f *fakeAPI) URL() string {
	return f.srv.URL
}

// CreateCount reports how many creates the fake has accepted.
func (f *fakeAPI) CreateCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createCount
}

// LastIdempotencyKey reports the Idempotency-Key header recorded on the most
// recent create.
func (f *fakeAPI) LastIdempotencyKey() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastIdemKey
}

// VmState reports the current state of vm id, and whether it exists at all
// (it is never actually removed from the store -- a deleted vm is retained
// with state "deleted" so callers can observe it).
func (f *fakeAPI) VmState(id string) (state string, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vm, ok := f.vms[id]
	if !ok {
		return "", false
	}
	return vm.State, true
}

func (f *fakeAPI) handle(w http.ResponseWriter, r *http.Request) {
	if !f.checkAuth(w, r) {
		return
	}

	org, rest, ok := splitOrgPath(r.URL.Path)
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found", "unknown path")
		return
	}
	if org != f.org {
		writeProblem(w, http.StatusNotFound, "org_not_found", fmt.Sprintf("organization %q not found", org))
		return
	}

	switch {
	case rest == "compute/vms" && r.Method == http.MethodPost:
		f.createVm(w, r)
	case strings.HasPrefix(rest, "compute/vms/") && r.Method == http.MethodGet:
		f.getVm(w, strings.TrimPrefix(rest, "compute/vms/"))
	case strings.HasPrefix(rest, "compute/vms/") && r.Method == http.MethodDelete:
		f.deleteVm(w, strings.TrimPrefix(rest, "compute/vms/"))
	default:
		writeProblem(w, http.StatusNotFound, "not_found", "unknown route")
	}
}

func (f *fakeAPI) checkAuth(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer "+f.apiKey {
		writeProblem(w, http.StatusUnauthorized, "invalid_api_key", "the api key is invalid")
		return false
	}
	return true
}

// splitOrgPath splits "/v1/orgs/{org}/rest/of/path" into (org, "rest/of/path").
func splitOrgPath(p string) (org, rest string, ok bool) {
	const prefix = "/v1/orgs/"
	if !strings.HasPrefix(p, prefix) {
		return "", "", false
	}
	p = strings.TrimPrefix(p, prefix)
	idx := strings.Index(p, "/")
	if idx < 0 {
		return "", "", false
	}
	orgEsc, err := url.PathUnescape(p[:idx])
	if err != nil {
		return "", "", false
	}
	return orgEsc, p[idx+1:], true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeProblem(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":   "about:blank",
		"title":  http.StatusText(status),
		"status": status,
		"code":   code,
		"detail": detail,
	})
}

type fakeVmCreateRequest struct {
	Name           string   `json:"name"`
	Image          string   `json:"image"`
	VCPUs          int      `json:"vcpus"`
	MemoryMB       int      `json:"memory_mb"`
	DiskGB         int      `json:"disk_gb"`
	StoragePool    string   `json:"storage_pool"`
	Region         string   `json:"region"`
	NetworkProfile string   `json:"network_profile"`
	SSHKeys        []string `json:"ssh_keys"`
}

func (f *fakeAPI) createVm(w http.ResponseWriter, r *http.Request) {
	var req fakeVmCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	for _, existing := range f.vms {
		if existing.Name == req.Name && existing.State != "deleted" {
			writeProblem(w, http.StatusConflict, "name_taken", fmt.Sprintf("an instance named %q already exists", req.Name))
			return
		}
	}

	f.createCount++
	f.lastIdemKey = r.Header.Get("Idempotency-Key")

	id := fmt.Sprintf("vm-%d", f.createCount)
	now := time.Now().UTC()

	vm := &client.Vm{
		ID:                id,
		OrgID:             f.org,
		RegionID:          req.Region,
		Name:              req.Name,
		FQDN:              nil,
		Image:             client.ImageRef{GlanceID: req.Image, Name: req.Image},
		VCPUs:             req.VCPUs,
		MemoryMB:          req.MemoryMB,
		DiskGB:            req.DiskGB,
		StoragePoolID:     req.StoragePool,
		NetworkProfileID:  req.NetworkProfile,
		State:             "pending",
		DesiredState:      "running",
		ObservedAddresses: nil,
		SSHKeyNames:       req.SSHKeys,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	f.vms[id] = vm

	op := client.Operation{
		ID:           id + "-create",
		Type:         "create_vm",
		State:        "pending",
		ResourceType: "vm",
		ResourceID:   id,
		CreatedAt:    now,
	}

	writeJSON(w, http.StatusAccepted, map[string]any{"vm": vm, "operation": op})
}

func (f *fakeAPI) getVm(w http.ResponseWriter, rawID string) {
	id, err := url.PathUnescape(rawID)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	vm, ok := f.vms[id]
	if !ok {
		writeProblem(w, http.StatusNotFound, "vm_not_found", fmt.Sprintf("instance %s not found", id))
		return
	}

	f.advance(vm)
	writeJSON(w, http.StatusOK, vm)
}

// advance moves vm one step through pending -> provisioning -> running (or
// -> error, for a name starting with "fail-"), and deleting -> deleted. It
// is a no-op in any terminal state, so repeated GETs after convergence are
// stable.
func (f *fakeAPI) advance(vm *client.Vm) {
	switch vm.State {
	case "pending":
		vm.State = "provisioning"
	case "provisioning":
		if strings.HasPrefix(vm.Name, "fail-") {
			errClass := "build_failed"
			errDetail := "the cloud could not build this instance"
			vm.State = "error"
			vm.ErrorClass = &errClass
			vm.ErrorDetail = &errDetail
		} else {
			vm.State = "running"
			f.nextOctet++
			vm.ObservedAddresses = map[string]any{
				"gro-tenant-default": []any{
					map[string]any{
						"addr":                    fmt.Sprintf("10.128.3.%d", f.nextOctet),
						"version":                 4,
						"OS-EXT-IPS:type":         "fixed",
						"OS-EXT-IPS-MAC:mac_addr": "fa:16:3e:00:00:00",
					},
				},
			}
		}
	case "deleting":
		vm.State = "deleted"
	}
	vm.UpdatedAt = time.Now().UTC()
}

func (f *fakeAPI) deleteVm(w http.ResponseWriter, rawID string) {
	id, err := url.PathUnescape(rawID)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	vm, ok := f.vms[id]
	if !ok {
		writeProblem(w, http.StatusNotFound, "vm_not_found", fmt.Sprintf("instance %s not found", id))
		return
	}

	vm.State = "deleting"
	vm.UpdatedAt = time.Now().UTC()

	op := client.Operation{
		ID:           id + "-delete",
		Type:         "delete_vm",
		State:        "pending",
		ResourceType: "vm",
		ResourceID:   id,
		CreatedAt:    time.Now().UTC(),
	}

	writeJSON(w, http.StatusAccepted, map[string]any{"vm": vm, "operation": op})
}

// --- Direct tests of the fake itself ---------------------------------------

func TestFakeAPI_AuthRejectsWrongBearer(t *testing.T) {
	f := newFakeAPI(t, "good-key", "org-1")

	req, err := http.NewRequest(http.MethodGet, f.URL()+"/v1/orgs/org-1/compute/vms/does-not-exist", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer wrong-key")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("expected application/problem+json, got %q", ct)
	}
}

func TestFakeAPI_DuplicateLiveNameConflicts(t *testing.T) {
	f := newFakeAPI(t, "good-key", "org-1")

	body := `{"name":"dup","image":"img","vcpus":1,"memory_mb":256,"disk_gb":10,"storage_pool":"pool","region":"gro","network_profile":"net"}`

	status1 := doCreate(t, f, body)
	if status1 != http.StatusAccepted {
		t.Fatalf("first create: expected 202, got %d", status1)
	}

	status2 := doCreate(t, f, body)
	if status2 != http.StatusConflict {
		t.Fatalf("duplicate create: expected 409, got %d", status2)
	}
}

func doCreate(t *testing.T, f *fakeAPI, body string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.URL()+"/v1/orgs/"+f.org+"/compute/vms", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+f.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestFakeAPI_DeleteThenGetShowsDeleted(t *testing.T) {
	f := newFakeAPI(t, "good-key", "org-1")

	body := `{"name":"to-delete","image":"img","vcpus":1,"memory_mb":256,"disk_gb":10,"storage_pool":"pool","region":"gro","network_profile":"net"}`
	if status := doCreate(t, f, body); status != http.StatusAccepted {
		t.Fatalf("create: expected 202, got %d", status)
	}

	if f.CreateCount() != 1 {
		t.Fatalf("expected create count 1, got %d", f.CreateCount())
	}

	id := "vm-1"

	req, err := http.NewRequest(http.MethodDelete, f.URL()+"/v1/orgs/"+f.org+"/compute/vms/"+id, nil)
	if err != nil {
		t.Fatalf("build delete request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+f.apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do delete: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete: expected 202, got %d", resp.StatusCode)
	}

	state, ok := f.VmState(id)
	if !ok || state != "deleting" {
		t.Fatalf("expected state deleting after delete, got %q (ok=%v)", state, ok)
	}

	getReq, err := http.NewRequest(http.MethodGet, f.URL()+"/v1/orgs/"+f.org+"/compute/vms/"+id, nil)
	if err != nil {
		t.Fatalf("build get request: %v", err)
	}
	getReq.Header.Set("Authorization", "Bearer "+f.apiKey)
	getResp, err := http.DefaultClient.Do(getReq)
	if err != nil {
		t.Fatalf("do get: %v", err)
	}
	getResp.Body.Close()

	state, ok = f.VmState(id)
	if !ok || state != "deleted" {
		t.Fatalf("expected state deleted after get, got %q (ok=%v)", state, ok)
	}
}
