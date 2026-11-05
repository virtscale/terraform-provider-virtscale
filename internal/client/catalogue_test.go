package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// serve answers every request with the given status and body and records the path and query of the last one.
func serve(t *testing.T, status int, body string) (*Client, *string) {
	t.Helper()
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		seen = r.Method + " " + r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "key", "org-1")
	if err != nil {
		t.Fatal(err)
	}
	return c, &seen
}

func TestGetMeAsksTheAPIRootNotTheOrganization(t *testing.T) {
	c, seen := serve(t, 200, `{"subject":"api-token:k","realm":"customers","credential":"api_token","permissions_enforced":true,`+
		`"organizations":[{"org_id":"org-1","alias":null,"role":null,"permissions":["org.read"]}],`+
		`"api_key":{"id":"k","project_id":null,"scopes":["read"],"expires_at":"2027-01-02T03:04:05Z"}}`)
	me, err := c.GetMe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if *seen != "GET /v1/me" {
		t.Errorf("request = %q, want GET /v1/me", *seen)
	}
	if me.Subject != "api-token:k" || me.Credential != "api_token" || !me.PermissionsEnforced {
		t.Errorf("identity = %+v", me)
	}
	if len(me.Organizations) != 1 || me.Organizations[0].OrgID != "org-1" || me.Organizations[0].Role != nil {
		t.Errorf("organizations = %+v", me.Organizations)
	}
	if me.APIKey == nil || me.APIKey.ID != "k" || me.APIKey.ProjectID != nil || me.APIKey.ExpiresAt == nil ||
		me.APIKey.ExpiresAt.Year() != 2027 || len(me.APIKey.Scopes) != 1 {
		t.Errorf("api key = %+v", me.APIKey)
	}
}

func TestOrganizationIsWhatTheClientWasBuiltFor(t *testing.T) {
	c, _ := serve(t, 200, `{}`)
	if got := c.Organization(); got != "org-1" {
		t.Errorf("Organization() = %q", got)
	}
}

func TestCatalogueListsUseTheInstancesNounPaths(t *testing.T) {
	cases := []struct {
		name, want, body string
		call             func(*Client) (int, error)
	}{
		{"regions", "GET /v1/orgs/org-1/compute/regions",
			`{"items":[{"id":"gro","name":"Groningen","datacenter":"GRO1","status":"active","access":"public","entitled":true,"default":true,"disk_types":[{"disk_type":"ssd","status":"active"}]}],"next_cursor":null}`,
			func(c *Client) (int, error) { r, err := c.ListRegions(context.Background()); return len(r), err }},
		{"instance types", "GET /v1/orgs/org-1/instance-types",
			`{"items":[{"id":"g1-small","vrn":"v","name":"n","family":"general","cpu_kind":"shared","vcpus":1,"memory_mb":2048,"disk_gb":20,"regions":["gro"],"status":"active"}],"next_cursor":null}`,
			func(c *Client) (int, error) {
				r, err := c.ListInstanceTypes(context.Background(), "")
				return len(r), err
			}},
		{"instance types in a region", "GET /v1/orgs/org-1/instance-types?region=gro",
			`{"items":[],"next_cursor":null}`,
			func(c *Client) (int, error) {
				r, err := c.ListInstanceTypes(context.Background(), "gro")
				return len(r), err
			}},
		{"plans", "GET /v1/orgs/org-1/compute/plans",
			`{"items":[{"id":"managed-s","name":"S","vcpus":1,"ram_mb":2048,"disk_gb":40,"monthly_price_cents":1500,"currency":"eur","status":"active","management_level":"full","backup_policy":null}],"next_cursor":null}`,
			func(c *Client) (int, error) { r, err := c.ListPlans(context.Background()); return len(r), err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, seen := serve(t, 200, tc.body)
			if _, err := tc.call(c); err != nil {
				t.Fatal(err)
			}
			if *seen != tc.want {
				t.Errorf("request = %q, want %q", *seen, tc.want)
			}
		})
	}
}
