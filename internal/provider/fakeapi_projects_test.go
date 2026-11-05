package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// Projects (PROV-005) and a project's VPCs (NET-001) in the fake. Shapes are ProjectOut, Vpc, VpcAccepted and
// OperationRef of customer-v1.json (internal/contract checks them); the rules are those of routes/projects.py and
// routes/vpcs.py of s3-control-plane 87607618.

type wireProjectOut struct {
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

type wireVpcOut struct {
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

type wireVpcAccepted struct {
	Vpc       wireVpcOut       `json:"vpc"`
	Operation wireOperationRef `json:"operation"`
}

type fakeVpc struct {
	wireVpcOut
	project   string
	stepsLeft int  // reads a delete stays in "deleting"
	failBuild bool // the build ends in status error
	notEmpty  bool // subnets hang off it: a delete is 409 vpc_not_empty
}

// projectState is the fake's projects and VPCs. All access is under fakeAPI.mu.
type projectState struct {
	projects   map[string]*wireProjectOut
	projectSeq int
	vpcs       map[string]*fakeVpc
	vpcSeq     int
	vpcsOff    bool // S3CP_COMPUTE_VPC_ENABLED=false: every VPC route is 503 not_ready
	failVpcs   bool // new VPCs end in status error
	vpcQuota   int
	patches    []string // "id If-Match" of every PATCH of a project, as it arrived
	idemKeys   []string
}

func newProjectState(org string) projectState {
	now := time.Now().UTC()
	id := fakeUUID(5, 1)
	return projectState{
		projects:   map[string]*wireProjectOut{id: {ID: id, VRN: "vrn:" + org + ":project:" + id, Name: "default", IsDefault: true, Labels: map[string]string{}, Generation: 1, CreatedAt: now, UpdatedAt: now}},
		projectSeq: 1,
		vpcs:       map[string]*fakeVpc{},
		vpcQuota:   5,
	}
}

var (
	projectNameRE  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	projectLabelRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,62}$`)
	vpcRegionRE    = regexp.MustCompile(`^[a-z0-9]+$`)
)

// DefaultProjectID is the id of the organization's default project.
func (f *fakeAPI) DefaultProjectID() string { return fakeUUID(5, 1) }

func (f *fakeAPI) ProjectCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.ps.projects)
}

func (f *fakeAPI) Project(id string) (wireProjectOut, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.ps.projects[id]
	if !ok {
		return wireProjectOut{}, false
	}
	return *p, true
}

func (f *fakeAPI) VanishProject(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.ps.projects, id)
}

// ProjectPatches is "id If-Match" of every PATCH of a project, in order.
func (f *fakeAPI) ProjectPatches() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ps.patches...)
}

// ProjectIdempotencyKeys is the Idempotency-Key of every keyed project or VPC write, in order.
func (f *fakeAPI) ProjectIdempotencyKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ps.idemKeys...)
}

func (f *fakeAPI) SetVpcsOff(off bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ps.vpcsOff = off
}

// FailVpcBuilds makes every VPC created from now on end in status error.
func (f *fakeAPI) FailVpcBuilds() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ps.failVpcs = true
}

func (f *fakeAPI) VpcCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, v := range f.ps.vpcs {
		if v.Status != "deleted" {
			n++
		}
	}
	return n
}

func (f *fakeAPI) Vpc(id string) (wireVpcOut, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.ps.vpcs[id]
	if !ok || v.Status == "deleted" {
		return wireVpcOut{}, false
	}
	return v.wireVpcOut, true
}

func (f *fakeAPI) VanishVpc(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.ps.vpcs, id)
}

// MakeVpcNotEmpty puts a subnet in the VPC: deleting it is 409 vpc_not_empty.
func (f *fakeAPI) MakeVpcNotEmpty(id string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.ps.vpcs[id]; ok {
		v.notEmpty = on
	}
}

// idemClaim is db/idempotency.py for one keyed write, one claim per (route, key): replied is true when the request was
// answered from the store (the same body) or refused (another body). Call with f.mu held; follow with idemKeep.
func (f *fakeAPI) idemClaim(rc *reqCtx, route string, payload []byte) (storeKey, digest string, replied bool) {
	key := rc.r.Header.Get("Idempotency-Key")
	if key == "" {
		return "", "", false
	}
	f.ps.idemKeys = append(f.ps.idemKeys, key)
	storeKey = route + "|" + key
	sum := sha256.Sum256(payload)
	digest = hex.EncodeToString(sum[:])
	if rec, ok := f.idem[storeKey]; ok {
		if rec.payloadHash != digest {
			rc.conflict("idempotency_key_reused", "this Idempotency-Key was already used with a different request body")
			return storeKey, digest, true
		}
		hdrs := []string{}
		if rec.location != "" {
			hdrs = append(hdrs, "Location", rec.location)
		}
		if rec.etag != "" {
			hdrs = append(hdrs, "ETag", rec.etag)
		}
		if rec.status == http.StatusNoContent {
			rc.noContent()
			return storeKey, digest, true
		}
		rc.raw(rec.status, rec.body, hdrs...)
		return storeKey, digest, true
	}
	return storeKey, digest, false
}

func (f *fakeAPI) idemKeep(storeKey, digest string, status int, body any, location, etag string) {
	if storeKey == "" {
		return
	}
	raw := []byte(nil)
	if body != nil {
		raw, _ = json.Marshal(body)
		raw = append(raw, '\n')
	}
	f.idem[storeKey] = &idemRecord{payloadHash: digest, status: status, body: raw, location: location, etag: etag}
}

func checkProjectName(b *bodyCheck, name string) {
	if !projectNameRE.MatchString(name) {
		b.fail("name", "String should match pattern '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'")
	}
}

func checkLabels(b *bodyCheck, raw json.RawMessage) map[string]string {
	labels := map[string]string{}
	if err := json.Unmarshal(raw, &labels); err != nil {
		b.fail("labels", "Input should be a valid dictionary")
		return nil
	}
	if len(labels) > 64 {
		b.fail("labels", "Dictionary should have at most 64 items after validation")
	}
	for k, v := range labels {
		if !projectLabelRE.MatchString(k) {
			b.fail("labels", "String should match pattern '^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,62}$'")
		}
		if len(v) > 255 {
			b.fail("labels", "String should have at most 255 characters")
		}
	}
	return labels
}

// createProject is POST /projects: 201 ProjectOut, Location and ETag; 409 name_taken; 422 quota_exceeded at 100.
func (f *fakeAPI) createProject(rc *reqCtx) {
	b := newBodyCheck(rc.r.Body)
	name := b.str("name", true, 1, 63)
	if name != "" {
		checkProjectName(b, name)
	}
	description := b.str("description", false, 0, 255)
	labels := map[string]string{}
	if raw, ok := b.present("labels", false); ok {
		labels = checkLabels(b, raw)
	}
	if errs := b.finish(); len(errs) > 0 {
		rc.invalid(errs...)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	payload, _ := json.Marshal([]any{name, description, labels})
	storeKey, digest, replied := f.idemClaim(rc, "POST projects", payload)
	if replied {
		return
	}
	if len(f.ps.projects) >= 100 {
		rc.unprocessable("quota_exceeded", "projects: 100 in use plus 1 requested exceeds the limit of 100 for this account")
		return
	}
	for _, p := range f.ps.projects {
		if p.Name == name {
			rc.conflict("name_taken", fmt.Sprintf("a project named '%s' already exists in this organization", name))
			return
		}
	}
	f.ps.projectSeq++
	id := fakeUUID(5, f.ps.projectSeq)
	now := time.Now().UTC()
	p := &wireProjectOut{ID: id, VRN: "vrn:" + f.org + ":project:" + id, Name: name, Description: description,
		Labels: labels, Generation: 1, CreatedAt: now, UpdatedAt: now}
	f.ps.projects[id] = p
	location := "/v1/orgs/" + f.org + "/projects/" + id
	f.idemKeep(storeKey, digest, http.StatusCreated, p, location, generationETag(p.Generation))
	rc.json(http.StatusCreated, p, "Location", location, "ETag", generationETag(p.Generation))
}

func (f *fakeAPI) listProjects(rc *reqCtx) {
	f.mu.Lock()
	defer f.mu.Unlock()
	items := make([]wireProjectOut, 0, len(f.ps.projects))
	for _, p := range f.ps.projects {
		items = append(items, *p)
	}
	sort.Slice(items, func(i, j int) bool { // the default first, then oldest first
		if items[i].IsDefault != items[j].IsDefault {
			return items[i].IsDefault
		}
		return items[i].ID < items[j].ID
	})
	rc.json(http.StatusOK, wireList[wireProjectOut]{Items: items})
}

func (f *fakeAPI) getProject(rc *reqCtx, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.ps.projects[id]
	if !ok {
		rc.notFound("project")
		return
	}
	rc.json(http.StatusOK, p, "ETag", generationETag(p.Generation))
}

// patchProject is PATCH /projects/{id}: If-Match required (428), stale is 412 generation_mismatch carrying the
// current ETag, name_taken is 409, a field that is sent may not be null.
func (f *fakeAPI) patchProject(rc *reqCtx, id string) {
	ifMatch := rc.r.Header.Get("If-Match")
	f.mu.Lock()
	f.ps.patches = append(f.ps.patches, id+" "+ifMatch)
	f.mu.Unlock()

	b := newBodyCheck(rc.r.Body)
	var name, description *string
	var labels map[string]string
	if raw, ok := b.present("name", false); ok {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			b.fail("name", "Input should be a valid string")
		} else {
			name = &s
			checkProjectName(b, s)
		}
	}
	if raw, ok := b.present("description", false); ok {
		var s string
		if json.Unmarshal(raw, &s) != nil || len(s) > 255 {
			b.fail("description", "Input should be a valid string of at most 255 characters")
		} else {
			description = &s
		}
	}
	if raw, ok := b.present("labels", false); ok {
		labels = checkLabels(b, raw)
	}
	if errs := b.finish(); len(errs) > 0 {
		rc.invalid(errs...)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.ps.projects[id]
	if !ok {
		rc.notFound("project")
		return
	}
	if ifMatch == "" {
		rc.problem(http.StatusPreconditionRequired, "precondition_required", "Precondition required", "send If-Match with the project's current ETag")
		return
	}
	if ifMatch != generationETag(p.Generation) {
		rc.w.Header().Set("ETag", generationETag(p.Generation))
		rc.problem(http.StatusPreconditionFailed, "generation_mismatch", "Precondition failed",
			"the project changed since it was read; retry with the new ETag")
		return
	}
	payload, _ := json.Marshal([]any{id, name, description, labels})
	storeKey, digest, replied := f.idemClaim(rc, "PATCH projects", payload)
	if replied {
		return
	}
	if name != nil {
		for _, other := range f.ps.projects {
			if other.ID != id && other.Name == *name {
				rc.conflict("name_taken", fmt.Sprintf("a project named '%s' already exists in this organization", *name))
				return
			}
		}
		p.Name = *name
	}
	if description != nil {
		p.Description = *description
	}
	if labels != nil {
		p.Labels = labels
	}
	p.Generation++
	p.UpdatedAt = time.Now().UTC()
	f.idemKeep(storeKey, digest, http.StatusOK, p, "", generationETag(p.Generation))
	rc.json(http.StatusOK, p, "ETag", generationETag(p.Generation))
}

// deleteProject is DELETE /projects/{id}: 204; 409 default_project; 409 project_not_empty while it holds a VPC.
func (f *fakeAPI) deleteProject(rc *reqCtx, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	storeKey, digest, replied := f.idemClaim(rc, "DELETE projects", []byte(id))
	if replied {
		return
	}
	p, ok := f.ps.projects[id]
	if !ok {
		rc.notFound("project")
		return
	}
	if p.IsDefault {
		rc.conflict("default_project", "the organization's default project cannot be deleted")
		return
	}
	for _, v := range f.ps.vpcs {
		if v.project == id && v.Status != "deleted" {
			rc.conflict("project_not_empty", "the project still holds a network")
			return
		}
	}
	delete(f.ps.projects, id)
	f.idemKeep(storeKey, digest, http.StatusNoContent, nil, "", "")
	rc.noContent()
}

// --- VPCs: /v1/projects/{project}/vpcs[/{vpc}] -------------------------------------------------

var vpcRouteRE = regexp.MustCompile(`^/v1/projects/([^/]+)/vpcs(?:/([^/]+))?$`)

// serveProjectScoped serves the routes that are not under /v1/orgs/{org}: a key's organization is the one it was
// issued for. The gates follow the org-scoped ones: credential, source address, permission, ids, then the flag
// (503 not_ready before anything else is read), then the project.
func (f *fakeAPI) serveProjectScoped(rc *reqCtx) {
	m := vpcRouteRE.FindStringSubmatch(rc.r.URL.Path)
	route := ""
	switch {
	case m == nil:
	case m[2] == "" && rc.r.Method == http.MethodPost:
		route = "POST projects/{project}/vpcs"
	case m[2] == "" && rc.r.Method == http.MethodGet:
		route = "GET projects/{project}/vpcs"
	case m[2] != "" && rc.r.Method == http.MethodGet:
		route = "GET projects/{project}/vpcs/{vpc}"
	case m[2] != "" && rc.r.Method == http.MethodDelete:
		route = "DELETE projects/{project}/vpcs/{vpc}"
	}
	if route == "" {
		rc.problem(http.StatusNotFound, "not_found", "Not Found", "Not Found")
		return
	}
	f.mu.Lock()
	f.calls = append(f.calls, route)
	denied, off := f.sourceDenied, f.ps.vpcsOff
	f.mu.Unlock()

	if !f.authenticate(rc) {
		return
	}
	if denied {
		rc.problem(http.StatusForbidden, "source_address_not_allowed", "Forbidden", "this API token cannot be used from this network address")
		return
	}
	if perm := routePermissions[route]; perm != "" && !rc.allows(perm) {
		rc.problem(http.StatusForbidden, "permission_denied", "Forbidden", "this credential does not carry the '"+perm+"' permission")
		return
	}
	var errs []wireFieldError
	if !anyUUIDRE.MatchString(m[1]) {
		errs = append(errs, wireFieldError{Field: "project", Message: "Input should be a valid UUID"})
	}
	if m[2] != "" && !anyUUIDRE.MatchString(m[2]) {
		errs = append(errs, wireFieldError{Field: "vpc", Message: "Input should be a valid UUID"})
	}
	if len(errs) > 0 {
		rc.invalid(errs...)
		return
	}
	if off {
		rc.problem(http.StatusServiceUnavailable, "not_ready", "Service unavailable", "VPCs are not switched on yet")
		return
	}
	f.mu.Lock()
	_, known := f.ps.projects[m[1]]
	f.mu.Unlock()
	if !known {
		rc.notFound("project")
		return
	}
	switch route {
	case "POST projects/{project}/vpcs":
		f.createVpc(rc, m[1])
	case "GET projects/{project}/vpcs":
		f.listVpcs(rc, m[1])
	case "GET projects/{project}/vpcs/{vpc}":
		f.getVpc(rc, m[1], m[2])
	case "DELETE projects/{project}/vpcs/{vpc}":
		f.deleteVpc(rc, m[1], m[2])
	}
}

func (f *fakeAPI) vpcAccepted(rc *reqCtx, v *fakeVpc, typ string) wireVpcAccepted {
	op := f.newOperation(typ, "vpc", v.ID, time.Now().UTC())
	return wireVpcAccepted{Vpc: v.wireVpcOut, Operation: op}
}

func (f *fakeAPI) createVpc(rc *reqCtx, project string) {
	b := newBodyCheck(rc.r.Body)
	name := b.str("name", true, 1, 63)
	if name != "" {
		checkProjectName(b, name)
	}
	region := b.str("region", true, 1, 0)
	if region != "" && !vpcRegionRE.MatchString(region) {
		b.fail("region", "String should match pattern '^[a-z0-9]+$'")
	}
	description := b.str("description", false, 0, 255)
	prefix := 22
	if raw, ok := b.present("prefix_length", false); ok && string(raw) != "null" {
		n, err := strconv.Atoi(string(raw))
		switch {
		case err != nil:
			b.fail("prefix_length", "Input should be a valid integer")
		case n < 20:
			b.fail("prefix_length", "Input should be greater than or equal to 20")
		case n > 24:
			b.fail("prefix_length", "Input should be less than or equal to 24")
		default:
			prefix = n
		}
	}
	if errs := b.finish(); len(errs) > 0 {
		rc.invalid(errs...)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	payload, _ := json.Marshal([]any{project, name, region, description, prefix})
	storeKey, digest, replied := f.idemClaim(rc, "POST vpcs", payload)
	if replied {
		return
	}
	known := false
	for _, r := range fakeRegions {
		if r.ID == region && r.Status == "active" {
			known = true
		}
	}
	if !known {
		rc.unprocessable("region_unavailable", "that region is not available to you")
		return
	}
	live := 0
	for _, v := range f.ps.vpcs {
		if v.Status == "deleted" {
			continue
		}
		live++
		if v.project == project && v.Region == region && v.Name == name {
			rc.conflict("name_taken", fmt.Sprintf("this project already has a VPC named '%s' in %s", name, region))
			return
		}
	}
	if live >= f.ps.vpcQuota {
		rc.unprocessable("quota_exceeded", fmt.Sprintf("vpcs: %d in use plus 1 requested exceeds the limit of %d for this account", live, f.ps.vpcQuota))
		return
	}
	f.ps.vpcSeq++
	id := fakeUUID(6, f.ps.vpcSeq)
	v := &fakeVpc{project: project, failBuild: f.ps.failVpcs, wireVpcOut: wireVpcOut{
		ID: id, VRN: "vrn:" + f.org + ":project:" + project + ":vpc:" + id, Name: name, Description: description, Region: region,
		CIDR: fmt.Sprintf("10.%d.0.0/%d", 100+f.ps.vpcSeq, prefix), Status: "pending", InternetEgress: true,
		CreatedAt: time.Now().UTC(), Generation: 1}}
	f.ps.vpcs[id] = v
	accepted := f.vpcAccepted(rc, v, "vpc.create")
	location := f.operationLocation(accepted.Operation)
	f.idemKeep(storeKey, digest, http.StatusAccepted, accepted, location, "")
	rc.json(http.StatusAccepted, accepted, "Location", location)
}

// advanceVpc moves a VPC one step per read: pending, creating, then active (or error for a failing build); a delete
// stays in "deleting" for stepsLeft reads and then the row reads as absent.
func advanceVpc(v *fakeVpc) {
	switch v.Status {
	case "pending":
		v.Status = "creating"
	case "creating":
		if v.failBuild {
			reason := "the network could not be built"
			v.Status, v.StatusReason = "error", &reason
		} else {
			v.Status = "active"
		}
	case "deleting":
		if v.stepsLeft--; v.stepsLeft <= 0 {
			v.Status = "deleted"
		}
	}
	v.Generation++
}

func (f *fakeAPI) lookupVpc(project, id string) *fakeVpc {
	v, ok := f.ps.vpcs[id]
	if !ok || v.project != project || v.Status == "deleted" {
		return nil
	}
	return v
}

func (f *fakeAPI) getVpc(rc *reqCtx, project, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := f.lookupVpc(project, id)
	if v == nil {
		rc.notFound("vpc")
		return
	}
	advanceVpc(v)
	if v.Status == "deleted" {
		rc.notFound("vpc")
		return
	}
	rc.json(http.StatusOK, v.wireVpcOut, "ETag", generationETag(v.Generation))
}

func (f *fakeAPI) listVpcs(rc *reqCtx, project string) {
	region := rc.r.URL.Query().Get("region")
	f.mu.Lock()
	defer f.mu.Unlock()
	items := []wireVpcOut{}
	for _, v := range f.ps.vpcs {
		if v.project == project && v.Status != "deleted" && (region == "" || v.Region == region) {
			items = append(items, v.wireVpcOut)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID > items[j].ID }) // newest first
	rc.json(http.StatusOK, wireList[wireVpcOut]{Items: items})
}

func (f *fakeAPI) deleteVpc(rc *reqCtx, project, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	storeKey, digest, replied := f.idemClaim(rc, "DELETE vpcs", []byte(id))
	if replied {
		return
	}
	v := f.lookupVpc(project, id)
	if v == nil {
		rc.notFound("vpc")
		return
	}
	if v.Status == "deleting" {
		rc.conflict("operation_in_flight", "another operation is already running for this VPC")
		return
	}
	if v.notEmpty {
		rc.conflict("vpc_not_empty", "subnets still hang off this VPC; delete them first")
		return
	}
	v.Status, v.stepsLeft = "deleting", 2
	op := f.newOperation("vpc.delete", "vpc", v.ID, time.Now().UTC())
	location := f.operationLocation(op)
	f.idemKeep(storeKey, digest, http.StatusAccepted, op, location, "")
	rc.json(http.StatusAccepted, op, "Location", location)
}
