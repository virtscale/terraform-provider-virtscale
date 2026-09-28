// Package client is a hand-written HTTP client for the VirtScale customer
// API (https://api.virtscale.nl/v1), used by the terraform-provider-virtscale
// provider and resources.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultEndpoint is used when New is called with an empty endpoint.
const DefaultEndpoint = "https://api.virtscale.nl"

// defaultUserAgentBase is the product token sent in the User-Agent header
// when no WithUserAgentVersion option is supplied.
const defaultUserAgentBase = "terraform-provider-virtscale"

// Client is a VirtScale API client scoped to a single organization.
type Client struct {
	endpoint   string
	apiKey     string
	org        string
	httpClient *http.Client
	userAgent  string
}

// Option configures a Client constructed by New.
type Option func(*Client)

// WithHTTPClient overrides the *http.Client used for requests. A nil client
// is ignored.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// WithUserAgentVersion sets the version segment of the User-Agent header,
// e.g. "terraform-provider-virtscale/1.2.3".
func WithUserAgentVersion(version string) Option {
	return func(c *Client) {
		c.userAgent = defaultUserAgentBase + "/" + version
	}
}

// New builds a Client for the given org. endpoint defaults to
// DefaultEndpoint when empty; a trailing slash is tolerated. apiKey and org
// are required.
func New(endpoint, apiKey, org string, opts ...Option) (*Client, error) {
	if apiKey == "" {
		return nil, errors.New("client: apiKey must not be empty")
	}
	if org == "" {
		return nil, errors.New("client: org must not be empty")
	}

	ep := strings.TrimRight(endpoint, "/")
	if ep == "" {
		ep = DefaultEndpoint
	}

	c := &Client{
		endpoint:   ep,
		apiKey:     apiKey,
		org:        org,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		userAgent:  defaultUserAgentBase + "/dev",
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// APIError is returned for any non-2xx response. It is built from the
// RFC 9457 problem+json body when the response decodes as one, and from the
// raw (truncated) body otherwise.
type APIError struct {
	Status    int
	Code      string
	Title     string
	Detail    string
	RequestID string
}

func (e *APIError) Error() string {
	code := e.Code
	if code == "" {
		code = "error"
	}
	msg := fmt.Sprintf("virtscale API %d %s: %s", e.Status, code, e.Detail)
	if e.RequestID != "" {
		msg += fmt.Sprintf(" (request %s)", e.RequestID)
	}
	return msg
}

// IsNotFound reports whether err is an *APIError with Status 404.
func IsNotFound(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status == http.StatusNotFound
	}
	return false
}

type problemBody struct {
	Type      string              `json:"type"`
	Title     string              `json:"title"`
	Status    int                 `json:"status"`
	Detail    string              `json:"detail"`
	Instance  string              `json:"instance"`
	Code      string              `json:"code"`
	RequestID string              `json:"request_id"`
	Errors    []problemFieldError `json:"errors"`
}

type problemFieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

const maxTruncatedBodyLen = 200

func newAPIError(status int, body []byte) *APIError {
	var p problemBody
	if err := json.Unmarshal(body, &p); err == nil && (p.Type != "" || p.Title != "" || p.Code != "") {
		return &APIError{
			Status:    status,
			Code:      p.Code,
			Title:     p.Title,
			Detail:    p.Detail,
			RequestID: p.RequestID,
		}
	}

	detail := strings.TrimSpace(string(body))
	if len(detail) > maxTruncatedBodyLen {
		detail = detail[:maxTruncatedBodyLen] + "…"
	}
	return &APIError{Status: status, Detail: detail}
}

// do issues an HTTP request against the API, setting auth, Accept,
// User-Agent and (when body is non-nil) Content-Type headers. idempotencyKey,
// when non-empty, is sent as the Idempotency-Key header.
func (c *Client) do(ctx context.Context, method, fullURL string, body any, idempotencyKey string) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("client: encode request body: %w", err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, reader)
	if err != nil {
		return nil, fmt.Errorf("client: build request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}

	return c.httpClient.Do(req)
}

// doJSON issues a request and, on a 2xx response, decodes the JSON body into
// out (when out is non-nil). Any non-2xx response yields an *APIError.
func (c *Client) doJSON(ctx context.Context, method, fullURL string, body any, idempotencyKey string, out any) error {
	resp, err := c.do(ctx, method, fullURL, body, idempotencyKey)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("client: read response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return newAPIError(resp.StatusCode, data)
	}

	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("client: decode response body: %w", err)
	}
	return nil
}

// orgPath builds an absolute URL under /v1/orgs/{org}/ for the given
// relative path (which may already include a query string).
func (c *Client) orgPath(p string) string {
	return c.endpoint + "/v1/orgs/" + url.PathEscape(c.org) + "/" + p
}

// --- Types -----------------------------------------------------------------

// VmCreateRequest is the body of POST /v1/orgs/{org}/compute/vms.
//
// user_data and workload_type are intentionally not represented: the API
// refuses any non-null user_data and any workload_type other than the
// server-side default for a customer organization.
type VmCreateRequest struct {
	Name           string   `json:"name"`
	Image          string   `json:"image"`
	VCPUs          int      `json:"vcpus"`
	MemoryMB       int      `json:"memory_mb"`
	DiskGB         int      `json:"disk_gb"`
	StoragePool    string   `json:"storage_pool"`
	Region         string   `json:"region"`
	NetworkProfile string   `json:"network_profile"`
	SSHKeys        []string `json:"ssh_keys,omitempty"`
}

// ImageRef is the embedded image summary on a Vm.
type ImageRef struct {
	GlanceID string `json:"glance_id"`
	Name     string `json:"name"`
}

// Vm is the VmOut representation returned by the compute/vms endpoints.
type Vm struct {
	ID                string         `json:"id"`
	OrgID             string         `json:"org_id"`
	RegionID          string         `json:"region_id"`
	Name              string         `json:"name"`
	FQDN              *string        `json:"fqdn"`
	Image             ImageRef       `json:"image"`
	VCPUs             int            `json:"vcpus"`
	MemoryMB          int            `json:"memory_mb"`
	DiskGB            int            `json:"disk_gb"`
	StoragePoolID     string         `json:"storage_pool_id"`
	NetworkProfileID  string         `json:"network_profile_id"`
	State             string         `json:"state"`
	DesiredState      string         `json:"desired_state"`
	ErrorClass        *string        `json:"error_class"`
	ErrorDetail       *string        `json:"error_detail"`
	ObservedStatus    *string        `json:"observed_status"`
	ObservedAddresses map[string]any `json:"observed_addresses"`
	SSHKeyNames       []string       `json:"ssh_key_names"`
	Protected         bool           `json:"protected"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
}

// Operation is the OperationRef representation returned alongside async
// mutations and by GET .../operations/{id}.
type Operation struct {
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

// PublicIp is the PublicIpOut representation returned by the
// compute/public-ips endpoints.
type PublicIp struct {
	ID        string    `json:"id"`
	RegionID  string    `json:"region_id"`
	Address   *string   `json:"address"`
	VmID      *string   `json:"vm_id"`
	State     string    `json:"state"`
	Error     *string   `json:"error"`
	CreatedAt time.Time `json:"created_at"`
}

// Image is the ImageOut representation returned by GET compute/images.
type Image struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	OSDistro  *string `json:"os_distro"`
	OSVersion *string `json:"os_version"`
	MinDiskGB int     `json:"min_disk_gb"`
	MinRAMMB  int     `json:"min_ram_mb"`
}

type listResponse[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

type vmAccepted struct {
	Vm        Vm        `json:"vm"`
	Operation Operation `json:"operation"`
}

type publicIpAccepted struct {
	PublicIp  PublicIp  `json:"public_ip"`
	Operation Operation `json:"operation"`
}

// --- VM methods --------------------------------------------------------

// CreateVm sends POST /v1/orgs/{org}/compute/vms. When idempotencyKey is
// non-empty it is sent as the Idempotency-Key header.
func (c *Client) CreateVm(ctx context.Context, req VmCreateRequest, idempotencyKey string) (*Vm, *Operation, error) {
	var out vmAccepted
	if err := c.doJSON(ctx, http.MethodPost, c.orgPath("compute/vms"), req, idempotencyKey, &out); err != nil {
		return nil, nil, err
	}
	return &out.Vm, &out.Operation, nil
}

// GetVm sends GET /v1/orgs/{org}/compute/vms/{id}.
func (c *Client) GetVm(ctx context.Context, id string) (*Vm, error) {
	var out Vm
	if err := c.doJSON(ctx, http.MethodGet, c.orgPath("compute/vms/"+url.PathEscape(id)), nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteVm sends DELETE /v1/orgs/{org}/compute/vms/{id}.
func (c *Client) DeleteVm(ctx context.Context, id string) (*Vm, *Operation, error) {
	var out vmAccepted
	if err := c.doJSON(ctx, http.MethodDelete, c.orgPath("compute/vms/"+url.PathEscape(id)), nil, "", &out); err != nil {
		return nil, nil, err
	}
	return &out.Vm, &out.Operation, nil
}

// ListVms sends GET /v1/orgs/{org}/compute/vms.
func (c *Client) ListVms(ctx context.Context) ([]Vm, error) {
	var out listResponse[Vm]
	if err := c.doJSON(ctx, http.MethodGet, c.orgPath("compute/vms"), nil, "", &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// GetOperation sends GET /v1/orgs/{org}/operations/{id}.
func (c *Client) GetOperation(ctx context.Context, id string) (*Operation, error) {
	var out Operation
	if err := c.doJSON(ctx, http.MethodGet, c.orgPath("operations/"+url.PathEscape(id)), nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListImages sends GET /v1/orgs/{org}/compute/images?region=<region>. region
// is required by the API (the catalogue is per region).
func (c *Client) ListImages(ctx context.Context, region string) ([]Image, error) {
	var out listResponse[Image]
	p := c.orgPath("compute/images") + "?region=" + url.QueryEscape(region)
	if err := c.doJSON(ctx, http.MethodGet, p, nil, "", &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// --- Public IP methods ---------------------------------------------------

// AllocatePublicIp sends POST /v1/orgs/{org}/compute/public-ips with body
// {"region_id": region}.
func (c *Client) AllocatePublicIp(ctx context.Context, region string) (*PublicIp, *Operation, error) {
	body := struct {
		RegionID string `json:"region_id"`
	}{RegionID: region}

	var out publicIpAccepted
	if err := c.doJSON(ctx, http.MethodPost, c.orgPath("compute/public-ips"), body, "", &out); err != nil {
		return nil, nil, err
	}
	return &out.PublicIp, &out.Operation, nil
}

// ListPublicIps sends GET /v1/orgs/{org}/compute/public-ips.
func (c *Client) ListPublicIps(ctx context.Context) ([]PublicIp, error) {
	var out listResponse[PublicIp]
	if err := c.doJSON(ctx, http.MethodGet, c.orgPath("compute/public-ips"), nil, "", &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// GetPublicIp has no single-item GET counterpart in the API; it lists and
// filters, returning an *APIError with Status 404 when id is absent.
func (c *Client) GetPublicIp(ctx context.Context, id string) (*PublicIp, error) {
	ips, err := c.ListPublicIps(ctx)
	if err != nil {
		return nil, err
	}
	for i := range ips {
		if ips[i].ID == id {
			return &ips[i], nil
		}
	}
	return nil, &APIError{
		Status: http.StatusNotFound,
		Code:   "public_ip_not_found",
		Detail: fmt.Sprintf("public ip %s not found", id),
	}
}

// AssociatePublicIp sends POST .../public-ips/{id}/associate with body
// {"vm_id": vmID}.
func (c *Client) AssociatePublicIp(ctx context.Context, id, vmID string) (*PublicIp, *Operation, error) {
	body := struct {
		VmID string `json:"vm_id"`
	}{VmID: vmID}

	var out publicIpAccepted
	if err := c.doJSON(ctx, http.MethodPost, c.orgPath("compute/public-ips/"+url.PathEscape(id)+"/associate"), body, "", &out); err != nil {
		return nil, nil, err
	}
	return &out.PublicIp, &out.Operation, nil
}

// DisassociatePublicIp sends POST .../public-ips/{id}/disassociate with no
// body.
func (c *Client) DisassociatePublicIp(ctx context.Context, id string) (*PublicIp, *Operation, error) {
	var out publicIpAccepted
	if err := c.doJSON(ctx, http.MethodPost, c.orgPath("compute/public-ips/"+url.PathEscape(id)+"/disassociate"), nil, "", &out); err != nil {
		return nil, nil, err
	}
	return &out.PublicIp, &out.Operation, nil
}

// ReleasePublicIp sends DELETE .../public-ips/{id}.
func (c *Client) ReleasePublicIp(ctx context.Context, id string) (*PublicIp, *Operation, error) {
	var out publicIpAccepted
	if err := c.doJSON(ctx, http.MethodDelete, c.orgPath("compute/public-ips/"+url.PathEscape(id)), nil, "", &out); err != nil {
		return nil, nil, err
	}
	return &out.PublicIp, &out.Operation, nil
}

// --- Waiters ---------------------------------------------------------------

// WaitVm polls GetVm every pollEvery until done reports true or ctx is
// done. A 404 is passed to done as a nil *Vm so callers can treat it as
// deletion; if done(nil) then reports true, WaitVm returns (nil, nil). On
// deadline it returns the last observed *Vm (possibly nil) and an error that
// wraps ctx.Err() and mentions the last observed state.
func (c *Client) WaitVm(ctx context.Context, id string, pollEvery time.Duration, done func(*Vm) (bool, error)) (*Vm, error) {
	var last *Vm
	for {
		vm, err := c.GetVm(ctx, id)
		switch {
		case err != nil && IsNotFound(err):
			ok, derr := done(nil)
			if derr != nil {
				return last, derr
			}
			if ok {
				return nil, nil
			}
		case err != nil:
			if ctx.Err() != nil {
				return last, waitVmTimeoutError(id, ctx.Err(), last)
			}
			return last, err
		default:
			last = vm
			ok, derr := done(vm)
			if derr != nil {
				return vm, derr
			}
			if ok {
				return vm, nil
			}
		}

		timer := time.NewTimer(pollEvery)
		select {
		case <-ctx.Done():
			timer.Stop()
			return last, waitVmTimeoutError(id, ctx.Err(), last)
		case <-timer.C:
		}
	}
}

func waitVmTimeoutError(id string, cerr error, last *Vm) error {
	state := "unknown"
	if last != nil {
		state = last.State
	}
	return fmt.Errorf("waiting for vm %s: %w (last state %s)", id, cerr, state)
}

// WaitPublicIp polls GetPublicIp every pollEvery until done reports true or
// ctx is done, mirroring WaitVm.
func (c *Client) WaitPublicIp(ctx context.Context, id string, pollEvery time.Duration, done func(*PublicIp) (bool, error)) (*PublicIp, error) {
	var last *PublicIp
	for {
		ip, err := c.GetPublicIp(ctx, id)
		switch {
		case err != nil && IsNotFound(err):
			ok, derr := done(nil)
			if derr != nil {
				return last, derr
			}
			if ok {
				return nil, nil
			}
		case err != nil:
			if ctx.Err() != nil {
				return last, waitPublicIpTimeoutError(id, ctx.Err(), last)
			}
			return last, err
		default:
			last = ip
			ok, derr := done(ip)
			if derr != nil {
				return ip, derr
			}
			if ok {
				return ip, nil
			}
		}

		timer := time.NewTimer(pollEvery)
		select {
		case <-ctx.Done():
			timer.Stop()
			return last, waitPublicIpTimeoutError(id, ctx.Err(), last)
		case <-timer.C:
		}
	}
}

func waitPublicIpTimeoutError(id string, cerr error, last *PublicIp) error {
	state := "unknown"
	if last != nil {
		state = last.State
	}
	return fmt.Errorf("waiting for public ip %s: %w (last state %s)", id, cerr, state)
}
