package provider

// The sweeper of the live job: it removes the instances an earlier run left behind, protected ones included, before the
// job counts what the org holds and before the first live test runs (ci/provider-staging-live.sh runs it first). The
// same helper is the cleanup of the instance attributes proof, which removes its own instance by name even when a step
// failed. The helper is tested here against a small stub API (the fake API has no instance list route).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.virtscale.nl/virtscale/virtscale-platform/terraform-provider-virtscale/internal/client"
)

// liveSweepTimeout bounds the removal of every instance one sweep finds.
const liveSweepTimeout = 10 * time.Minute

// liveSweepPoll is how often a sweep reads an instance it waits for. It is not pollInterval: TestMain shortens that
// to 10 ms, which is no cadence for a real API.
var liveSweepPoll = 5 * time.Second

// hasLiveNamePrefix is what the sweeper matches: the names the live tests give their objects, and nothing else.
func hasLiveNamePrefix(name string) bool { return strings.HasPrefix(name, liveNamePrefix) }

// liveSweepInstances removes every instance of the organization whose name match accepts: protection is lifted first (a
// protected instance refuses the delete), then the instance is deleted and waited for until it is gone, each within its
// own timeout. It logs names and ids only, and reads every error as its status and code. A removal that fails does not
// stop the others; the returned error lists what is left.
func liveSweepInstances(t *testing.T, c *client.Client, match func(name string) bool, timeout time.Duration) error {
	t.Helper()
	listCtx, cancelList := context.WithTimeout(context.Background(), timeout)
	vms, err := c.ListVms(listCtx)
	cancelList()
	if err != nil {
		return errors.New("listing the instances failed: " + errKind(err))
	}
	var left []string
	for _, vm := range vms {
		if !match(vm.Name) || vm.State == "deleted" {
			continue
		}
		t.Logf("sweep: removing instance %s (%s, state %s, protected %v)", vm.Name, vm.ID, vm.State, vm.Protected)
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		err := liveRemoveInstance(ctx, c, vm)
		cancel()
		if err != nil {
			left = append(left, vm.Name+" ("+vm.ID+"): "+err.Error())
		}
	}
	if len(left) > 0 {
		return errors.New("not removed: " + strings.Join(left, "; "))
	}
	return nil
}

// liveRemoveInstance lifts the protection of vm, deletes it and waits until the API no longer has it. A delete that meets
// another operation of the instance (409, for example one still building) is sent again on the next read.
func liveRemoveInstance(ctx context.Context, c *client.Client, vm client.Vm) error {
	if vm.Protected {
		if _, err := c.SetVmProtection(ctx, vm.ID, false, ""); err != nil && !client.IsNotFound(err, client.ResourceVm) {
			return errors.New("lifting the protection failed: " + errKind(err))
		}
	}
	state := vm.State
	for {
		cur, err := c.GetVm(ctx, vm.ID)
		switch {
		case client.IsNotFound(err, client.ResourceVm):
			return nil
		case err != nil:
			return errors.New("reading it failed: " + errKind(err))
		case cur.State == "deleted":
			return nil
		}
		state = cur.State
		if cur.State != "deleting" && cur.DesiredState != "deleted" {
			_, _, err := c.DeleteVm(ctx, vm.ID)
			var apiErr *client.APIError
			busy := errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict && apiErr.Code != "vm_protected"
			if err != nil && !busy && !client.IsNotFound(err, client.ResourceVm) {
				return errors.New("deleting it failed: " + errKind(err))
			}
		}
		select {
		case <-ctx.Done():
			return errors.New("still there when the time ran out (last state " + state + ")")
		case <-time.After(liveSweepPoll):
		}
	}
}

// TestLiveSweepLeftovers is the first test of the live job: it removes tf-live-* instances an earlier run (a killed job,
// a failed destroy of a protected instance) left behind, so that they cannot block the clean-start guard or this run's
// own server. Without any it passes at once.
func TestLiveSweepLeftovers(t *testing.T) {
	endpoint, apiKey, org := liveSshKeyEnv(t)
	c, err := liveClient(endpoint, apiKey, org)
	if err != nil {
		t.Fatalf("build the API client: %v", err)
	}
	if err := liveSweepInstances(t, c, hasLiveNamePrefix, liveSweepTimeout); err != nil {
		t.Fatalf("the sweep left instances behind: %v", err)
	}
}

// --- the stub API --------------------------------------------------------------------------------------------------

// sweepStub is an instance API of one organization (o1): list, read, delete and protection. A delete of a protected
// instance is 409 vm_protected; an unprotected one is "deleting" for one read and then gone. A stuck instance never goes.
type sweepStub struct {
	mu    sync.Mutex
	vms   map[string]*client.Vm
	stuck map[string]bool
	calls []string // "METHOD id" of every call that names an instance
}

func newSweepStub(t *testing.T, vms ...client.Vm) (*sweepStub, *client.Client) {
	t.Helper()
	s := &sweepStub{vms: map[string]*client.Vm{}, stuck: map[string]bool{}}
	for i := range vms {
		vm := vms[i]
		s.vms[vm.ID] = &vm
	}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	c, err := client.New(srv.URL, "vsk_test", "o1")
	if err != nil {
		t.Fatal(err)
	}
	old := liveSweepPoll
	liveSweepPoll = 5 * time.Millisecond
	t.Cleanup(func() { liveSweepPoll = old })
	return s, c
}

func (s *sweepStub) called(id string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, c := range s.calls {
		if strings.HasSuffix(c, " "+id) {
			out = append(out, strings.Fields(c)[0])
		}
	}
	return out
}

func (s *sweepStub) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rest, ok := strings.CutPrefix(r.URL.Path, "/v1/orgs/o1/compute/instances")
	if !ok {
		http.NotFound(w, r)
		return
	}
	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	problem := func(status int, code, detail string) {
		reply(status, map[string]any{"type": "about:blank", "title": code, "status": status, "code": code, "detail": detail})
	}
	if rest == "" {
		items := []client.Vm{}
		for _, vm := range s.vms {
			items = append(items, *vm)
		}
		reply(http.StatusOK, map[string]any{"items": items, "next_cursor": nil})
		return
	}
	id, sub, _ := strings.Cut(strings.TrimPrefix(rest, "/"), "/")
	s.calls = append(s.calls, r.Method+" "+id)
	vm, found := s.vms[id]
	if !found {
		problem(http.StatusNotFound, "not_found", "no vm matching that identifier is visible to this organization")
		return
	}
	switch {
	case r.Method == http.MethodPut && sub == "protection":
		var body struct {
			Protected bool `json:"protected"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		vm.Protected = body.Protected
		reply(http.StatusOK, vm)
	case r.Method == http.MethodDelete && vm.Protected:
		problem(http.StatusConflict, "vm_protected", "this VM is protected against deletion")
	case r.Method == http.MethodDelete:
		vm.State, vm.DesiredState = "deleting", "deleted"
		reply(http.StatusAccepted, map[string]any{"vm": vm, "operation": map[string]any{"id": "op-" + id, "state": "running"}})
	case r.Method == http.MethodGet:
		shown := *vm
		if vm.State == "deleting" && !s.stuck[id] {
			delete(s.vms, id) // the next read finds it gone
		}
		reply(http.StatusOK, shown)
	default:
		problem(http.StatusMethodNotAllowed, "method_not_allowed", "no")
	}
}

func sweepVM(id, name, state string, protected bool) client.Vm {
	return client.Vm{ID: id, Name: name, State: state, DesiredState: "running", Protected: protected}
}

func TestLiveSweepRemovesOnlyLivePrefixedInstancesAndLiftsProtectionFirst(t *testing.T) {
	stub, c := newSweepStub(t,
		sweepVM("vm-protected", "tf-live-aaaaaaaa", "running", true),
		sweepVM("vm-plain", "tf-live-bbbbbbbb", "running", false),
		sweepVM("vm-keep-protected", "keep-me", "running", true),
		sweepVM("vm-keep-infix", "my-tf-live-cccccccc", "running", false),
		sweepVM("vm-done", "tf-live-dddddddd", "deleted", false),
	)

	if err := liveSweepInstances(t, c, hasLiveNamePrefix, 10*time.Second); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	for _, id := range []string{"vm-protected", "vm-plain"} {
		if _, there := stub.vms[id]; there {
			t.Errorf("%s is still there after the sweep", id)
		}
	}
	calls := stub.called("vm-protected")
	put, del := slices.Index(calls, "PUT"), slices.Index(calls, "DELETE")
	if put < 0 || del < 0 || put > del {
		t.Errorf("calls for the protected instance = %v, want the protection lifted (PUT) before the first DELETE", calls)
	}
	for _, id := range []string{"vm-keep-protected", "vm-keep-infix", "vm-done"} {
		if got := stub.called(id); len(got) > 0 {
			t.Errorf("%s does not match the live prefix (or is already deleted) but the sweep called %v on it", id, got)
		}
	}
	if !stub.vms["vm-keep-protected"].Protected {
		t.Error("the protection of an instance outside the live prefix was lifted")
	}
}

func TestLiveSweepMatchesTheNameOfOneTest(t *testing.T) {
	stub, c := newSweepStub(t,
		sweepVM("vm-mine", "tf-live-11111111", "running", true),
		sweepVM("vm-other-run", "tf-live-22222222", "running", true),
	)
	if err := liveSweepInstances(t, c, func(n string) bool { return n == "tf-live-11111111" }, 10*time.Second); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, there := stub.vms["vm-mine"]; there {
		t.Error("the instance of the test is still there")
	}
	if got := stub.called("vm-other-run"); len(got) > 0 {
		t.Errorf("another run's instance was called with %v", got)
	}
}

func TestLiveSweepGivesUpWhenAnInstanceDoesNotGoAndSaysWhich(t *testing.T) {
	stub, c := newSweepStub(t,
		sweepVM("vm-stuck", "tf-live-eeeeeeee", "running", false),
		sweepVM("vm-fine", "tf-live-ffffffff", "running", false),
	)
	stub.stuck["vm-stuck"] = true

	err := liveSweepInstances(t, c, hasLiveNamePrefix, 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "vm-stuck") || !strings.Contains(err.Error(), "tf-live-eeeeeeee") {
		t.Fatalf("err = %v, want it to name the instance that did not go", err)
	}
	if strings.Contains(err.Error(), "vm-fine") {
		t.Errorf("err = %v: the instance that went must not be listed", err)
	}
}
