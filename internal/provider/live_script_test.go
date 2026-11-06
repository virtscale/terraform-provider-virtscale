package provider

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The live job's script (ci/provider-staging-live.sh) only ever runs on protected main with the staging key, so the part
// that decides what a run left behind is tested here: its own text is extracted and run against a local API.

// scriptCounting is the text of the script from api_get up to, not including, the first call of leftovers.
func scriptCounting(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../ci/provider-staging-live.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	start := strings.Index(text, "# GET path (below the API root)")
	end := strings.Index(text, "before=$(leftovers)")
	if start < 0 || end < start {
		t.Fatal("the counting functions are not where the test expects them in ci/provider-staging-live.sh")
	}
	return text[start:end]
}

// runCounting runs leftovers against a server that answers the given path to JSON (status 200) or to a status.
func runCounting(t *testing.T, answers map[string]any) (stdout, stderr string, ok bool) {
	t.Helper()
	for _, tool := range []string{"sh", "jq", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("CI") != "" { // the pipeline installs jq: a missing tool must not turn these tests off unseen
				t.Fatalf("%s is not on PATH in CI; the counting of the live script would go untested", tool)
			}
			t.Skipf("%s is not on PATH; the counting of the live script is tested where it is", tool)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer vsk_test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch a := answers[r.URL.Path].(type) {
		case int:
			w.WriteHeader(a)
		case string:
			_, _ = w.Write([]byte(a))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	harness := `set -eu
fail() { echo "FAIL: $*" >&2; exit 1; }
curl_safe() { curl -q --noproxy '*' "$@"; }
ENDPOINT=` + srv.URL + `
org=o1
api_key=vsk_test
work=` + "'" + t.TempDir() + "'" + `
` + scriptCounting(t) + `
leftovers
`
	cmd := exec.Command("sh", "-c", harness)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return strings.TrimSpace(out.String()), errb.String(), err == nil
}

func TestLiveScriptCountsOnlyWhatARunLeavesBehind(t *testing.T) {
	answers := map[string]any{
		"/v1/orgs/o1/compute/ssh-keys":  `{"items":[{"name":"tf-live-aaaa"},{"name":"keep"}]}`,
		"/v1/orgs/o1/projects":          `{"items":[{"id":"p1","name":"default"},{"id":"p2","name":"tf-live-bbbb"}]}`,
		"/v1/orgs/o1/compute/instances": `{"items":[]}`,
		"/v1/orgs/o1/s3/buckets": `{"items":[{"name":"tf-live-c","state":"active"},{"name":"tf-live-d","state":"deleting_soft"},` +
			`{"name":"tf-live-e","state":"purged"},{"name":"other","state":"active"}]}`,
		"/v1/projects/p1/vpcs": `{"items":[{"name":"tf-live-f"},{"name":"other"},{"name":"tf-other"}]}`,
		"/v1/projects/p2/vpcs": http.StatusServiceUnavailable, // VPCs are not switched on for that project
	}
	out, errs, ok := runCounting(t, answers)
	if !ok || out != "ssh_keys=1 projects=1 instances=0 buckets=1 vpcs=1" {
		t.Errorf("leftovers = %q (ok %v, stderr %q); a soft-deleted or purged bucket and other names are not counted, a 503 counts none", out, ok, errs)
	}
}

func TestLiveScriptTreatsAFeatureThatIsSwitchedOffAsNothingLeft(t *testing.T) {
	out, _, ok := runCounting(t, map[string]any{
		"/v1/orgs/o1/compute/ssh-keys":  http.StatusServiceUnavailable,
		"/v1/orgs/o1/projects":          http.StatusServiceUnavailable,
		"/v1/orgs/o1/compute/instances": `{"items":[]}`,
		"/v1/orgs/o1/s3/buckets":        http.StatusServiceUnavailable,
	})
	if !ok || out != "ssh_keys=0 projects=0 instances=0 buckets=0 vpcs=0" {
		t.Errorf("leftovers = %q (ok %v), want all zero", out, ok)
	}
}

func TestLiveScriptStopsOnAnAnswerItCannotCount(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{
			"/v1/orgs/o1/compute/ssh-keys":  `{"items":[]}`,
			"/v1/orgs/o1/projects":          `{"items":[]}`,
			"/v1/orgs/o1/compute/instances": `{"items":[]}`,
			"/v1/orgs/o1/s3/buckets":        `{"items":[]}`,
		}
	}
	for _, path := range []string{"/v1/orgs/o1/compute/ssh-keys", "/v1/orgs/o1/projects", "/v1/orgs/o1/compute/instances", "/v1/orgs/o1/s3/buckets"} {
		for _, status := range []int{http.StatusInternalServerError, http.StatusUnauthorized, http.StatusTooManyRequests} {
			answers := base()
			answers[path] = status
			if out, errs, ok := runCounting(t, answers); ok || !strings.Contains(errs, "answered HTTP") {
				t.Errorf("%s answering %d: leftovers = %q, stderr %q (ok %v), want the run to stop", path, status, out, errs, ok)
			}
		}
	}
}

func TestLiveScriptStopsWhenAProjectsVpcListCannotBeRead(t *testing.T) {
	out, errs, ok := runCounting(t, map[string]any{
		"/v1/orgs/o1/compute/ssh-keys":  `{"items":[]}`,
		"/v1/orgs/o1/projects":          `{"items":[{"id":"p1","name":"default"}]}`,
		"/v1/orgs/o1/compute/instances": `{"items":[]}`,
		"/v1/orgs/o1/s3/buckets":        `{"items":[]}`,
		"/v1/projects/p1/vpcs":          http.StatusInternalServerError,
	})
	if ok || !strings.Contains(errs, "VPCs of a project answered HTTP 500") {
		t.Errorf("leftovers = %q, stderr %q (ok %v), want the run to stop", out, errs, ok)
	}
}
