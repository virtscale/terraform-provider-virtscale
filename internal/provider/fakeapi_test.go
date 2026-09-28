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

	publicIPs        map[string]*client.PublicIp
	pubIPCreateCount int
	pubIPNextOctet   int

	// callSeq records, per public IP id, the order "disassociate"/"release"
	// calls were observed -- used to assert Delete disassociates before it
	// releases.
	callSeq map[string][]string
}

// newFakeAPI starts a fake API server scoped to apiKey/org and registers its
// shutdown with t.Cleanup.
func newFakeAPI(t *testing.T, apiKey, org string) *fakeAPI {
	t.Helper()

	f := &fakeAPI{
		apiKey:         apiKey,
		org:            org,
		vms:            make(map[string]*client.Vm),
		nextOctet:      10,
		publicIPs:      make(map[string]*client.PublicIp),
		pubIPNextOctet: 10,
		callSeq:        make(map[string][]string),
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

// PublicIPCount reports how many public IPs are currently tracked by the
// fake. A released public IP is removed entirely (unlike a deleted vm), so
// this reaching 0 is the destroy signal for public IP tests.
func (f *fakeAPI) PublicIPCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.publicIPs)
}

// CallSequence reports, in call order, which of "disassociate"/"release"
// the fake observed for public ip id.
func (f *fakeAPI) CallSequence(id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.callSeq[id]))
	copy(out, f.callSeq[id])
	return out
}

// Vanish removes vm id from the store entirely, so the next GetVm returns a
// genuine 404 -- unlike deleteVm, which retains the vm in state "deleted".
// It simulates an out-of-band deletion.
func (f *fakeAPI) Vanish(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.vms, id)
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
	case rest == "compute/public-ips" && r.Method == http.MethodPost:
		f.createPublicIp(w, r)
	case rest == "compute/public-ips" && r.Method == http.MethodGet:
		f.listPublicIps(w, r)
	case strings.HasPrefix(rest, "compute/public-ips/") && strings.HasSuffix(rest, "/associate") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(rest, "compute/public-ips/"), "/associate")
		f.associatePublicIp(w, r, id)
	case strings.HasPrefix(rest, "compute/public-ips/") && strings.HasSuffix(rest, "/disassociate") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(rest, "compute/public-ips/"), "/disassociate")
		f.disassociatePublicIp(w, id)
	case strings.HasPrefix(rest, "compute/public-ips/") && r.Method == http.MethodDelete:
		f.releasePublicIp(w, strings.TrimPrefix(rest, "compute/public-ips/"))
	case rest == "compute/images" && r.Method == http.MethodGet:
		f.listImages(w, r)
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

	// An "alias-" prefixed image id simulates a catalogue id (ImageOut.id)
	// that diverges from the glance_id the API echoes back on the created vm
	// (VmOut.image.glance_id) -- see Finding 1. Every other image id echoes
	// back unchanged, matching prior behavior.
	glanceID := req.Image
	if strings.HasPrefix(req.Image, "alias-") {
		glanceID = req.Image + "-glance-uuid"
	}

	vm := &client.Vm{
		ID:                id,
		OrgID:             f.org,
		RegionID:          req.Region,
		Name:              req.Name,
		FQDN:              nil,
		Image:             client.ImageRef{GlanceID: glanceID, Name: req.Image},
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

// --- Public IP handlers ------------------------------------------------

type fakePublicIpCreateRequest struct {
	RegionID string `json:"region_id"`
}

// createPublicIp allocates a public IP in state "pending", or 422
// quota_exceeded when the org already has one tracked (this fake enforces a
// quota of one live public IP, matching the real API).
func (f *fakeAPI) createPublicIp(w http.ResponseWriter, r *http.Request) {
	var req fakePublicIpCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.publicIPs) > 0 {
		writeProblem(w, http.StatusUnprocessableEntity, "quota_exceeded", "the organization's public IP quota (1) is already in use")
		return
	}

	f.pubIPCreateCount++
	id := fmt.Sprintf("pubip-%d", f.pubIPCreateCount)
	now := time.Now().UTC()

	ip := &client.PublicIp{
		ID:        id,
		RegionID:  req.RegionID,
		Address:   nil,
		VmID:      nil,
		State:     "pending",
		CreatedAt: now,
	}
	f.publicIPs[id] = ip

	op := client.Operation{
		ID:           id + "-allocate",
		Type:         "allocate_public_ip",
		State:        "pending",
		ResourceType: "public_ip",
		ResourceID:   id,
		CreatedAt:    now,
	}

	writeJSON(w, http.StatusAccepted, map[string]any{"public_ip": ip, "operation": op})
}

// listPublicIps is the only read path for public IPs (there is no
// single-item GET, matching the real API); it advances every tracked public
// IP one step and drops any that just finished releasing, so it doubles as
// the state machine driver the client's GetPublicIp/WaitPublicIp rely on.
func (f *fakeAPI) listPublicIps(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	items := make([]*client.PublicIp, 0, len(f.publicIPs))
	for id, ip := range f.publicIPs {
		if ip.State == "releasing" {
			delete(f.publicIPs, id)
			continue
		}
		f.advancePublicIp(ip)
		items = append(items, ip)
	}

	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": nil})
}

// advancePublicIp moves ip one step through pending -> allocated,
// associating -> associated (or -> error, when the vm being associated has a
// name starting "ipfail-", simulating an edge that refuses the bind), and
// disassociating -> allocated, clearing vm_id (or -> error, when the vm being
// disassociated has a name starting "ipdisfail-", simulating an edge that
// refuses to release the binding). It is a no-op in any other state, so
// repeated GETs after convergence are stable. Releasing is handled by the
// caller (listPublicIps) since it removes the entry rather than mutating it.
func (f *fakeAPI) advancePublicIp(ip *client.PublicIp) {
	switch ip.State {
	case "pending":
		ip.State = "allocated"
		f.pubIPNextOctet++
		addr := fmt.Sprintf("203.0.113.%d", f.pubIPNextOctet)
		ip.Address = &addr
	case "associating":
		if ip.VmID != nil {
			if vm, ok := f.vms[*ip.VmID]; ok && strings.HasPrefix(vm.Name, "ipfail-") {
				errMsg := "the edge could not bind this address"
				ip.State = "error"
				ip.Error = &errMsg
				return
			}
		}
		ip.State = "associated"
	case "disassociating":
		if ip.VmID != nil {
			if vm, ok := f.vms[*ip.VmID]; ok && strings.HasPrefix(vm.Name, "ipdisfail-") {
				errMsg := "the edge could not release this binding"
				ip.State = "error"
				ip.Error = &errMsg
				return
			}
		}
		ip.State = "allocated"
		ip.VmID = nil
	}
}

type fakePublicIpAssociateRequest struct {
	VmID string `json:"vm_id"`
}

func (f *fakeAPI) associatePublicIp(w http.ResponseWriter, r *http.Request, rawID string) {
	id, err := url.PathUnescape(rawID)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}

	var req fakePublicIpAssociateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	ip, ok := f.publicIPs[id]
	if !ok {
		writeProblem(w, http.StatusNotFound, "public_ip_not_found", fmt.Sprintf("public ip %s not found", id))
		return
	}

	ip.State = "associating"
	ip.VmID = &req.VmID

	op := client.Operation{
		ID:           id + "-associate",
		Type:         "associate_public_ip",
		State:        "pending",
		ResourceType: "public_ip",
		ResourceID:   id,
		CreatedAt:    time.Now().UTC(),
	}

	writeJSON(w, http.StatusAccepted, map[string]any{"public_ip": ip, "operation": op})
}

func (f *fakeAPI) disassociatePublicIp(w http.ResponseWriter, rawID string) {
	id, err := url.PathUnescape(rawID)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	ip, ok := f.publicIPs[id]
	if !ok {
		writeProblem(w, http.StatusNotFound, "public_ip_not_found", fmt.Sprintf("public ip %s not found", id))
		return
	}

	ip.State = "disassociating"
	f.callSeq[id] = append(f.callSeq[id], "disassociate")

	op := client.Operation{
		ID:           id + "-disassociate",
		Type:         "disassociate_public_ip",
		State:        "pending",
		ResourceType: "public_ip",
		ResourceID:   id,
		CreatedAt:    time.Now().UTC(),
	}

	writeJSON(w, http.StatusAccepted, map[string]any{"public_ip": ip, "operation": op})
}

func (f *fakeAPI) releasePublicIp(w http.ResponseWriter, rawID string) {
	id, err := url.PathUnescape(rawID)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	ip, ok := f.publicIPs[id]
	if !ok {
		writeProblem(w, http.StatusNotFound, "public_ip_not_found", fmt.Sprintf("public ip %s not found", id))
		return
	}

	ip.State = "releasing"
	f.callSeq[id] = append(f.callSeq[id], "release")

	op := client.Operation{
		ID:           id + "-release",
		Type:         "release_public_ip",
		State:        "pending",
		ResourceType: "public_ip",
		ResourceID:   id,
		CreatedAt:    time.Now().UTC(),
	}

	writeJSON(w, http.StatusAccepted, map[string]any{"public_ip": ip, "operation": op})
}

// --- Images handler ------------------------------------------------------

// listImages returns a fixed two-image catalogue for region "gro" and an
// empty one for any other region; region is required, matching the real
// API's per-region catalogue.
func (f *fakeAPI) listImages(w http.ResponseWriter, r *http.Request) {
	region := r.URL.Query().Get("region")
	if region == "" {
		writeProblem(w, http.StatusUnprocessableEntity, "region_required", "region is required")
		return
	}

	if region != "gro" {
		writeJSON(w, http.StatusOK, map[string]any{"items": []client.Image{}, "next_cursor": nil})
		return
	}

	debianDistro, debianVersion := "debian", "12"
	ubuntuDistro, ubuntuVersion := "ubuntu", "22.04"
	items := []client.Image{
		{ID: "image-debian-12", Name: "Debian 12", OSDistro: &debianDistro, OSVersion: &debianVersion, MinDiskGB: 10, MinRAMMB: 512},
		{ID: "image-ubuntu-2204", Name: "Ubuntu 22.04", OSDistro: &ubuntuDistro, OSVersion: &ubuntuVersion, MinDiskGB: 10, MinRAMMB: 512},
	}

	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": nil})
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
