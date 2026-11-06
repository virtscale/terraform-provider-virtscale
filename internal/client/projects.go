package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// The resources a call can ask about, as the API's not_found helper names them (see ResourceVm).
const (
	ResourceProject = "project"
	ResourceVpc     = "vpc"
)

// Project is the ProjectOut representation returned by the projects endpoints.
type Project struct {
	ID          string            `json:"id"`
	VRN         string            `json:"vrn"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	IsDefault   bool              `json:"is_default"`
	Labels      map[string]string `json:"labels"`
	Generation  int64             `json:"generation"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// ProjectCreateRequest is the body of POST /v1/orgs/{org}/projects.
type ProjectCreateRequest struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Labels      map[string]string `json:"labels"`
}

// ProjectUpdateRequest is the body of PATCH /v1/orgs/{org}/projects/{id}: only the fields set change. Labels is a
// pointer so that an empty set ({}), which clears the labels, is sent and an absent one is not.
type ProjectUpdateRequest struct {
	Name        *string            `json:"name,omitempty"`
	Description *string            `json:"description,omitempty"`
	Labels      *map[string]string `json:"labels,omitempty"`
}

// CreateProject sends POST /v1/orgs/{org}/projects, which answers 201 with the project.
func (c *Client) CreateProject(ctx context.Context, req ProjectCreateRequest, idempotencyKey string) (*Project, error) {
	var out Project
	if err := c.doJSON(ctx, http.MethodPost, c.orgPath("projects"), req, idempotencyKey, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetProject sends GET /v1/orgs/{org}/projects/{id}.
func (c *Client) GetProject(ctx context.Context, id string) (*Project, error) {
	var out Project
	if err := c.doJSON(ctx, http.MethodGet, c.orgPath("projects/"+url.PathEscape(id)), nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListProjects sends GET /v1/orgs/{org}/projects: the default project first.
func (c *Client) ListProjects(ctx context.Context) ([]Project, error) {
	var out listResponse[Project]
	if err := c.doJSON(ctx, http.MethodGet, c.orgPath("projects"), nil, "", &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// UpdateProject sends PATCH /v1/orgs/{org}/projects/{id} under If-Match: generation, so a change built on a stale
// read is refused (412 generation_mismatch) instead of overwriting a newer one. A non-empty idempotencyKey makes the
// PATCH safe to repeat.
func (c *Client) UpdateProject(ctx context.Context, id string, generation int64, req ProjectUpdateRequest, idempotencyKey string) (*Project, error) {
	headers := http.Header{}
	headers.Set("If-Match", `"`+strconv.FormatInt(generation, 10)+`"`)
	var out Project
	if err := c.doJSONWithHeaders(ctx, http.MethodPatch, c.orgPath("projects/"+url.PathEscape(id)), req, idempotencyKey, headers, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteProject sends DELETE /v1/orgs/{org}/projects/{id}, which answers 204: 409 default_project for the default
// project, 409 project_not_empty while it holds a server, public IP or network.
func (c *Client) DeleteProject(ctx context.Context, id, idempotencyKey string) error {
	return c.doJSON(ctx, http.MethodDelete, c.orgPath("projects/"+url.PathEscape(id)), nil, idempotencyKey, nil)
}

// Vpc is the Vpc representation returned by the project-scoped VPC endpoints. Status is pending, creating, active,
// deleting, deleted or error; StatusReason says why it is error.
type Vpc struct {
	ID             string    `json:"id"`
	VRN            string    `json:"vrn"`
	Name           string    `json:"name"`
	Description    string    `json:"description"`
	Region         string    `json:"region"`
	CIDR           string    `json:"cidr"`
	IsDefault      bool      `json:"is_default"`
	Status         string    `json:"status"`
	StatusReason   *string   `json:"status_reason"`
	InternetEgress bool      `json:"internet_egress"`
	CreatedAt      time.Time `json:"created_at"`
	Generation     int64     `json:"generation"`
}

// VpcCreateRequest is the body of POST /v1/projects/{project}/vpcs. PrefixLength (20 to 24) is left out when nil,
// and the address plan's default applies.
type VpcCreateRequest struct {
	Name         string `json:"name"`
	Region       string `json:"region"`
	Description  string `json:"description,omitempty"`
	PrefixLength *int   `json:"prefix_length,omitempty"`
}

type vpcAccepted struct {
	Vpc       Vpc       `json:"vpc"`
	Operation Operation `json:"operation"`
}

// projectPath builds an absolute URL under /v1/projects/{project}/.
func (c *Client) projectPath(project, p string) string {
	return c.endpoint + "/v1/projects/" + url.PathEscape(project) + "/" + p
}

// CreateVpc sends POST /v1/projects/{project}/vpcs, which answers 202 with the VPC as it stands and the operation.
func (c *Client) CreateVpc(ctx context.Context, project string, req VpcCreateRequest, idempotencyKey string) (*Vpc, *Operation, error) {
	var out vpcAccepted
	if err := c.doJSON(ctx, http.MethodPost, c.projectPath(project, "vpcs"), req, idempotencyKey, &out); err != nil {
		return nil, nil, err
	}
	return &out.Vpc, &out.Operation, nil
}

// GetVpc sends GET /v1/projects/{project}/vpcs/{id}.
func (c *Client) GetVpc(ctx context.Context, project, id string) (*Vpc, error) {
	var out Vpc
	if err := c.doJSON(ctx, http.MethodGet, c.projectPath(project, "vpcs/"+url.PathEscape(id)), nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteVpc sends DELETE /v1/projects/{project}/vpcs/{id}, which answers 202 with the operation; the VPC stays
// visible (status deleting) until the cloud has let go of it.
func (c *Client) DeleteVpc(ctx context.Context, project, id, idempotencyKey string) (*Operation, error) {
	var out Operation
	if err := c.doJSON(ctx, http.MethodDelete, c.projectPath(project, "vpcs/"+url.PathEscape(id)), nil, idempotencyKey, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Poll calls get every pollEvery until done reports true or ctx is done. An error that isGone recognises (the 404
// that says the resource itself is gone) is passed to done as a nil *T, so a caller can treat it as deletion; if
// done(nil) then reports true, Poll returns (nil, nil). Any other error ends the wait. On the deadline the error wraps
// ctx.Err() and says what was last seen, as describe words it.
func Poll[T any](ctx context.Context, pollEvery time.Duration, what string, get func(context.Context) (*T, error),
	isGone func(error) bool, done func(*T) (bool, error), describe func(*T) string) (*T, error) {
	var last *T
	timeout := func() error {
		seen := "nothing yet"
		if last != nil {
			seen = describe(last)
		}
		return fmt.Errorf("waiting for %s: %w (last seen: %s)", what, ctx.Err(), seen)
	}
	for {
		v, err := get(ctx)
		switch {
		case err != nil && isGone(err):
			if ok, derr := done(nil); derr != nil {
				return last, derr
			} else if ok {
				return nil, nil
			}
		case err != nil:
			if ctx.Err() != nil {
				return last, timeout()
			}
			return last, err
		default:
			last = v
			ok, derr := done(v)
			if derr != nil {
				return v, derr
			}
			if ok {
				return v, nil
			}
		}
		timer := time.NewTimer(pollEvery)
		select {
		case <-ctx.Done():
			timer.Stop()
			return last, timeout()
		case <-timer.C:
		}
	}
}

// WaitVpc polls GetVpc until done reports true, with Poll's rules; the 404 that says the VPC is gone is done(nil).
func (c *Client) WaitVpc(ctx context.Context, project, id string, pollEvery time.Duration, done func(*Vpc) (bool, error)) (*Vpc, error) {
	return Poll(ctx, pollEvery, "vpc "+id,
		func(ctx context.Context) (*Vpc, error) { return c.GetVpc(ctx, project, id) },
		func(err error) bool { return IsNotFound(err, ResourceVpc) },
		done, func(v *Vpc) string { return "status " + v.Status })
}
