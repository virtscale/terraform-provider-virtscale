package client

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// apiPath builds an absolute URL that is not under /v1/orgs/{org}/ (GET /v1/me).
func (c *Client) apiPath(p string) string {
	return c.endpoint + "/" + p
}

// Organization is the organization this client was built for.
func (c *Client) Organization() string {
	return c.org
}

// Me is the MeOut representation returned by GET /v1/me: who the credential is and what it holds.
type Me struct {
	Subject             string           `json:"subject"`
	Realm               string           `json:"realm"`
	Credential          string           `json:"credential"`
	PermissionsEnforced bool             `json:"permissions_enforced"`
	Organizations       []MeOrganization `json:"organizations"`
	APIKey              *MeAPIKey        `json:"api_key"`
}

// MeOrganization is one organization the credential names, and what the caller holds in it.
type MeOrganization struct {
	OrgID       string   `json:"org_id"`
	Alias       *string  `json:"alias"`
	Role        *string  `json:"role"`
	Permissions []string `json:"permissions"`
}

// MeAPIKey is the vsk_ key the request authenticated with. It carries no secret.
type MeAPIKey struct {
	ID        string     `json:"id"`
	ProjectID *string    `json:"project_id"`
	Scopes    []string   `json:"scopes"`
	ExpiresAt *time.Time `json:"expires_at"`
}

// DiskType is a disk type a new instance may name in a region, and whether it is offered yet.
type DiskType struct {
	DiskType string `json:"disk_type"`
	Status   string `json:"status"`
}

// Region is the RegionOut representation returned by GET compute/regions.
type Region struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Datacenter string     `json:"datacenter"`
	Status     string     `json:"status"`
	Access     string     `json:"access"`
	Entitled   bool       `json:"entitled"`
	DiskTypes  []DiskType `json:"disk_types"`
	Default    bool       `json:"default"`
}

// InstanceType is the InstanceTypeOut representation returned by GET instance-types.
type InstanceType struct {
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

// Plan is the PlanOut representation returned by GET compute/plans: one Managed VPS tier.
type Plan struct {
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

// GetMe sends GET /v1/me. It names no organization: the credential's own are in the answer.
func (c *Client) GetMe(ctx context.Context) (*Me, error) {
	var out Me
	if err := c.doJSON(ctx, http.MethodGet, c.apiPath("v1/me"), nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListRegions sends GET /v1/orgs/{org}/compute/regions. The catalogue lists take no cursor, so one page is all.
func (c *Client) ListRegions(ctx context.Context) ([]Region, error) {
	var out listResponse[Region]
	if err := c.doJSON(ctx, http.MethodGet, c.orgPath("compute/regions"), nil, "", &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// ListInstanceTypes sends GET /v1/orgs/{org}/instance-types, keeping the types that can be ordered in region when
// region is not empty.
func (c *Client) ListInstanceTypes(ctx context.Context, region string) ([]InstanceType, error) {
	p := c.orgPath("instance-types")
	if region != "" {
		p += "?region=" + url.QueryEscape(region)
	}
	var out listResponse[InstanceType]
	if err := c.doJSON(ctx, http.MethodGet, p, nil, "", &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// ListPlans sends GET /v1/orgs/{org}/compute/plans.
func (c *Client) ListPlans(ctx context.Context) ([]Plan, error) {
	var out listResponse[Plan]
	if err := c.doJSON(ctx, http.MethodGet, c.orgPath("compute/plans"), nil, "", &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}
