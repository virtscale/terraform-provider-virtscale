// Package contract checks traffic against the OpenAPI document of the customer API (customer-v1.json, vendored from
// s3-control-plane; see SOURCE). The provider has no generated client yet (D12), so this is what keeps its hand-written
// client and the fake API of its tests honest: a route, a request body or a response the contract does not have is a
// problem. It is used by tests only.
package contract

import (
	"bytes"
	_ "embed"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed customer-v1.json
var specJSON []byte

const resourceURL = "customer-v1.json"

type operation struct {
	method, template string
	re               *regexp.Regexp
	literal          int // characters of the template that are not parameters: the most specific template wins
	deprecated       bool
	raw              map[string]any
}

// Spec is the loaded document.
type Spec struct {
	doc      map[string]any
	compiler *jsonschema.Compiler
	ops      []*operation
	mu       sync.Mutex // the compiler and its cache are not safe for the fake API's concurrent requests
	schemas  map[string]*jsonschema.Schema
}

var paramRE = regexp.MustCompile(`\{[^}]+\}`)

// Load parses the embedded document.
func Load() (*Spec, error) {
	parsed, err := jsonschema.UnmarshalJSON(bytes.NewReader(specJSON))
	if err != nil {
		return nil, fmt.Errorf("contract: parse customer-v1.json: %w", err)
	}
	doc, ok := parsed.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("contract: customer-v1.json is not an object")
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020) // OpenAPI 3.1 schemas are JSON Schema 2020-12
	c.AssertFormat()
	if err := c.AddResource(resourceURL, doc); err != nil {
		return nil, fmt.Errorf("contract: add resource: %w", err)
	}
	s := &Spec{doc: doc, compiler: c, schemas: map[string]*jsonschema.Schema{}}

	paths, _ := doc["paths"].(map[string]any)
	for template, item := range paths {
		methods, _ := item.(map[string]any)
		for method, raw := range methods {
			o, isOp := raw.(map[string]any)
			if !isOp || !isHTTPMethod(method) {
				continue
			}
			parts := paramRE.Split(template, -1)
			for i := range parts {
				parts[i] = regexp.QuoteMeta(parts[i])
			}
			re := "^" + strings.Join(parts, "[^/]+") + "$"
			dep, _ := o["deprecated"].(bool)
			s.ops = append(s.ops, &operation{
				method: strings.ToUpper(method), template: template,
				re: regexp.MustCompile(re), literal: len(paramRE.ReplaceAllString(template, "")),
				deprecated: dep, raw: o,
			})
		}
	}
	sort.Slice(s.ops, func(i, j int) bool { return s.ops[i].literal > s.ops[j].literal })
	return s, nil
}

func isHTTPMethod(m string) bool {
	switch m {
	case "get", "put", "post", "delete", "patch", "head", "options":
		return true
	}
	return false
}

// HasOperation reports whether "METHOD /template" is in the contract.
func (s *Spec) HasOperation(key string) bool {
	method, template, _ := strings.Cut(key, " ")
	for _, o := range s.ops {
		if o.method == method && o.template == template {
			return true
		}
	}
	return false
}

func (s *Spec) find(method, path string) *operation {
	for _, o := range s.ops {
		if o.method == method && o.re.MatchString(path) {
			return o
		}
	}
	return nil
}

// Check validates one exchange: the route and method, the request body (when there is one), the answer's status and
// its body. contentType is the answer's Content-Type. It returns what does not conform, one line each; none means it
// conforms. Query strings are not part of path.
func (s *Spec) Check(method, path string, reqBody []byte, status int, contentType string, respBody []byte) []string {
	path, _, _ = strings.Cut(path, "?")
	o := s.find(strings.ToUpper(method), path)
	if o == nil {
		return []string{fmt.Sprintf("%s %s is not in the contract", method, path)}
	}
	who := fmt.Sprintf("%s %s", o.method, o.template)
	var problems []string
	if o.deprecated {
		problems = append(problems, who+" is deprecated in the contract: use its replacement")
	}

	if len(bytes.TrimSpace(reqBody)) > 0 {
		body, base := s.resolve(o.raw["requestBody"], "/paths/"+escapeToken(o.template)+"/"+strings.ToLower(o.method)+"/requestBody")
		if body == nil {
			problems = append(problems, who+": the request has a body, the contract has none")
		} else if loc := s.schemaLocation(body, base, "application/json"); loc == "" {
			problems = append(problems, who+": the contract has no application/json request body")
		} else {
			problems = append(problems, s.validate(who+" request", loc, reqBody)...)
		}
	}

	responses, _ := o.raw["responses"].(map[string]any)
	key := strconv.Itoa(status)
	if _, ok := responses[key]; !ok {
		key = "default"
	}
	declared, ok := responses[key]
	if !ok || (key == "default" && status >= 200 && status < 300) {
		return append(problems, fmt.Sprintf("%s: status %d is not declared", who, status))
	}
	resp, base := s.resolve(declared, "/paths/"+escapeToken(o.template)+"/"+strings.ToLower(o.method)+"/responses/"+key)
	if resp == nil {
		return append(problems, fmt.Sprintf("%s: response %s cannot be resolved", who, key))
	}
	ct, _, _ := strings.Cut(contentType, ";")
	loc := s.schemaLocation(resp, base, strings.TrimSpace(ct))
	switch {
	case len(bytes.TrimSpace(respBody)) == 0:
	case loc == "":
		problems = append(problems, fmt.Sprintf("%s: status %d has a %q body, the contract declares none for it", who, status, ct))
	default:
		problems = append(problems, s.validate(fmt.Sprintf("%s response %d", who, status), loc, respBody)...)
	}
	return problems
}

// resolve follows a $ref into components and returns the object and its pointer.
func (s *Spec) resolve(v any, ptr string) (map[string]any, string) {
	for i := 0; i < 8; i++ {
		obj, ok := v.(map[string]any)
		if !ok {
			return nil, ptr
		}
		ref, isRef := obj["$ref"].(string)
		if !isRef {
			return obj, ptr
		}
		ptr = strings.TrimPrefix(ref, "#")
		v = s.lookup(ptr)
	}
	return nil, ptr
}

func (s *Spec) lookup(ptr string) any {
	var cur any = s.doc
	for _, tok := range strings.Split(strings.TrimPrefix(ptr, "/"), "/") {
		tok, _ = url.PathUnescape(tok)
		tok = strings.NewReplacer("~1", "/", "~0", "~").Replace(tok)
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[tok]
	}
	return cur
}

// schemaLocation is the pointer of the schema obj (found at base) declares for content type ct; a JSON content type
// that is not listed (application/problem+json) falls back to application/json. "" when there is none.
func (s *Spec) schemaLocation(obj map[string]any, base, ct string) string {
	content, _ := obj["content"].(map[string]any)
	if _, ok := content[ct]; !ok {
		if !strings.Contains(ct, "json") {
			return ""
		}
		ct = "application/json"
	}
	media, _ := content[ct].(map[string]any)
	if media == nil || media["schema"] == nil {
		return ""
	}
	return base + "/content/" + escapeToken(ct) + "/schema"
}

func (s *Spec) validate(what, loc string, body []byte) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	sch, ok := s.schemas[loc]
	if !ok {
		var err error
		sch, err = s.compiler.Compile(resourceURL + "#" + loc)
		if err != nil {
			return []string{fmt.Sprintf("%s: the contract schema %s does not compile: %v", what, loc, err)}
		}
		s.schemas[loc] = sch
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		return []string{fmt.Sprintf("%s: the body is not JSON: %v", what, err)}
	}
	if err := sch.Validate(value); err != nil {
		msg := err.Error()
		if _, rest, found := strings.Cut(msg, "\n"); found {
			msg = strings.TrimSpace(rest) // the first line only repeats the schema's location
		}
		if len(msg) > 700 {
			msg = msg[:700] + "..."
		}
		return []string{fmt.Sprintf("%s does not conform: %s", what, msg)}
	}
	return nil
}

// escapeToken makes a pointer token (a path template, a content type) safe for a JSON pointer inside a URL fragment.
func escapeToken(s string) string {
	return url.PathEscape(strings.NewReplacer("~", "~0", "/", "~1").Replace(s))
}
