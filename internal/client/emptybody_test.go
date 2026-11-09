package client

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// A 2xx answer with no body, or only null, on a call that expects one is an error: decoding it would hand back an
// empty struct, so a create would "succeed" with a VM that has no id. A 204 with no body stays what it is on a call
// that expects none (DeleteSshKey, DeleteProject).
func TestEmptyBody_OnACallThatExpectsOneIsAnError(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"empty 202", "", http.StatusAccepted},
		{"empty 200", "", http.StatusOK},
		{"null 202", "null", http.StatusAccepted},
		{"whitespace 200", " \n", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			vm, op, err := c.CreateVm(context.Background(), VmCreateRequest{Name: "web-01"}, "idem-1")
			if err == nil {
				t.Fatalf("CreateVm() = %+v, %+v, nil error; want an error for an empty %d body", vm, op, tc.status)
			}
			if vm != nil || op != nil {
				t.Errorf("CreateVm() returned %+v, %+v with an error; want nil values", vm, op)
			}
			if !strings.Contains(err.Error(), "empty") {
				t.Errorf("error %q does not say the body was empty", err)
			}
			// The API answered 2xx, so it made the VM: the keyed create says how to find it.
			if !strings.Contains(err.Error(), createRecoveryHint("")) {
				t.Errorf("error %q lacks the create recovery hint", err)
			}
		})
	}
}

func TestEmptyBody_GetStillErrorsAndANoContentDeleteStillWorks(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	if _, err := c.GetVm(context.Background(), "vm-1"); err == nil {
		t.Error("GetVm() on an empty 204 = nil error, want one: a read needs a body")
	}
	if err := c.DeleteSshKey(context.Background(), "key-1"); err != nil {
		t.Errorf("DeleteSshKey() on an empty 204 = %v, want nil", err)
	}
}
