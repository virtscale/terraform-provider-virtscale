package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAPI is an in-process httptest fake of the VirtScale customer API
// (s3-control-plane 94f5021f), scoped to the compute surface the provider
// needs: VMs, public IPs and the image catalogue.
//
// It deliberately does NOT reuse internal/client's types. Its wire structs
// below are transcribed from the deployed OpenAPI spec, so a wrong JSON tag in
// the client shows up as a failing test instead of being mirrored on the way
// out. docs/verification/terraform-provider-virtscale/04-contract-94f5021f.md
// lists, behaviour by behaviour, what is emulated and from which handler.
type fakeAPI struct {
	apiKey string
	org    string
	srv    *httptest.Server

	mu          sync.Mutex
	vms         map[string]*fakeVm
	createCount int
	lastIdemKey string
	nextOctet   int
	vmSeq       int
	opSeq       int
	reqSeq      int

	publicIPs        map[string]*fakePublicIP
	pubIPCreateCount int
	pubIPNextOctet   int

	// callSeq records, per public IP id, the order "disassociate"/"release"
	// calls were observed -- used to assert Delete disassociates before it
	// releases.
	callSeq map[string][]string
}

// --- wire types (customer-v1.json) ---------------------------------------

type wireImageRef struct {
	GlanceID string `json:"glance_id"`
	Name     string `json:"name"`
}

type wireVmOut struct {
	ID                string         `json:"id"`
	OrgID             string         `json:"org_id"`
	RegionID          string         `json:"region_id"`
	Name              string         `json:"name"`
	FQDN              *string        `json:"fqdn"`
	Image             wireImageRef   `json:"image"`
	VCPUs             int            `json:"vcpus"`
	MemoryMB          int            `json:"memory_mb"`
	DiskGB            int            `json:"disk_gb"`
	StoragePoolID     string         `json:"storage_pool_id"`
	WorkloadType      string         `json:"workload_type"`
	NetworkProfileID  string         `json:"network_profile_id"`
	State             string         `json:"state"`
	DesiredState      string         `json:"desired_state"`
	ErrorClass        *string        `json:"error_class"`
	ErrorDetail       *string        `json:"error_detail"`
	ObservedStatus    *string        `json:"observed_status"`
	ObservedAddresses map[string]any `json:"observed_addresses"`
	ObservedAt        *time.Time     `json:"observed_at"`
	SSHKeyNames       []string       `json:"ssh_key_names"`
	UserDataPresent   bool           `json:"user_data_present"`
	Protected         bool           `json:"protected"`
	AllowedActions    []string       `json:"allowed_actions"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
}

type wireOperationRef struct {
	ID           string     `json:"id"`
	Type         string     `json:"type"`
	State        string     `json:"state"`
	ResourceType string     `json:"resource_type"`
	ResourceID   string     `json:"resource_id"`
	Attempt      int        `json:"attempt"`
	MaxAttempts  int        `json:"max_attempts"`
	ErrorClass   *string    `json:"error_class"`
	ErrorDetail  *string    `json:"error_detail"`
	CreatedAt    time.Time  `json:"created_at"`
	FinishedAt   *time.Time `json:"finished_at"`
}

type wirePublicIpOut struct {
	ID        string    `json:"id"`
	RegionID  string    `json:"region_id"`
	Address   *string   `json:"address"`
	VmID      *string   `json:"vm_id"`
	State     string    `json:"state"`
	Error     *string   `json:"error"`
	CreatedAt time.Time `json:"created_at"`
}

type wireImageOut struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	OSDistro  *string `json:"os_distro"`
	OSVersion *string `json:"os_version"`
	MinDiskGB int     `json:"min_disk_gb"`
	MinRAMMB  int     `json:"min_ram_mb"`
}

type wireList[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

type wireFieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// fakeVm is one VM row: what the API shows, plus what only the control plane
// knows.
type fakeVm struct {
	wireVmOut
}

// fakePublicIP is one public IP row.
type fakePublicIP struct {
	wirePublicIpOut
}

// allVmActions is VmOut.allowed_actions for a VM in an ordinary region.
var allVmActions = []string{
	"start", "stop", "reboot", "console", "resize", "rebuild", "rescue", "unrescue",
	"delete", "firewall", "public_ip", "protection",
}

var anyUUIDRE = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// fakeUUID is a deterministic, valid version-4 UUID: kind 1 vms, 2 public IPs,
// 3 operations.
func fakeUUID(kind, n int) string {
	return fmt.Sprintf("00000000-0000-4%03x-8000-%012x", kind, n)
}

// newFakeAPI starts a fake API server scoped to apiKey/org and registers its
// shutdown with t.Cleanup. org must be a UUID, as it is on the real API.
func newFakeAPI(t *testing.T, apiKey, org string) *fakeAPI {
	t.Helper()

	f := &fakeAPI{
		apiKey:         apiKey,
		org:            org,
		vms:            make(map[string]*fakeVm),
		nextOctet:      10,
		publicIPs:      make(map[string]*fakePublicIP),
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

// --- request plumbing ------------------------------------------------------

// reqCtx is one request in flight: it knows its own request id, which every
// response (success or problem) carries, like the real API's X-Request-ID.
type reqCtx struct {
	f   *fakeAPI
	w   http.ResponseWriter
	r   *http.Request
	rid string
}

func (f *fakeAPI) newReqCtx(w http.ResponseWriter, r *http.Request) *reqCtx {
	f.mu.Lock()
	f.reqSeq++
	rid := fmt.Sprintf("req-%06d", f.reqSeq)
	f.mu.Unlock()
	return &reqCtx{f: f, w: w, r: r, rid: rid}
}

// json writes a success body. headers is a flat list of name/value pairs.
func (rc *reqCtx) json(status int, v any, headers ...string) {
	rc.w.Header().Set("Content-Type", "application/json")
	rc.w.Header().Set("X-Request-ID", rc.rid)
	for i := 0; i+1 < len(headers); i += 2 {
		rc.w.Header().Set(headers[i], headers[i+1])
	}
	rc.w.WriteHeader(status)
	_ = json.NewEncoder(rc.w).Encode(v)
}

const problemTypeBase = "https://docs.virtscale.nl/s3/problems/"

// problem writes an RFC 9457 problem+json response in the shape errors.py
// renders: type, title, status, detail, instance, code, request_id and, when
// there are any, errors[].
func (rc *reqCtx) problem(status int, code, title, detail string, errs ...wireFieldError) {
	body := map[string]any{
		"type":       problemTypeBase + code,
		"title":      title,
		"status":     status,
		"detail":     detail,
		"instance":   rc.r.URL.Path,
		"code":       code,
		"request_id": rc.rid,
	}
	if len(errs) > 0 {
		body["errors"] = errs
	}
	rc.w.Header().Set("Content-Type", "application/problem+json")
	rc.w.Header().Set("X-Request-ID", rc.rid)
	rc.w.WriteHeader(status)
	_ = json.NewEncoder(rc.w).Encode(body)
}

func (rc *reqCtx) notFound(resource string) {
	rc.problem(http.StatusNotFound, "not_found", "Resource not found",
		"no "+resource+" matching that identifier is visible to this organization")
}

func (rc *reqCtx) conflict(code, detail string) {
	rc.problem(http.StatusConflict, code, "Conflict", detail)
}

func (rc *reqCtx) unprocessable(code, detail string) {
	rc.problem(http.StatusUnprocessableEntity, code, "Unprocessable", detail)
}

// invalid is a 400 invalid_request, the shape every validation failure takes.
func (rc *reqCtx) invalid(errs ...wireFieldError) {
	rc.problem(http.StatusBadRequest, "invalid_request", "Invalid request",
		"the request body or parameters failed validation", errs...)
}

// parseRoute maps (method, path below the org) to a route key and the uuid
// path parameter, if any.
func parseRoute(method, rest string) (key, param, id string, ok bool) {
	parts := strings.Split(rest, "/")
	switch {
	case rest == "compute/vms":
		key = "compute/vms"
	case len(parts) == 3 && parts[0] == "compute" && parts[1] == "vms":
		key, param, id = "compute/vms/{vm_id}", "vm_id", parts[2]
	case rest == "compute/images":
		key = "compute/images"
	case rest == "compute/public-ips":
		key = "compute/public-ips"
	case len(parts) == 3 && parts[0] == "compute" && parts[1] == "public-ips":
		key, param, id = "compute/public-ips/{public_ip_id}", "public_ip_id", parts[2]
	case len(parts) == 4 && parts[0] == "compute" && parts[1] == "public-ips" && (parts[3] == "associate" || parts[3] == "disassociate"):
		key, param, id = "compute/public-ips/{public_ip_id}/"+parts[3], "public_ip_id", parts[2]
	default:
		return "", "", "", false
	}
	switch method + " " + key {
	case "POST compute/vms", "GET compute/vms/{vm_id}", "DELETE compute/vms/{vm_id}",
		"GET compute/images",
		"GET compute/public-ips", "POST compute/public-ips", "DELETE compute/public-ips/{public_ip_id}",
		"POST compute/public-ips/{public_ip_id}/associate", "POST compute/public-ips/{public_ip_id}/disassociate":
		return method + " " + key, param, id, true
	}
	return "", "", "", false
}

func (f *fakeAPI) handle(w http.ResponseWriter, r *http.Request) {
	rc := f.newReqCtx(w, r)

	orgRaw, rest, ok := splitOrgPath(r.URL.Path)
	if !ok {
		rc.problem(http.StatusNotFound, "not_found", "Not Found", "Not Found")
		return
	}
	route, param, id, ok := parseRoute(r.Method, rest)
	if !ok {
		rc.problem(http.StatusNotFound, "not_found", "Not Found", "Not Found")
		return
	}

	// A malformed organization is refused before the credential is looked at
	// (deps.py current_principal), so it is a 400 whatever the key.
	if !anyUUIDRE.MatchString(orgRaw) {
		rc.invalid(wireFieldError{Field: "org", Message: "Input should be a valid UUID"})
		return
	}
	if !f.authenticate(rc) {
		return
	}
	// The org boundary: a key for another organization reads exactly like an
	// organization that does not exist (deps.py: 404 not_found).
	if orgRaw != f.org {
		rc.notFound("organization")
		return
	}
	// Path parameters are validated with the rest of the request, after the
	// credential and the org boundary (FastAPI raises dependency errors first).
	if param != "" && !anyUUIDRE.MatchString(id) {
		rc.invalid(wireFieldError{Field: param, Message: "Input should be a valid UUID"})
		return
	}

	switch route {
	case "POST compute/vms":
		f.createVm(rc)
	case "GET compute/vms/{vm_id}":
		f.getVm(rc, id)
	case "DELETE compute/vms/{vm_id}":
		f.deleteVm(rc, id)
	case "POST compute/public-ips":
		f.createPublicIp(rc)
	case "GET compute/public-ips":
		f.listPublicIps(rc)
	case "POST compute/public-ips/{public_ip_id}/associate":
		f.associatePublicIp(rc, id)
	case "POST compute/public-ips/{public_ip_id}/disassociate":
		f.disassociatePublicIp(rc, id)
	case "DELETE compute/public-ips/{public_ip_id}":
		f.releasePublicIp(rc, id)
	case "GET compute/images":
		f.listImages(rc)
	}
}

// authenticate checks the bearer credential: any other key is the deployed
// 401 ("one message for unparseable / unknown / wrong secret / revoked /
// expired").
func (f *fakeAPI) authenticate(rc *reqCtx) bool {
	if rc.r.Header.Get("Authorization") != "Bearer "+f.apiKey {
		rc.problem(http.StatusUnauthorized, "unauthorized", "Unauthorized", "the presented API token is not valid")
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

// --- strict body decoding (pydantic RequestModel, extra="forbid") ----------

// bodyCheck decodes a JSON object field by field the way a pydantic model with
// extra="forbid" does, collecting {field, message} entries instead of stopping
// at the first.
type bodyCheck struct {
	raw  map[string]json.RawMessage
	seen map[string]bool
	errs []wireFieldError
}

func newBodyCheck(r io.Reader) *bodyCheck {
	b := &bodyCheck{seen: map[string]bool{}}
	data, _ := io.ReadAll(r)
	if err := json.Unmarshal(data, &b.raw); err != nil || b.raw == nil {
		b.errs = append(b.errs, wireFieldError{Field: "0", Message: "JSON decode error"})
		b.raw = map[string]json.RawMessage{}
	}
	return b
}

func (b *bodyCheck) fail(field, message string) {
	b.errs = append(b.errs, wireFieldError{Field: field, Message: message})
}

// present reports whether name was sent, recording a "Field required" error
// when it is required and absent.
func (b *bodyCheck) present(name string, required bool) (json.RawMessage, bool) {
	b.seen[name] = true
	raw, ok := b.raw[name]
	if !ok && required {
		b.fail(name, "Field required")
	}
	return raw, ok
}

func (b *bodyCheck) str(name string, required bool, minLen, maxLen int) string {
	raw, ok := b.present(name, required)
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		b.fail(name, "Input should be a valid string")
		return ""
	}
	if len([]rune(s)) < minLen {
		b.fail(name, fmt.Sprintf("String should have at least %d character", minLen))
	}
	if maxLen > 0 && len([]rune(s)) > maxLen {
		b.fail(name, fmt.Sprintf("String should have at most %d characters", maxLen))
	}
	return s
}

func (b *bodyCheck) uuid(name string, required bool) string {
	s := b.str(name, required, 0, 0)
	if s != "" && !anyUUIDRE.MatchString(s) {
		b.fail(name, "Input should be a valid UUID")
	}
	return s
}

func (b *bodyCheck) integer(name string, required bool, ge int64) int {
	raw, ok := b.present(name, required)
	if !ok {
		return 0
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		b.fail(name, "Input should be a valid integer, unable to parse string as an integer")
		return 0
	}
	if n < ge {
		b.fail(name, fmt.Sprintf("Input should be greater than or equal to %d", ge))
	}
	return int(n)
}

func (b *bodyCheck) strList(name string) []string {
	raw, ok := b.present(name, false)
	if !ok {
		return []string{}
	}
	var l []string
	if err := json.Unmarshal(raw, &l); err != nil {
		b.fail(name, "Input should be a valid list")
		return []string{}
	}
	if l == nil {
		l = []string{}
	}
	return l
}

// finish reports every field the model does not declare, then all errors.
func (b *bodyCheck) finish() []wireFieldError {
	for name := range b.raw {
		if !b.seen[name] {
			b.fail(name, "Extra inputs are not permitted")
		}
	}
	sort.SliceStable(b.errs, func(i, j int) bool { return b.errs[i].Field < b.errs[j].Field })
	return b.errs
}

// --- VM handlers -----------------------------------------------------------

type vmCreateIn struct {
	Name, Image, StoragePool, Region, NetworkProfile string
	VCPUs, MemoryMB, DiskGB                          int
	SSHKeys                                          []string
}

func decodeVmCreate(r io.Reader) (vmCreateIn, []wireFieldError) {
	b := newBodyCheck(r)
	in := vmCreateIn{
		Name:           b.str("name", true, 1, 80),
		Image:          b.str("image", true, 0, 0),
		VCPUs:          b.integer("vcpus", true, 1),
		MemoryMB:       b.integer("memory_mb", true, 256),
		DiskGB:         b.integer("disk_gb", true, 1),
		StoragePool:    b.str("storage_pool", true, 0, 0),
		Region:         b.str("region", true, 0, 0),
		NetworkProfile: b.str("network_profile", true, 0, 0),
		SSHKeys:        b.strList("ssh_keys"),
	}
	return in, b.finish()
}

func (f *fakeAPI) newOperation(typ, resourceType, resourceID string, now time.Time) wireOperationRef {
	f.opSeq++
	return wireOperationRef{
		ID:           fakeUUID(3, f.opSeq),
		Type:         typ,
		State:        "pending",
		ResourceType: resourceType,
		ResourceID:   resourceID,
		Attempt:      0,
		MaxAttempts:  3,
		CreatedAt:    now,
	}
}

func (f *fakeAPI) operationLocation(op wireOperationRef) string {
	return "/v1/orgs/" + f.org + "/operations/" + op.ID
}

func (f *fakeAPI) createVm(rc *reqCtx) {
	req, errs := decodeVmCreate(rc.r.Body)
	if len(errs) > 0 {
		rc.invalid(errs...)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	for _, existing := range f.vms {
		if existing.Name == req.Name && existing.RegionID == req.Region && existing.State != "deleted" {
			rc.conflict("name_taken", fmt.Sprintf("a VM named %q already exists in %s", req.Name, req.Region))
			return
		}
	}

	f.createCount++
	f.vmSeq++
	f.lastIdemKey = rc.r.Header.Get("Idempotency-Key")

	id := fakeUUID(1, f.vmSeq)
	now := time.Now().UTC()

	// An "alias-" prefixed image id simulates a catalogue id (ImageOut.id)
	// that diverges from the glance_id the API echoes back on the created vm
	// (VmOut.image.glance_id) -- see Finding 1. Every other image id echoes
	// back unchanged, matching prior behavior.
	glanceID := req.Image
	if strings.HasPrefix(req.Image, "alias-") {
		glanceID = req.Image + "-glance-uuid"
	}

	vm := &fakeVm{wireVmOut{
		ID:               id,
		OrgID:            f.org,
		RegionID:         req.Region,
		Name:             req.Name,
		Image:            wireImageRef{GlanceID: glanceID, Name: req.Image},
		VCPUs:            req.VCPUs,
		MemoryMB:         req.MemoryMB,
		DiskGB:           req.DiskGB,
		StoragePoolID:    req.StoragePool,
		WorkloadType:     "customer",
		NetworkProfileID: req.NetworkProfile,
		State:            "pending",
		DesiredState:     "running",
		SSHKeyNames:      req.SSHKeys,
		AllowedActions:   allVmActions,
		CreatedAt:        now,
		UpdatedAt:        now,
	}}
	f.vms[id] = vm

	op := f.newOperation("vm.create", "vm", id, now)
	rc.json(http.StatusAccepted, map[string]any{"vm": vm.wireVmOut, "operation": op},
		"Location", f.operationLocation(op))
}

func (f *fakeAPI) getVm(rc *reqCtx, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	vm, ok := f.vms[id]
	if !ok {
		rc.notFound("vm")
		return
	}

	f.advance(vm)
	rc.json(http.StatusOK, vm.wireVmOut)
}

// advance moves vm one step through pending -> provisioning -> running (or
// -> error, for a name starting with "fail-"), and deleting -> deleted. It
// is a no-op in any terminal state, so repeated GETs after convergence are
// stable.
func (f *fakeAPI) advance(vm *fakeVm) {
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
			observed := time.Now().UTC()
			vm.ObservedAt = &observed
		}
	case "deleting":
		vm.State = "deleted"
	}
	vm.UpdatedAt = time.Now().UTC()
}

func (f *fakeAPI) deleteVm(rc *reqCtx, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	vm, ok := f.vms[id]
	if !ok {
		rc.notFound("vm")
		return
	}

	vm.State = "deleting"
	vm.UpdatedAt = time.Now().UTC()

	op := f.newOperation("vm.delete", "vm", id, time.Now().UTC())
	rc.json(http.StatusAccepted, map[string]any{"vm": vm.wireVmOut, "operation": op},
		"Location", f.operationLocation(op))
}

// --- Public IP handlers ------------------------------------------------

func (f *fakeAPI) newPublicIPOperation(typ, id string) wireOperationRef {
	return f.newOperation(typ, "public_ip", id, time.Now().UTC())
}

func (f *fakeAPI) publicIPAccepted(rc *reqCtx, ip *fakePublicIP, op wireOperationRef) {
	rc.json(http.StatusAccepted, map[string]any{"public_ip": ip.wirePublicIpOut, "operation": op},
		"Location", f.operationLocation(op))
}

// createPublicIp allocates a public IP in state "pending", or 422
// quota_exceeded when the org already has one tracked (this fake enforces a
// quota of one live public IP, matching the real API).
func (f *fakeAPI) createPublicIp(rc *reqCtx) {
	b := newBodyCheck(rc.r.Body)
	regionID := b.str("region_id", true, 0, 0)
	if errs := b.finish(); len(errs) > 0 {
		rc.invalid(errs...)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.publicIPs) > 0 {
		rc.unprocessable("quota_exceeded", "the organization's public IP quota (1) is already in use")
		return
	}

	f.pubIPCreateCount++
	id := fakeUUID(2, f.pubIPCreateCount)
	now := time.Now().UTC()

	ip := &fakePublicIP{wirePublicIpOut{
		ID:        id,
		RegionID:  regionID,
		State:     "pending",
		CreatedAt: now,
	}}
	f.publicIPs[id] = ip

	f.publicIPAccepted(rc, ip, f.newPublicIPOperation("ip.allocate", id))
}

// listPublicIps is the only read path for public IPs (there is no
// single-item GET, matching the real API); it advances every tracked public
// IP one step and drops any that just finished releasing, so it doubles as
// the state machine driver the client's GetPublicIp/WaitPublicIp rely on.
func (f *fakeAPI) listPublicIps(rc *reqCtx) {
	f.mu.Lock()
	defer f.mu.Unlock()

	ids := make([]string, 0, len(f.publicIPs))
	for id := range f.publicIPs {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	items := make([]wirePublicIpOut, 0, len(ids))
	for _, id := range ids {
		ip := f.publicIPs[id]
		if ip.State == "releasing" {
			delete(f.publicIPs, id)
			continue
		}
		f.advancePublicIp(ip)
		items = append(items, ip.wirePublicIpOut)
	}

	rc.json(http.StatusOK, wireList[wirePublicIpOut]{Items: items})
}

// advancePublicIp moves ip one step through pending -> allocated,
// associating -> associated (or -> error, when the vm being associated has a
// name starting "ipfail-", simulating an edge that refuses the bind), and
// disassociating -> allocated, clearing vm_id (or -> error, when the vm being
// disassociated has a name starting "ipdisfail-", simulating an edge that
// refuses to release the binding). It is a no-op in any other state, so
// repeated GETs after convergence are stable. Releasing is handled by the
// caller (listPublicIps) since it removes the entry rather than mutating it.
func (f *fakeAPI) advancePublicIp(ip *fakePublicIP) {
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

func (f *fakeAPI) associatePublicIp(rc *reqCtx, id string) {
	b := newBodyCheck(rc.r.Body)
	vmID := b.uuid("vm_id", true)
	if errs := b.finish(); len(errs) > 0 {
		rc.invalid(errs...)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	ip, ok := f.publicIPs[id]
	if !ok {
		rc.notFound("public_ip")
		return
	}

	ip.State = "associating"
	ip.VmID = &vmID

	f.publicIPAccepted(rc, ip, f.newPublicIPOperation("ip.associate", id))
}

func (f *fakeAPI) disassociatePublicIp(rc *reqCtx, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	ip, ok := f.publicIPs[id]
	if !ok {
		rc.notFound("public_ip")
		return
	}

	ip.State = "disassociating"
	f.callSeq[id] = append(f.callSeq[id], "disassociate")

	f.publicIPAccepted(rc, ip, f.newPublicIPOperation("ip.disassociate", id))
}

func (f *fakeAPI) releasePublicIp(rc *reqCtx, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	ip, ok := f.publicIPs[id]
	if !ok {
		rc.notFound("public_ip")
		return
	}

	ip.State = "releasing"
	f.callSeq[id] = append(f.callSeq[id], "release")

	f.publicIPAccepted(rc, ip, f.newPublicIPOperation("ip.release", id))
}

// --- Images handler ------------------------------------------------------

// listImages returns a fixed two-image catalogue for region "gro" and an
// empty one for any other region. region is a required query parameter, so
// leaving it out is a 400 invalid_request like any other validation failure.
func (f *fakeAPI) listImages(rc *reqCtx) {
	region, ok := rc.r.URL.Query()["region"]
	if !ok {
		rc.invalid(wireFieldError{Field: "region", Message: "Field required"})
		return
	}

	if len(region) == 0 || region[0] != "gro" {
		rc.json(http.StatusOK, wireList[wireImageOut]{Items: []wireImageOut{}})
		return
	}

	debianDistro, debianVersion := "debian", "12"
	ubuntuDistro, ubuntuVersion := "ubuntu", "22.04"
	rc.json(http.StatusOK, wireList[wireImageOut]{Items: []wireImageOut{
		{ID: "image-debian-12", Name: "Debian 12", OSDistro: &debianDistro, OSVersion: &debianVersion, MinDiskGB: 10, MinRAMMB: 512},
		{ID: "image-ubuntu-2204", Name: "Ubuntu 22.04", OSDistro: &ubuntuDistro, OSVersion: &ubuntuVersion, MinDiskGB: 10, MinRAMMB: 512},
	}})
}

// --- Direct tests of the fake itself ---------------------------------------

func TestFakeAPI_AuthRejectsWrongBearer(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)

	req, err := http.NewRequest(http.MethodGet, f.URL()+"/v1/orgs/"+testOrgID+"/compute/vms/"+testSomeUUID, nil)
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
	f := newFakeAPI(t, "good-key", testOrgID)

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
	f := newFakeAPI(t, "good-key", testOrgID)

	body := `{"name":"to-delete","image":"img","vcpus":1,"memory_mb":256,"disk_gb":10,"storage_pool":"pool","region":"gro","network_profile":"net"}`
	if status := doCreate(t, f, body); status != http.StatusAccepted {
		t.Fatalf("create: expected 202, got %d", status)
	}

	if f.CreateCount() != 1 {
		t.Fatalf("expected create count 1, got %d", f.CreateCount())
	}

	id := fakeUUID(1, 1)

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

// --- Contract tests of the fake against the 94f5021f customer API ----------
//
// The provider's own tests are only as good as the fake they run against, and
// until this file the fake serialised internal/client's own structs: a wrong
// JSON tag in the client was mirrored on the way out and never noticed. These
// tests pin the fake to the deployed API (s3-control-plane 94f5021f, spec
// docs/openapi/generated/customer-v1.json plus the handlers) with wire shapes
// transcribed from the spec, independent of internal/client.

const (
	testOrgID     = "7b2c1f10-52a1-4c3e-9d57-0a1b2c3d4e5f"
	testOtherOrg  = "0d9c8b7a-6f5e-4d3c-8b2a-1f0e9d8c7b6a"
	testSomeUUID  = "00000000-0000-4000-8000-0000000000aa"
	testValidVmJS = `{"name":"web-01","image":"image-1","vcpus":1,"memory_mb":1024,"disk_gb":10,"storage_pool":"pool-1","region":"gro","network_profile":"net-1"}`
)

// Required keys per schema, transcribed from customer-v1.json. The API always
// serialises every field (null for an unset optional one), so the set a client
// sees is exactly this.
var (
	specVmOutKeys = []string{
		"id", "org_id", "region_id", "name", "fqdn", "image", "vcpus", "memory_mb", "disk_gb",
		"storage_pool_id", "workload_type", "network_profile_id", "state", "desired_state",
		"error_class", "error_detail", "observed_status", "observed_addresses", "observed_at",
		"ssh_key_names", "user_data_present", "protected", "allowed_actions", "created_at", "updated_at",
	}
	specOperationRefKeys = []string{
		"id", "type", "state", "resource_type", "resource_id", "attempt", "max_attempts",
		"created_at", "error_class", "error_detail", "finished_at",
	}
	specPublicIpOutKeys = []string{"id", "region_id", "address", "vm_id", "state", "error", "created_at"}
	specImageOutKeys    = []string{"id", "name", "os_distro", "os_version", "min_disk_gb", "min_ram_mb"}
	specProblemKeys     = []string{"type", "title", "status", "detail", "instance", "code", "request_id"}
	specListKeys        = []string{"items", "next_cursor"}
)

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// fakeHTTP performs one request against f with the given bearer key (none when
// key is empty) and returns status, headers and the raw body. extra is a flat
// list of header name/value pairs.
func fakeHTTP(t *testing.T, f *fakeAPI, key, method, path, body string, extra ...string) (int, http.Header, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, f.URL()+path, rdr)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(extra); i += 2 {
		req.Header.Set(extra[i], extra[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, resp.Header, b
}

func decodeObject(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("response is not a JSON object: %v\n%s", err, b)
	}
	return m
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func wantKeys(t *testing.T, what string, got map[string]any, want []string) {
	t.Helper()
	w := append([]string(nil), want...)
	sort.Strings(w)
	g := sortedKeys(got)
	if strings.Join(g, ",") != strings.Join(w, ",") {
		t.Errorf("%s keys = %v, want exactly %v", what, g, w)
	}
}

func vmsPath(org string) string { return "/v1/orgs/" + org + "/compute/vms" }

func TestFakeAPI_ProblemEnvelopeMatchesSpec(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)

	path := vmsPath(testOrgID) + "/" + testSomeUUID
	status, hdr, body := fakeHTTP(t, f, "good-key", http.MethodGet, path, "")
	if status != http.StatusNotFound {
		t.Fatalf("unknown vm: status = %d, want 404 (%s)", status, body)
	}
	if ct := hdr.Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	p := decodeObject(t, body)
	wantKeys(t, "problem", p, specProblemKeys)
	if p["code"] != "not_found" || p["title"] != "Resource not found" {
		t.Errorf("code/title = %v/%v, want not_found/Resource not found", p["code"], p["title"])
	}
	if p["detail"] != "no vm matching that identifier is visible to this organization" {
		t.Errorf("detail = %q", p["detail"])
	}
	if p["instance"] != path || p["status"] != float64(404) {
		t.Errorf("instance/status = %v/%v", p["instance"], p["status"])
	}
	if p["type"] != "https://docs.virtscale.nl/s3/problems/not_found" {
		t.Errorf("type = %v", p["type"])
	}
	rid, _ := p["request_id"].(string)
	if rid == "" || hdr.Get("X-Request-ID") != rid {
		t.Errorf("request_id %q must be non-empty and equal the X-Request-ID header %q", rid, hdr.Get("X-Request-ID"))
	}
}

func TestFakeAPI_ForeignOrgIsTheSame404AsAMissingResource(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)

	status, _, body := fakeHTTP(t, f, "good-key", http.MethodGet, vmsPath(testOtherOrg)+"/"+testSomeUUID, "")
	if status != http.StatusNotFound {
		t.Fatalf("foreign org: status = %d, want 404", status)
	}
	p := decodeObject(t, body)
	if p["code"] != "not_found" || p["detail"] != "no organization matching that identifier is visible to this organization" {
		t.Errorf("foreign org problem = %v / %v", p["code"], p["detail"])
	}
}

func TestFakeAPI_BadKeyIs401UnauthorizedWithTheDeployedDetail(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)

	status, _, body := fakeHTTP(t, f, "wrong-key", http.MethodGet, vmsPath(testOrgID)+"/"+testSomeUUID, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("bad key: status = %d, want 401", status)
	}
	p := decodeObject(t, body)
	if p["code"] != "unauthorized" || p["detail"] != "the presented API token is not valid" {
		t.Errorf("401 problem = %v / %v", p["code"], p["detail"])
	}
}

func TestFakeAPI_PathIDsMustBeUUIDs(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)

	// A malformed id in the path is 400 invalid_request naming the parameter.
	status, _, body := fakeHTTP(t, f, "good-key", http.MethodGet, vmsPath(testOrgID)+"/not-a-uuid", "")
	if status != http.StatusBadRequest {
		t.Fatalf("malformed vm id: status = %d, want 400 (%s)", status, body)
	}
	p := decodeObject(t, body)
	if p["code"] != "invalid_request" || p["detail"] != "the request body or parameters failed validation" {
		t.Errorf("problem = %v / %v", p["code"], p["detail"])
	}
	errs, _ := p["errors"].([]any)
	if len(errs) != 1 || errs[0].(map[string]any)["field"] != "vm_id" {
		t.Errorf("errors = %v, want one entry for field vm_id", p["errors"])
	}

	// A malformed organization is refused before the credential is looked at
	// (deps.py current_principal), so even a missing key gets the 400.
	status, _, body = fakeHTTP(t, f, "", http.MethodGet, vmsPath("not-a-uuid")+"/"+testSomeUUID, "")
	if status != http.StatusBadRequest {
		t.Fatalf("malformed org: status = %d, want 400 (%s)", status, body)
	}
	errs, _ = decodeObject(t, body)["errors"].([]any)
	if len(errs) != 1 || errs[0].(map[string]any)["field"] != "org" {
		t.Errorf("errors = %v, want one entry for field org", errs)
	}
}

func TestFakeAPI_RequestBodiesAreStrict(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)

	cases := []struct {
		name, body, field, message string
	}{
		{"unknown field", `{"name":"web-01","image":"i","vcpus":1,"memory_mb":1024,"disk_gb":10,"storage_pool":"p","region":"gro","network_profile":"n","bogus":1}`, "bogus", "Extra inputs are not permitted"},
		{"missing required", `{"image":"i","vcpus":1,"memory_mb":1024,"disk_gb":10,"storage_pool":"p","region":"gro","network_profile":"n"}`, "name", "Field required"},
		{"vcpus below minimum", `{"name":"web-01","image":"i","vcpus":0,"memory_mb":1024,"disk_gb":10,"storage_pool":"p","region":"gro","network_profile":"n"}`, "vcpus", "Input should be greater than or equal to 1"},
		{"memory below minimum", `{"name":"web-01","image":"i","vcpus":1,"memory_mb":128,"disk_gb":10,"storage_pool":"p","region":"gro","network_profile":"n"}`, "memory_mb", "Input should be greater than or equal to 256"},
		{"wrong type", `{"name":"web-01","image":"i","vcpus":"one","memory_mb":1024,"disk_gb":10,"storage_pool":"p","region":"gro","network_profile":"n"}`, "vcpus", "Input should be a valid integer, unable to parse string as an integer"},
		{"name too long", `{"name":"` + strings.Repeat("a", 81) + `","image":"i","vcpus":1,"memory_mb":1024,"disk_gb":10,"storage_pool":"p","region":"gro","network_profile":"n"}`, "name", "String should have at most 80 characters"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, _, body := fakeHTTP(t, f, "good-key", http.MethodPost, vmsPath(testOrgID), tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", status, body)
			}
			p := decodeObject(t, body)
			if p["code"] != "invalid_request" {
				t.Errorf("code = %v, want invalid_request", p["code"])
			}
			errs, _ := p["errors"].([]any)
			found := false
			for _, e := range errs {
				m := e.(map[string]any)
				if m["field"] == tc.field && m["message"] == tc.message {
					found = true
				}
			}
			if !found {
				t.Errorf("errors = %v, want an entry {%s: %s}", p["errors"], tc.field, tc.message)
			}
		})
	}
	if f.CreateCount() != 0 {
		t.Errorf("a refused body created %d vms", f.CreateCount())
	}
}

func TestFakeAPI_ResponseShapesMatchSpec(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)

	// POST /compute/vms -> 202 {vm, operation} with a Location header.
	status, hdr, body := fakeHTTP(t, f, "good-key", http.MethodPost, vmsPath(testOrgID), testValidVmJS)
	if status != http.StatusAccepted {
		t.Fatalf("create: status = %d, want 202 (%s)", status, body)
	}
	created := decodeObject(t, body)
	wantKeys(t, "VmAccepted", created, []string{"vm", "operation"})
	vm := created["vm"].(map[string]any)
	op := created["operation"].(map[string]any)
	wantKeys(t, "VmOut", vm, specVmOutKeys)
	wantKeys(t, "OperationRef", op, specOperationRefKeys)
	wantKeys(t, "ImageRef", vm["image"].(map[string]any), []string{"glance_id", "name"})
	if id, _ := vm["id"].(string); !uuidRE.MatchString(id) {
		t.Errorf("vm id %q is not a UUID", vm["id"])
	}
	if id, _ := op["id"].(string); !uuidRE.MatchString(id) {
		t.Errorf("operation id %q is not a UUID", op["id"])
	}
	if want := "/v1/orgs/" + testOrgID + "/operations/" + op["id"].(string); hdr.Get("Location") != want {
		t.Errorf("Location = %q, want %q", hdr.Get("Location"), want)
	}
	if op["type"] != "vm.create" || op["resource_type"] != "vm" || op["resource_id"] != vm["id"] {
		t.Errorf("operation = %v, want vm.create on the new vm", op)
	}
	if vm["state"] != "pending" || vm["desired_state"] != "running" || vm["workload_type"] != "customer" {
		t.Errorf("vm state/desired/workload = %v/%v/%v", vm["state"], vm["desired_state"], vm["workload_type"])
	}

	// GET /compute/vms/{id} -> VmOut.
	status, _, body = fakeHTTP(t, f, "good-key", http.MethodGet, vmsPath(testOrgID)+"/"+vm["id"].(string), "")
	if status != http.StatusOK {
		t.Fatalf("get: status = %d (%s)", status, body)
	}
	wantKeys(t, "VmOut (GET)", decodeObject(t, body), specVmOutKeys)

	// GET /compute/images?region= -> ListResponse[ImageOut].
	status, _, body = fakeHTTP(t, f, "good-key", http.MethodGet, "/v1/orgs/"+testOrgID+"/compute/images?region=gro", "")
	if status != http.StatusOK {
		t.Fatalf("images: status = %d (%s)", status, body)
	}
	list := decodeObject(t, body)
	wantKeys(t, "ListResponse[ImageOut]", list, specListKeys)
	items := list["items"].([]any)
	if len(items) == 0 {
		t.Fatal("images: empty catalogue for region gro")
	}
	wantKeys(t, "ImageOut", items[0].(map[string]any), specImageOutKeys)

	// POST /compute/public-ips -> 202 {public_ip, operation}; list -> ListResponse[PublicIpOut].
	status, _, body = fakeHTTP(t, f, "good-key", http.MethodPost, "/v1/orgs/"+testOrgID+"/compute/public-ips", `{"region_id":"gro"}`)
	if status != http.StatusAccepted {
		t.Fatalf("allocate: status = %d (%s)", status, body)
	}
	acc := decodeObject(t, body)
	wantKeys(t, "PublicIpAccepted", acc, []string{"public_ip", "operation"})
	wantKeys(t, "PublicIpOut", acc["public_ip"].(map[string]any), specPublicIpOutKeys)
	wantKeys(t, "OperationRef (ip)", acc["operation"].(map[string]any), specOperationRefKeys)
	if acc["operation"].(map[string]any)["type"] != "ip.allocate" {
		t.Errorf("ip operation type = %v, want ip.allocate", acc["operation"].(map[string]any)["type"])
	}
	status, _, body = fakeHTTP(t, f, "good-key", http.MethodGet, "/v1/orgs/"+testOrgID+"/compute/public-ips", "")
	if status != http.StatusOK {
		t.Fatalf("list public ips: status = %d (%s)", status, body)
	}
	pl := decodeObject(t, body)
	wantKeys(t, "ListResponse[PublicIpOut]", pl, specListKeys)
	wantKeys(t, "PublicIpOut (list)", pl["items"].([]any)[0].(map[string]any), specPublicIpOutKeys)
}

func TestFakeAPI_ImagesRequireRegionAs400(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)

	status, _, body := fakeHTTP(t, f, "good-key", http.MethodGet, "/v1/orgs/"+testOrgID+"/compute/images", "")
	if status != http.StatusBadRequest {
		t.Fatalf("images without region: status = %d, want 400 (%s)", status, body)
	}
	p := decodeObject(t, body)
	errs, _ := p["errors"].([]any)
	if p["code"] != "invalid_request" || len(errs) != 1 || errs[0].(map[string]any)["field"] != "region" {
		t.Errorf("problem = %v errors = %v, want invalid_request naming region", p["code"], p["errors"])
	}
}
