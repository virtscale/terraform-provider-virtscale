package provider

import (
	"net/http"
	"sort"
	"time"
)

// The catalogue and identity routes of the fake: GET /v1/me, regions, instance types and plans. Shapes are the
// schemas MeOut, RegionOut, InstanceTypeOut and PlanOut of customer-v1.json; internal/contract checks them.

type wireMeOrganization struct {
	OrgID       string   `json:"org_id"`
	Alias       *string  `json:"alias"`
	Role        *string  `json:"role"`
	Permissions []string `json:"permissions"`
}

type wireMeApiKey struct {
	ID        string     `json:"id"`
	ProjectID *string    `json:"project_id"`
	Scopes    []string   `json:"scopes"`
	ExpiresAt *time.Time `json:"expires_at"`
}

type wireMeOut struct {
	Subject             string               `json:"subject"`
	Realm               string               `json:"realm"`
	Credential          string               `json:"credential"`
	PermissionsEnforced bool                 `json:"permissions_enforced"`
	Organizations       []wireMeOrganization `json:"organizations"`
	ApiKey              *wireMeApiKey        `json:"api_key"`
}

type wireDiskTypeOut struct {
	DiskType string `json:"disk_type"`
	Status   string `json:"status"`
}

type wireRegionOut struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Datacenter string            `json:"datacenter"`
	Status     string            `json:"status"`
	Access     string            `json:"access"`
	Entitled   bool              `json:"entitled"`
	DiskTypes  []wireDiskTypeOut `json:"disk_types"`
	Default    bool              `json:"default"`
}

type wireInstanceTypeOut struct {
	ID       string   `json:"id"`
	VRN      string   `json:"vrn"`
	Name     string   `json:"name"`
	Family   string   `json:"family"`
	CPUKind  string   `json:"cpu_kind"`
	VCPUs    int      `json:"vcpus"`
	MemoryMB int      `json:"memory_mb"`
	DiskGB   int      `json:"disk_gb"`
	Regions  []string `json:"regions"`
	Status   string   `json:"status"`
}

type wirePlanOut struct {
	ID                string  `json:"id"`
	Name              string  `json:"name"`
	VCPUs             int     `json:"vcpus"`
	RAMMB             int     `json:"ram_mb"`
	DiskGB            int     `json:"disk_gb"`
	MonthlyPriceCents int     `json:"monthly_price_cents"`
	Currency          string  `json:"currency"`
	Status            string  `json:"status"`
	ManagementLevel   string  `json:"management_level"`
	BackupPolicy      *string `json:"backup_policy"`
}

var fakeRegions = []wireRegionOut{
	{ID: "gro", Name: "Groningen", Datacenter: "GRO1", Status: "active", Access: "public", Entitled: true, Default: true,
		DiskTypes: []wireDiskTypeOut{{"ssd", "active"}, {"hdd", "coming_soon"}}},
	{ID: "ams", Name: "Amsterdam", Datacenter: "AMS1", Status: "coming_soon", Access: "restricted", Entitled: false,
		DiskTypes: []wireDiskTypeOut{{"ssd", "coming_soon"}, {"hdd", "coming_soon"}}},
}

var fakeInstanceTypes = []wireInstanceTypeOut{
	{ID: "g1-small", VRN: "vrn:virtscale:instance-type:g1-small", Name: "General small", Family: "general", CPUKind: "shared",
		VCPUs: 1, MemoryMB: 2048, DiskGB: 20, Regions: []string{"gro"}, Status: "active"},
	{ID: "g1-medium", VRN: "vrn:virtscale:instance-type:g1-medium", Name: "General medium", Family: "general", CPUKind: "shared",
		VCPUs: 2, MemoryMB: 4096, DiskGB: 40, Regions: []string{"gro"}, Status: "active"},
	{ID: "c1-large", VRN: "vrn:virtscale:instance-type:c1-large", Name: "CPU large", Family: "cpu", CPUKind: "dedicated",
		VCPUs: 4, MemoryMB: 8192, DiskGB: 80, Regions: []string{}, Status: "coming_soon"},
}

func fakePlans() []wirePlanOut {
	daily := "daily-7d"
	return []wirePlanOut{
		{ID: "managed-s", Name: "Managed S", VCPUs: 1, RAMMB: 2048, DiskGB: 40, MonthlyPriceCents: 1500, Currency: "eur",
			Status: "active", ManagementLevel: "full", BackupPolicy: &daily},
		{ID: "managed-m", Name: "Managed M", VCPUs: 2, RAMMB: 4096, DiskGB: 80, MonthlyPriceCents: 2900, Currency: "eur",
			Status: "active", ManagementLevel: "business"},
	}
}

// serveMe is GET /v1/me: any authenticated key, no organization in the path, no permission needed.
func (f *fakeAPI) serveMe(rc *reqCtx) {
	f.mu.Lock()
	f.calls = append(f.calls, "GET me")
	denied := f.sourceDenied
	f.mu.Unlock()
	if rc.r.Method != http.MethodGet {
		rc.problem(http.StatusMethodNotAllowed, "method_not_allowed", "Method Not Allowed", "Method Not Allowed")
		return
	}
	if !f.authenticate(rc) {
		return
	}
	if denied {
		rc.problem(http.StatusForbidden, "source_address_not_allowed", "Forbidden",
			"this API token cannot be used from this network address")
		return
	}
	scopes := append([]string(nil), rc.scopes...)
	sort.Strings(scopes)
	held := map[string]bool{}
	for _, perm := range routePermissions {
		if rc.allows(perm) {
			held[perm] = true
		}
	}
	held["org.read"] = true
	permissions := make([]string, 0, len(held))
	for perm := range held {
		permissions = append(permissions, perm)
	}
	sort.Strings(permissions)
	rc.json(http.StatusOK, wireMeOut{
		Subject: "api-token:" + fakeUUID(4, 1), Realm: "customers", Credential: "api_token", PermissionsEnforced: true,
		Organizations: []wireMeOrganization{{OrgID: f.org, Permissions: permissions}},
		ApiKey:        &wireMeApiKey{ID: fakeUUID(4, 1), Scopes: scopes},
	})
}

func (f *fakeAPI) listRegions(rc *reqCtx) {
	rc.json(http.StatusOK, wireList[wireRegionOut]{Items: fakeRegions})
}

// listInstanceTypes is GET instance-types: the types in the API's order; region keeps those orderable there now.
func (f *fakeAPI) listInstanceTypes(rc *reqCtx) {
	region := rc.r.URL.Query().Get("region")
	items := []wireInstanceTypeOut{}
	for _, it := range fakeInstanceTypes {
		if region == "" || containsString(it.Regions, region) {
			items = append(items, it)
		}
	}
	rc.json(http.StatusOK, wireList[wireInstanceTypeOut]{Items: items})
}

func (f *fakeAPI) listPlans(rc *reqCtx) {
	rc.json(http.StatusOK, wireList[wirePlanOut]{Items: fakePlans()})
}

func containsString(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}
