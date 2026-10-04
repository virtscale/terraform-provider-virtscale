// Package client is a hand-written HTTP client for the VirtScale customer
// API (https://api.virtscale.nl/v1), used by the terraform-provider-virtscale
// provider and resources.
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
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
	retry      retryConfig
}

// retryConfig is the retry policy of a Client, modelled on the AWS SDK's
// standard retry mode and adapted to this API (see retryable and retryDelay).
// The hooks exist so tests can run the policy without waiting.
type retryConfig struct {
	maxAttempts   int           // in total, the first try included
	baseDelay     time.Duration // first backoff ceiling
	maxDelay      time.Duration // backoff ceiling cap
	maxRetryAfter time.Duration // a longer Retry-After is not waited for
	sleep         func(ctx context.Context, d time.Duration) error
	now           func() time.Time
	jitter        func(max time.Duration) time.Duration
}

const (
	defaultMaxAttempts   = 5
	defaultBaseDelay     = time.Second
	defaultMaxDelay      = 20 * time.Second
	defaultMaxRetryAfter = 60 * time.Second
)

func defaultRetryConfig() retryConfig {
	return retryConfig{
		maxAttempts:   defaultMaxAttempts,
		baseDelay:     defaultBaseDelay,
		maxDelay:      defaultMaxDelay,
		maxRetryAfter: defaultMaxRetryAfter,
		sleep:         sleepContext,
		now:           time.Now,
		jitter:        defaultJitter,
	}
}

// sleepContext waits d, or until ctx is done, whichever is first. It returns
// ctx's error when the context ended the wait.
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// defaultJitter returns a uniformly random duration in [0, max): "full
// jitter", so clients that were throttled together do not retry together.
func defaultJitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	d := time.Duration(rand.Float64() * float64(max))
	if d >= max {
		d = max - 1
	}
	return d
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
		retry:      defaultRetryConfig(),
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
	Errors    []FieldError
	// Attempts is how many times the request was sent before this error was
	// returned; more than one means the client retried.
	Attempts int
}

// FieldError is one entry of a problem body's errors[]: which request field
// the API refused and why.
type FieldError struct {
	Field   string
	Message string
}

func (e *APIError) Error() string {
	code := e.Code
	if code == "" {
		code = "error"
	}
	msg := fmt.Sprintf("virtscale API %d %s: %s", e.Status, code, e.Detail)
	if len(e.Errors) > 0 {
		parts := make([]string, len(e.Errors))
		for i, fe := range e.Errors {
			parts[i] = fe.Message
			if fe.Field != "" {
				parts[i] = fe.Field + ": " + fe.Message
			}
		}
		msg += ": " + strings.Join(parts, "; ")
	}
	if e.RequestID != "" {
		msg += fmt.Sprintf(" (request %s)", e.RequestID)
	}
	if e.Attempts > 1 {
		msg += fmt.Sprintf("; gave up after %d attempts", e.Attempts)
	}
	return msg
}

// organizationNotVisiblePrefix starts the detail of the 404 the API gives for
// an organization the key cannot see (errors.not_found("organization"), raised
// in customer_api/deps.py). It shares code not_found with a missing vm or
// public IP, so the detail is the only thing that tells them apart; if the
// wording ever changes, the check degrades to "any 404 is not found".
const organizationNotVisiblePrefix = "no organization matching that identifier"

func isOrganizationNotVisible(e *APIError) bool {
	return e.Status == http.StatusNotFound && strings.HasPrefix(e.Detail, organizationNotVisiblePrefix)
}

// IsNotFound reports whether err is an *APIError with Status 404 that says the
// resource is not there. A 404 about the organization itself (a wrong
// organization id, or a key issued for another organization) is not: it says
// nothing about the resource, and treating it as "gone" would drop a live
// resource from state.
func IsNotFound(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status == http.StatusNotFound && !isOrganizationNotVisible(apiErr)
	}
	return false
}

// IsOrganizationNotVisible reports whether err is the 404 the API gives for an
// organization the key cannot see.
func IsOrganizationNotVisible(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && isOrganizationNotVisible(apiErr)
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
		apiErr := &APIError{
			Status:    status,
			Code:      p.Code,
			Title:     p.Title,
			Detail:    p.Detail,
			RequestID: p.RequestID,
		}
		for _, fe := range p.Errors {
			apiErr.Errors = append(apiErr.Errors, FieldError{Field: fe.Field, Message: fe.Message})
		}
		return apiErr
	}

	detail := strings.TrimSpace(string(body))
	if len(detail) > maxTruncatedBodyLen {
		detail = detail[:maxTruncatedBodyLen] + "…"
	}
	return &APIError{Status: status, Detail: detail}
}

// newRequest builds one attempt's HTTP request. payload is the encoded body
// (nil for none) and is wrapped in a fresh reader every time, so a retry never
// sends a drained one. It sets auth, Accept, User-Agent and, when there is a
// body, Content-Type; idempotencyKey, when non-empty, goes out as the
// Idempotency-Key header.
func (c *Client) newRequest(ctx context.Context, method, fullURL string, payload []byte, idempotencyKey string) (*http.Request, error) {
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, reader)
	if err != nil {
		return nil, fmt.Errorf("client: build request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	return req, nil
}

// exchange is one completed HTTP round trip.
type exchange struct {
	status int
	header http.Header
	body   []byte
}

// roundTrip sends req and reads the whole response. A failure to get the
// status line or the body is a transport error.
func (c *Client) roundTrip(req *http.Request) (*exchange, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("client: read response body: %w", err)
	}
	return &exchange{status: resp.StatusCode, header: resp.Header, body: data}, nil
}

// doJSON issues a request and, on a 2xx response, decodes the JSON body into
// out (when out is non-nil). Any non-2xx response yields an *APIError.
//
// Throttling and transient failures are retried, like the AWS SDK's standard
// mode, within what the API makes safe (see retryable): at most
// retryConfig.maxAttempts sends in all, waiting what Retry-After says or an
// exponential backoff with full jitter, and never sleeping past ctx's
// deadline -- when the next wait would end after it, the last error is
// returned instead. The body is rebuilt, and the Idempotency-Key reused, for
// every attempt.
func (c *Client) doJSON(ctx context.Context, method, fullURL string, body any, idempotencyKey string, out any) error {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("client: encode request body: %w", err)
		}
		payload = b
	}

	var lastErr error
	for attempt := 1; ; attempt++ {
		req, err := c.newRequest(ctx, method, fullURL, payload, idempotencyKey)
		if err != nil {
			return err
		}

		ex, transportErr := c.roundTrip(req)

		var apiErr *APIError
		switch {
		case transportErr != nil:
			if ctx.Err() != nil {
				// The caller gave up (deadline or cancellation): the failure is
				// theirs. An earlier attempt's API error says more than the
				// cancellation does.
				if lastErr != nil {
					return lastErr
				}
				return transportErr
			}
			lastErr = transportErr
		case ex.status >= 200 && ex.status < 300:
			if out == nil || len(ex.body) == 0 {
				return nil
			}
			if err := json.Unmarshal(ex.body, out); err != nil {
				return fmt.Errorf("client: decode response body: %w", err)
			}
			return nil
		default:
			apiErr = newAPIError(ex.status, ex.body)
			apiErr.Attempts = attempt
			lastErr = apiErr
		}

		if attempt >= c.retry.maxAttempts || !retryable(method, idempotencyKey, ex, apiErr, transportErr) {
			return withAttempts(lastErr, attempt)
		}
		var header http.Header
		if ex != nil {
			header = ex.header
		}
		wait, ok := c.retryDelay(attempt, header)
		if !ok {
			return withAttempts(lastErr, attempt)
		}
		if deadline, has := ctx.Deadline(); has && c.retry.now().Add(wait).After(deadline) {
			return withAttempts(lastErr, attempt)
		}
		if err := c.retry.sleep(ctx, wait); err != nil {
			return withAttempts(lastErr, attempt)
		}
	}
}

// withAttempts says, for an error that is not an *APIError (those carry their
// own count), how many sends it took to give up.
func withAttempts(err error, attempts int) error {
	var apiErr *APIError
	if attempts <= 1 || errors.As(err, &apiErr) {
		return err
	}
	return fmt.Errorf("%w; gave up after %d attempts", err, attempts)
}

// repeatable reports whether sending the request twice is safe: a GET, a
// DELETE, or a POST that carries an Idempotency-Key (the API replays the first
// answer for the same key and body).
func repeatable(method, idempotencyKey string) bool {
	return method == http.MethodGet || method == http.MethodDelete ||
		(method == http.MethodPost && idempotencyKey != "")
}

// retryable decides whether a failed attempt may be repeated.
//
//   - 429 is retried for every method: the rate limiter refuses a request
//     before it runs.
//   - 502, 503, 504 and transport errors may have been applied, so only a
//     repeatable request is retried.
//   - A 503 whose code is not_ready says a feature flag is off; retrying
//     cannot help.
//   - Nothing else is retried: a 4xx is a decision about the request, and a
//     500 says nothing about whether it ran.
func retryable(method, idempotencyKey string, ex *exchange, apiErr *APIError, transportErr error) bool {
	if transportErr != nil {
		return repeatable(method, idempotencyKey) && !permanentTransportError(transportErr)
	}
	switch ex.status {
	case http.StatusTooManyRequests:
		return true
	case http.StatusBadGateway, http.StatusGatewayTimeout:
		return repeatable(method, idempotencyKey)
	case http.StatusServiceUnavailable:
		return repeatable(method, idempotencyKey) && (apiErr == nil || apiErr.Code != "not_ready")
	}
	return false
}

// permanentTransportError reports transport failures no retry can fix: a
// certificate that does not verify, or an endpoint that is not an http(s) URL.
func permanentTransportError(err error) bool {
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return true
	}
	var urlErr *url.Error
	return errors.As(err, &urlErr) && strings.HasPrefix(urlErr.Err.Error(), "unsupported protocol scheme")
}

// maxRetryAfterSeconds saturates absurd delay-seconds values before they can
// overflow a Duration.
const maxRetryAfterSeconds = 1 << 20

// parseRetryAfter reads a Retry-After value: delay-seconds (digits only) or an
// HTTP date (relative to now; a date in the past is no wait). ok is false for
// anything else, which the caller treats as absent.
func parseRetryAfter(value string, now time.Time) (d time.Duration, ok bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if strings.Trim(value, "0123456789") == "" {
		secs, err := strconv.ParseInt(value, 10, 64)
		if err != nil || secs > maxRetryAfterSeconds {
			secs = maxRetryAfterSeconds
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(value); err == nil {
		if d := t.Sub(now); d > 0 {
			return d, true
		}
		return 0, true
	}
	return 0, false
}

// retryDelay is how long to wait before retry number retryNumber (1 for the
// first retry): what Retry-After says when it is usable, otherwise a random
// draw from [0, min(maxDelay, baseDelay*2^(n-1))) -- exponential backoff with
// full jitter. ok is false when Retry-After asks for longer than maxRetryAfter:
// the server is not going to be ready within anything worth waiting for here.
func (c *Client) retryDelay(retryNumber int, header http.Header) (wait time.Duration, ok bool) {
	if header != nil {
		if d, parsed := parseRetryAfter(header.Get("Retry-After"), c.retry.now()); parsed {
			if d > c.retry.maxRetryAfter {
				return 0, false
			}
			return d, true
		}
	}

	ceiling := c.retry.baseDelay
	for i := 1; i < retryNumber && ceiling < c.retry.maxDelay; i++ {
		ceiling *= 2
	}
	if ceiling > c.retry.maxDelay {
		ceiling = c.retry.maxDelay
	}
	return c.retry.jitter(ceiling), true
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
// filters, returning an *APIError with Status 404 (code not_found, like the
// API's own answer for a missing public IP) when id is absent.
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
		Code:   "not_found",
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
