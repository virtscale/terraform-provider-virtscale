package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

func mustLoad(t *testing.T) *Spec {
	t.Helper()
	s, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return s
}

func wantNoProblems(t *testing.T, what string, problems []string) {
	t.Helper()
	if len(problems) != 0 {
		t.Errorf("%s: want no problem, got %q", what, problems)
	}
}

func wantProblem(t *testing.T, what string, problems []string, substr string) {
	t.Helper()
	for _, p := range problems {
		if strings.Contains(p, substr) {
			return
		}
	}
	t.Errorf("%s: want a problem containing %q, got %q", what, substr, problems)
}

// The vendored document is the one SOURCE names: a hand edit, or a copy of another commit, shows here.
func TestVendoredSpecMatchesSource(t *testing.T) {
	data, err := os.ReadFile("customer-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	source, err := os.ReadFile("SOURCE")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), "sha256: "+hex.EncodeToString(sum[:])) {
		t.Errorf("SOURCE does not name sha256 %x: update both together", sum)
	}
}

func TestLoadFindsTheOperationsTheProviderCalls(t *testing.T) {
	s := mustLoad(t)
	for _, want := range []string{
		"POST /v1/orgs/{org}/compute/instances",
		"GET /v1/orgs/{org}/compute/instances/{vm_id}",
		"PATCH /v1/orgs/{org}/compute/instances/{vm_id}",
		"GET /v1/me",
		"GET /v1/orgs/{org}/instance-types",
		"POST /v1/projects/{project}/vpcs",
	} {
		if !s.HasOperation(want) {
			t.Errorf("operation %q is not in the contract", want)
		}
	}
}

func TestCheckAcceptsAConformingExchange(t *testing.T) {
	s := mustLoad(t)
	body := `{"subject":"api-token:1","realm":"customers","credential":"api_token","permissions_enforced":true,` +
		`"organizations":[{"org_id":"00000000-0000-4000-8000-000000000001","alias":null,"role":null,"permissions":["org.read"]}],` +
		`"api_key":{"id":"00000000-0000-4000-8000-000000000002","project_id":null,"scopes":["read"],"expires_at":null}}`
	wantNoProblems(t, "GET /v1/me", s.Check("GET", "/v1/me", nil, 200, "application/json", []byte(body)))
}

func TestCheckRefusesWhatTheContractDoesNot(t *testing.T) {
	s := mustLoad(t)

	wantProblem(t, "unknown route",
		s.Check("GET", "/v1/orgs/00000000-0000-4000-8000-000000000001/nope", nil, 200, "application/json", []byte(`{}`)),
		"not in the contract")

	wantProblem(t, "method the route does not have",
		s.Check("DELETE", "/v1/me", nil, 200, "application/json", []byte(`{}`)),
		"not in the contract")

	org := "/v1/orgs/00000000-0000-4000-8000-000000000001"
	wantProblem(t, "deprecated alias",
		s.Check("GET", org+"/compute/vms/00000000-0000-4000-8000-000000000003", nil, 404, "application/problem+json",
			[]byte(`{"type":"t","title":"t","status":404,"detail":"d","code":"not_found","request_id":"r"}`)),
		"deprecated")

	wantProblem(t, "request field the contract does not have",
		s.Check("POST", org+"/compute/ssh-keys", []byte(`{"name":"a","public_key":"k","surprise":1}`), 201, "application/json", []byte(`{}`)),
		"surprise")

	wantProblem(t, "response missing a required field",
		s.Check("GET", "/v1/me", nil, 200, "application/json", []byte(`{"subject":"x"}`)),
		"realm")

	wantProblem(t, "status the operation does not declare",
		s.Check("GET", "/v1/me", nil, 201, "application/json", []byte(`{}`)),
		"201")
}
