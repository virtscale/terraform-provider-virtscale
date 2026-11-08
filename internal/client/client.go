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
	"regexp"
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
	jitter        func(ceiling time.Duration) time.Duration
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

// defaultJitter returns a uniformly random duration in [0, ceiling): "full
// jitter", so clients that were throttled together do not retry together.
func defaultJitter(ceiling time.Duration) time.Duration {
	if ceiling <= 0 {
		return 0
	}
	d := time.Duration(rand.Float64() * float64(ceiling))
	if d >= ceiling {
		d = ceiling - 1
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
	// MayHaveBeenApplied is set when the request carried an Idempotency-Key (it
	// creates something) and the client gave up on an answer that leaves open that
	// the API processed it anyway: a gateway error, a duplicate still being
	// processed, a lost connection. Error() then says how to find what it may
	// have made.
	MayHaveBeenApplied bool

	// retryable is what retryable() said about the attempt that produced this error: the suffix "gave up after N
	// attempts" is only true when the client stopped at its limits (attempts, Retry-After, deadline) and not because the
	// answer was final.
	retryable bool

	// recovery is the sentence Error() appends for MayHaveBeenApplied, set by
	// giveUp from the request method (lostCreateHint for a create, otherwise the
	// change's own hint). Empty on a hand-built APIError, which keeps the create
	// hint.
	recovery string
}

// lostCreateHint is what the error of a keyed create that gave up adds: the
// first request may have been processed although no answer came back, so the
// resource may exist, and a new apply (with a new Idempotency-Key) would meet
// it as name_taken.
const lostCreateHint = "the first request may still have created the resource: " +
	"if the next apply reports name_taken, find it in the panel or with the API " +
	"and bring it under management with tofu import"

// lostPatchHint is what the error of a keyed PATCH or DELETE that gave up adds
// instead: nothing was created, so the create hint would send the user looking
// for a resource that does not exist. Another plan reads the current state,
// which is what the change may have set.
const lostPatchHint = "the first request may still have been applied: " +
	"run another plan to see the current state"

// hintKey carries the recovery sentence a particular create wants instead of the generic one.
type hintKey struct{}

// withLostCreateHint makes the error of a keyed create that gives up after a lost answer say hint instead of
// lostCreateHint: the generic one speaks of name_taken and tofu import, which is wrong for a resource that has no name
// to collide on (an access key) or another code for it (a bucket).
func withLostCreateHint(ctx context.Context, hint string) context.Context {
	return context.WithValue(ctx, hintKey{}, hint)
}

// createRecoveryHint is the sentence for a keyed create that may have made its resource: the create's own, or the generic one.
func createRecoveryHint(createHint string) string {
	if createHint != "" {
		return createHint
	}
	return lostCreateHint
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
	// Only an error the client would have retried "gave up": any other answer (a 422, a 500, a 502 to a request that cannot
	// be repeated) is just the answer, however many throttled attempts came before it.
	if e.Attempts > 1 && e.retryable {
		msg += fmt.Sprintf("; gave up after %d attempts", e.Attempts)
	}
	if e.MayHaveBeenApplied {
		hint := e.recovery
		if hint == "" {
			hint = lostCreateHint
		}
		msg += "; " + hint
	}
	if hint := e.hint(); hint != "" {
		msg += "; " + hint
	}
	return msg
}

// permissionNameRE finds the permission a 403 permission_denied names: "this
// credential does not carry the '<permission>' permission" (customer_api/deps.py).
var permissionNameRE = regexp.MustCompile(`'([A-Za-z0-9_.]+)' permission`)

// hint says what to change for the failures a user can fix themselves: the
// API's own message stays in front of it, this only adds the next step.
func (e *APIError) hint() string {
	switch {
	case e.Status == http.StatusUnauthorized:
		// One answer for a wrong, revoked or expired key: the API does not say which.
		return "check VIRTSCALE_API_KEY: the key may be wrong, revoked or expired"
	case e.Status == http.StatusForbidden && e.Code == "permission_denied":
		return scopeHint(e.Detail)
	case e.Status == http.StatusForbidden && e.Code == "source_address_not_allowed":
		return "the key's network limits do not include this address; " +
			"run Terraform from an allowed address or widen the key's network limits"
	case e.Status == http.StatusConflict && e.Code == "vm_protected":
		// The provider never lifts a protection for the user: the way out is the attribute that set it.
		return "set disable_api_termination = false and apply first"
	case e.Status == http.StatusPreconditionFailed && e.Code == "generation_mismatch":
		// The API says to retry with the new ETag; the provider read the resource just before, so
		// this is a race with another change of the same resource, and what the user can do is plan again.
		return "it was changed by something else while this change was being made; run apply again"
	case isOrganizationNotVisible(e):
		return "check VIRTSCALE_ORGANIZATION: the organization id may be wrong, " +
			"or VIRTSCALE_API_KEY belongs to another organization"
	case e.Status == http.StatusNotFound && (e.Code == "" || (e.Code == "not_found" && e.Detail == unknownRouteDetail)):
		// Not a problem answer of the API, or the answer of its router for a path it does not have: a proxy's or
		// another service's page, or an endpoint with a wrong host or path. A 404 about a resource has a code and a sentence.
		return "check VIRTSCALE_ENDPOINT (or the provider's endpoint): the request did not reach the VirtScale API, " +
			"whose address is " + DefaultEndpoint + "; unset it to use that"
	case e.Code == "ssh_key_not_found":
		// The API tells a client to import the key with a POST; a Terraform user makes it a resource.
		return "create the key with a virtscale_ssh_key resource and name it in ssh_keys"
	}
	return ""
}

// scopeHint maps the permission a 403 names to the key scope that grants it
// (permissions.py SCOPE_PERMISSIONS): compute.* and operation.* come with
// compute:write, edge.* and network.* with network:write, and the read halves
// (permissions ending .read or .list) with the :read scope as well. Any other
// family, or a detail it cannot read, only sends the user to the key's scopes.
func scopeHint(detail string) string {
	const generic = "check the scopes of the API key (VIRTSCALE_API_KEY)"

	m := permissionNameRE.FindStringSubmatch(detail)
	if m == nil {
		return generic
	}
	permission := m[1]

	var family string
	switch {
	case strings.HasPrefix(permission, "compute."), strings.HasPrefix(permission, "operation."):
		family = "compute"
	case strings.HasPrefix(permission, "edge."), strings.HasPrefix(permission, "network."):
		family = "network"
	case strings.HasPrefix(permission, "storage."):
		family = "storage"
	case strings.HasPrefix(permission, "project."):
		// compute:read and compute:write carry project.list and project.read; creating, renaming and deleting a
		// project is the legacy write scope's (and the roles').
		if strings.HasSuffix(permission, ".read") || strings.HasSuffix(permission, ".list") {
			return "the API key needs the 'compute:read' or 'compute:write' scope"
		}
		return "the API key needs the 'write' scope (compute:write does not create, rename or delete projects)"
	default:
		return generic
	}
	if strings.HasSuffix(permission, ".read") || strings.HasSuffix(permission, ".list") {
		return fmt.Sprintf("the API key needs the '%s:read' or '%s:write' scope", family, family)
	}
	return fmt.Sprintf("the API key needs the '%s:write' scope", family)
}

// organizationNotVisiblePrefix starts the detail of the 404 the API gives for
// an organization the key cannot see (errors.not_found("organization"), raised
// in customer_api/deps.py). It shares code not_found with every other resource
// 404, so the detail is the only thing that tells them apart. It only decides
// which hint an error gets: IsNotFound has its own, stricter rule.
const organizationNotVisiblePrefix = "no organization matching that identifier"

// unknownRouteDetail is the detail of the not_found the API's router gives a path it does not have.
const unknownRouteDetail = "Not Found"

func isOrganizationNotVisible(e *APIError) bool {
	return e.Status == http.StatusNotFound && strings.HasPrefix(e.Detail, organizationNotVisiblePrefix)
}

// The resources a call can ask about, as the API's not_found helper names them
// in the detail of a 404 (errors.py not_found(resource)).
const (
	ResourceVm       = "vm"
	ResourcePublicIp = "public_ip"
	ResourceSshKey   = "ssh_key"
)

// notFoundDetail is the sentence the API's not_found helper gives a 404: code
// not_found and "no <resource> matching that identifier is visible to this
// organization". The same helper words the organization, the compute service
// and a region, and an unknown route answers not_found with the detail "Not
// Found".
func notFoundDetail(resource string) string {
	return "no " + resource + " matching that identifier is visible to this organization"
}

// IsNotFound reports whether err says that the resource the call asked about is
// gone: an *APIError with status 404, code not_found and exactly the helper's
// sentence for that resource (ResourceVm, ResourcePublicIp, ResourceSshKey).
//
// Any other 404 says nothing about the resource: an unknown route (a mistyped
// endpoint), a proxy's page, the sentence for the organization, the compute
// service, a region or another kind of resource. Reading one of those as "gone"
// would drop a live resource from state, or end a destroy that nobody
// confirmed, so it stays an error. If the API ever words the sentence
// differently, this fails safe: a resource that is really gone reads as an
// error until the client is updated, never the other way round.
func IsNotFound(err error, resource string) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) &&
		apiErr.Status == http.StatusNotFound &&
		apiErr.Code == "not_found" &&
		apiErr.Detail == notFoundDetail(resource)
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

// redact replaces every occurrence of the API key in text that came from the
// response, so that an upstream that echoes the request (a debug page of a proxy
// that prints the Authorization header) cannot make the key appear in an error.
func (c *Client) redact(s string) string {
	if c.apiKey == "" {
		return s
	}
	return strings.ReplaceAll(s, c.apiKey, "[redacted]")
}

// newAPIError builds the error of a non-2xx answer. Everything in it that came
// from the body has the API key replaced first; a raw body is cut short after
// that, so that a key which straddles the cut cannot leave a piece of itself.
func (c *Client) newAPIError(status int, body []byte) *APIError {
	var p problemBody
	if err := json.Unmarshal(body, &p); err == nil && (p.Type != "" || p.Title != "" || p.Code != "") {
		apiErr := &APIError{
			Status:    status,
			Code:      c.redact(p.Code),
			Title:     c.redact(p.Title),
			Detail:    c.redact(p.Detail),
			RequestID: c.redact(p.RequestID),
		}
		for _, fe := range p.Errors {
			apiErr.Errors = append(apiErr.Errors, FieldError{Field: c.redact(fe.Field), Message: c.redact(fe.Message)})
		}
		return apiErr
	}

	detail := strings.TrimSpace(c.redact(string(body)))
	if len(detail) > maxTruncatedBodyLen {
		detail = detail[:maxTruncatedBodyLen] + "…"
	}
	return &APIError{Status: status, Detail: detail}
}

// newRequest builds one attempt's HTTP request. payload is the encoded body
// (nil for none) and is wrapped in a fresh reader every time, so a retry never
// sends a drained one. It sets auth, Accept, User-Agent and, when there is a
// body, Content-Type; idempotencyKey, when non-empty, goes out as the
// Idempotency-Key header. headers, when non-nil, are added to every attempt
// (used for preconditions such as If-Match).
func (c *Client) newRequest(ctx context.Context, method, fullURL string, payload []byte, idempotencyKey string, headers http.Header) (*http.Request, error) {
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
	for name, values := range headers {
		for _, v := range values {
			req.Header.Add(name, v)
		}
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
// out (when out is non-nil). Any non-2xx response yields an *APIError. It is
// doJSONWithHeaders without extra headers.
func (c *Client) doJSON(ctx context.Context, method, fullURL string, body any, idempotencyKey string, out any) error {
	return c.doJSONWithHeaders(ctx, method, fullURL, body, idempotencyKey, nil, out)
}

// doJSONWithHeaders is doJSON with headers added to every attempt (for
// preconditions such as If-Match).
//
// Throttling and transient failures are retried, like the AWS SDK's standard
// mode, within what the API makes safe (see retryable): at most
// retryConfig.maxAttempts sends in all, waiting what Retry-After says or an
// exponential backoff with full jitter, and never sleeping past ctx's
// deadline -- when the next wait would end after it, the last error is
// returned instead. The body is rebuilt, and the Idempotency-Key and headers
// reused, for every attempt.
func (c *Client) doJSONWithHeaders(ctx context.Context, method, fullURL string, body any, idempotencyKey string, headers http.Header, out any) error {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("client: encode request body: %w", err)
		}
		payload = b
	}

	createHint, _ := ctx.Value(hintKey{}).(string)
	var lastErr error
	for attempt := 1; ; attempt++ {
		req, err := c.newRequest(ctx, method, fullURL, payload, idempotencyKey, headers)
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
					return giveUp(lastErr, 0, idempotencyKey, method, createHint)
				}
				return giveUp(transportErr, 0, idempotencyKey, method, createHint)
			}
			lastErr = transportErr
		case ex.status >= 200 && ex.status < 300:
			if out == nil || len(ex.body) == 0 {
				return nil
			}
			if err := json.Unmarshal(ex.body, out); err != nil {
				err = fmt.Errorf("client: decode response body: %w", err)
				if method == http.MethodPost && idempotencyKey != "" {
					// The API answered 2xx, so it made the resource, but nothing here knows what: say how to find it.
					err = fmt.Errorf("%w; %s", err, createRecoveryHint(createHint))
				}
				return err
			}
			return nil
		default:
			apiErr = c.newAPIError(ex.status, ex.body)
			apiErr.Attempts = attempt
			apiErr.retryable = retryable(method, idempotencyKey, ex, apiErr, nil)
			lastErr = apiErr
		}

		if attempt >= c.retry.maxAttempts || !retryable(method, idempotencyKey, ex, apiErr, transportErr) {
			return giveUp(lastErr, attempt, idempotencyKey, method, createHint)
		}
		var header http.Header
		if ex != nil {
			header = ex.header
		}
		wait, ok := c.retryDelay(attempt, header)
		if !ok {
			return giveUp(lastErr, attempt, idempotencyKey, method, createHint)
		}
		if deadline, has := ctx.Deadline(); has && c.retry.now().Add(wait).After(deadline) {
			return giveUp(lastErr, attempt, idempotencyKey, method, createHint)
		}
		if err := c.retry.sleep(ctx, wait); err != nil {
			return giveUp(lastErr, attempt, idempotencyKey, method, createHint)
		}
	}
}

// giveUp is the error doJSON returns when it stops with err, the last failure:
// the error as it came, saying (for an error that is not an *APIError, which
// carries its own count) how many sends it took, and, for a request that
// carried an Idempotency-Key, how to recover when the failure leaves open that
// the API processed it anyway. The recovery sentence depends on the method: a
// POST creates a resource (see lostCreateHint, or the hint of that create), while a
// PATCH, a PUT or a DELETE changes or removes one (see lostPatchHint).
func giveUp(err error, attempts int, idempotencyKey, method, createHint string) error {
	lost := idempotencyKey != "" && mayHaveBeenApplied(err)
	// Only a POST creates something to look for; a keyed PATCH, PUT or DELETE changed or removed a resource that is in
	// the state already, so another plan shows where it stands.
	hint := lostPatchHint
	if method == http.MethodPost {
		hint = createRecoveryHint(createHint)
	}

	var apiErr *APIError
	if errors.As(err, &apiErr) {
		apiErr.MayHaveBeenApplied = lost
		if lost {
			apiErr.recovery = hint
		}
		return err
	}
	if attempts > 1 {
		err = fmt.Errorf("%w; gave up after %d attempts", err, attempts)
	}
	if lost {
		err = fmt.Errorf("%w; %s", err, hint)
	}
	return err
}

// mayHaveBeenApplied reports whether a failed attempt leaves open that the API
// processed the request all the same: a transport error (the answer may have
// been lost; a certificate that does not verify, or an endpoint that is no
// http(s) URL, never got there), a 502, 503 or 504 (not the 503 not_ready of a
// feature that is switched off), or a 409 idempotency_in_progress (the first
// request is still being processed). A 429 was refused before it ran, and any
// other answer is a decision.
func mayHaveBeenApplied(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case http.StatusBadGateway, http.StatusGatewayTimeout:
			return true
		case http.StatusServiceUnavailable:
			return apiErr.Code != "not_ready"
		case http.StatusConflict:
			return apiErr.Code == "idempotency_in_progress"
		}
		return false
	}
	return !permanentTransportError(err)
}

// repeatable reports whether sending the request twice is safe: a GET, a
// DELETE, or a POST, PATCH or PUT that carries an Idempotency-Key (the API replays
// the first answer for the same key and body).
func repeatable(method, idempotencyKey string) bool {
	return method == http.MethodGet || method == http.MethodDelete ||
		((method == http.MethodPost || method == http.MethodPatch || method == http.MethodPut) && idempotencyKey != "")
}

// retryable decides whether a failed attempt may be repeated.
//
//   - 429 is retried for every method: the rate limiter refuses a request
//     before it runs.
//   - 502, 503, 504 and transport errors may have been applied, so only a
//     repeatable request (a GET, a DELETE, or a keyed POST, PATCH or PUT) is
//     retried.
//   - A 503 whose code is not_ready says a feature flag is off; retrying
//     cannot help.
//   - A 409 idempotency_in_progress on a request that carries an
//     Idempotency-Key says the first attempt with that key is still being
//     processed; the key replays its result once the API has stored it.
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
	case http.StatusConflict:
		return idempotencyKey != "" && apiErr != nil && apiErr.Code == "idempotency_in_progress"
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
	return errors.As(err, &urlErr) && urlErr.Err != nil &&
		strings.HasPrefix(urlErr.Err.Error(), "unsupported protocol scheme")
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

// VmCreateRequest is the body of POST /v1/orgs/{org}/compute/instances.
//
// Name an InstanceType, or give VCPUs, MemoryMB, DiskGB and a StoragePool, not both: the API refuses a mix with 422
// invalid_spec. Every field left at its zero value is left out of the body, which is what asks the API for its default:
// the type's sizes, the type's storage pool (a type is created on its own pool unless one is named) and the first
// network profile the organization may use in the region. An empty string would not do, since the API reads it as a pool
// or profile that was named. No valid size is zero.
//
// Hostname, when set, is the lower-case FQDN the instance should carry; a nil
// Hostname is left out of the body entirely, so the instance has no hostname.
//
// UserData is the cloud-init text as standard base64 (at most 32 KiB decoded); the API keeps only its presence and never
// returns it. WorkloadType is "customer" (the API's default) or "infra", which only an internal organization may ask
// for. Both are left out of the body when empty. disk_type is not represented: the provider names a storage pool.
type VmCreateRequest struct {
	Name           string   `json:"name"`
	Image          string   `json:"image"`
	InstanceType   string   `json:"instance_type,omitempty"`
	VCPUs          int      `json:"vcpus,omitempty"`
	MemoryMB       int      `json:"memory_mb,omitempty"`
	DiskGB         int      `json:"disk_gb,omitempty"`
	StoragePool    string   `json:"storage_pool,omitempty"`
	Region         string   `json:"region"`
	NetworkProfile string   `json:"network_profile,omitempty"`
	Hostname       *string  `json:"hostname,omitempty"`
	SSHKeys        []string `json:"ssh_keys,omitempty"`
	UserData       string   `json:"user_data,omitempty"`
	WorkloadType   string   `json:"workload_type,omitempty"`
}

// ImageRef is the embedded image summary on a Vm.
type ImageRef struct {
	ImageID string `json:"image_id"`
	Name    string `json:"name"`
}

// Vm is the VmOut representation returned by the compute/instances endpoints.
//
//   - Hostname is the lower-case FQDN the customer gave the instance. It is
//     unique per organization among live instances, and is only a handle: it
//     creates no DNS record.
//   - InstanceType is the catalogue instance type the instance was created from
//     (null when the API reports none).
//   - Status is the VirtScale status word.
//   - PrivateIP is the instance's private IPv4, once it has been observed.
//   - Addresses are the instance's addresses, once they have been observed.
//   - Generation is the number the PATCH precondition (If-Match) uses.
//
// The API still publishes observed_status and observed_addresses, but they are
// deprecated cloud vocabulary: this struct deliberately does not decode them.
type Vm struct {
	ID               string    `json:"id"`
	OrgID            string    `json:"org_id"`
	RegionID         string    `json:"region_id"`
	Name             string    `json:"name"`
	FQDN             *string   `json:"fqdn"`
	Hostname         *string   `json:"hostname"`
	InstanceType     *string   `json:"instance_type"`
	Status           string    `json:"status"`
	PrivateIP        *string   `json:"private_ip"`
	Addresses        []string  `json:"addresses"`
	Generation       int64     `json:"generation"`
	Image            ImageRef  `json:"image"`
	VCPUs            int       `json:"vcpus"`
	MemoryMB         int       `json:"memory_mb"`
	DiskGB           int       `json:"disk_gb"`
	StoragePoolID    string    `json:"storage_pool_id"`
	NetworkProfileID string    `json:"network_profile_id"`
	State            string    `json:"state"`
	DesiredState     string    `json:"desired_state"`
	ErrorClass       *string   `json:"error_class"`
	ErrorDetail      *string   `json:"error_detail"`
	SSHKeyNames      []string  `json:"ssh_key_names"`
	WorkloadType     string    `json:"workload_type"`
	UserDataPresent  bool      `json:"user_data_present"`
	Protected        bool      `json:"protected"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
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

// SshKey is the SshKeyOut representation returned by the compute/ssh-keys
// endpoints: a public key imported into the organization. PublicKey is the
// canonical form, "<type> <base64>" with the comment dropped; Fingerprint is
// "SHA256:" and the unpadded base64 of the SHA-256 of the key, as ssh-keygen -lf
// prints it.
type SshKey struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	KeyType     string    `json:"key_type"`
	Fingerprint string    `json:"fingerprint"`
	PublicKey   string    `json:"public_key"`
	CreatedAt   time.Time `json:"created_at"`
}

// SshKeyCreateRequest is the body of POST /v1/orgs/{org}/compute/ssh-keys (the
// API's SshKeyImportRequest). PublicKey is one OpenSSH line, "<type> <base64>
// [comment]"; the API refuses anything else.
type SshKeyCreateRequest struct {
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
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

// CreateVm sends POST /v1/orgs/{org}/compute/instances. When idempotencyKey is
// non-empty it is sent as the Idempotency-Key header.
func (c *Client) CreateVm(ctx context.Context, req VmCreateRequest, idempotencyKey string) (*Vm, *Operation, error) {
	var out vmAccepted
	if err := c.doJSON(ctx, http.MethodPost, c.orgPath("compute/instances"), req, idempotencyKey, &out); err != nil {
		return nil, nil, err
	}
	return &out.Vm, &out.Operation, nil
}

// GetVm sends GET /v1/orgs/{org}/compute/instances/{id}.
func (c *Client) GetVm(ctx context.Context, id string) (*Vm, error) {
	var out Vm
	if err := c.doJSON(ctx, http.MethodGet, c.orgPath("compute/instances/"+url.PathEscape(id)), nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteVm sends DELETE /v1/orgs/{org}/compute/instances/{id}.
func (c *Client) DeleteVm(ctx context.Context, id string) (*Vm, *Operation, error) {
	var out vmAccepted
	if err := c.doJSON(ctx, http.MethodDelete, c.orgPath("compute/instances/"+url.PathEscape(id)), nil, "", &out); err != nil {
		return nil, nil, err
	}
	return &out.Vm, &out.Operation, nil
}

// ListVms sends GET /v1/orgs/{org}/compute/instances.
func (c *Client) ListVms(ctx context.Context) ([]Vm, error) {
	var out listResponse[Vm]
	if err := c.doJSON(ctx, http.MethodGet, c.orgPath("compute/instances"), nil, "", &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// UpdateVmHostname sends PATCH /v1/orgs/{org}/compute/instances/{id} with body
// {"hostname": hostname} (null when hostname is nil, which clears it), and
// answers 200 with the updated instance. The generation travels as the If-Match
// precondition, so a change built on a stale read is refused instead of
// overwriting a newer one. When idempotencyKey is non-empty it is sent as the
// Idempotency-Key header, which makes the PATCH safe to repeat.
func (c *Client) UpdateVmHostname(ctx context.Context, id string, generation int64, hostname *string, idempotencyKey string) (*Vm, error) {
	body := struct {
		Hostname *string `json:"hostname"`
	}{Hostname: hostname}

	headers := http.Header{}
	headers.Set("If-Match", `"`+strconv.FormatInt(generation, 10)+`"`)

	var out Vm
	if err := c.doJSONWithHeaders(ctx, http.MethodPatch, c.orgPath("compute/instances/"+url.PathEscape(id)), body, idempotencyKey, headers, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// resizeLostHint is what a keyed resize that gave up after a lost answer says: nothing was created, but the resize may
// be running.
const resizeLostHint = "the first request may still have started the resize: run another plan to see the size the instance has now"

// ResizeVm sends POST /v1/orgs/{org}/compute/instances/{id}/resize with {"vcpus","memory_mb"} and answers 202 with the
// instance and the operation that converges it. The instance restarts; a resize that cannot be completed is reverted by
// the platform. The Idempotency-Key makes the request safe to send again after a lost answer.
func (c *Client) ResizeVm(ctx context.Context, id string, vcpus, memoryMB int, idempotencyKey string) (*Vm, *Operation, error) {
	body := struct {
		VCPUs    int `json:"vcpus"`
		MemoryMB int `json:"memory_mb"`
	}{vcpus, memoryMB}

	var out vmAccepted
	ctx = withLostCreateHint(ctx, resizeLostHint)
	if err := c.doJSON(ctx, http.MethodPost, c.orgPath("compute/instances/"+url.PathEscape(id)+"/resize"), body, idempotencyKey, &out); err != nil {
		return nil, nil, err
	}
	return &out.Vm, &out.Operation, nil
}

// SetVmProtection sends PUT /v1/orgs/{org}/compute/instances/{id}/protection with {"protected": protected}: while it is
// on, deleting the instance is refused with 409 vm_protected. It changes nothing about the instance itself and answers
// 200 with it. Setting a flag is idempotent, and the Idempotency-Key lets the request be sent again after a lost answer.
func (c *Client) SetVmProtection(ctx context.Context, id string, protected bool, idempotencyKey string) (*Vm, error) {
	body := struct {
		Protected bool `json:"protected"`
	}{protected}

	var out Vm
	if err := c.doJSON(ctx, http.MethodPut, c.orgPath("compute/instances/"+url.PathEscape(id)+"/protection"), body, idempotencyKey, &out); err != nil {
		return nil, err
	}
	return &out, nil
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
// filters, returning an *APIError with Status 404 (code not_found and the
// helper's public_ip sentence, like the API's own answer for a missing public
// IP) when id is absent.
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
		Detail: notFoundDetail(ResourcePublicIp),
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

// --- SSH key methods -----------------------------------------------------

// CreateSshKey sends POST /v1/orgs/{org}/compute/ssh-keys, which imports the
// key and answers 201 with it. When idempotencyKey is non-empty it is sent as
// the Idempotency-Key header, so a request repeated after a lost answer gets the
// first answer back instead of importing twice.
func (c *Client) CreateSshKey(ctx context.Context, req SshKeyCreateRequest, idempotencyKey string) (*SshKey, error) {
	var out SshKey
	if err := c.doJSON(ctx, http.MethodPost, c.orgPath("compute/ssh-keys"), req, idempotencyKey, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetSshKey sends GET /v1/orgs/{org}/compute/ssh-keys/{id}.
func (c *Client) GetSshKey(ctx context.Context, id string) (*SshKey, error) {
	var out SshKey
	if err := c.doJSON(ctx, http.MethodGet, c.orgPath("compute/ssh-keys/"+url.PathEscape(id)), nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteSshKey sends DELETE /v1/orgs/{org}/compute/ssh-keys/{id}, which answers
// 204. Instances that were created with the key keep it.
func (c *Client) DeleteSshKey(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodDelete, c.orgPath("compute/ssh-keys/"+url.PathEscape(id)), nil, "", nil)
}

// --- Waiters ---------------------------------------------------------------

// WaitVm polls GetVm every pollEvery until done reports true or ctx is
// done. A 404 that says the vm is gone (IsNotFound with ResourceVm) is passed
// to done as a nil *Vm so callers can treat it as deletion; if done(nil) then
// reports true, WaitVm returns (nil, nil). Any other error, a 404 about
// something else included, ends the wait with that error. On deadline it
// returns the last observed *Vm (possibly nil) and an error that wraps
// ctx.Err() and mentions the last observed state.
func (c *Client) WaitVm(ctx context.Context, id string, pollEvery time.Duration, done func(*Vm) (bool, error)) (*Vm, error) {
	var last *Vm
	for {
		vm, err := c.GetVm(ctx, id)
		switch {
		case err != nil && IsNotFound(err, ResourceVm):
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
		case err != nil && IsNotFound(err, ResourcePublicIp):
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
