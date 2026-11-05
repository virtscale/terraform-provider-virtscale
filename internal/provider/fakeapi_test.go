package provider

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/bits"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.virtscale.nl/virtscale/virtscale-platform/terraform-provider-virtscale/internal/contract"
)

// fakeAPI is an in-process httptest fake of the VirtScale customer API
// (s3-control-plane 94f5021f, plus the SSH-key keystore of the TF7 plan §4),
// scoped to the compute surface the provider needs: VMs, public IPs, SSH keys
// and the image catalogue.
//
// It deliberately does NOT reuse internal/client's types. Its wire structs
// below are transcribed from the deployed OpenAPI spec, so a wrong JSON tag in
// the client shows up as a failing test instead of being mirrored on the way
// out. docs/verification/terraform-provider-virtscale/04-contract-94f5021f.md
// lists, behaviour by behaviour, what is emulated and from which handler. The
// keystore routes follow docs/plans/terraform-tf7-ssh-keys.md §4 (the s3cp
// branch tf7/ssh-keys is not merged yet), and parse and fingerprint keys with
// code of their own, not the provider's.
type fakeAPI struct {
	apiKey string
	org    string
	srv    *httptest.Server

	// ps is the projects and VPCs (fakeapi_projects_test.go).
	ps projectState

	// contract checks every exchange with the provider's client against customer-v1.json (internal/contract);
	// a mismatch fails t.
	contract *contract.Spec
	t        *testing.T

	mu          sync.Mutex
	vms         map[string]*fakeVm
	createCount int
	lastIdemKey string
	nextOctet   int
	vmSeq       int
	opSeq       int
	reqSeq      int

	// idem is the Idempotency-Key store of the VM create (db/idempotency.py):
	// same key and same body replays the stored response.
	idem         map[string]*idemRecord
	idemKeysSeen []string

	// keys maps an API key to the scopes it was issued; sourceDenied makes
	// every authenticated request fail the key's allowed_cidrs check.
	keys         map[string][]string
	sourceDenied bool

	// faults are injected failures, served in order of registration.
	faults       []*fakeFault
	faultsServed int

	publicIPs        map[string]*fakePublicIP
	pubIPCreateCount int
	pubIPNextOctet   int
	// publicIPQuota is QuotaProfile.public_ips: 1 for customer-default,
	// negative for unlimited.
	publicIPQuota int

	// Pacing: how many reads a delete stays in "deleting", and how many list
	// reads still show a release as "releasing".
	deleteSteps  int
	releaseSteps int

	// callSeq records, per public IP id, the order "disassociate"/"release"
	// calls were observed -- used to assert Delete disassociates before it
	// releases.
	callSeq map[string][]string

	// The SSH-key keystore (TF7 plan §4). sshKeysOff is
	// S3CP_COMPUTE_SSH_KEYS_ENABLED=false: importing a key, and creating a VM
	// with a non-empty ssh_keys, answer 503 not_ready. sshKeyQuota is
	// QuotaProfile.ssh_keys (20 for customer-default, negative: unlimited).
	sshKeys         map[string]*wireSshKeyOut
	sshKeySeq       int
	sshKeysOff      bool
	sshKeyQuota     int
	sshIdemKeysSeen []string

	// inProgressLeft is how many repeats of an already-claimed Idempotency-Key
	// still answer 409 idempotency_in_progress before the stored replay: the
	// duplicate lost the insert race while the first request is still inside
	// its transaction (db/idempotency.py claim()).
	inProgressLeft int

	// calls records "METHOD route" for every request that reached a route.
	calls []string
	// requestedPaths records the URL path of every request that arrived at
	// handle(), unknown routes and requests an injected fault answered included.
	requestedPaths []string
	// patchCalls records every PATCH that reached the instance route, as it
	// arrived and before any gate.
	patchCalls []patchCall
}

// patchCall is one PATCH that reached compute/instances/{vm_id}: the headers it
// carried and the body it sent, recorded before any gate runs.
type patchCall struct{ ID, IfMatch, IdempotencyKey, Body string }

// --- wire types (customer-v1.json) ---------------------------------------

type wireImageRef struct {
	ImageID string `json:"image_id"`
	Name    string `json:"name"`
}

type wireVmOut struct {
	ID                string         `json:"id"`
	OrgID             string         `json:"org_id"`
	RegionID          string         `json:"region_id"`
	Name              string         `json:"name"`
	FQDN              *string        `json:"fqdn"`
	Hostname          *string        `json:"hostname"`
	InstanceType      *string        `json:"instance_type"`
	Image             wireImageRef   `json:"image"`
	VCPUs             int            `json:"vcpus"`
	MemoryMB          int            `json:"memory_mb"`
	DiskGB            int            `json:"disk_gb"`
	StoragePoolID     string         `json:"storage_pool_id"`
	WorkloadType      string         `json:"workload_type"`
	NetworkProfileID  string         `json:"network_profile_id"`
	State             string         `json:"state"`
	Status            string         `json:"status"`
	DesiredState      string         `json:"desired_state"`
	PrivateIP         *string        `json:"private_ip"`
	Addresses         []string       `json:"addresses"`
	Generation        int64          `json:"generation"`
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

// wireActionAvailability is ActionAvailabilityOut: whether one action can be asked of the VM right now.
type wireActionAvailability struct {
	Action    string  `json:"action"`
	Available bool    `json:"available"`
	Code      *string `json:"code"`
	Reason    *string `json:"reason"`
}

// MarshalJSON adds available_actions, which VmOut requires: one entry per action of the vocabulary, available when
// allowed_actions lists it and refused by policy otherwise.
func (v wireVmOut) MarshalJSON() ([]byte, error) {
	type plain wireVmOut
	out := struct {
		plain
		AvailableActions []wireActionAvailability `json:"available_actions"`
	}{plain: plain(v)}
	allowed := map[string]bool{}
	for _, a := range v.AllowedActions {
		allowed[a] = true
	}
	for _, a := range allVmActions {
		e := wireActionAvailability{Action: a, Available: allowed[a]}
		if !e.Available {
			code, reason := "vm_action_not_allowed", "this action is not allowed here"
			e.Code, e.Reason = &code, &reason
		}
		out.AvailableActions = append(out.AvailableActions, e)
	}
	return json.Marshal(out)
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

// wireSshKeyOut is SshKeyOut (TF7 plan §4): the key as the keystore holds it,
// public_key in canonical form "<type> <base64>" with the comment dropped.
type wireSshKeyOut struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	KeyType     string    `json:"key_type"`
	Fingerprint string    `json:"fingerprint"`
	PublicKey   string    `json:"public_key"`
	CreatedAt   time.Time `json:"created_at"`
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

	// inFlight is true while an operation (the create, then a delete) is
	// converging the row: uq_operations_one_in_flight allows one per resource.
	inFlight bool
	// deletedAt is the soft-delete tombstone the executor writes together with
	// state=deleted; a row carrying it reads as absent.
	deletedAt *time.Time
	// deletingLeft counts the reads the vm still stays in state deleting.
	deletingLeft int
}

// fakeFault is one injected failure.
type fakeFault struct {
	match     func(*http.Request) bool
	respond   func(*reqCtx)
	after     bool // process the request first, then replace the answer
	remaining int
}

// idemRecord is one stored Idempotency-Key answer. location is the create's
// Location header; etag is the PATCH answer's ETag (the create leaves it empty).
type idemRecord struct {
	payloadHash string
	status      int
	body        []byte
	location    string
	etag        string
}

// fakePublicIP is one public IP row.
type fakePublicIP struct {
	wirePublicIpOut

	// releasingLeft counts the list reads that still show the row releasing.
	releasingLeft int
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
		idem:           make(map[string]*idemRecord),
		keys:           map[string][]string{apiKey: {"compute:write", "network:write"}},
		nextOctet:      10,
		publicIPs:      make(map[string]*fakePublicIP),
		pubIPNextOctet: 10,
		publicIPQuota:  1,
		deleteSteps:    1,
		callSeq:        make(map[string][]string),
		sshKeys:        make(map[string]*wireSshKeyOut),
		sshKeyQuota:    20,
	}
	spec, err := contract.Load()
	if err != nil {
		t.Fatalf("load the contract: %v", err)
	}
	f.contract, f.t = spec, t
	f.ps = newProjectState(org)
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

// capture records the status and body a handler writes, and passes them on.
type capture struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (c *capture) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
	c.ResponseWriter.WriteHeader(status)
}

func (c *capture) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	c.body.Write(p)
	return c.ResponseWriter.Write(p)
}

// serveChecked serves rc and, for a request the provider's client made (the fake's own tests send others, some
// deliberately wrong), checks the exchange against the contract.
func (f *fakeAPI) serveChecked(rc *reqCtx) {
	if !strings.HasPrefix(rc.r.UserAgent(), "terraform-provider-virtscale/") {
		f.serve(rc)
		return
	}
	body, _ := io.ReadAll(rc.r.Body)
	rc.r.Body = io.NopCloser(bytes.NewReader(body))
	rec := &capture{ResponseWriter: rc.w}
	rc.w = rec
	f.serve(rc)
	for _, problem := range f.contract.Check(rc.r.Method, rc.r.URL.Path, body, rec.status, rec.Header().Get("Content-Type"), rec.body.Bytes()) {
		f.t.Errorf("contract: %s", problem)
	}
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

// SetPublicIPQuota sets the per-region public IP ceiling (negative: unlimited).
func (f *fakeAPI) SetPublicIPQuota(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publicIPQuota = n
}

// AddKey registers another API key with the given scopes. The fake's own
// apiKey holds compute:write and network:write, which is what the provider
// needs for every resource it has (README, "API key scopes").
func (f *fakeAPI) AddKey(key string, scopes ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys[key] = append([]string(nil), scopes...)
}

// DenySourceAddress makes every authenticated request answer 403
// source_address_not_allowed, as a key whose allowed_cidrs exclude the caller.
func (f *fakeAPI) DenySourceAddress() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sourceDenied = true
}

// FailNext answers the next n requests matching match with respond, without
// processing them (a proxy or rate limiter in front of the API).
func (f *fakeAPI) FailNext(n int, match func(*http.Request) bool, respond func(*reqCtx)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults = append(f.faults, &fakeFault{match: match, respond: respond, remaining: n})
}

// FailNextAfterHandling processes the next n requests matching match and then
// answers with respond instead: the response was lost.
func (f *fakeAPI) FailNextAfterHandling(n int, match func(*http.Request) bool, respond func(*reqCtx)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults = append(f.faults, &fakeFault{match: match, respond: respond, after: true, remaining: n})
}

// FaultsServed reports how many injected failures have been served.
func (f *fakeAPI) FaultsServed() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.faultsServed
}

// IdempotencyKeys lists the Idempotency-Key of every create that reached the API.
func (f *fakeAPI) IdempotencyKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.idemKeysSeen...)
}

// takeFault returns the first registered fault matching r that has failures
// left, consuming one.
func (f *fakeAPI) takeFault(r *http.Request) *fakeFault {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, fault := range f.faults {
		if fault.remaining > 0 && fault.match(r) {
			fault.remaining--
			f.faultsServed++
			return fault
		}
	}
	return nil
}

// ClearFaults forgets every injected failure, served or not.
func (f *fakeAPI) ClearFaults() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults = nil
}

// SSHKeyNames reports the key names the API recorded for vm id
// (VmOut.ssh_key_names): a launch-time snapshot, so deleting a key from the
// keystore later does not change it.
func (f *fakeAPI) SSHKeyNames(id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	vm, ok := f.vms[id]
	if !ok {
		return nil
	}
	return append([]string(nil), vm.SSHKeyNames...)
}

// VmIDByName returns the id of the live vm called name, or "" when there is none.
func (f *fakeAPI) VmIDByName(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, vm := range f.vms {
		if vm.Name == name && vm.deletedAt == nil {
			return id
		}
	}
	return ""
}

// SetSSHKeyNames overwrites VmOut.ssh_key_names of vm id, as a deployment that
// reports the names in another order would.
func (f *fakeAPI) SetSSHKeyNames(id string, names ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if vm, ok := f.vms[id]; ok {
		vm.SSHKeyNames = append([]string{}, names...)
	}
}

// SetSSHKeysOn switches the keystore on or off (S3CP_COMPUTE_SSH_KEYS_ENABLED).
// It is on by default; while it is off, importing a key and creating a VM with
// a non-empty ssh_keys answer 503 not_ready, and reading or deleting keys is
// still served.
func (f *fakeAPI) SetSSHKeysOn(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sshKeysOff = !on
}

// SetSSHKeyQuota sets the per-organization ceiling on keys (negative:
// unlimited). The default is 20, customer-default's.
func (f *fakeAPI) SetSSHKeyQuota(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sshKeyQuota = n
}

// SSHKeyCount reports how many keys the keystore holds.
func (f *fakeAPI) SSHKeyCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sshKeys)
}

// SSHKey returns a copy of key id, and whether the keystore holds it.
func (f *fakeAPI) SSHKey(id string) (wireSshKeyOut, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.sshKeys[id]
	if !ok {
		return wireSshKeyOut{}, false
	}
	return *k, true
}

// SSHKeyID returns the id of the key called name, or "" when there is none.
func (f *fakeAPI) SSHKeyID(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.sshKeys {
		if k.Name == name {
			return k.ID
		}
	}
	return ""
}

// VanishSSHKey removes key id, as a deletion out of band.
func (f *fakeAPI) VanishSSHKey(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sshKeys, id)
}

// SSHKeyIdempotencyKeys lists the Idempotency-Key of every key import that
// reached the API.
func (f *fakeAPI) SSHKeyIdempotencyKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sshIdemKeysSeen...)
}

// InProgressFor makes the next n repeats of an already-claimed Idempotency-Key
// (on the VM create or on the key import) answer 409 idempotency_in_progress
// before the stored replay: the first request is still inside its transaction.
func (f *fakeAPI) InProgressFor(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inProgressLeft = n
}

// Calls lists "METHOD route" of every request that reached a route, in order,
// whatever its outcome (a request answered by an injected fault before it
// reached the fake is not listed).
func (f *fakeAPI) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// CallsSince lists the calls recorded after the first n.
func (f *fakeAPI) CallsSince(n int) []string {
	calls := f.Calls()
	if n >= len(calls) {
		return nil
	}
	return calls[n:]
}

// CallCount counts the recorded calls of route ("POST compute/ssh-keys").
func (f *fakeAPI) CallCount(route string) int {
	n := 0
	for _, c := range f.Calls() {
		if c == route {
			n++
		}
	}
	return n
}

// PatchCalls lists every PATCH that reached the instance route, in arrival
// order, copied: it is recorded before any gate, so refused attempts are there
// too.
func (f *fakeAPI) PatchCalls() []patchCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]patchCall(nil), f.patchCalls...)
}

// Generation reports the generation of vm id, or 0 when there is none.
func (f *fakeAPI) Generation(id string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if vm, ok := f.vms[id]; ok {
		return vm.Generation
	}
	return 0
}

// Hostname reports the hostname of vm id, or "" when it is null or unknown.
func (f *fakeAPI) Hostname(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if vm, ok := f.vms[id]; ok && vm.Hostname != nil {
		return *vm.Hostname
	}
	return ""
}

// RequestedPaths lists the URL path of every request that arrived at handle(),
// in order, unknown routes and requests answered by an injected fault included.
func (f *fakeAPI) RequestedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requestedPaths...)
}

// SeedForeignInstance inserts a live vm owned by ANOTHER organization
// (testOtherOrg) that holds hostname. It is not visible to f.org's routes and
// never counts as a holder of hostname in f.org, so a create or patch in f.org
// may still take it.
func (f *fakeAPI) SeedForeignInstance(hostname string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vmSeq++
	id := fakeUUID(1, f.vmSeq)
	h := hostname
	now := time.Now().UTC()
	f.vms[id] = &fakeVm{wireVmOut: wireVmOut{
		ID:               id,
		OrgID:            testOtherOrg,
		RegionID:         "gro",
		Name:             "foreign-instance",
		Hostname:         &h,
		Image:            wireImageRef{ImageID: "image-1", Name: "image-1"},
		VCPUs:            1,
		MemoryMB:         1024,
		DiskGB:           10,
		StoragePoolID:    "pool-1",
		WorkloadType:     "customer",
		NetworkProfileID: "net-1",
		State:            "running",
		Status:           "running",
		DesiredState:     "running",
		Addresses:        []string{},
		Generation:       1,
		AllowedActions:   allVmActions,
		CreatedAt:        now,
		UpdatedAt:        now,
	}}
}

// mutating filters calls down to those that can change something: anything
// but a GET.
func mutating(calls []string) []string {
	var out []string
	for _, c := range calls {
		if !strings.HasPrefix(c, "GET ") {
			out = append(out, c)
		}
	}
	return out
}

// Protect switches deletion protection on or off for vm id (VmOut.protected).
// While it is on a delete is 409 vm_protected, and allowed_actions drops delete.
func (f *fakeAPI) Protect(id string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vm, ok := f.vms[id]
	if !ok {
		return
	}
	vm.Protected = on
	vm.AllowedActions = nil
	for _, action := range allVmActions {
		if !(on && action == "delete") {
			vm.AllowedActions = append(vm.AllowedActions, action)
		}
	}
}

// SetDeleteSteps makes a delete stay in state "deleting" for n reads before
// the vm is gone (the default is one), so a test can act while it runs.
func (f *fakeAPI) SetDeleteSteps(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteSteps = n
}

// SetReleaseSteps makes a release keep its row, in state "releasing", for n
// list reads before it disappears (the default is zero: the first list drops it).
func (f *fakeAPI) SetReleaseSteps(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseSteps = n
}

// Vanish removes vm id from the store entirely, so the next GetVm returns a
// genuine 404 -- unlike deleteVm, which retains the vm in state "deleted".
// It simulates an out-of-band deletion.
func (f *fakeAPI) Vanish(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.vms, id)
}

// VanishPublicIP removes public ip id from the store, as a release out of band:
// the list no longer shows it, so the client's GetPublicIp finds it absent.
func (f *fakeAPI) VanishPublicIP(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.publicIPs, id)
}

// --- request plumbing ------------------------------------------------------

// reqCtx is one request in flight: it knows its own request id, which every
// response (success or problem) carries, like the real API's X-Request-ID.
type reqCtx struct {
	f      *fakeAPI
	w      http.ResponseWriter
	r      *http.Request
	rid    string
	scopes []string // the authenticated key's scopes
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

// raw writes an already-encoded success body (an idempotent replay).
func (rc *reqCtx) raw(status int, body []byte, headers ...string) {
	rc.w.Header().Set("Content-Type", "application/json")
	rc.w.Header().Set("X-Request-ID", rc.rid)
	for i := 0; i+1 < len(headers); i += 2 {
		rc.w.Header().Set(headers[i], headers[i+1])
	}
	rc.w.WriteHeader(status)
	_, _ = rc.w.Write(body)
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

// inProgress is db/idempotency.py's 409 idempotency_in_progress. The real API
// sends no Retry-After with it, so the client backs off with jitter (which has
// its own client tests); the fake sends 0 so that the acceptance tests, which
// cannot shorten that backoff, do not sleep.
func (rc *reqCtx) inProgress() {
	rc.w.Header().Set("Retry-After", "0")
	rc.conflict("idempotency_in_progress", detailIdempotencyInProgress)
}

// noContent writes an empty 204, the success of a delete that has nothing to say.
func (rc *reqCtx) noContent() {
	rc.w.Header().Set("X-Request-ID", rc.rid)
	rc.w.WriteHeader(http.StatusNoContent)
}

// notFound is errors.py not_found(resource): the one helper every resource 404
// of the API comes from (vm, public_ip, ssh_key, and also organization,
// "compute service" and region).
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
	case rest == "compute/instances":
		key = "compute/instances"
	case len(parts) == 3 && parts[0] == "compute" && parts[1] == "instances":
		key, param, id = "compute/instances/{vm_id}", "vm_id", parts[2]
	case rest == "compute/images":
		key = "compute/images"
	case rest == "compute/regions", rest == "compute/plans", rest == "instance-types", rest == "projects":
		key = rest
	case len(parts) == 2 && parts[0] == "projects":
		key, param, id = "projects/{project}", "project", parts[1]
	case rest == "compute/public-ips":
		key = "compute/public-ips"
	case len(parts) == 3 && parts[0] == "compute" && parts[1] == "public-ips":
		key, param, id = "compute/public-ips/{public_ip_id}", "public_ip_id", parts[2]
	case len(parts) == 4 && parts[0] == "compute" && parts[1] == "public-ips" && (parts[3] == "associate" || parts[3] == "disassociate"):
		key, param, id = "compute/public-ips/{public_ip_id}/"+parts[3], "public_ip_id", parts[2]
	case rest == "compute/ssh-keys":
		key = "compute/ssh-keys"
	case len(parts) == 3 && parts[0] == "compute" && parts[1] == "ssh-keys":
		key, param, id = "compute/ssh-keys/{ssh_key_id}", "ssh_key_id", parts[2]
	default:
		return "", "", "", false
	}
	switch method + " " + key {
	case "POST compute/instances", "GET compute/instances/{vm_id}",
		"DELETE compute/instances/{vm_id}", "PATCH compute/instances/{vm_id}",
		"GET compute/images", "GET compute/regions", "GET compute/plans", "GET instance-types",
		"GET projects", "POST projects", "GET projects/{project}", "PATCH projects/{project}", "DELETE projects/{project}",
		"GET compute/public-ips", "POST compute/public-ips", "DELETE compute/public-ips/{public_ip_id}",
		"POST compute/public-ips/{public_ip_id}/associate", "POST compute/public-ips/{public_ip_id}/disassociate",
		"POST compute/ssh-keys", "GET compute/ssh-keys",
		"GET compute/ssh-keys/{ssh_key_id}", "DELETE compute/ssh-keys/{ssh_key_id}":
		return method + " " + key, param, id, true
	}
	return "", "", "", false
}

func (f *fakeAPI) handle(w http.ResponseWriter, r *http.Request) {
	rc := f.newReqCtx(w, r)
	f.mu.Lock()
	f.requestedPaths = append(f.requestedPaths, r.URL.Path)
	f.mu.Unlock()
	if fault := f.takeFault(r); fault != nil {
		if fault.after {
			f.serve(&reqCtx{f: f, w: httptest.NewRecorder(), r: r, rid: rc.rid})
		}
		fault.respond(rc)
		return
	}
	f.serveChecked(rc)
}

// routePermissions is ROUTE_PERMISSIONS (permissions.py) restricted to the
// routes this fake serves.
var routePermissions = map[string]string{
	"POST compute/instances":                              "compute.instance.create",
	"GET compute/instances/{vm_id}":                       "compute.instance.read",
	"DELETE compute/instances/{vm_id}":                    "compute.instance.delete",
	"PATCH compute/instances/{vm_id}":                     "compute.instance.update",
	"GET compute/images":                                  "compute.instance.list",
	"GET compute/regions":                                 "compute.instance.list",
	"GET compute/plans":                                   "compute.instance.list",
	"GET instance-types":                                  "compute.instance_type.list",
	"GET projects":                                        "project.list",
	"POST projects":                                       "project.create",
	"GET projects/{project}":                              "project.read",
	"PATCH projects/{project}":                            "project.update",
	"DELETE projects/{project}":                           "project.delete",
	"POST projects/{project}/vpcs":                        "network.vpc.create",
	"GET projects/{project}/vpcs":                         "network.vpc.list",
	"GET projects/{project}/vpcs/{vpc}":                   "network.vpc.read",
	"DELETE projects/{project}/vpcs/{vpc}":                "network.vpc.delete",
	"GET compute/public-ips":                              "edge.public_ip.read",
	"POST compute/public-ips":                             "edge.public_ip.allocate",
	"DELETE compute/public-ips/{public_ip_id}":            "edge.public_ip.release",
	"POST compute/public-ips/{public_ip_id}/associate":    "edge.public_ip.allocate",
	"POST compute/public-ips/{public_ip_id}/disassociate": "edge.public_ip.release",
	// The four compute.ssh_key.* permissions of the TF7 plan §3.3 point 3: the
	// reads sit with compute:read, the mutations with compute:write.
	"POST compute/ssh-keys":                "compute.ssh_key.create",
	"GET compute/ssh-keys":                 "compute.ssh_key.list",
	"GET compute/ssh-keys/{ssh_key_id}":    "compute.ssh_key.read",
	"DELETE compute/ssh-keys/{ssh_key_id}": "compute.ssh_key.delete",
}

// scopeGrants is SCOPE_PERMISSIONS (permissions.py): the legacy read and write
// scopes, and the family bundles compute:* (compute. and operation.) and
// network:* (network. and edge.), each also carrying org.read.
func scopeGrants(scope, perm string) bool {
	isRead := strings.HasSuffix(perm, ".read") || strings.HasSuffix(perm, ".list")
	switch scope {
	case "write":
		return !strings.HasPrefix(perm, "iam.")
	case "read":
		return !strings.HasPrefix(perm, "iam.") && isRead
	case "compute:read", "compute:write", "network:read", "network:write":
		if perm == "org.read" {
			return true
		}
		// compute:read and compute:write also carry project.list and project.read (permissions.py).
		if (perm == "project.list" || perm == "project.read") && strings.HasPrefix(scope, "compute:") {
			return true
		}
		family, half, _ := strings.Cut(scope, ":")
		prefixes := []string{"compute.", "operation."}
		if family == "network" {
			prefixes = []string{"network.", "edge."}
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(perm, prefix) {
				return half == "write" || isRead
			}
		}
	}
	return false
}

func (rc *reqCtx) allows(perm string) bool {
	for _, scope := range rc.scopes {
		if scopeGrants(scope, perm) {
			return true
		}
	}
	return false
}

func (f *fakeAPI) serve(rc *reqCtx) {
	r := rc.r
	if r.URL.Path == "/v1/me" {
		f.serveMe(rc)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/projects/") {
		f.serveProjectScoped(rc)
		return
	}
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
	f.mu.Lock()
	f.calls = append(f.calls, route)
	f.mu.Unlock()

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
	// A key restricted to other networks is refused once it has proved to be a
	// valid key of this organization (deps.py _principal_from_api_token).
	f.mu.Lock()
	denied := f.sourceDenied
	f.mu.Unlock()
	if denied {
		rc.problem(http.StatusForbidden, "source_address_not_allowed", "Forbidden",
			"this API token cannot be used from this network address")
		return
	}
	// Then the permission (require_permission), before anything about the
	// request itself is validated.
	if perm := routePermissions[route]; perm != "" && !rc.allows(perm) {
		rc.problem(http.StatusForbidden, "permission_denied", "Forbidden",
			"this credential does not carry the '"+perm+"' permission")
		return
	}
	// Path parameters are validated with the rest of the request, after the
	// credential and the org boundary (FastAPI raises dependency errors first).
	if param != "" && !anyUUIDRE.MatchString(id) {
		rc.invalid(wireFieldError{Field: param, Message: "Input should be a valid UUID"})
		return
	}

	switch route {
	case "POST compute/instances":
		f.createVm(rc)
	case "GET compute/instances/{vm_id}":
		f.getVm(rc, id)
	case "DELETE compute/instances/{vm_id}":
		f.deleteVm(rc, id)
	case "PATCH compute/instances/{vm_id}":
		f.patchVm(rc, id)
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
	case "GET compute/regions":
		f.listRegions(rc)
	case "GET compute/plans":
		f.listPlans(rc)
	case "GET instance-types":
		f.listInstanceTypes(rc)
	case "POST projects":
		f.createProject(rc)
	case "GET projects":
		f.listProjects(rc)
	case "GET projects/{project}":
		f.getProject(rc, id)
	case "PATCH projects/{project}":
		f.patchProject(rc, id)
	case "DELETE projects/{project}":
		f.deleteProject(rc, id)
	case "POST compute/ssh-keys":
		f.importSshKey(rc)
	case "GET compute/ssh-keys":
		f.listSshKeys(rc)
	case "GET compute/ssh-keys/{ssh_key_id}":
		f.getSshKey(rc, id)
	case "DELETE compute/ssh-keys/{ssh_key_id}":
		f.deleteSshKey(rc, id)
	}
}

// authenticate checks the bearer credential: any other key is the deployed
// 401 ("one message for unparseable / unknown / wrong secret / revoked /
// expired").
func (f *fakeAPI) authenticate(rc *reqCtx) bool {
	key, found := strings.CutPrefix(rc.r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	scopes, ok := f.keys[key]
	f.mu.Unlock()
	if !found || !ok {
		rc.problem(http.StatusUnauthorized, "unauthorized", "Unauthorized", "the presented API token is not valid")
		return false
	}
	rc.scopes = scopes
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

func (b *bodyCheck) nullableStr(name string) *string {
	raw, ok := b.present(name, false)
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		b.fail(name, "Input should be a valid string")
		return nil
	}
	return &s
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

// vmHostnamePattern is the customer API's hostname rule: a dotted sequence of
// DNS labels. The create and patch bodies are checked against it after the 253
// character ceiling.
const vmHostnamePattern = `^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`

var vmHostnameRE = regexp.MustCompile(vmHostnamePattern)

// checkHostname records the length, then the pattern, error for a hostname,
// with the wording the API's validator uses.
func (b *bodyCheck) checkHostname(field, value string) {
	if len([]rune(value)) > 253 {
		b.fail(field, "String should have at most 253 characters")
		return
	}
	if !vmHostnameRE.MatchString(value) {
		b.fail(field, fmt.Sprintf("String should match pattern '%s'", vmHostnamePattern))
	}
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

// vmCreateIn holds the create body. Hostname and InstanceType are plain
// strings -- empty meaning not given -- on purpose: the idempotency digest is
// fmt.Sprintf("%+v", req), which would print a pointer as an address.
type vmCreateIn struct {
	Name, Image, StoragePool, Region, NetworkProfile, WorkloadType string
	Hostname, InstanceType                                         string
	VCPUs, MemoryMB, DiskGB                                        int
	SSHKeys                                                        []string
	UserData                                                       *string
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
		WorkloadType:   b.str("workload_type", false, 0, 0),
		Hostname:       strOrEmpty(b.nullableStr("hostname")),
		InstanceType:   strOrEmpty(b.nullableStr("instance_type")),
		SSHKeys:        b.strList("ssh_keys"),
		UserData:       b.nullableStr("user_data"),
	}
	if in.WorkloadType == "" {
		in.WorkloadType = "customer"
	}
	if in.Hostname != "" {
		b.checkHostname("hostname", in.Hostname)
	}
	return in, b.finish()
}

// strOrEmpty reads an optional string: absent and null both mean "".
func strOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
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

// vmNameRE is naming.py's NAME_RE: a DNS label, 1-63 characters.
var vmNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// canonicalVmName is naming.py canonical_name: trimmed, lower-cased, a
// trailing .virtscale.nl stripped, then it must be a DNS label.
func canonicalVmName(raw string) (string, bool) {
	name := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".virtscale.nl")
	return name, vmNameRE.MatchString(name)
}

const (
	detailOperationInFlightVm = "another operation is already running for this VM; wait for it to finish and retry"
	detailUserDataUnsupported = "user_data is not accepted: the instance is built asynchronously, so cloud-init would " +
		"have to be persisted to reach the worker that boots it -- and cloud-init is where bootstrap secrets go, " +
		"so where such a payload may live is still an open design question"
)

// createVm follows compute.py create_vm's gate order: body validation, then
// user_data (before the key is claimed), then -- TF7 -- the SSH-key flag
// (a non-empty ssh_keys while the keystore is off is 503 not_ready, also
// before the key is claimed), then the Idempotency-Key claim, then
// create_vm_request's own checks, which resolve ssh_keys against the keystore.
func (f *fakeAPI) createVm(rc *reqCtx) {
	req, errs := decodeVmCreate(rc.r.Body)
	if len(errs) > 0 {
		rc.invalid(errs...)
		return
	}
	if req.UserData != nil {
		rc.unprocessable("user_data_unsupported", detailUserDataUnsupported)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if len(req.SSHKeys) > 0 && f.sshKeysOff {
		rc.problem(http.StatusServiceUnavailable, "not_ready", "Service unavailable", detailSshKeysOff)
		return
	}

	key := rc.r.Header.Get("Idempotency-Key")
	var digest string
	if key != "" {
		f.idemKeysSeen = append(f.idemKeysSeen, key)
		sum := sha256.Sum256([]byte(fmt.Sprintf("%+v", req)))
		digest = hex.EncodeToString(sum[:])
		if rec, ok := f.idem[key]; ok {
			if f.inProgressLeft > 0 {
				f.inProgressLeft--
				rc.inProgress()
				return
			}
			if rec.payloadHash != digest {
				rc.conflict("idempotency_key_reused", "this Idempotency-Key was already used with a different request body")
				return
			}
			rc.raw(rec.status, rec.body, "Location", rec.location)
			return
		}
	}

	name, ok := canonicalVmName(req.Name)
	if !ok {
		rc.unprocessable("invalid_name", "name must be 1-63 characters of a-z, 0-9 and '-', not starting or ending with '-'")
		return
	}
	if !f.resolveSshKeys(rc, req.SSHKeys) {
		return
	}
	if req.WorkloadType != "customer" {
		if req.WorkloadType != "infra" {
			rc.unprocessable("invalid_spec", "workload_type must be one of: infra, customer")
			return
		}
		rc.unprocessable("workload_not_allowed", "a customer account cannot request infra workloads")
		return
	}
	for _, existing := range f.vms {
		if existing.Name == name && existing.RegionID == req.Region && existing.deletedAt == nil {
			rc.conflict("name_taken", fmt.Sprintf("a VM named '%s' already exists in %s", name, req.Region))
			return
		}
	}
	if req.Hostname != "" && f.hostnameTaken(req.Hostname, "") {
		rc.conflict("hostname_taken", hostnameTakenDetail(req.Hostname))
		return
	}

	f.createCount++
	f.vmSeq++
	f.lastIdemKey = key

	id := fakeUUID(1, f.vmSeq)
	now := time.Now().UTC()

	// An "alias-" prefixed image id simulates a catalogue id (ImageOut.id)
	// that diverges from the image_id the API echoes back on the created vm
	// (VmOut.image.image_id) -- see Finding 1. Every other image id echoes
	// back unchanged, matching prior behavior.
	imageID := req.Image
	if strings.HasPrefix(req.Image, "alias-") {
		imageID = req.Image + "-image-uuid"
	}

	var hostname *string
	if req.Hostname != "" {
		h := req.Hostname
		hostname = &h
	}
	var instanceType *string
	switch {
	case req.InstanceType != "":
		it := req.InstanceType
		instanceType = &it
	case strings.HasPrefix(name, "typed-"):
		it := "general.small"
		instanceType = &it
	}

	vm := &fakeVm{
		wireVmOut: wireVmOut{
			ID:               id,
			OrgID:            f.org,
			RegionID:         req.Region,
			Name:             name,
			Hostname:         hostname,
			InstanceType:     instanceType,
			Image:            wireImageRef{ImageID: imageID, Name: req.Image},
			VCPUs:            req.VCPUs,
			MemoryMB:         req.MemoryMB,
			DiskGB:           req.DiskGB,
			StoragePoolID:    req.StoragePool,
			WorkloadType:     "customer",
			NetworkProfileID: req.NetworkProfile,
			State:            "pending",
			Status:           "pending",
			DesiredState:     "running",
			Addresses:        []string{},
			Generation:       1,
			SSHKeyNames:      req.SSHKeys,
			AllowedActions:   allVmActions,
			CreatedAt:        now,
			UpdatedAt:        now,
		},
		inFlight: true,
	}
	f.vms[id] = vm

	op := f.newOperation("vm.create", "vm", id, now)
	payload := map[string]any{"vm": vm.wireVmOut, "operation": op}
	location := f.operationLocation(op)
	if key != "" {
		body, _ := json.Marshal(payload)
		f.idem[key] = &idemRecord{
			payloadHash: digest,
			status:      http.StatusAccepted,
			body:        append(body, '\n'),
			location:    location,
		}
	}
	rc.json(http.StatusAccepted, payload, "Location", location, "ETag", generationETag(vm.Generation))
}

// hostnameTaken reports whether a live instance of f.org other than exceptID
// holds hostname. A row of another organization never counts as a holder.
// f.mu must be held.
func (f *fakeAPI) hostnameTaken(hostname, exceptID string) bool {
	for id, vm := range f.vms {
		if id == exceptID || vm.OrgID != f.org || vm.deletedAt != nil || vm.Hostname == nil {
			continue
		}
		if *vm.Hostname == hostname {
			return true
		}
	}
	return false
}

func hostnameTakenDetail(hostname string) string {
	return fmt.Sprintf("the hostname '%s' is already in use by another instance of this organization", hostname)
}

// getVm: "a foreign row and a soft-deleted row both read as absent"
// (compute.py get_vm). The read that moves a deleting vm to deleted therefore
// answers 404 at once, as the real GET does once the executor has committed.
func (f *fakeAPI) getVm(rc *reqCtx, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	vm, ok := f.vms[id]
	if !ok || vm.deletedAt != nil {
		rc.notFound("vm")
		return
	}

	f.advance(vm)
	if vm.deletedAt != nil {
		rc.notFound("vm")
		return
	}
	rc.json(http.StatusOK, vm.wireVmOut, "ETag", generationETag(vm.Generation))
}

// generationETag is the ETag every GET, the create answer and the patch answer
// carry: the generation, in double quotes.
func generationETag(n int64) string { return fmt.Sprintf("\"%d\"", n) }

// advance moves vm one step the way the executor does: pending ->
// provisioning -> running (or -> error, for a name starting with "fail-");
// and, once a delete has set desired_state=deleted, -> deleting -> deleted
// with the tombstone. It is a no-op in any other state, so repeated GETs after
// convergence are stable.
func (f *fakeAPI) advance(vm *fakeVm) {
	switch {
	case vm.State == "deleting":
		vm.deletingLeft--
		if vm.deletingLeft <= 0 {
			vm.State = "deleted"
			now := time.Now().UTC()
			vm.deletedAt = &now
			vm.inFlight = false
			vm.ErrorClass, vm.ErrorDetail = nil, nil
		}
	case vm.DesiredState == "deleted":
		vm.State = "deleting"
	case vm.State == "pending":
		vm.State = "provisioning"
	case vm.State == "provisioning":
		vm.inFlight = false
		if strings.HasPrefix(vm.Name, "fail-") {
			errClass := "build_failed"
			errDetail := "the cloud could not build this instance"
			vm.State = "error"
			vm.ErrorClass = &errClass
			vm.ErrorDetail = &errDetail
		} else {
			vm.State = "running"
			f.nextOctet++
			addr := fmt.Sprintf("10.128.3.%d", f.nextOctet)
			// observed_status and observed_addresses are still published, as
			// the real API still publishes them, but they are decoys now: a
			// client that keeps reading them reports the wrong values.
			active := "ACTIVE"
			vm.ObservedStatus = &active
			vm.ObservedAddresses = map[string]any{fakeNetwork: []any{map[string]any{
				"addr":                    fmt.Sprintf("198.51.100.%d", f.nextOctet),
				"version":                 4,
				"OS-EXT-IPS:type":         "fixed",
				"OS-EXT-IPS-MAC:mac_addr": "fa:16:3e:00:00:00",
			}}}
			observed := time.Now().UTC()
			vm.ObservedAt = &observed
			if strings.HasPrefix(vm.Name, "noip-") {
				vm.PrivateIP = nil
				vm.Addresses = []string{}
			} else {
				vm.PrivateIP = &addr
				vm.Addresses = []string{addr}
			}
		}
	}
	vm.Status = vm.State
	vm.UpdatedAt = time.Now().UTC()
}

// fakeNetwork is the one network the fake's vms sit on.
const fakeNetwork = "gro-tenant-default"

// attachFloating PREPENDS an associated public IP to the vm's addresses, so
// addresses[0] is the public one while private_ip keeps the private one. It no
// longer touches observed_addresses: those are decoys now.
func attachFloating(vm *fakeVm, addr string) {
	vm.Addresses = append([]string{addr}, vm.Addresses...)
}

// detachFloating removes addr from the vm's addresses again.
func detachFloating(vm *fakeVm, addr string) {
	kept := make([]string, 0, len(vm.Addresses))
	for _, a := range vm.Addresses {
		if a != addr {
			kept = append(kept, a)
		}
	}
	vm.Addresses = kept
}

// deleteVm declares desired_state=deleted and queues the teardown; state is
// left alone (the executor commits deleting itself). One operation per
// resource: a delete while the create or an earlier delete is still running is
// 409 operation_in_flight, and a vm that is gone is 404 (vms.py
// delete_vm_request).
func (f *fakeAPI) deleteVm(rc *reqCtx, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	vm, ok := f.vms[id]
	// _refuse_while_protected comes first and only looks at a live vm; an
	// unknown or gone one falls through to the 404 below.
	if ok && vm.deletedAt == nil && vm.Protected {
		rc.conflict("vm_protected", fmt.Sprintf("this VM is protected against deletion; an owner of this "+
			"organization has to switch that off first, with PUT /v1/orgs/%s/compute/instances/%s/protection", f.org, id))
		return
	}
	if !ok || vm.deletedAt != nil {
		rc.notFound("vm")
		return
	}
	if vm.inFlight {
		rc.conflict("operation_in_flight", detailOperationInFlightVm)
		return
	}

	vm.DesiredState = "deleted"
	vm.deletingLeft = f.deleteSteps
	vm.inFlight = true
	vm.Generation++
	vm.UpdatedAt = time.Now().UTC()

	op := f.newOperation("vm.delete", "vm", id, time.Now().UTC())
	rc.json(http.StatusAccepted, map[string]any{"vm": vm.wireVmOut, "operation": op},
		"Location", f.operationLocation(op))
}

// vmPatchIn is the PATCH body: hostname is required and may be null (which
// clears it).
type vmPatchIn struct {
	Hostname *string
}

// decodeVmPatch reads {"hostname": "<fqdn>"} or {"hostname": null}; hostname is
// required and nothing else is accepted (the body decoder is strict).
func decodeVmPatch(r io.Reader) (vmPatchIn, []wireFieldError) {
	b := newBodyCheck(r)
	in := vmPatchIn{}
	if raw, ok := b.present("hostname", true); ok {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			in.Hostname = nil
		} else {
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				b.fail("hostname", "Input should be a valid string")
			} else {
				in.Hostname = &s
				b.checkHostname("hostname", s)
			}
		}
	}
	return in, b.finish()
}

// ifMatchRE is a well-formed If-Match: a double-quoted integer.
var ifMatchRE = regexp.MustCompile(`^"(\d+)"$`)

// patchDigest is the PATCH Idempotency-Key digest: the instance id and the
// hostname value, null differing from any string.
func patchDigest(id string, hostname *string) string {
	h, has := "", false
	if hostname != nil {
		h, has = *hostname, true
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%+v", struct {
		InstanceID  string
		Hostname    string
		HasHostname bool
	}{id, h, has})))
	return hex.EncodeToString(sum[:])
}

// patchVm is PATCH /compute/instances/{id}: it sets or clears the hostname,
// gated in the API's order -- body, liveness, Idempotency-Key replay, If-Match,
// the hostname's uniqueness -- and answers the bare VmOut at its new
// generation, with the ETag header.
func (f *fakeAPI) patchVm(rc *reqCtx, id string) {
	raw, _ := io.ReadAll(rc.r.Body)

	f.mu.Lock()
	f.patchCalls = append(f.patchCalls, patchCall{
		ID:             id,
		IfMatch:        rc.r.Header.Get("If-Match"),
		IdempotencyKey: rc.r.Header.Get("Idempotency-Key"),
		Body:           string(raw),
	})
	f.mu.Unlock()

	in, errs := decodeVmPatch(bytes.NewReader(raw))
	if len(errs) > 0 {
		rc.invalid(errs...)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	vm, ok := f.vms[id]
	if !ok || vm.deletedAt != nil {
		rc.notFound("vm")
		return
	}

	key := rc.r.Header.Get("Idempotency-Key")
	var digest string
	if key != "" {
		f.idemKeysSeen = append(f.idemKeysSeen, key)
		digest = patchDigest(id, in.Hostname)
		if rec, ok := f.idem[key]; ok {
			if f.inProgressLeft > 0 {
				f.inProgressLeft--
				rc.inProgress()
				return
			}
			if rec.payloadHash != digest {
				rc.conflict("idempotency_key_reused", "this Idempotency-Key was already used with a different request body")
				return
			}
			// The replay comes before If-Match: the retry of a request that
			// already succeeded gets its first answer back, stale condition or
			// not.
			rc.raw(rec.status, rec.body, "ETag", rec.etag)
			return
		}
	}

	ifMatch := rc.r.Header.Get("If-Match")
	if ifMatch == "" {
		rc.problem(http.StatusPreconditionRequired, "precondition_required", "Precondition required",
			"this request needs an If-Match header carrying the instance's generation")
		return
	}
	m := ifMatchRE.FindStringSubmatch(ifMatch)
	if m == nil {
		rc.invalid(wireFieldError{Field: "If-Match", Message: `must be a quoted generation, for example "3"`})
		return
	}
	sent, _ := strconv.ParseInt(m[1], 10, 64)
	if sent != vm.Generation {
		rc.w.Header().Set("ETag", generationETag(vm.Generation))
		rc.problem(http.StatusPreconditionFailed, "generation_mismatch", "Precondition failed",
			fmt.Sprintf("the instance is at generation %d, not %d; read it again and retry", vm.Generation, sent))
		return
	}

	if in.Hostname != nil && f.hostnameTaken(*in.Hostname, id) {
		rc.conflict("hostname_taken", hostnameTakenDetail(*in.Hostname))
		return
	}

	current := strOrEmpty(vm.Hostname)
	next := strOrEmpty(in.Hostname)
	if next != current {
		vm.Generation++
		if in.Hostname == nil {
			vm.Hostname = nil
		} else {
			h := next
			vm.Hostname = &h
		}
	}
	vm.UpdatedAt = time.Now().UTC()

	if key != "" {
		body, _ := json.Marshal(vm.wireVmOut)
		f.idem[key] = &idemRecord{
			payloadHash: digest,
			status:      http.StatusOK,
			body:        append(body, '\n'),
			etag:        generationETag(vm.Generation),
		}
	}
	rc.json(http.StatusOK, vm.wireVmOut, "ETag", generationETag(vm.Generation))
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
// quota_exceeded at the per-region ceiling (one for customer-default).
func (f *fakeAPI) createPublicIp(rc *reqCtx) {
	b := newBodyCheck(rc.r.Body)
	regionID := b.str("region_id", true, 0, 0)
	if errs := b.finish(); len(errs) > 0 {
		rc.invalid(errs...)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	// edge.py allocate_public_ip_request: this org's rows in this region that
	// are not in error, against QuotaProfile.public_ips.
	if f.publicIPQuota >= 0 {
		used := 0
		for _, other := range f.publicIPs {
			if other.RegionID == regionID && other.State != "error" {
				used++
			}
		}
		if used >= f.publicIPQuota {
			rc.unprocessable("quota_exceeded", fmt.Sprintf(
				"public IPs: %d in use plus 1 requested exceeds the limit of %d for this account", used, f.publicIPQuota))
			return
		}
	}

	f.pubIPCreateCount++
	id := fakeUUID(2, f.pubIPCreateCount)
	now := time.Now().UTC()

	ip := &fakePublicIP{wirePublicIpOut: wirePublicIpOut{
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
			if ip.releasingLeft <= 0 {
				delete(f.publicIPs, id)
				continue
			}
			ip.releasingLeft--
			items = append(items, ip.wirePublicIpOut)
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
		if ip.VmID != nil && ip.Address != nil {
			if vm, ok := f.vms[*ip.VmID]; ok {
				attachFloating(vm, *ip.Address)
			}
		}
	case "disassociating":
		if ip.VmID != nil {
			if vm, ok := f.vms[*ip.VmID]; ok && strings.HasPrefix(vm.Name, "ipdisfail-") {
				errMsg := "the edge could not release this binding"
				ip.State = "error"
				ip.Error = &errMsg
				return
			}
		}
		if ip.VmID != nil && ip.Address != nil {
			if vm, ok := f.vms[*ip.VmID]; ok {
				detachFloating(vm, *ip.Address)
			}
		}
		ip.State = "allocated"
		ip.VmID = nil
	}
}

// associatePublicIp is edge.py associate_public_ip_request, gate by gate: the
// address must be allocated, the vm must exist, be in the same region and be
// running, and carry no other address.
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
	if ip.State != "allocated" {
		rc.conflict("public_ip_not_allocated", fmt.Sprintf("this public IP is %s, so it cannot be associated", ip.State))
		return
	}
	vm, ok := f.vms[vmID]
	if !ok || vm.deletedAt != nil {
		rc.notFound("vm")
		return
	}
	if vm.RegionID != ip.RegionID {
		rc.unprocessable("region_mismatch", "a public IP can only be associated with a VM in the same region")
		return
	}
	if vm.State != "running" {
		rc.conflict("vm_not_in_a_state_for_this", "this VM is not running, so a public IP cannot be attached to it")
		return
	}
	for _, other := range f.publicIPs {
		if other.ID != ip.ID && other.VmID != nil && *other.VmID == vm.ID &&
			(other.State == "associating" || other.State == "associated") {
			rc.conflict("public_ip_already_associated", "this VM already has a public IP associated with it")
			return
		}
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
	if ip.State != "associated" {
		rc.conflict("public_ip_not_associated", fmt.Sprintf("this public IP is %s, so it cannot be disassociated", ip.State))
		return
	}

	ip.State = "disassociating"
	f.callSeq[id] = append(f.callSeq[id], "disassociate")

	f.publicIPAccepted(rc, ip, f.newPublicIPOperation("ip.disassociate", id))
}

// releasePublicIp accepts a release from allocated, associated or error; the
// executor disassociates first when the row still names a vm.
func (f *fakeAPI) releasePublicIp(rc *reqCtx, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	ip, ok := f.publicIPs[id]
	if !ok {
		rc.notFound("public_ip")
		return
	}
	switch ip.State {
	case "allocated", "associated", "error":
	default:
		rc.conflict("public_ip_not_releasable", fmt.Sprintf("this public IP is %s, so it cannot be released", ip.State))
		return
	}

	// The executor disassociates first when the row still names a vm.
	if ip.VmID != nil && ip.Address != nil {
		if vm, ok := f.vms[*ip.VmID]; ok {
			detachFloating(vm, *ip.Address)
		}
	}
	ip.State = "releasing"
	ip.releasingLeft = f.releaseSteps
	f.callSeq[id] = append(f.callSeq[id], "release")

	f.publicIPAccepted(rc, ip, f.newPublicIPOperation("ip.release", id))
}

// --- SSH keys (docs/plans/terraform-tf7-ssh-keys.md §3.3 and §4) ------------

const (
	maxSshKeysPerVm = 10

	detailSshKeysOff            = "SSH keys are not switched on yet"
	detailSshKeyName            = "an SSH key name is 1 to 64 characters from letters, digits, '.', '_', '@' and '-', starting with a letter or digit"
	detailIdempotencyInProgress = "a request with this Idempotency-Key is still being processed; wait for it to finish and retry"
)

// sshKeyNameRE is the §4 name rule, case-sensitive.
var sshKeyNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,63}$`)

// importSshKey is POST /compute/ssh-keys, in the order of §4: the body is
// validated, then the flag (503 not_ready before the Idempotency-Key is
// claimed), the claim, the name, the key, the quota, and last the insert, whose
// unique (org, name) constraint is 409 name_taken. A new key answers 201 with
// SshKeyOut and a Location header.
func (f *fakeAPI) importSshKey(rc *reqCtx) {
	b := newBodyCheck(rc.r.Body)
	name := b.str("name", true, 1, 64)
	publicKey := b.str("public_key", true, 1, 8192)
	if errs := b.finish(); len(errs) > 0 {
		rc.invalid(errs...)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.sshKeysOff {
		rc.problem(http.StatusServiceUnavailable, "not_ready", "Service unavailable", detailSshKeysOff)
		return
	}

	idemKey := ""
	var digest string
	if key := rc.r.Header.Get("Idempotency-Key"); key != "" {
		// The mechanism is the VM create's, one claim per (route, key).
		idemKey = "ssh-keys|" + key
		f.sshIdemKeysSeen = append(f.sshIdemKeysSeen, key)
		sum := sha256.Sum256([]byte(name + "\x00" + publicKey))
		digest = hex.EncodeToString(sum[:])
		if rec, ok := f.idem[idemKey]; ok {
			if f.inProgressLeft > 0 {
				f.inProgressLeft--
				rc.inProgress()
				return
			}
			if rec.payloadHash != digest {
				rc.conflict("idempotency_key_reused", "this Idempotency-Key was already used with a different request body")
				return
			}
			rc.raw(rec.status, rec.body, "Location", rec.location)
			return
		}
	}

	if !sshKeyNameRE.MatchString(name) {
		rc.unprocessable("invalid_name", detailSshKeyName)
		return
	}
	keyType, canonical, fingerprint, refusal := fakeParsePublicKey(publicKey)
	if refusal != "" {
		rc.unprocessable("invalid_public_key", refusal)
		return
	}
	if f.sshKeyQuota >= 0 && len(f.sshKeys) >= f.sshKeyQuota {
		rc.unprocessable("quota_exceeded", fmt.Sprintf(
			"ssh_keys: %d in use plus 1 requested exceeds the limit of %d for this account", len(f.sshKeys), f.sshKeyQuota))
		return
	}
	for _, existing := range f.sshKeys {
		if existing.Name == name {
			rc.conflict("name_taken", fmt.Sprintf("an SSH key named '%s' already exists in this organization", name))
			return
		}
	}

	f.sshKeySeq++
	id := fakeUUID(4, f.sshKeySeq)
	key := &wireSshKeyOut{
		ID: id, Name: name, KeyType: keyType, Fingerprint: fingerprint,
		PublicKey: canonical, CreatedAt: time.Now().UTC(),
	}
	f.sshKeys[id] = key

	location := "/v1/orgs/" + f.org + "/compute/ssh-keys/" + id
	if idemKey != "" {
		body, _ := json.Marshal(key)
		f.idem[idemKey] = &idemRecord{
			payloadHash: digest,
			status:      http.StatusCreated,
			body:        append(body, '\n'),
			location:    location,
		}
	}
	rc.json(http.StatusCreated, key, "Location", location)
}

// listSshKeys is GET /compute/ssh-keys: ListResponse[SshKeyOut], oldest first.
func (f *fakeAPI) listSshKeys(rc *reqCtx) {
	f.mu.Lock()
	defer f.mu.Unlock()

	items := make([]wireSshKeyOut, 0, len(f.sshKeys))
	for _, k := range f.sshKeys {
		items = append(items, *k)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	rc.json(http.StatusOK, wireList[wireSshKeyOut]{Items: items})
}

// getSshKey is GET /compute/ssh-keys/{id}: another organization's key, and one
// that does not exist, read the same, as the ssh_key 404 of the shared helper.
func (f *fakeAPI) getSshKey(rc *reqCtx, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	key, ok := f.sshKeys[id]
	if !ok {
		rc.notFound("ssh_key")
		return
	}
	rc.json(http.StatusOK, key)
}

// deleteSshKey is DELETE /compute/ssh-keys/{id}: 204, and VMs that were
// created with the key keep their ssh_key_names (a launch-time snapshot).
func (f *fakeAPI) deleteSshKey(rc *reqCtx, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.sshKeys[id]; !ok {
		rc.notFound("ssh_key")
		return
	}
	delete(f.sshKeys, id)
	rc.noContent()
}

// resolveSshKeys is the VM create's resolution of ssh_keys against the
// keystore (§3.3 point 4): at most 10 names, none twice, every one imported.
// The caller holds f.mu. It answers the refusal itself and reports false.
func (f *fakeAPI) resolveSshKeys(rc *reqCtx, names []string) bool {
	if len(names) > maxSshKeysPerVm {
		rc.unprocessable("too_many_ssh_keys", fmt.Sprintf("a VM can be given at most %d SSH keys", maxSshKeysPerVm))
		return false
	}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			rc.unprocessable("duplicate_ssh_key", fmt.Sprintf("SSH key '%s' is listed more than once", n))
			return false
		}
		seen[n] = true
	}
	held := map[string]bool{}
	for _, k := range f.sshKeys {
		held[k.Name] = true
	}
	var missing []string
	for _, n := range names {
		if !held[n] {
			missing = append(missing, "'"+n+"'")
		}
	}
	if len(missing) > 0 {
		rc.unprocessable("ssh_key_not_found", fmt.Sprintf(
			"no SSH key named %s in this organization; import it with POST /v1/orgs/%s/compute/ssh-keys",
			strings.Join(missing, ", "), f.org))
		return false
	}
	return true
}

// fakeParsePublicKey is what the API does with an imported key: one OpenSSH
// line "<type> <base64> [comment]", surrounding whitespace and CRLF tolerated.
// The stored form is "<type> <base64>" with the blob re-encoded, so spare bits
// in the last base64 character are normalised away, and the fingerprint is
// "SHA256:" plus the unpadded base64 of the SHA-256 of the blob, as ssh-keygen
// -lf prints it. refusal is one of the plan's S10 sentences, "" for a key the
// API takes. It is written from the plan, not from the provider's own parser.
func fakeParsePublicKey(text string) (keyType, canonical, fingerprint, refusal string) {
	const (
		refusePrivate = "this is a private key; upload the public key (the .pub file) instead. Nothing was stored."
		refuseFormat  = "upload one public key in OpenSSH format: <type> <base64> [comment]"
		refuseMany    = "upload one key at a time"
		refuseOptions = "the key may not carry authorized_keys options such as command= or from="
		refuseType    = "this key type is not supported; use ssh-ed25519, ecdsa-sha2-nistp256, -nistp384, -nistp521 or ssh-rsa of at least 2048 bits"
		refuseData    = "the key data is not valid for its type"
	)
	supported := map[string]bool{
		"ssh-ed25519": true, "ecdsa-sha2-nistp256": true, "ecdsa-sha2-nistp384": true,
		"ecdsa-sha2-nistp521": true, "ssh-rsa": true,
	}

	s := strings.TrimSpace(text)
	switch {
	case strings.Contains(s, "PRIVATE KEY"):
		return "", "", "", refusePrivate
	case strings.ContainsAny(s, "\r\n"):
		return "", "", "", refuseMany
	}
	fields := strings.Fields(s)
	if len(fields) < 2 {
		return "", "", "", refuseFormat
	}
	typ := fields[0]
	if !supported[typ] {
		for _, later := range fields[1:] {
			if supported[later] {
				return "", "", "", refuseOptions
			}
		}
		return "", "", "", refuseType
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return "", "", "", refuseData
	}

	// The blob is SSH wire format: a string naming the type, then what the type
	// needs.
	embedded, rest, ok := sshWireString(blob)
	if !ok || string(embedded) != typ {
		return "", "", "", refuseData
	}
	switch typ {
	case "ssh-ed25519":
		if key, rest, ok := sshWireString(rest); !ok || len(key) != 32 || len(rest) != 0 {
			return "", "", "", refuseData
		}
	case "ssh-rsa":
		e, rest, ok1 := sshWireString(rest)
		n, rest, ok2 := sshWireString(rest)
		if !ok1 || !ok2 || len(rest) != 0 || len(e) == 0 || len(n) == 0 {
			return "", "", "", refuseData
		}
		if size := sshMpintBits(n); size < 2048 || size > 16384 {
			return "", "", "", fmt.Sprintf("RSA keys must be 2048 to 16384 bits; this one has %d", size)
		}
	default: // ecdsa-sha2-nistpNNN: the curve name, then the public point
		curve, rest, ok1 := sshWireString(rest)
		point, rest, ok2 := sshWireString(rest)
		if !ok1 || !ok2 || len(rest) != 0 || "ecdsa-sha2-"+string(curve) != typ || len(point) < 2 || point[0] != 4 {
			return "", "", "", refuseData
		}
	}

	sum := sha256.Sum256(blob)
	return typ, typ + " " + base64.StdEncoding.EncodeToString(blob),
		"SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]), ""
}

// sshWireString reads one SSH wire string (a uint32 length and that many
// bytes) off the front of b.
func sshWireString(b []byte) (s, rest []byte, ok bool) {
	if len(b) < 4 {
		return nil, nil, false
	}
	n := binary.BigEndian.Uint32(b)
	if uint64(n) > uint64(len(b)-4) {
		return nil, nil, false
	}
	return b[4 : 4+n], b[4+n:], true
}

// sshMpintBits is the bit length of an SSH mpint (a big-endian number, with a
// leading zero byte when the top bit would otherwise read as a sign).
func sshMpintBits(mpint []byte) int {
	for len(mpint) > 0 && mpint[0] == 0 {
		mpint = mpint[1:]
	}
	if len(mpint) == 0 {
		return 0
	}
	return 8*(len(mpint)-1) + bits.Len8(mpint[0])
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

	req, err := http.NewRequest(http.MethodGet, f.URL()+"/v1/orgs/"+testOrgID+"/compute/instances/"+testSomeUUID, nil)
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
	req, err := http.NewRequest(http.MethodPost, f.URL()+"/v1/orgs/"+f.org+"/compute/instances", strings.NewReader(body))
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
		"id", "org_id", "region_id", "name", "fqdn", "hostname", "instance_type", "image", "vcpus",
		"memory_mb", "disk_gb", "storage_pool_id", "workload_type", "network_profile_id", "state",
		"status", "desired_state", "private_ip", "addresses", "generation", "error_class",
		"error_detail", "observed_status", "observed_addresses", "observed_at", "ssh_key_names",
		"user_data_present", "protected", "allowed_actions", "available_actions", "created_at", "updated_at",
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

func vmsPath(org string) string { return "/v1/orgs/" + org + "/compute/instances" }

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

	// POST /compute/instances -> 202 {vm, operation} with a Location header.
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
	wantKeys(t, "ImageRef", vm["image"].(map[string]any), []string{"image_id", "name"})
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

	// GET /compute/instances/{id} -> VmOut.
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

// createVmVia posts a create and returns the decoded {vm, operation} body.
func createVmVia(t *testing.T, f *fakeAPI, key, body string, extra ...string) jsonObject {
	t.Helper()
	status, _, resp := fakeHTTP(t, f, key, http.MethodPost, vmsPath(f.org), body, extra...)
	if status != http.StatusAccepted {
		t.Fatalf("create: status = %d, want 202 (%s)", status, resp)
	}
	return jsonObject(decodeObject(t, resp))
}

func getVmVia(t *testing.T, f *fakeAPI, key, id string) (int, map[string]any) {
	t.Helper()
	status, _, resp := fakeHTTP(t, f, key, http.MethodGet, vmsPath(f.org)+"/"+id, "")
	if status == http.StatusOK {
		return status, decodeObject(t, resp)
	}
	return status, decodeObject(t, resp)
}

func deleteVmVia(t *testing.T, f *fakeAPI, key, id string) (int, map[string]any) {
	t.Helper()
	status, _, resp := fakeHTTP(t, f, key, http.MethodDelete, vmsPath(f.org)+"/"+id, "")
	return status, decodeObject(t, resp)
}

// runVm polls a freshly created vm until it is running, as a waiter would.
func runVm(t *testing.T, f *fakeAPI, key, id string) {
	t.Helper()
	for i := 0; i < 5; i++ {
		_, vm := getVmVia(t, f, key, id)
		if vm["state"] == "running" {
			return
		}
	}
	t.Fatalf("vm %s never reached running", id)
}

// TestFakeAPI_DeleteFlowEndsIn404 pins what a delete looks like from outside
// (compute.py delete_vm, executors/compute.py): the 202 body still shows the
// old state with desired_state=deleted, then the executor commits deleting,
// then state=deleted together with deleted_at -- and a soft-deleted row reads
// as absent, so the GET that would have shown "deleted" is a 404.
func TestFakeAPI_DeleteFlowEndsIn404(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)
	id := createVmVia(t, f, "good-key", testValidVmJS)["vm"].(map[string]any)["id"].(string)
	runVm(t, f, "good-key", id)

	status, body := deleteVmVia(t, f, "good-key", id)
	if status != http.StatusAccepted {
		t.Fatalf("delete: status = %d, want 202 (%v)", status, body)
	}
	vm, op := body["vm"].(map[string]any), body["operation"].(map[string]any)
	if vm["state"] != "running" || vm["desired_state"] != "deleted" {
		t.Errorf("202 body vm state/desired = %v/%v, want running/deleted (state is not moved by the delete)", vm["state"], vm["desired_state"])
	}
	if op["type"] != "vm.delete" {
		t.Errorf("operation type = %v, want vm.delete", op["type"])
	}

	status, got := getVmVia(t, f, "good-key", id)
	if status != http.StatusOK || got["state"] != "deleting" {
		t.Fatalf("first GET after delete: status %d state %v, want 200 deleting", status, got["state"])
	}
	status, got = getVmVia(t, f, "good-key", id)
	if status != http.StatusNotFound || got["code"] != "not_found" {
		t.Fatalf("GET of a deleted vm: status %d code %v, want 404 not_found", status, got["code"])
	}
	if state, ok := f.VmState(id); !ok || state != "deleted" {
		t.Errorf("the fake keeps the row for inspection: VmState = %q ok=%v, want deleted", state, ok)
	}
	if status, _ := deleteVmVia(t, f, "good-key", id); status != http.StatusNotFound {
		t.Errorf("delete of a gone vm: status = %d, want 404", status)
	}
}

// TestFakeAPI_SecondDeleteIs409OperationInFlight pins uq_operations_one_in_flight:
// one operation per resource, so a second request while one runs is a 409 --
// during the create as much as during a delete.
func TestFakeAPI_SecondDeleteIs409OperationInFlight(t *testing.T) {
	const detail = "another operation is already running for this VM; wait for it to finish and retry"
	f := newFakeAPI(t, "good-key", testOrgID)
	id := createVmVia(t, f, "good-key", testValidVmJS)["vm"].(map[string]any)["id"].(string)

	status, p := deleteVmVia(t, f, "good-key", id)
	if status != http.StatusConflict || p["code"] != "operation_in_flight" || p["detail"] != detail {
		t.Fatalf("delete during create: %d %v %v, want 409 operation_in_flight", status, p["code"], p["detail"])
	}

	runVm(t, f, "good-key", id)
	if status, _ := deleteVmVia(t, f, "good-key", id); status != http.StatusAccepted {
		t.Fatalf("delete of a running vm: status = %d, want 202", status)
	}
	status, p = deleteVmVia(t, f, "good-key", id)
	if status != http.StatusConflict || p["code"] != "operation_in_flight" || p["detail"] != detail {
		t.Fatalf("second delete: %d %v %v, want 409 operation_in_flight", status, p["code"], p["detail"])
	}
}

func TestFakeAPI_NamesAreCanonicalisedLikeTheAPI(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)

	body := func(name string) string {
		return `{"name":` + fmt.Sprintf("%q", name) + `,"image":"i","vcpus":1,"memory_mb":1024,"disk_gb":10,"storage_pool":"p","region":"gro","network_profile":"n"}`
	}

	// Lower-cased, trimmed, and a trailing .virtscale.nl stripped (naming.py).
	vm := createVmVia(t, f, "good-key", body("  Web-01.virtscale.nl ")).get("vm")
	if vm["name"] != "web-01" {
		t.Errorf("name = %q, want the canonical web-01", vm["name"])
	}

	// Anything that is not a DNS label afterwards is 422 invalid_name.
	for _, bad := range []string{"under_score", "-lead", "trail-", "has space", strings.Repeat("a", 64)} {
		status, _, resp := fakeHTTP(t, f, "good-key", http.MethodPost, vmsPath(testOrgID), body(bad))
		p := decodeObject(t, resp)
		if status != http.StatusUnprocessableEntity || p["code"] != "invalid_name" {
			t.Errorf("name %q: %d %v, want 422 invalid_name", bad, status, p["code"])
		}
		if p["detail"] != "name must be 1-63 characters of a-z, 0-9 and '-', not starting or ending with '-'" {
			t.Errorf("name %q: detail = %q", bad, p["detail"])
		}
	}
	if status, _, _ := fakeHTTP(t, f, "good-key", http.MethodPost, vmsPath(testOrgID), body(strings.Repeat("a", 63))); status != http.StatusAccepted {
		t.Errorf("a 63 character name: status = %d, want 202", status)
	}

	// Uniqueness is on the canonical name, among live vms of the region.
	status, _, resp := fakeHTTP(t, f, "good-key", http.MethodPost, vmsPath(testOrgID), body("WEB-01"))
	p := decodeObject(t, resp)
	if status != http.StatusConflict || p["code"] != "name_taken" || p["detail"] != "a VM named 'web-01' already exists in gro" {
		t.Errorf("duplicate canonical name: %d %v %v", status, p["code"], p["detail"])
	}
}

func TestFakeAPI_RefusesWhatTheAPIRefuses(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)

	withExtra := func(extra string) string {
		return `{"name":"web-01","image":"i","vcpus":1,"memory_mb":1024,"disk_gb":10,"storage_pool":"p","region":"gro","network_profile":"n",` + extra + `}`
	}
	cases := []struct {
		name, extra, code, detail string
	}{
		{"an ssh key that was never imported", `"ssh_keys":["a"]`, "ssh_key_not_found", "no SSH key named 'a' in this organization; import it with POST /v1/orgs/" + testOrgID + "/compute/ssh-keys"},
		{"user_data", `"user_data":"#cloud-config"`, "user_data_unsupported", "user_data is not accepted: the instance is built asynchronously, so cloud-init would have to be persisted to reach the worker that boots it -- and cloud-init is where bootstrap secrets go, so where such a payload may live is still an open design question"},
		{"empty user_data", `"user_data":""`, "user_data_unsupported", ""},
		{"infra workload", `"workload_type":"infra"`, "workload_not_allowed", "a customer account cannot request infra workloads"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, _, resp := fakeHTTP(t, f, "good-key", http.MethodPost, vmsPath(testOrgID), withExtra(tc.extra))
			p := decodeObject(t, resp)
			if status != http.StatusUnprocessableEntity || p["code"] != tc.code {
				t.Fatalf("%d %v, want 422 %s (%s)", status, p["code"], tc.code, resp)
			}
			if tc.detail != "" && p["detail"] != tc.detail {
				t.Errorf("detail = %q, want %q", p["detail"], tc.detail)
			}
		})
	}
	if f.CreateCount() != 0 {
		t.Errorf("refused requests created %d vms", f.CreateCount())
	}

	// The allowed shapes still work: an imported key's name, null user_data, customer workload.
	importKeyVia(t, f, "k1", testKeyEd25519)
	vm := createVmVia(t, f, "good-key", withExtra(`"ssh_keys":["k1"],"user_data":null,"workload_type":"customer"`)).get("vm")
	if names, _ := vm["ssh_key_names"].([]any); len(names) != 1 || names[0] != "k1" {
		t.Errorf("ssh_key_names = %v, want [k1]", vm["ssh_key_names"])
	}
	if vm["user_data_present"] != false {
		t.Errorf("user_data_present = %v, want false", vm["user_data_present"])
	}
}

// TestFakeAPI_IdempotencyKeyReplaysTheStoredResponse pins db/idempotency.py:
// same key and same body replays the stored 202 (no second vm, same ids);
// same key and another body is 409 idempotency_key_reused.
func TestFakeAPI_IdempotencyKeyReplaysTheStoredResponse(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)

	status, hdr1, first := fakeHTTP(t, f, "good-key", http.MethodPost, vmsPath(testOrgID), testValidVmJS, "Idempotency-Key", "key-1")
	if status != http.StatusAccepted {
		t.Fatalf("first: status = %d (%s)", status, first)
	}
	status, hdr2, second := fakeHTTP(t, f, "good-key", http.MethodPost, vmsPath(testOrgID), testValidVmJS, "Idempotency-Key", "key-1")
	if status != http.StatusAccepted {
		t.Fatalf("replay: status = %d, want the stored 202 (%s)", status, second)
	}
	if string(first) != string(second) {
		t.Errorf("replayed body differs:\nfirst  %s\nreplay %s", first, second)
	}
	if hdr1.Get("Location") != hdr2.Get("Location") || hdr1.Get("Location") == "" {
		t.Errorf("Location %q vs %q, want the same non-empty value", hdr1.Get("Location"), hdr2.Get("Location"))
	}
	if f.CreateCount() != 1 {
		t.Errorf("CreateCount = %d, want 1: a replay must not build a second vm", f.CreateCount())
	}

	other := strings.Replace(testValidVmJS, `"vcpus":1`, `"vcpus":2`, 1)
	status, _, resp := fakeHTTP(t, f, "good-key", http.MethodPost, vmsPath(testOrgID), other, "Idempotency-Key", "key-1")
	p := decodeObject(t, resp)
	if status != http.StatusConflict || p["code"] != "idempotency_key_reused" ||
		p["detail"] != "this Idempotency-Key was already used with a different request body" {
		t.Errorf("reused key: %d %v %v, want 409 idempotency_key_reused", status, p["code"], p["detail"])
	}

	// Without a key the natural uniqueness answers instead.
	status, _, resp = fakeHTTP(t, f, "good-key", http.MethodPost, vmsPath(testOrgID), testValidVmJS)
	if status != http.StatusConflict || decodeObject(t, resp)["code"] != "name_taken" {
		t.Errorf("keyless duplicate: %d, want 409 name_taken (%s)", status, resp)
	}
}

// get returns the nested object under key; it exists so tests can chain off
// createVmVia without a type assertion at every call site.
type jsonObject map[string]any

func (o jsonObject) get(key string) map[string]any { return o[key].(map[string]any) }

// --- public IP gates (domain/compute/edge.py) -------------------------------

func ipsPath(org string) string { return "/v1/orgs/" + org + "/compute/public-ips" }

// allocateIPVia allocates a public IP in region and returns its id.
func allocateIPVia(t *testing.T, f *fakeAPI, region string) string {
	t.Helper()
	status, _, resp := fakeHTTP(t, f, f.apiKey, http.MethodPost, ipsPath(f.org), `{"region_id":"`+region+`"}`)
	if status != http.StatusAccepted {
		t.Fatalf("allocate in %s: status = %d (%s)", region, status, resp)
	}
	return decodeObject(t, resp)["public_ip"].(map[string]any)["id"].(string)
}

// ipStateVia reads a public IP's state through the list, which also advances it.
func ipStateVia(t *testing.T, f *fakeAPI, id string) string {
	t.Helper()
	_, _, resp := fakeHTTP(t, f, f.apiKey, http.MethodGet, ipsPath(f.org), "")
	for _, it := range decodeObject(t, resp)["items"].([]any) {
		if m := it.(map[string]any); m["id"] == id {
			return m["state"].(string)
		}
	}
	return "absent"
}

func settleIP(t *testing.T, f *fakeAPI, id, want string) {
	t.Helper()
	for i := 0; i < 5; i++ {
		if ipStateVia(t, f, id) == want {
			return
		}
	}
	t.Fatalf("public ip %s never reached %s (now %s)", id, want, ipStateVia(t, f, id))
}

func ipAction(t *testing.T, f *fakeAPI, id, action, body string) (int, map[string]any) {
	t.Helper()
	method, path := http.MethodPost, ipsPath(f.org)+"/"+id+"/"+action
	if action == "" {
		method, path = http.MethodDelete, ipsPath(f.org)+"/"+id
	}
	status, _, resp := fakeHTTP(t, f, f.apiKey, method, path, body)
	return status, decodeObject(t, resp)
}

func wantProblem(t *testing.T, what string, status int, p map[string]any, wantStatus int, code, detail string) {
	t.Helper()
	if status != wantStatus || p["code"] != code {
		t.Errorf("%s: %d %v, want %d %s (%v)", what, status, p["code"], wantStatus, code, p["detail"])
		return
	}
	if detail != "" && p["detail"] != detail {
		t.Errorf("%s: detail = %q, want %q", what, p["detail"], detail)
	}
}

func TestFakeAPI_PublicIPGatesMatchTheDeployedAPI(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)
	vmID := createVmVia(t, f, "good-key", testValidVmJS).get("vm")["id"].(string)
	ipID := allocateIPVia(t, f, "gro")
	assocBody := `{"vm_id":"` + vmID + `"}`

	// pending: not associable, not releasable, not disassociable.
	st, p := ipAction(t, f, ipID, "associate", assocBody)
	wantProblem(t, "associate while pending", st, p, 409, "public_ip_not_allocated", "this public IP is pending, so it cannot be associated")
	st, p = ipAction(t, f, ipID, "", "")
	wantProblem(t, "release while pending", st, p, 409, "public_ip_not_releasable", "this public IP is pending, so it cannot be released")
	st, p = ipAction(t, f, ipID, "disassociate", "")
	wantProblem(t, "disassociate while pending", st, p, 409, "public_ip_not_associated", "this public IP is pending, so it cannot be disassociated")

	settleIP(t, f, ipID, "allocated")

	// allocated: disassociate has nothing to undo; an unknown or foreign vm is 404.
	st, p = ipAction(t, f, ipID, "disassociate", "")
	wantProblem(t, "disassociate while allocated", st, p, 409, "public_ip_not_associated", "this public IP is allocated, so it cannot be disassociated")
	st, p = ipAction(t, f, ipID, "associate", `{"vm_id":"`+testSomeUUID+`"}`)
	wantProblem(t, "associate an unknown vm", st, p, 404, "not_found", "no vm matching that identifier is visible to this organization")
	st, p = ipAction(t, f, testSomeUUID, "associate", assocBody)
	wantProblem(t, "associate an unknown ip", st, p, 404, "not_found", "no public_ip matching that identifier is visible to this organization")

	// the vm is still pending: not running, so nothing can be attached to it.
	st, p = ipAction(t, f, ipID, "associate", assocBody)
	wantProblem(t, "associate to a vm that is not running", st, p, 409, "vm_not_in_a_state_for_this", "this VM is not running, so a public IP cannot be attached to it")

	runVm(t, f, "good-key", vmID)
	st, _ = ipAction(t, f, ipID, "associate", assocBody)
	if st != http.StatusAccepted {
		t.Fatalf("associate to a running vm: status = %d, want 202", st)
	}
	st, p = ipAction(t, f, ipID, "associate", assocBody)
	wantProblem(t, "associate twice", st, p, 409, "public_ip_not_allocated", "this public IP is associating, so it cannot be associated")
	settleIP(t, f, ipID, "associated")

	// an associated address can be released directly: the executor disassociates first.
	st, _ = ipAction(t, f, ipID, "", "")
	if st != http.StatusAccepted {
		t.Errorf("release of an associated ip: status = %d, want 202", st)
	}
	st, p = ipAction(t, f, ipID, "", "")
	wantProblem(t, "release while releasing", st, p, 409, "public_ip_not_releasable", "this public IP is releasing, so it cannot be released")
	if got := ipStateVia(t, f, ipID); got != "absent" {
		t.Errorf("after a release the row is gone from the list, got state %q", got)
	}
}

func TestFakeAPI_PublicIPRegionAndOneAddressPerVm(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)
	f.SetPublicIPQuota(2)
	vmID := createVmVia(t, f, "good-key", testValidVmJS).get("vm")["id"].(string)
	runVm(t, f, "good-key", vmID)

	other := allocateIPVia(t, f, "ams")
	settleIP(t, f, other, "allocated")
	st, p := ipAction(t, f, other, "associate", `{"vm_id":"`+vmID+`"}`)
	wantProblem(t, "associate across regions", st, p, 422, "region_mismatch", "a public IP can only be associated with a VM in the same region")

	first, second := allocateIPVia(t, f, "gro"), allocateIPVia(t, f, "gro")
	settleIP(t, f, first, "allocated")
	settleIP(t, f, second, "allocated")
	if st, _ := ipAction(t, f, first, "associate", `{"vm_id":"`+vmID+`"}`); st != http.StatusAccepted {
		t.Fatalf("first association: status = %d, want 202", st)
	}
	st, p = ipAction(t, f, second, "associate", `{"vm_id":"`+vmID+`"}`)
	wantProblem(t, "second address on one vm", st, p, 409, "public_ip_already_associated", "this VM already has a public IP associated with it")
}

// TestFakeAPI_PublicIPQuotaIsPerRegionAndIgnoresErrorRows pins edge.py
// allocate_public_ip_request: the count is of this org's rows in this region
// that are not in error, against QuotaProfile.public_ips (1 for customer-default).
func TestFakeAPI_PublicIPQuotaIsPerRegionAndIgnoresErrorRows(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)

	first := allocateIPVia(t, f, "gro")
	status, _, resp := fakeHTTP(t, f, "good-key", http.MethodPost, ipsPath(testOrgID), `{"region_id":"gro"}`)
	wantProblem(t, "second ip in the region", status, decodeObject(t, resp), 422, "quota_exceeded",
		"public IPs: 1 in use plus 1 requested exceeds the limit of 1 for this account")

	other := allocateIPVia(t, f, "ams") // another region has its own count
	_ = other

	// A row in error holds no address and does not count: drive the first one
	// into error (a vm named ipfail-* refuses the bind) and allocate again.
	vm := `{"name":"ipfail-x","image":"i","vcpus":1,"memory_mb":1024,"disk_gb":10,"storage_pool":"p","region":"gro","network_profile":"n"}`
	vmID := createVmVia(t, f, "good-key", vm).get("vm")["id"].(string)
	runVm(t, f, "good-key", vmID)
	settleIP(t, f, first, "allocated")
	if st, _ := ipAction(t, f, first, "associate", `{"vm_id":"`+vmID+`"}`); st != http.StatusAccepted {
		t.Fatalf("associate: status = %d", st)
	}
	settleIP(t, f, first, "error")
	if id := allocateIPVia(t, f, "gro"); id == "" {
		t.Error("an error row must not count against the quota")
	}
}

// --- keys, scopes and refusals (deps.py, permissions.py) --------------------

func TestFakeAPI_KeyScopesGatePermissions(t *testing.T) {
	f := newFakeAPI(t, "full-key", testOrgID)
	f.AddKey("compute-read", "compute:read")
	f.AddKey("compute-write", "compute:write")
	f.AddKey("network-write", "network:write")
	f.AddKey("read-halves", "compute:read", "network:read")
	f.AddKey("legacy-read", "read")
	f.AddKey("legacy-write", "write")

	vmPath := vmsPath(testOrgID) + "/" + testSomeUUID
	ips := ipsPath(testOrgID)
	images := "/v1/orgs/" + testOrgID + "/compute/images?region=gro"
	denied := func(perm string) string { return "this credential does not carry the '" + perm + "' permission" }

	type call struct {
		method, path, body string
		perm               string // permission the route needs
	}
	create := call{http.MethodPost, vmsPath(testOrgID), testValidVmJS, "compute.instance.create"}
	getVm := call{http.MethodGet, vmPath, "", "compute.instance.read"}
	delVm := call{http.MethodDelete, vmPath, "", "compute.instance.delete"}
	listImages := call{http.MethodGet, images, "", "compute.instance.list"}
	listIPs := call{http.MethodGet, ips, "", "edge.public_ip.read"}
	allocate := call{http.MethodPost, ips, `{"region_id":"gro"}`, "edge.public_ip.allocate"}
	associate := call{http.MethodPost, ips + "/" + testSomeUUID + "/associate", `{"vm_id":"` + testSomeUUID + `"}`, "edge.public_ip.allocate"}
	disassociate := call{http.MethodPost, ips + "/" + testSomeUUID + "/disassociate", "", "edge.public_ip.release"}
	release := call{http.MethodDelete, ips + "/" + testSomeUUID, "", "edge.public_ip.release"}
	keys := sshKeysPath(testOrgID)
	importKey := call{http.MethodPost, keys, `{"name":"scoped","public_key":"` + testKeyEd25519 + `"}`, "compute.ssh_key.create"}
	listKeys := call{http.MethodGet, keys, "", "compute.ssh_key.list"}
	getKey := call{http.MethodGet, keys + "/" + testSomeUUID, "", "compute.ssh_key.read"}
	delKey := call{http.MethodDelete, keys + "/" + testSomeUUID, "", "compute.ssh_key.delete"}

	cases := []struct {
		key     string
		allowed []call
		refused []call
	}{
		// The four compute.ssh_key.* permissions ride with the compute scopes: the
		// reads with compute:read, all four with compute:write, none with network:*.
		{"compute-read", []call{getVm, listImages, listKeys, getKey}, []call{create, delVm, listIPs, allocate, associate, disassociate, release, importKey, delKey}},
		{"compute-write", []call{create, getVm, delVm, listImages, importKey, listKeys, getKey, delKey}, []call{listIPs, allocate, associate, disassociate, release}},
		{"network-write", []call{listIPs, allocate, associate, disassociate, release}, []call{create, getVm, delVm, listImages, importKey, listKeys, getKey, delKey}},
		{"read-halves", []call{getVm, listImages, listIPs, listKeys, getKey}, []call{create, delVm, allocate, associate, disassociate, release, importKey, delKey}},
		{"legacy-read", []call{getVm, listImages, listIPs, listKeys, getKey}, []call{create, delVm, allocate, release, importKey, delKey}},
		{"legacy-write", []call{create, getVm, delVm, listImages, listIPs, allocate, release, importKey, listKeys, getKey, delKey}, nil},
		{"full-key", []call{create, getVm, delVm, listImages, listIPs, allocate, release, importKey, listKeys, getKey, delKey}, nil},
	}
	for _, tc := range cases {
		for _, c := range tc.refused {
			status, _, body := fakeHTTP(t, f, tc.key, c.method, c.path, c.body)
			p := decodeObject(t, body)
			if status != http.StatusForbidden || p["code"] != "permission_denied" || p["detail"] != denied(c.perm) || p["title"] != "Forbidden" {
				t.Errorf("%s %s %s: %d %v %v, want 403 permission_denied naming %s", tc.key, c.method, c.path, status, p["code"], p["detail"], c.perm)
			}
		}
		for _, c := range tc.allowed {
			status, _, body := fakeHTTP(t, f, tc.key, c.method, c.path, c.body)
			if status == http.StatusForbidden || status == http.StatusUnauthorized {
				t.Errorf("%s %s %s: %d, want the request to pass the permission check (%s)", tc.key, c.method, c.path, status, body)
			}
		}
	}

	// The order of the gates: the credential, then the org boundary, then the
	// permission, then the body -- so a read-only key posting garbage is told
	// it may not post, and a foreign org is 404 whatever the key holds.
	status, _, _ := fakeHTTP(t, f, "compute-read", http.MethodPost, vmsPath(testOrgID), `{"bogus":1}`)
	if status != http.StatusForbidden {
		t.Errorf("permission before body validation: status = %d, want 403", status)
	}
	status, _, _ = fakeHTTP(t, f, "compute-read", http.MethodPost, vmsPath(testOtherOrg), testValidVmJS)
	if status != http.StatusNotFound {
		t.Errorf("foreign org with a limited key: status = %d, want 404", status)
	}
}

func TestFakeAPI_SourceAddressRestriction(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)
	f.DenySourceAddress()

	images := "/v1/orgs/" + testOrgID + "/compute/images?region=gro"
	status, _, body := fakeHTTP(t, f, "good-key", http.MethodGet, images, "")
	p := decodeObject(t, body)
	if status != http.StatusForbidden || p["code"] != "source_address_not_allowed" ||
		p["detail"] != "this API token cannot be used from this network address" || p["title"] != "Forbidden" {
		t.Errorf("restricted key: %d %v %v, want 403 source_address_not_allowed", status, p["code"], p["detail"])
	}
	// The credential is checked first: a wrong key learns nothing about the restriction.
	if status, _, _ := fakeHTTP(t, f, "wrong-key", http.MethodGet, images, ""); status != http.StatusUnauthorized {
		t.Errorf("wrong key: status = %d, want 401", status)
	}
}

func matchAny(*http.Request) bool { return true }

func matchMethod(method string) func(*http.Request) bool {
	return func(r *http.Request) bool { return r.Method == method }
}

// respondThrottle is the deployed 429 (middleware/rate_limit.py).
func respondThrottle(retryAfter string) func(*reqCtx) {
	return func(rc *reqCtx) {
		rc.w.Header().Set("Retry-After", retryAfter)
		rc.problem(http.StatusTooManyRequests, "rate_limited", "Too Many Requests",
			"too many requests for this organization; wait for the duration in the Retry-After header before retrying")
	}
}

// respondGateway is what a proxy in front of the API answers: not problem+json.
func respondGateway(status int) func(*reqCtx) {
	return func(rc *reqCtx) {
		rc.w.Header().Set("Content-Type", "text/html")
		rc.w.WriteHeader(status)
		_, _ = rc.w.Write([]byte("<html><body>" + http.StatusText(status) + "</body></html>"))
	}
}

// respondGatewayRetryAfter is respondGateway with a Retry-After, which some
// proxies add; tests use "0" so a retry does not wait.
func respondGatewayRetryAfter(status int, retryAfter string) func(*reqCtx) {
	return func(rc *reqCtx) {
		rc.w.Header().Set("Retry-After", retryAfter)
		respondGateway(status)(rc)
	}
}

// respondUnavailable is the deployed 503 for an unreachable dependency, which
// carries a Retry-After.
func respondUnavailable(retryAfter string) func(*reqCtx) {
	return func(rc *reqCtx) {
		rc.w.Header().Set("Retry-After", retryAfter)
		rc.problem(http.StatusServiceUnavailable, "dependency_unavailable", "Service unavailable",
			"a dependency this request needs is temporarily unreachable; this is not a problem with the request -- retry it shortly")
	}
}

// respondDrop closes the connection without a response: a transport error.
func respondDrop(rc *reqCtx) {
	if hj, ok := rc.w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close()
		}
	}
}

// matchRoute matches a request by method and by a piece of its path.
func matchRoute(method, pathPiece string) func(*http.Request) bool {
	return func(r *http.Request) bool {
		return r.Method == method && strings.Contains(r.URL.Path, pathPiece)
	}
}

// respondNoSuchRoute is what an unknown route (a mistyped endpoint) answers:
// code not_found and the detail "Not Found", with nothing about a resource.
func respondNoSuchRoute(rc *reqCtx) {
	rc.problem(http.StatusNotFound, "not_found", "Not Found", "Not Found")
}

// respondNotFoundOf is the shared helper's 404 for a resource the request did
// not ask about: organization, "compute service", region, or another kind.
func respondNotFoundOf(resource string) func(*reqCtx) {
	return func(rc *reqCtx) { rc.notFound(resource) }
}

// respondEchoingTheKey is an upstream that prints the request's Authorization
// header into a plain-text error page, as a debug page of a proxy might.
func respondEchoingTheKey(status int) func(*reqCtx) {
	return func(rc *reqCtx) {
		rc.w.Header().Set("Content-Type", "text/plain")
		rc.w.WriteHeader(status)
		_, _ = rc.w.Write([]byte("debug: Authorization: " + rc.r.Header.Get("Authorization")))
	}
}

// notFoundVariants are the 404s that do not say "this resource is gone": a
// mistyped endpoint, a proxy's page, and the shared helper's sentence for what
// the request did not ask about (the compute service, another kind of
// resource; the organization and a region have their own cases in the client's
// tests). Reading them as "gone" would empty the state.
func notFoundVariants() map[string]func(*reqCtx) {
	return map[string]func(*reqCtx){
		"an unknown route":         respondNoSuchRoute,
		"a proxy's html page":      respondGateway(http.StatusNotFound),
		"the compute service":      respondNotFoundOf("compute service"),
		"another kind of resource": respondNotFoundOf("operation"),
	}
}

func TestFakeAPI_FaultInjection(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)
	images := "/v1/orgs/" + testOrgID + "/compute/images?region=gro"

	f.FailNext(2, matchAny, respondThrottle("1"))
	for i := 0; i < 2; i++ {
		status, hdr, body := fakeHTTP(t, f, "good-key", http.MethodGet, images, "")
		p := decodeObject(t, body)
		if status != http.StatusTooManyRequests || hdr.Get("Retry-After") != "1" ||
			p["code"] != "rate_limited" || p["request_id"] == "" {
			t.Fatalf("throttled request %d: %d %q %v", i, status, hdr.Get("Retry-After"), p["code"])
		}
	}
	if status, _, _ := fakeHTTP(t, f, "good-key", http.MethodGet, images, ""); status != http.StatusOK {
		t.Errorf("after the injected failures: status = %d, want 200", status)
	}
	if f.FaultsServed() != 2 {
		t.Errorf("FaultsServed = %d, want 2", f.FaultsServed())
	}

	// "After handling": the request is processed, the answer is replaced. The
	// vm exists although the caller saw a 502, which is what makes a retry of
	// a create without an Idempotency-Key dangerous.
	f.FailNextAfterHandling(1, matchMethod(http.MethodPost), respondGateway(502))
	status, _, _ := fakeHTTP(t, f, "good-key", http.MethodPost, vmsPath(testOrgID), testValidVmJS, "Idempotency-Key", "k1")
	if status != http.StatusBadGateway {
		t.Fatalf("lost response: status = %d, want 502", status)
	}
	if f.CreateCount() != 1 {
		t.Errorf("CreateCount = %d, want 1: the create was processed before the response was lost", f.CreateCount())
	}
	status, _, _ = fakeHTTP(t, f, "good-key", http.MethodPost, vmsPath(testOrgID), testValidVmJS, "Idempotency-Key", "k1")
	if status != http.StatusAccepted || f.CreateCount() != 1 {
		t.Errorf("retry with the same key: status %d, creates %d, want the replayed 202 and still 1 create", status, f.CreateCount())
	}
	if keys := f.IdempotencyKeys(); len(keys) != 2 || keys[0] != "k1" || keys[1] != "k1" {
		t.Errorf("IdempotencyKeys = %v, want [k1 k1]", keys)
	}

	// A dropped connection is a transport error for the caller. A fresh
	// connection is used on purpose: net/http silently re-sends an idempotent
	// request when a *reused* keep-alive connection dies before any response.
	f.FailNext(1, matchAny, respondDrop)
	req, _ := http.NewRequest(http.MethodGet, f.URL()+images, nil)
	req.Header.Set("Authorization", "Bearer good-key")
	fresh := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	if resp, err := fresh.Do(req); err == nil {
		resp.Body.Close()
		t.Error("a dropped connection must surface as a transport error")
	}
}

// TestFakeAPI_ProtectedVmRefusesDelete pins compute.py _refuse_while_protected:
// while protected, a delete is 409 vm_protected whoever asks, and nothing is
// written (so no operation is left in flight).
func TestFakeAPI_ProtectedVmRefusesDelete(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)
	id := createVmVia(t, f, "good-key", testValidVmJS).get("vm")["id"].(string)
	runVm(t, f, "good-key", id)

	f.Protect(id, true)
	_, vm := getVmVia(t, f, "good-key", id)
	if vm["protected"] != true {
		t.Errorf("protected = %v, want true", vm["protected"])
	}
	status, p := deleteVmVia(t, f, "good-key", id)
	wantProblem(t, "delete of a protected vm", status, p, 409, "vm_protected",
		"this VM is protected against deletion; an owner of this organization has to switch that off first, with PUT /v1/orgs/"+testOrgID+"/compute/instances/"+id+"/protection")

	f.Protect(id, false)
	if status, _ := deleteVmVia(t, f, "good-key", id); status != http.StatusAccepted {
		t.Errorf("delete after unprotecting: status = %d, want 202 (a refused delete must leave nothing in flight)", status)
	}
}

// TestFakeAPI_PacingKnobsKeepDeletesVisible: SetDeleteSteps and SetReleaseSteps
// only slow the state machines down; they exist so acceptance tests can act
// while a delete or release is running.
func TestFakeAPI_PacingKnobsKeepDeletesVisible(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)
	f.SetDeleteSteps(3)
	f.SetReleaseSteps(3)

	id := createVmVia(t, f, "good-key", testValidVmJS).get("vm")["id"].(string)
	runVm(t, f, "good-key", id)
	if status, _ := deleteVmVia(t, f, "good-key", id); status != http.StatusAccepted {
		t.Fatalf("delete: status = %d", status)
	}
	for i := 0; i < 3; i++ {
		if status, vm := getVmVia(t, f, "good-key", id); status != http.StatusOK || vm["state"] != "deleting" {
			t.Fatalf("read %d of a slow delete: %d %v, want 200 deleting", i+1, status, vm["state"])
		}
	}
	if status, _ := getVmVia(t, f, "good-key", id); status != http.StatusNotFound {
		t.Errorf("after the slow delete: status = %d, want 404", status)
	}

	ip := allocateIPVia(t, f, "gro")
	settleIP(t, f, ip, "allocated")
	if st, _ := ipAction(t, f, ip, "", ""); st != http.StatusAccepted {
		t.Fatalf("release: status = %d", st)
	}
	for i := 0; i < 3; i++ {
		if got := ipStateVia(t, f, ip); got != "releasing" {
			t.Fatalf("list %d of a slow release: state %q, want releasing", i+1, got)
		}
	}
	if got := ipStateVia(t, f, ip); got != "absent" {
		t.Errorf("after the slow release: %q, want absent", got)
	}
}

// TestFakeAPI_AssociatedAddressShowsUpInAddresses: associating a public IP
// PREPENDS it to VmOut.addresses, so addresses[0] is the public one while
// private_ip keeps the private one; disassociating removes it again. The
// Nova-shaped observed_addresses decoy is left alone.
func TestFakeAPI_AssociatedAddressShowsUpInAddresses(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)
	vmID := createVmVia(t, f, "good-key", testValidVmJS).get("vm")["id"].(string)
	runVm(t, f, "good-key", vmID)

	addresses := func() []any {
		_, vm := getVmVia(t, f, "good-key", vmID)
		return vm["addresses"].([]any)
	}
	before := addresses()
	if len(before) != 1 {
		t.Fatalf("addresses before association = %v, want just the private one", before)
	}
	private := before[0].(string)
	if !strings.HasPrefix(private, "10.128.3.") {
		t.Fatalf("private address = %q, want a 10.128.3.x address", private)
	}

	ipID := allocateIPVia(t, f, "gro")
	settleIP(t, f, ipID, "allocated")
	if st, _ := ipAction(t, f, ipID, "associate", `{"vm_id":"`+vmID+`"}`); st != http.StatusAccepted {
		t.Fatalf("associate: status = %d", st)
	}
	settleIP(t, f, ipID, "associated")

	public := publicAddressOf(t, f, ipID)
	got := addresses()
	if len(got) != 2 || got[0] != public || got[1] != private {
		t.Fatalf("addresses after association = %v, want [%s %s]", got, public, private)
	}
	_, vm := getVmVia(t, f, "good-key", vmID)
	if vm["private_ip"] != private {
		t.Errorf("private_ip = %v, want %s unchanged by the association", vm["private_ip"], private)
	}
	if decoy := vm["observed_addresses"].(map[string]any)[fakeNetwork].([]any)[0].(map[string]any)["addr"]; decoy == public {
		t.Errorf("observed_addresses decoy = %v: the association must not touch it", decoy)
	}

	if st, _ := ipAction(t, f, ipID, "disassociate", ""); st != http.StatusAccepted {
		t.Fatalf("disassociate: status = %d", st)
	}
	settleIP(t, f, ipID, "allocated")
	if got := addresses(); len(got) != 1 || got[0] != private {
		t.Errorf("addresses after disassociation = %v, want only the private one", got)
	}
}

// publicAddressOf reads a public IP's address from the list, which also
// advances it.
func publicAddressOf(t *testing.T, f *fakeAPI, id string) string {
	t.Helper()
	_, _, resp := fakeHTTP(t, f, f.apiKey, http.MethodGet, ipsPath(f.org), "")
	for _, it := range decodeObject(t, resp)["items"].([]any) {
		if m := it.(map[string]any); m["id"] == id {
			addr, _ := m["address"].(string)
			return addr
		}
	}
	t.Fatalf("public ip %s is not in the list", id)
	return ""
}

// namedVmBody is the valid create body with its name replaced.
func namedVmBody(name string) string {
	return strings.Replace(testValidVmJS, `"name":"web-01"`, `"name":"`+name+`"`, 1)
}

// --- the renamed instance routes and the new VmOut fields -------------------

// TestFakeAPI_OldComputeVmsRoutesAreGone: the routes were renamed to
// /compute/instances, so the old /compute/vms paths are unknown routes now --
// the generic "Not Found" problem for every verb -- and every arrival shows up
// in RequestedPaths, unknown routes and fault-answered requests included.
func TestFakeAPI_OldComputeVmsRoutesAreGone(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)
	oldBase := "/v1/orgs/" + testOrgID + "/compute/vms"

	old := []struct {
		method, path, body string
	}{
		{http.MethodPost, oldBase, testValidVmJS},
		{http.MethodGet, oldBase + "/" + testSomeUUID, ""},
		{http.MethodDelete, oldBase + "/" + testSomeUUID, ""},
		{http.MethodPatch, oldBase + "/" + testSomeUUID, `{"hostname":null}`},
	}
	var want []string
	for _, c := range old {
		status, _, body := fakeHTTP(t, f, "good-key", c.method, c.path, c.body)
		p := decodeObject(t, body)
		if status != http.StatusNotFound || p["code"] != "not_found" || p["title"] != "Not Found" || p["detail"] != "Not Found" {
			t.Errorf("%s %s: %d %v %v %v, want 404 not_found Not Found", c.method, c.path, status, p["code"], p["title"], p["detail"])
		}
		want = append(want, c.path)
	}
	if f.CreateCount() != 0 {
		t.Errorf("a request on the old route created %d vm(s)", f.CreateCount())
	}

	// A mistyped path and a request an injected fault swallowed before it
	// reached a route are arrivals too.
	unknown := "/v1/orgs/" + testOrgID + "/compute/nope"
	f.FailNext(1, matchAny, respondNoSuchRoute)
	if status, _, _ := fakeHTTP(t, f, "good-key", http.MethodGet, unknown, ""); status != http.StatusNotFound {
		t.Errorf("fault-answered request: status = %d, want the injected 404", status)
	}
	want = append(want, unknown)

	got := f.RequestedPaths()
	if len(got) != len(want) {
		t.Fatalf("RequestedPaths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("RequestedPaths[%d] = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
}

// TestFakeAPI_NewVmFieldsMatchTheContract pins the fields the new contract
// gained: status (always the state), private_ip and addresses, generation,
// instance_type, hostname, and the Nova-shaped observed_* decoys.
func TestFakeAPI_NewVmFieldsMatchTheContract(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)

	status, hdr, body := fakeHTTP(t, f, "good-key", http.MethodPost, vmsPath(testOrgID), testValidVmJS)
	if status != http.StatusAccepted {
		t.Fatalf("create: status = %d, want 202 (%s)", status, body)
	}
	if got := hdr.Get("ETag"); got != `"1"` {
		t.Errorf("create ETag = %q, want \"1\"", got)
	}
	vm := decodeObject(t, body)["vm"].(map[string]any)
	id := vm["id"].(string)

	addresses := func(m map[string]any) []any {
		a, ok := m["addresses"].([]any)
		if !ok || a == nil {
			t.Fatalf("addresses = %v, want a list, never null", m["addresses"])
		}
		return a
	}

	if vm["status"] != "pending" || vm["status"] != vm["state"] {
		t.Errorf("created status/state = %v/%v, want pending for both", vm["status"], vm["state"])
	}
	if vm["hostname"] != nil || vm["instance_type"] != nil || vm["private_ip"] != nil {
		t.Errorf("created hostname/instance_type/private_ip = %v/%v/%v, want null",
			vm["hostname"], vm["instance_type"], vm["private_ip"])
	}
	if got := addresses(vm); len(got) != 0 {
		t.Errorf("created addresses = %v, want []", got)
	}
	if vm["generation"] != float64(1) {
		t.Errorf("created generation = %v, want 1", vm["generation"])
	}

	// A GET carries the generation in its ETag too.
	status, hdr, body = fakeHTTP(t, f, "good-key", http.MethodGet, vmsPath(testOrgID)+"/"+id, "")
	if status != http.StatusOK {
		t.Fatalf("get: status = %d (%s)", status, body)
	}
	if got := hdr.Get("ETag"); got != `"1"` {
		t.Errorf("GET ETag = %q, want \"1\"", got)
	}

	runVm(t, f, "good-key", id)
	_, _, body = fakeHTTP(t, f, "good-key", http.MethodGet, vmsPath(testOrgID)+"/"+id, "")
	vm = decodeObject(t, body)
	private, _ := vm["private_ip"].(string)
	if !strings.HasPrefix(private, "10.128.3.") {
		t.Errorf("running private_ip = %v, want a 10.128.3.x address", vm["private_ip"])
	}
	if got := addresses(vm); len(got) != 1 || got[0] != private {
		t.Errorf("running addresses = %v, want [%s]", got, private)
	}
	if vm["status"] != "running" || vm["status"] != vm["state"] {
		t.Errorf("running status/state = %v/%v, want running for both", vm["status"], vm["state"])
	}
	if vm["generation"] != float64(1) {
		t.Errorf("running generation = %v, want 1: a GET never bumps it", vm["generation"])
	}

	// The Nova-shaped fields are still published, but as decoys.
	if vm["observed_status"] != "ACTIVE" {
		t.Errorf("observed_status = %v, want the decoy ACTIVE", vm["observed_status"])
	}
	decoy, _ := vm["observed_addresses"].(map[string]any)[fakeNetwork].([]any)[0].(map[string]any)["addr"].(string)
	if !strings.HasPrefix(decoy, "198.51.100.") || decoy == private {
		t.Errorf("observed_addresses decoy = %q, want a 198.51.100.x address different from private_ip %q", decoy, private)
	}

	// A "noip-" name reaches running without an address at all.
	noip := createVmVia(t, f, "good-key", namedVmBody("noip-web")).get("vm")["id"].(string)
	runVm(t, f, "good-key", noip)
	_, got := getVmVia(t, f, "good-key", noip)
	if got["private_ip"] != nil {
		t.Errorf("noip- private_ip = %v, want null", got["private_ip"])
	}
	if a := addresses(got); len(a) != 0 {
		t.Errorf("noip- addresses = %v, want []", a)
	}

	// instance_type: "general.small" for a typed- name, else the body's value
	// or null.
	typed := createVmVia(t, f, "good-key", namedVmBody("typed-web")).get("vm")
	if typed["instance_type"] != "general.small" {
		t.Errorf("typed- instance_type = %v, want general.small", typed["instance_type"])
	}
	given := strings.Replace(testValidVmJS, `"name":"web-01"`, `"name":"sized-web","instance_type":"general.large"`, 1)
	if got := createVmVia(t, f, "good-key", given).get("vm")["instance_type"]; got != "general.large" {
		t.Errorf("instance_type from the body = %v, want general.large", got)
	}
}

// vmWithHostname is a valid create body (like vmWithKeys) that also carries a
// hostname.
func vmWithHostname(name, hostname string) string {
	quoted, _ := json.Marshal(hostname)
	return `{"name":"` + name + `","image":"image-1","vcpus":1,"memory_mb":1024,"disk_gb":10,` +
		`"storage_pool":"pool-1","region":"gro","network_profile":"net-1","hostname":` + string(quoted) + `}`
}

// TestFakeAPI_HostnameOnCreate pins the hostname rules of the create body: the
// pattern and the 253 character ceiling, uniqueness among the live instances of
// the one organization (soft-deleted and foreign rows do not hold it), and that
// a hostname still takes part in the Idempotency-Key digest.
func TestFakeAPI_HostnameOnCreate(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)

	const host = "web-01.example.com"
	vm := createVmVia(t, f, "good-key", vmWithHostname("web-01", host)).get("vm")
	id := vm["id"].(string)
	if vm["hostname"] != host {
		t.Errorf("hostname = %v, want %q", vm["hostname"], host)
	}
	if got := f.Hostname(id); got != host {
		t.Errorf("Hostname(%s) = %q, want %q", id, got, host)
	}

	// A bad hostname is a 400 invalid_request naming the field.
	bad := []struct{ what, hostname, message string }{
		{"upper case", "Web-01.example.com", ""},
		{"no dot", "web01", ""},
		{"a leading hyphen label", "-bad.example.com", ""},
		{"254 characters", strings.Repeat("a", 254), "String should have at most 253 characters"},
	}
	for _, tc := range bad {
		status, _, body := fakeHTTP(t, f, "good-key", http.MethodPost, vmsPath(testOrgID), vmWithHostname("badhost", tc.hostname))
		p := decodeObject(t, body)
		errs, _ := p["errors"].([]any)
		want := tc.message
		if want == "" {
			want = "String should match pattern '" + vmHostnamePattern + "'"
		}
		if status != http.StatusBadRequest || p["code"] != "invalid_request" || len(errs) != 1 {
			t.Fatalf("%s: %d %v %v, want 400 invalid_request with one error", tc.what, status, p["code"], p["errors"])
		}
		if e := errs[0].(map[string]any); e["field"] != "hostname" || e["message"] != want {
			t.Errorf("%s: error = %v, want {hostname: %s}", tc.what, e, want)
		}
	}
	if f.CreateCount() != 1 {
		t.Errorf("CreateCount = %d, want 1: a refused hostname must not build a vm", f.CreateCount())
	}

	// A second live instance of the organization may not take the hostname.
	first := createVmVia(t, f, "good-key", vmWithHostname("host-a", "a.example.com")).get("vm")
	status, _, body := fakeHTTP(t, f, "good-key", http.MethodPost, vmsPath(testOrgID), vmWithHostname("host-b", "a.example.com"))
	p := decodeObject(t, body)
	if status != http.StatusConflict || p["code"] != "hostname_taken" ||
		p["detail"] != "the hostname 'a.example.com' is already in use by another instance of this organization" {
		t.Errorf("second holder: %d %v %v, want 409 hostname_taken", status, p["code"], p["detail"])
	}

	// A row of another organization is no holder at all.
	f.SeedForeignInstance("foreign.example.com")
	if status, _, _ := fakeHTTP(t, f, "good-key", http.MethodPost, vmsPath(testOrgID), vmWithHostname("host-c", "foreign.example.com")); status != http.StatusAccepted {
		t.Errorf("a foreign holder: status = %d, want 202", status)
	}

	// Once the holder is soft-deleted and gone, the hostname is free again.
	firstID := first["id"].(string)
	runVm(t, f, "good-key", firstID)
	if status, _ := deleteVmVia(t, f, "good-key", firstID); status != http.StatusAccepted {
		t.Fatalf("delete of the holder: status = %d, want 202", status)
	}
	for i := 0; ; i++ {
		status, _ := getVmVia(t, f, "good-key", firstID)
		if status == http.StatusNotFound {
			break
		}
		if i == 4 {
			t.Fatalf("the deleted holder never went away (status %d)", status)
		}
	}
	if status, _, _ := fakeHTTP(t, f, "good-key", http.MethodPost, vmsPath(testOrgID), vmWithHostname("host-d", "a.example.com")); status != http.StatusAccepted {
		t.Errorf("after the holder is gone: status = %d, want 202: a soft-deleted instance must not hold its hostname", status)
	}

	// The Idempotency-Key digest covers the hostname: the same key and body
	// replay, another hostname under that key does not.
	keyed := vmWithHostname("host-e", "e.example.com")
	status1, _, firstAnswer := fakeHTTP(t, f, "good-key", http.MethodPost, vmsPath(testOrgID), keyed, "Idempotency-Key", "hk-1")
	status2, _, secondAnswer := fakeHTTP(t, f, "good-key", http.MethodPost, vmsPath(testOrgID), keyed, "Idempotency-Key", "hk-1")
	if status1 != http.StatusAccepted || status2 != http.StatusAccepted || string(firstAnswer) != string(secondAnswer) {
		t.Errorf("keyed replay: %d/%d, bodies equal %v; want the stored 202", status1, status2, string(firstAnswer) == string(secondAnswer))
	}
	status, _, body = fakeHTTP(t, f, "good-key", http.MethodPost, vmsPath(testOrgID), vmWithHostname("host-e", "e2.example.com"), "Idempotency-Key", "hk-1")
	if p := decodeObject(t, body); status != http.StatusConflict || p["code"] != "idempotency_key_reused" {
		t.Errorf("another hostname under the key: %d %v, want 409 idempotency_key_reused", status, p["code"])
	}
}

// patchHostnameVia reads the current generation with a GET, then PATCHes
// {"hostname": ...} with If-Match "<generation>". A nil hostname sends null.
func patchHostnameVia(t *testing.T, f *fakeAPI, key, id string, hostname *string) (int, map[string]any) {
	t.Helper()
	status, _, body := fakeHTTP(t, f, key, http.MethodGet, vmsPath(f.org)+"/"+id, "")
	if status != http.StatusOK {
		t.Fatalf("read before patch: status = %d (%s)", status, body)
	}
	generation, _ := decodeObject(t, body)["generation"].(float64)
	payload, _ := json.Marshal(map[string]any{"hostname": hostname})
	status, _, body = fakeHTTP(t, f, key, http.MethodPatch, vmsPath(f.org)+"/"+id, string(payload),
		"If-Match", fmt.Sprintf("\"%d\"", int64(generation)))
	return status, decodeObject(t, body)
}

// TestFakeAPI_PatchHostnameGates pins PATCH /compute/instances/{id}: the body,
// the liveness of the instance, the Idempotency-Key replay (before If-Match),
// then If-Match and the hostname's uniqueness.
func TestFakeAPI_PatchHostnameGates(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)
	id := createVmVia(t, f, "good-key", testValidVmJS).get("vm")["id"].(string)
	runVm(t, f, "good-key", id)
	path := vmsPath(testOrgID) + "/" + id

	// 428 without If-Match.
	status, _, body := fakeHTTP(t, f, "good-key", http.MethodPatch, path, `{"hostname":"x.example.com"}`)
	p := decodeObject(t, body)
	if status != http.StatusPreconditionRequired || p["code"] != "precondition_required" ||
		p["title"] != "Precondition required" ||
		p["detail"] != "this request needs an If-Match header carrying the instance's generation" {
		t.Errorf("no If-Match: %d %v %v %v", status, p["code"], p["title"], p["detail"])
	}

	// Anything but a double-quoted integer is a 400 naming the header.
	for _, bad := range []string{"1", `"x"`, `"1`, `1"`, `"1 "`} {
		status, _, body = fakeHTTP(t, f, "good-key", http.MethodPatch, path, `{"hostname":"x.example.com"}`, "If-Match", bad)
		p = decodeObject(t, body)
		errs, _ := p["errors"].([]any)
		if status != http.StatusBadRequest || p["code"] != "invalid_request" || len(errs) != 1 ||
			errs[0].(map[string]any)["field"] != "If-Match" ||
			errs[0].(map[string]any)["message"] != `must be a quoted generation, for example "3"` {
			t.Errorf("If-Match %q: %d %v %v, want 400 invalid_request naming If-Match", bad, status, p["code"], p["errors"])
		}
	}

	// A stale generation is a 412 carrying the current ETag.
	status, hdr, body := fakeHTTP(t, f, "good-key", http.MethodPatch, path, `{"hostname":"x.example.com"}`, "If-Match", `"0"`)
	p = decodeObject(t, body)
	if status != http.StatusPreconditionFailed || p["code"] != "generation_mismatch" || p["title"] != "Precondition failed" ||
		p["detail"] != "the instance is at generation 1, not 0; read it again and retry" {
		t.Errorf("stale If-Match: %d %v %v %v", status, p["code"], p["title"], p["detail"])
	}
	if got := hdr.Get("ETag"); got != `"1"` {
		t.Errorf("412 ETag = %q, want \"1\"", got)
	}

	// A real change: 200, the bare VmOut, generation 2 and its ETag.
	status, hdr, body = fakeHTTP(t, f, "good-key", http.MethodPatch, path, `{"hostname":"x.example.com"}`, "If-Match", `"1"`)
	if status != http.StatusOK {
		t.Fatalf("patch: status = %d (%s)", status, body)
	}
	got := decodeObject(t, body)
	if _, wrapped := got["vm"]; wrapped {
		t.Errorf("the PATCH answer is wrapped: %s", body)
	}
	wantKeys(t, "VmOut (PATCH)", got, specVmOutKeys)
	if got["hostname"] != "x.example.com" || got["generation"] != float64(2) {
		t.Errorf("hostname/generation = %v/%v, want x.example.com/2", got["hostname"], got["generation"])
	}
	if hdr.Get("ETag") != `"2"` {
		t.Errorf("patch ETag = %q, want \"2\"", hdr.Get("ETag"))
	}
	if f.Generation(id) != 2 || f.Hostname(id) != "x.example.com" {
		t.Errorf("stored generation/hostname = %d/%q, want 2/x.example.com", f.Generation(id), f.Hostname(id))
	}

	// The same hostname is a no-op: 200, generation unchanged.
	status, hdr, body = fakeHTTP(t, f, "good-key", http.MethodPatch, path, `{"hostname":"x.example.com"}`, "If-Match", `"2"`)
	if status != http.StatusOK || decodeObject(t, body)["generation"] != float64(2) ||
		f.Generation(id) != 2 || hdr.Get("ETag") != `"2"` {
		t.Errorf("no-op patch: %d generation %v stored %d ETag %q, want 200/2/2/\"2\"",
			status, decodeObject(t, body)["generation"], f.Generation(id), hdr.Get("ETag"))
	}

	// null clears the hostname, which does bump the generation.
	status, hdr, body = fakeHTTP(t, f, "good-key", http.MethodPatch, path, `{"hostname":null}`, "If-Match", `"2"`)
	got = decodeObject(t, body)
	if status != http.StatusOK || got["hostname"] != nil || got["generation"] != float64(3) ||
		hdr.Get("ETag") != `"3"` || f.Hostname(id) != "" {
		t.Errorf("clearing: %d %v %v %q %q, want 200/null/3/\"3\"/\"\"",
			status, got["hostname"], got["generation"], hdr.Get("ETag"), f.Hostname(id))
	}

	// Another live instance of the organization holds it: 409, nothing changes.
	createVmVia(t, f, "good-key", vmWithHostname("holder-vm", "h.example.com"))
	status, _, body = fakeHTTP(t, f, "good-key", http.MethodPatch, path, `{"hostname":"h.example.com"}`, "If-Match", `"3"`)
	p = decodeObject(t, body)
	if status != http.StatusConflict || p["code"] != "hostname_taken" ||
		p["detail"] != "the hostname 'h.example.com' is already in use by another instance of this organization" {
		t.Errorf("taken: %d %v %v, want 409 hostname_taken", status, p["code"], p["detail"])
	}
	if f.Generation(id) != 3 {
		t.Errorf("a refused patch bumped the generation to %d, want 3", f.Generation(id))
	}

	// Unknown and soft-deleted ids are the plain vm 404.
	status, _, body = fakeHTTP(t, f, "good-key", http.MethodPatch, vmsPath(testOrgID)+"/"+testSomeUUID, `{"hostname":"y.example.com"}`, "If-Match", `"1"`)
	p = decodeObject(t, body)
	if status != http.StatusNotFound || p["code"] != "not_found" ||
		p["detail"] != "no vm matching that identifier is visible to this organization" {
		t.Errorf("unknown id: %d %v %v, want 404 not_found", status, p["code"], p["detail"])
	}

	// The body is strict: no extra fields, hostname required, same pattern.
	for _, tc := range []struct{ what, body, field, message string }{
		{"an extra field", `{"hostname":"a.example.com","extra":1}`, "extra", "Extra inputs are not permitted"},
		{"a missing hostname", `{}`, "hostname", "Field required"},
		{"a bad hostname", `{"hostname":"NoDot"}`, "hostname", "String should match pattern '" + vmHostnamePattern + "'"},
	} {
		status, _, body := fakeHTTP(t, f, "good-key", http.MethodPatch, path, tc.body, "If-Match", `"3"`)
		p := decodeObject(t, body)
		errs, _ := p["errors"].([]any)
		found := false
		for _, e := range errs {
			m := e.(map[string]any)
			if m["field"] == tc.field && m["message"] == tc.message {
				found = true
			}
		}
		if status != http.StatusBadRequest || p["code"] != "invalid_request" || !found {
			t.Errorf("%s: %d %v %v, want 400 with {%s: %s}", tc.what, status, p["code"], p["errors"], tc.field, tc.message)
		}
	}

	// Idempotency: the first answer replays even though If-Match is stale now.
	status, hdr, body = fakeHTTP(t, f, "good-key", http.MethodPatch, path, `{"hostname":"i.example.com"}`,
		"If-Match", `"3"`, "Idempotency-Key", "pk-1")
	if status != http.StatusOK {
		t.Fatalf("keyed patch: status = %d (%s)", status, body)
	}
	if gen := decodeObject(t, body)["generation"]; gen != float64(4) {
		t.Fatalf("keyed patch generation = %v, want 4", gen)
	}
	firstETag := hdr.Get("ETag")
	status, hdr, body2 := fakeHTTP(t, f, "good-key", http.MethodPatch, path, `{"hostname":"i.example.com"}`,
		"If-Match", `"3"`, "Idempotency-Key", "pk-1")
	if status != http.StatusOK || hdr.Get("ETag") != firstETag || string(body) != string(body2) {
		t.Errorf("replay: %d ETag %q vs %q, bodies equal %v; want the stored answer",
			status, hdr.Get("ETag"), firstETag, string(body) == string(body2))
	}
	if f.Generation(id) != 4 {
		t.Errorf("a replay bumped the generation to %d, want 4", f.Generation(id))
	}

	// Same key, another hostname: the digest differs, so 409.
	status, _, body = fakeHTTP(t, f, "good-key", http.MethodPatch, path, `{"hostname":"other.example.com"}`,
		"If-Match", `"4"`, "Idempotency-Key", "pk-1")
	if p := decodeObject(t, body); status != http.StatusConflict || p["code"] != "idempotency_key_reused" {
		t.Errorf("reused key: %d %v, want 409 idempotency_key_reused", status, p["code"])
	}

	// Every PATCH that reached the route is recorded, headers and body.
	calls := f.PatchCalls()
	if len(calls) == 0 {
		t.Fatal("PatchCalls is empty")
	}
	if first := calls[0]; first != (patchCall{ID: id, Body: `{"hostname":"x.example.com"}`}) {
		t.Errorf("first PatchCall = %+v, want the condition-less, keyless first attempt", first)
	}
	if want := (patchCall{ID: id, IfMatch: `"3"`, IdempotencyKey: "pk-1", Body: `{"hostname":"i.example.com"}`}); !containsPatchCall(calls, want) {
		t.Errorf("PatchCalls %v does not contain %+v", calls, want)
	}
	if want := (patchCall{ID: testSomeUUID, IfMatch: `"1"`, Body: `{"hostname":"y.example.com"}`}); !containsPatchCall(calls, want) {
		t.Errorf("PatchCalls %v does not record the unknown-id call %+v", calls, want)
	}
}

func containsPatchCall(calls []patchCall, want patchCall) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}

// TestFakeAPI_PatchHostnameViaReadsThenPatches exercises the helper the
// acceptance tests drive: it reads the generation with a GET, then PATCHes with
// If-Match, and a nil hostname clears the field.
func TestFakeAPI_PatchHostnameViaReadsThenPatches(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)
	id := createVmVia(t, f, "good-key", testValidVmJS).get("vm")["id"].(string)
	runVm(t, f, "good-key", id)

	h := "via.example.com"
	status, vm := patchHostnameVia(t, f, "good-key", id, &h)
	if status != http.StatusOK || vm["hostname"] != h || vm["generation"] != float64(2) {
		t.Errorf("set: %d %v %v, want 200/%s/2", status, vm["hostname"], vm["generation"], h)
	}
	status, vm = patchHostnameVia(t, f, "good-key", id, nil)
	if status != http.StatusOK || vm["hostname"] != nil || vm["generation"] != float64(3) {
		t.Errorf("clear: %d %v %v, want 200/null/3", status, vm["hostname"], vm["generation"])
	}
	if got := f.Hostname(id); got != "" {
		t.Errorf("Hostname after clearing = %q, want \"\"", got)
	}

	// A soft-deleted instance answers the plain vm 404, like a GET does.
	gone := createVmVia(t, f, "good-key", namedVmBody("gone-vm")).get("vm")["id"].(string)
	runVm(t, f, "good-key", gone)
	if st, _ := deleteVmVia(t, f, "good-key", gone); st != http.StatusAccepted {
		t.Fatalf("delete of the second vm: status = %d, want 202", st)
	}
	for i := 0; ; i++ {
		if st, _ := getVmVia(t, f, "good-key", gone); st == http.StatusNotFound {
			break
		} else if i == 4 {
			t.Fatalf("the deleted vm never went away (status %d)", st)
		}
	}
	if st, _, body := fakeHTTP(t, f, "good-key", http.MethodPatch, vmsPath(testOrgID)+"/"+gone,
		`{"hostname":"z.example.com"}`, "If-Match", `"1"`); st != http.StatusNotFound || decodeObject(t, body)["code"] != "not_found" {
		t.Errorf("patch of a soft-deleted vm: %d, want 404 not_found", st)
	}
}

// --- the keystore (TF7 plan §4) ----------------------------------------------

// specSshKeyOutKeys is SshKeyOut, from the plan's §4: the contract the s3cp
// branch tf7/ssh-keys implements.
var specSshKeyOutKeys = []string{"id", "name", "key_type", "fingerprint", "public_key", "created_at"}

func sshKeysPath(org string) string { return "/v1/orgs/" + org + "/compute/ssh-keys" }

// importKeyVia imports a key as the fake's own key and returns the decoded
// SshKeyOut, failing the test unless the answer is 201.
func importKeyVia(t *testing.T, f *fakeAPI, name, publicKey string, extra ...string) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"name": name, "public_key": publicKey})
	status, _, resp := fakeHTTP(t, f, f.apiKey, http.MethodPost, sshKeysPath(f.org), string(body), extra...)
	if status != http.StatusCreated {
		t.Fatalf("import of %q: status = %d, want 201 (%s)", name, status, resp)
	}
	return decodeObject(t, resp)
}

// importKeyRaw imports a key and returns the status and decoded body, whatever
// the answer.
func importKeyRaw(t *testing.T, f *fakeAPI, name, publicKey string, extra ...string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"name": name, "public_key": publicKey})
	status, _, resp := fakeHTTP(t, f, f.apiKey, http.MethodPost, sshKeysPath(f.org), string(body), extra...)
	return status, decodeObject(t, resp)
}

// vmWithKeys is a valid create body that names the given SSH keys.
func vmWithKeys(name string, keys ...string) string {
	if keys == nil {
		keys = []string{}
	}
	quoted, _ := json.Marshal(keys)
	return `{"name":"` + name + `","image":"image-1","vcpus":1,"memory_mb":1024,"disk_gb":10,"storage_pool":"pool-1","region":"gro","network_profile":"net-1","ssh_keys":` + string(quoted) + `}`
}

func TestFakeAPI_SshKeyRoutesMatchThePlan(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)

	// POST -> 201 SshKeyOut with a Location header; the key comes back canonical.
	body, _ := json.Marshal(map[string]string{"name": "laptop", "public_key": testKeyEd25519 + " " + testKeyEd25519Comment + "\r\n"})
	status, hdr, resp := fakeHTTP(t, f, "good-key", http.MethodPost, sshKeysPath(testOrgID), string(body))
	if status != http.StatusCreated {
		t.Fatalf("import: status = %d, want 201 (%s)", status, resp)
	}
	key := decodeObject(t, resp)
	wantKeys(t, "SshKeyOut", key, specSshKeyOutKeys)
	id, _ := key["id"].(string)
	if !uuidRE.MatchString(id) {
		t.Errorf("id %q is not a UUID", key["id"])
	}
	if want := sshKeysPath(testOrgID) + "/" + id; hdr.Get("Location") != want {
		t.Errorf("Location = %q, want %q", hdr.Get("Location"), want)
	}
	if key["name"] != "laptop" || key["key_type"] != "ssh-ed25519" ||
		key["public_key"] != testKeyEd25519 || key["fingerprint"] != testKeyEd25519Fingerprint {
		t.Errorf("SshKeyOut = %v, want the canonical key (no comment) with its ssh-keygen fingerprint", key)
	}

	// GET one, GET the list.
	status, _, resp = fakeHTTP(t, f, "good-key", http.MethodGet, sshKeysPath(testOrgID)+"/"+id, "")
	if status != http.StatusOK {
		t.Fatalf("get: status = %d (%s)", status, resp)
	}
	wantKeys(t, "SshKeyOut (GET)", decodeObject(t, resp), specSshKeyOutKeys)
	status, _, resp = fakeHTTP(t, f, "good-key", http.MethodGet, sshKeysPath(testOrgID), "")
	list := decodeObject(t, resp)
	if status != http.StatusOK {
		t.Fatalf("list: status = %d (%s)", status, resp)
	}
	wantKeys(t, "ListResponse[SshKeyOut]", list, specListKeys)
	if items, _ := list["items"].([]any); len(items) != 1 {
		t.Errorf("list items = %v, want the one key", list["items"])
	}

	// DELETE -> 204 with no body; afterwards the key is the ssh_key 404, for GET and DELETE.
	status, hdr, resp = fakeHTTP(t, f, "good-key", http.MethodDelete, sshKeysPath(testOrgID)+"/"+id, "")
	if status != http.StatusNoContent || len(resp) != 0 || hdr.Get("X-Request-ID") == "" {
		t.Errorf("delete: status %d, body %q, request id %q, want an empty 204 with a request id", status, resp, hdr.Get("X-Request-ID"))
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		status, _, resp = fakeHTTP(t, f, "good-key", method, sshKeysPath(testOrgID)+"/"+id, "")
		p := decodeObject(t, resp)
		if status != http.StatusNotFound || p["code"] != "not_found" ||
			p["detail"] != "no ssh_key matching that identifier is visible to this organization" {
			t.Errorf("%s of a gone key: %d %v %v, want 404 not_found with the ssh_key sentence", method, status, p["code"], p["detail"])
		}
	}
}

// The fingerprints are the ones ssh-keygen -lf printed for keys it made, and
// the stored form drops the comment.
func TestFakeAPI_FingerprintsAreWhatSshKeygenPrints(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)

	for i, k := range []struct{ keyType, key, comment, fingerprint string }{
		{"ssh-ed25519", testKeyEd25519, testKeyEd25519Comment, testKeyEd25519Fingerprint},
		{"ssh-ed25519", testKeyOther, testKeyOtherComment, testKeyOtherFingerprint},
		{"ecdsa-sha2-nistp256", testKeyECDSA256, "tf7-ecdsa@example.invalid", testKeyECDSA256Fingerprint},
		{"ecdsa-sha2-nistp384", testKeyECDSA384, "tf7-ecdsa384@example.invalid", testKeyECDSA384Fingerprint},
		{"ssh-rsa", testKeyRSA2048, "tf7-rsa@example.invalid", testKeyRSA2048Fingerprint},
	} {
		got := importKeyVia(t, f, fmt.Sprintf("key-%d", i), k.key+" "+k.comment)
		if got["key_type"] != k.keyType || got["public_key"] != k.key || got["fingerprint"] != k.fingerprint {
			t.Errorf("%s: key_type %v, public_key %v, fingerprint %v; want %s, the key without its comment, %s",
				k.keyType, got["key_type"], got["public_key"], got["fingerprint"], k.keyType, k.fingerprint)
		}
	}
}

// Two base64 texts can decode to one key when the last character carries spare
// bits; the API stores the blob re-encoded, so there is one text per key.
func TestFakeAPI_SpareBitsAreNormalisedAway(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)

	variant := withSpareBitsFlipped(t, testKeyECDSA256)
	if variant == testKeyECDSA256 {
		t.Fatal("the variant must differ from the key in its text")
	}
	got := importKeyVia(t, f, "spare", variant)
	if got["public_key"] != testKeyECDSA256 || got["fingerprint"] != testKeyECDSA256Fingerprint {
		t.Errorf("public_key %v, fingerprint %v; want the canonical text and the key's own fingerprint", got["public_key"], got["fingerprint"])
	}
}

func TestFakeAPI_SshKeyRefusals(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)

	t.Run("names", func(t *testing.T) {
		for _, name := range []string{"-lead", ".dot", "has space", "semi;colon", "ünï"} {
			status, p := importKeyRaw(t, f, name, testKeyEd25519)
			wantProblem(t, fmt.Sprintf("name %q", name), status, p, 422, "invalid_name", detailSshKeyName)
		}
		for _, name := range []string{"a", "Laptop", "ci@build.01_x-y", strings.Repeat("a", 64)} {
			if status, p := importKeyRaw(t, f, name, testKeyEd25519); status != http.StatusCreated {
				t.Errorf("name %q: %d %v, want it accepted", name, status, p["code"])
			}
		}
	})

	t.Run("keys", func(t *testing.T) {
		f := newFakeAPI(t, "good-key", testOrgID)
		blob := strings.Fields(testKeyEd25519)[1]
		cases := []struct{ what, key, detail string }{
			{"a private key", "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\n-----END OPENSSH PRIVATE KEY-----\n",
				"this is a private key; upload the public key (the .pub file) instead. Nothing was stored."},
			{"one word", "hello", "upload one public key in OpenSSH format: <type> <base64> [comment]"},
			{"two keys", testKeyEd25519 + "\n" + testKeyOther, "upload one key at a time"},
			{"an options prefix", `command="ls" ` + testKeyEd25519, "the key may not carry authorized_keys options such as command= or from="},
			{"ssh-dss", "ssh-dss " + blob, "this key type is not supported; use ssh-ed25519, ecdsa-sha2-nistp256, -nistp384, -nistp521 or ssh-rsa of at least 2048 bits"},
			{"a security key", "sk-ssh-ed25519@openssh.com " + blob, "this key type is not supported; use ssh-ed25519, ecdsa-sha2-nistp256, -nistp384, -nistp521 or ssh-rsa of at least 2048 bits"},
			{"RSA below 2048 bits", testKeyRSA1024, "RSA keys must be 2048 to 16384 bits; this one has 1024"},
			{"base64 that is not base64", "ssh-ed25519 !!!!", "the key data is not valid for its type"},
			{"a truncated blob", "ssh-ed25519 " + blob[:30], "the key data is not valid for its type"},
			{"a blob of another type", "ssh-rsa " + blob, "the key data is not valid for its type"},
		}
		for _, tc := range cases {
			status, p := importKeyRaw(t, f, "bad", tc.key)
			wantProblem(t, tc.what, status, p, 422, "invalid_public_key", tc.detail)
			if strings.Contains(fmt.Sprint(p), "PRIVATE KEY") || strings.Contains(fmt.Sprint(p), "b3BlbnNz") {
				t.Errorf("%s: the refusal echoes the submitted text: %v", tc.what, p)
			}
		}
		if f.SSHKeyCount() != 0 {
			t.Errorf("refused keys left %d key(s) in the keystore", f.SSHKeyCount())
		}
	})

	t.Run("bodies", func(t *testing.T) {
		f := newFakeAPI(t, "good-key", testOrgID)
		for _, tc := range []struct{ what, body, field, message string }{
			{"empty name", `{"name":"","public_key":"x"}`, "name", "String should have at least 1 character"},
			{"a name above 64 characters", `{"name":"` + strings.Repeat("a", 65) + `","public_key":"x"}`, "name", "String should have at most 64 characters"},
			{"missing key", `{"name":"a"}`, "public_key", "Field required"},
			{"an unknown field", `{"name":"a","public_key":"x","comment":"c"}`, "comment", "Extra inputs are not permitted"},
			{"a key above 8192 characters", `{"name":"a","public_key":"` + strings.Repeat("A", 8193) + `"}`, "public_key", "String should have at most 8192 characters"},
		} {
			status, _, resp := fakeHTTP(t, f, "good-key", http.MethodPost, sshKeysPath(testOrgID), tc.body)
			p := decodeObject(t, resp)
			errs, _ := p["errors"].([]any)
			if status != http.StatusBadRequest || p["code"] != "invalid_request" || len(errs) != 1 ||
				errs[0].(map[string]any)["field"] != tc.field || errs[0].(map[string]any)["message"] != tc.message {
				t.Errorf("%s: %d %v %v, want 400 invalid_request with {%s: %s}", tc.what, status, p["code"], p["errors"], tc.field, tc.message)
			}
		}
	})

	t.Run("a name that is taken", func(t *testing.T) {
		f := newFakeAPI(t, "good-key", testOrgID)
		importKeyVia(t, f, "taken", testKeyEd25519)
		// The same key material under another name is allowed; the same name is not.
		importKeyVia(t, f, "other-name", testKeyEd25519)
		status, p := importKeyRaw(t, f, "taken", testKeyOther)
		wantProblem(t, "the same name", status, p, 409, "name_taken", "an SSH key named 'taken' already exists in this organization")
		if f.SSHKeyCount() != 2 {
			t.Errorf("keys = %d, want 2", f.SSHKeyCount())
		}
	})

	t.Run("the quota", func(t *testing.T) {
		f := newFakeAPI(t, "good-key", testOrgID)
		f.SetSSHKeyQuota(2)
		importKeyVia(t, f, "one", testKeyEd25519)
		importKeyVia(t, f, "two", testKeyOther)
		status, p := importKeyRaw(t, f, "three", testKeyECDSA256)
		wantProblem(t, "a third key", status, p, 422, "quota_exceeded", "ssh_keys: 2 in use plus 1 requested exceeds the limit of 2 for this account")
		f.SetSSHKeyQuota(-1)
		if status, _ := importKeyRaw(t, f, "three", testKeyECDSA256); status != http.StatusCreated {
			t.Errorf("unlimited quota: status = %d, want 201", status)
		}
	})

	t.Run("the switch is off", func(t *testing.T) {
		f := newFakeAPI(t, "good-key", testOrgID)
		id := importKeyVia(t, f, "before", testKeyEd25519)["id"].(string)
		f.SetSSHKeysOn(false)

		status, p := importKeyRaw(t, f, "after", testKeyOther)
		wantProblem(t, "import while off", status, p, 503, "not_ready", "SSH keys are not switched on yet")
		// Reading and deleting stay served, so switching the feature off does not strand what exists.
		if status, _, _ := fakeHTTP(t, f, "good-key", http.MethodGet, sshKeysPath(testOrgID)+"/"+id, ""); status != http.StatusOK {
			t.Errorf("get while off: status = %d, want 200", status)
		}
		if status, _, _ := fakeHTTP(t, f, "good-key", http.MethodGet, sshKeysPath(testOrgID), ""); status != http.StatusOK {
			t.Errorf("list while off: status = %d, want 200", status)
		}
		if status, _, _ := fakeHTTP(t, f, "good-key", http.MethodDelete, sshKeysPath(testOrgID)+"/"+id, ""); status != http.StatusNoContent {
			t.Errorf("delete while off: status = %d, want 204", status)
		}
	})
}

// The key import takes the Idempotency-Key the VM create takes: the same key
// and body replay the stored answer, another body is a client bug, and a repeat
// that races the first request meets idempotency_in_progress first.
func TestFakeAPI_SshKeyImportIsIdempotent(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)

	body, _ := json.Marshal(map[string]string{"name": "once", "public_key": testKeyEd25519})
	status, hdr1, first := fakeHTTP(t, f, "good-key", http.MethodPost, sshKeysPath(testOrgID), string(body), "Idempotency-Key", "k1")
	if status != http.StatusCreated {
		t.Fatalf("first: status = %d (%s)", status, first)
	}
	status, hdr2, second := fakeHTTP(t, f, "good-key", http.MethodPost, sshKeysPath(testOrgID), string(body), "Idempotency-Key", "k1")
	if status != http.StatusCreated || string(first) != string(second) || hdr1.Get("Location") != hdr2.Get("Location") {
		t.Errorf("replay: status %d, body equal %v, Location %q vs %q; want the stored 201", status,
			string(first) == string(second), hdr1.Get("Location"), hdr2.Get("Location"))
	}
	if f.SSHKeyCount() != 1 {
		t.Errorf("keys = %d, want 1: a replay must not import twice", f.SSHKeyCount())
	}

	other, _ := json.Marshal(map[string]string{"name": "once", "public_key": testKeyOther})
	status, _, resp := fakeHTTP(t, f, "good-key", http.MethodPost, sshKeysPath(testOrgID), string(other), "Idempotency-Key", "k1")
	wantProblem(t, "a reused key", status, decodeObject(t, resp), 409, "idempotency_key_reused", "this Idempotency-Key was already used with a different request body")

	// In flight: the next two repeats lose the race, then the stored answer replays.
	f.InProgressFor(2)
	for i := 0; i < 2; i++ {
		status, _, resp = fakeHTTP(t, f, "good-key", http.MethodPost, sshKeysPath(testOrgID), string(body), "Idempotency-Key", "k1")
		wantProblem(t, "a repeat that races the first request", status, decodeObject(t, resp), 409, "idempotency_in_progress", detailIdempotencyInProgress)
	}
	if status, _, _ = fakeHTTP(t, f, "good-key", http.MethodPost, sshKeysPath(testOrgID), string(body), "Idempotency-Key", "k1"); status != http.StatusCreated {
		t.Errorf("after the race: status = %d, want the stored 201", status)
	}
	if keys := f.SSHKeyIdempotencyKeys(); len(keys) != 6 {
		t.Errorf("SSHKeyIdempotencyKeys = %v, want the 6 keyed requests that reached the API", keys)
	}

	// The same knob races a VM create.
	if status, _, _ = fakeHTTP(t, f, "good-key", http.MethodPost, vmsPath(testOrgID), testValidVmJS, "Idempotency-Key", "v1"); status != http.StatusAccepted {
		t.Fatalf("vm create: status = %d", status)
	}
	f.InProgressFor(1)
	status, _, resp = fakeHTTP(t, f, "good-key", http.MethodPost, vmsPath(testOrgID), testValidVmJS, "Idempotency-Key", "v1")
	wantProblem(t, "a vm create that races the first", status, decodeObject(t, resp), 409, "idempotency_in_progress", detailIdempotencyInProgress)
	if status, _, _ = fakeHTTP(t, f, "good-key", http.MethodPost, vmsPath(testOrgID), testValidVmJS, "Idempotency-Key", "v1"); status != http.StatusAccepted || f.CreateCount() != 1 {
		t.Errorf("after the race: status %d, creates %d, want the stored 202 and still 1 create", status, f.CreateCount())
	}
}

// §3.3 point 4: the customer route resolves ssh_keys against the org's keystore
// (never against Nova), keeps the names as a launch-time snapshot, and refuses
// a non-empty list while the keystore is off before it claims the key.
func TestFakeAPI_VmCreateResolvesSshKeysAgainstTheKeystore(t *testing.T) {
	f := newFakeAPI(t, "good-key", testOrgID)
	create := func(body string, extra ...string) (int, map[string]any) {
		status, _, resp := fakeHTTP(t, f, "good-key", http.MethodPost, vmsPath(testOrgID), body, extra...)
		return status, decodeObject(t, resp)
	}

	// An unknown name, more than ten, and a name twice.
	status, p := create(vmWithKeys("vm-a", "nope"))
	wantProblem(t, "an unknown key", status, p, 422, "ssh_key_not_found",
		"no SSH key named 'nope' in this organization; import it with POST /v1/orgs/"+testOrgID+"/compute/ssh-keys")
	importKeyVia(t, f, "alpha", testKeyEd25519)
	importKeyVia(t, f, "beta", testKeyOther)
	status, p = create(vmWithKeys("vm-a", "alpha", "nope", "beta", "gone"))
	wantProblem(t, "two unknown keys", status, p, 422, "ssh_key_not_found", "")
	if d, _ := p["detail"].(string); !strings.Contains(d, "'nope', 'gone'") {
		t.Errorf("detail %q does not name both unknown keys", d)
	}
	eleven := make([]string, 11)
	for i := range eleven {
		eleven[i] = fmt.Sprintf("k%d", i)
	}
	status, p = create(vmWithKeys("vm-a", eleven...))
	wantProblem(t, "eleven keys", status, p, 422, "too_many_ssh_keys", "a VM can be given at most 10 SSH keys")
	status, p = create(vmWithKeys("vm-a", "alpha", "beta", "alpha"))
	wantProblem(t, "a key twice", status, p, 422, "duplicate_ssh_key", "SSH key 'alpha' is listed more than once")
	if f.CreateCount() != 0 {
		t.Fatalf("refused creates built %d vm(s)", f.CreateCount())
	}

	// Two imported keys are fine, the names come back in the order given, and
	// deleting a key afterwards changes nothing on the VM.
	status, p = create(vmWithKeys("vm-a", "beta", "alpha"))
	if status != http.StatusAccepted {
		t.Fatalf("create with two keys: %d %v", status, p)
	}
	vm := p["vm"].(map[string]any)
	if names, _ := vm["ssh_key_names"].([]any); len(names) != 2 || names[0] != "beta" || names[1] != "alpha" {
		t.Errorf("ssh_key_names = %v, want [beta alpha]", vm["ssh_key_names"])
	}
	id := vm["id"].(string)
	if status, _, _ := fakeHTTP(t, f, "good-key", http.MethodDelete, sshKeysPath(testOrgID)+"/"+f.SSHKeyID("alpha"), ""); status != http.StatusNoContent {
		t.Fatalf("delete of the key: status = %d", status)
	}
	if got := f.SSHKeyNames(id); len(got) != 2 || got[0] != "beta" || got[1] != "alpha" {
		t.Errorf("after the key was deleted the vm lists %v, want [beta alpha] still", got)
	}

	// Switched off: a non-empty list is 503 not_ready before the key is claimed
	// (so the same Idempotency-Key works once it is on), an empty list is served.
	f.SetSSHKeysOn(false)
	status, p = create(vmWithKeys("vm-b", "beta"), "Idempotency-Key", "k-off")
	wantProblem(t, "keys while off", status, p, 503, "not_ready", "SSH keys are not switched on yet")
	if status, _ := create(vmWithKeys("vm-c"), "Idempotency-Key", "k-empty"); status != http.StatusAccepted {
		t.Errorf("an empty list while off: status = %d, want 202", status)
	}
	f.SetSSHKeysOn(true)
	if status, p := create(vmWithKeys("vm-b", "beta"), "Idempotency-Key", "k-off"); status != http.StatusAccepted {
		t.Errorf("the same Idempotency-Key once on: %d %v, want 202: the refusal must not have claimed it", status, p["code"])
	}
}
