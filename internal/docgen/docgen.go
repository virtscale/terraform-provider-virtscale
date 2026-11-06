// Package docgen writes the documentation of the provider (docs/) from its schemas and the examples/ directory. The
// pages are what the schemas say, so they cannot drift from them: a test regenerates them and compares (`go generate
// ./...` rewrites them). It is used by a test and by tools/gendocs only.
package docgen

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	dsschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	pschema "github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"

	vsprovider "gitlab.virtscale.nl/virtscale/virtscale-platform/terraform-provider-virtscale/internal/provider"
)

// field is one attribute reduced to what a page says about it.
type field struct {
	name                                 string
	typ, desc                            string
	required, optional, computed, secret bool
	replaces                             bool
	nestedTitle                          string
	nested                               []field
}

// attribute is what the three schema kinds' attributes have in common.
type attribute interface {
	GetDescription() string
	IsRequired() bool
	IsOptional() bool
	IsComputed() bool
	IsSensitive() bool
	GetType() attr.Type
}

func typeName(t attr.Type) string {
	switch tt := t.(type) {
	case basetypes.StringTypable:
		return "String"
	case basetypes.Int64Typable, basetypes.NumberTypable, basetypes.Float64Typable:
		return "Number"
	case basetypes.BoolTypable:
		return "Boolean"
	case basetypes.ListType:
		return "List of " + typeName(tt.ElemType)
	case basetypes.SetType:
		return "Set of " + typeName(tt.ElemType)
	case basetypes.MapType:
		return "Map of " + typeName(tt.ElemType)
	}
	return "Object"
}

// forcesReplacement reports whether a plan modifier of the attribute destroys and recreates the resource.
func forcesReplacement(a any) bool {
	ctx := context.Background()
	says := func(d string) bool { return strings.Contains(d, "destroy and recreate") }
	if x, ok := a.(interface{ StringPlanModifiers() []planmodifier.String }); ok {
		for _, m := range x.StringPlanModifiers() {
			if says(m.Description(ctx)) {
				return true
			}
		}
	}
	if x, ok := a.(interface{ Int64PlanModifiers() []planmodifier.Int64 }); ok {
		for _, m := range x.Int64PlanModifiers() {
			if says(m.Description(ctx)) {
				return true
			}
		}
	}
	if x, ok := a.(interface{ BoolPlanModifiers() []planmodifier.Bool }); ok {
		for _, m := range x.BoolPlanModifiers() {
			if says(m.Description(ctx)) {
				return true
			}
		}
	}
	if x, ok := a.(interface{ ListPlanModifiers() []planmodifier.List }); ok {
		for _, m := range x.ListPlanModifiers() {
			if says(m.Description(ctx)) {
				return true
			}
		}
	}
	return false
}

func toField(name string, a attribute) field {
	f := field{name: name, typ: typeName(a.GetType()), desc: a.GetDescription(), required: a.IsRequired(), optional: a.IsOptional(),
		computed: a.IsComputed(), secret: a.IsSensitive(), replaces: forcesReplacement(a)}
	return f
}

func sorted[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func resourceFields(attrs map[string]rschema.Attribute, blocks map[string]rschema.Block) []field {
	var out []field
	for _, name := range sorted(attrs) {
		f := toField(name, attrs[name])
		switch a := attrs[name].(type) {
		case rschema.ListNestedAttribute:
			f.typ, f.nestedTitle, f.nested = "Attributes List", name, resourceFields(a.NestedObject.Attributes, nil)
		case rschema.SetNestedAttribute:
			f.typ, f.nestedTitle, f.nested = "Attributes Set", name, resourceFields(a.NestedObject.Attributes, nil)
		}
		out = append(out, f)
	}
	for _, name := range sorted(blocks) {
		switch b := blocks[name].(type) {
		case rschema.SetNestedBlock:
			out = append(out, field{name: name, typ: "Block Set", desc: b.Description, optional: true, nestedTitle: name,
				nested: resourceFields(b.NestedObject.Attributes, nil)})
		case rschema.ListNestedBlock:
			out = append(out, field{name: name, typ: "Block List", desc: b.Description, optional: true, nestedTitle: name,
				nested: resourceFields(b.NestedObject.Attributes, nil)})
		}
	}
	return out
}

func dataSourceFields(attrs map[string]dsschema.Attribute) []field {
	var out []field
	for _, name := range sorted(attrs) {
		f := toField(name, attrs[name])
		if a, ok := attrs[name].(dsschema.ListNestedAttribute); ok {
			f.typ, f.nestedTitle, f.nested = "Attributes List", name, dataSourceFields(a.NestedObject.Attributes)
		}
		out = append(out, f)
	}
	return out
}

func providerFields(attrs map[string]pschema.Attribute) []field {
	var out []field
	for _, name := range sorted(attrs) {
		out = append(out, toField(name, attrs[name]))
	}
	return out
}

func line(f field) string {
	s := fmt.Sprintf("- `%s` (%s)", f.name, f.typ)
	if f.desc != "" {
		s += " " + f.desc
	}
	if f.replaces {
		s += " **Forces replacement.**"
	}
	if f.secret {
		s += " **Sensitive.**"
	}
	return s
}

func renderFields(b *strings.Builder, fields []field, nestedLevel int) {
	groups := []struct {
		title string
		keep  func(field) bool
	}{
		{"Required", func(f field) bool { return f.required }},
		{"Optional", func(f field) bool { return f.optional && !f.required }},
		{"Read-Only", func(f field) bool { return f.computed && !f.optional && !f.required }},
	}
	hashes := strings.Repeat("#", 3+nestedLevel)
	for _, g := range groups {
		var matching []field
		for _, f := range fields {
			if g.keep(f) {
				matching = append(matching, f)
			}
		}
		if len(matching) == 0 {
			continue
		}
		fmt.Fprintf(b, "%s %s\n\n", hashes, g.title)
		for _, f := range matching {
			b.WriteString(line(f) + "\n")
		}
		b.WriteString("\n")
	}
	for _, f := range fields {
		if len(f.nested) > 0 {
			fmt.Fprintf(b, "%s Nested Schema for `%s`\n\n", strings.Repeat("#", 3+nestedLevel), f.nestedTitle)
			renderFields(b, f.nested, nestedLevel+1)
		}
	}
}

func read(root, rel string) string {
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(b), "\n") + "\n"
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return strings.ReplaceAll(s, `"`, `'`)
}

func front(title, description string) string {
	return fmt.Sprintf("---\npage_title: %q\nsubcategory: \"\"\ndescription: |-\n  %s\n---\n\n", title, oneLine(description))
}

// Generate returns the pages of docs/ (relative to root, with forward slashes) with their content.
func Generate(root string) (map[string]string, error) {
	ctx := context.Background()
	p := vsprovider.New("dev")()
	pages := map[string]string{}

	// the provider page
	var ps provider.SchemaResponse
	p.Schema(ctx, provider.SchemaRequest{}, &ps)
	var b strings.Builder
	b.WriteString(front("Provider: virtscale", ps.Schema.Description))
	b.WriteString("# virtscale Provider\n\n" + ps.Schema.Description + "\n\n## Example Usage\n\n```terraform\n" + read(root, "examples/provider/provider.tf") + "```\n\n## Schema\n\n")
	renderFields(&b, providerFields(ps.Schema.Attributes), 0)
	b.WriteString("See the [guide on state, secrets and import](guides/state-secrets-and-import.md).\n")
	pages["docs/index.md"] = b.String()

	var secrets []string
	for _, a := range providerFields(ps.Schema.Attributes) {
		if a.secret {
			secrets = append(secrets, "provider."+a.name)
		}
	}
	imports := map[string]string{}

	for _, newResource := range p.Resources(ctx) {
		r := newResource()
		var meta resource.MetadataResponse
		r.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "virtscale"}, &meta)
		var rs resource.SchemaResponse
		r.Schema(ctx, resource.SchemaRequest{}, &rs)
		name := meta.TypeName
		fields := resourceFields(rs.Schema.Attributes, rs.Schema.Blocks)
		var b strings.Builder
		b.WriteString(front(name+" Resource - terraform-provider-virtscale", rs.Schema.Description))
		fmt.Fprintf(&b, "# %s (Resource)\n\n%s\n\n## Example Usage\n\n```terraform\n%s```\n\n## Schema\n\n", name, rs.Schema.Description, read(root, "examples/resources/"+name+"/resource.tf"))
		renderFields(&b, fields, 0)
		importSh := read(root, "examples/resources/"+name+"/import.sh")
		fmt.Fprintf(&b, "## Import\n\nImport is supported using the following syntax:\n\n```shell\n%s```\n", importSh)
		pages["docs/resources/"+strings.TrimPrefix(name, "virtscale_")+".md"] = b.String()
		for _, l := range strings.Split(importSh, "\n") {
			if strings.HasPrefix(l, "tofu import ") {
				if f := strings.Fields(l); len(f) >= 4 {
					imports[name] = f[len(f)-1]
				}
			}
		}
		for _, f := range fields {
			if f.secret {
				secrets = append(secrets, name+"."+f.name)
			}
		}
	}

	for _, newDataSource := range p.DataSources(ctx) {
		d := newDataSource()
		var meta datasource.MetadataResponse
		d.Metadata(ctx, datasource.MetadataRequest{ProviderTypeName: "virtscale"}, &meta)
		var ds datasource.SchemaResponse
		d.Schema(ctx, datasource.SchemaRequest{}, &ds)
		name := meta.TypeName
		var b strings.Builder
		b.WriteString(front(name+" Data Source - terraform-provider-virtscale", ds.Schema.Description))
		fmt.Fprintf(&b, "# %s (Data Source)\n\n%s\n\n## Example Usage\n\n```terraform\n%s```\n\n## Schema\n\n", name, ds.Schema.Description, read(root, "examples/data-sources/"+name+"/data-source.tf"))
		fields := dataSourceFields(ds.Schema.Attributes)
		renderFields(&b, fields, 0)
		pages["docs/data-sources/"+strings.TrimPrefix(name, "virtscale_")+".md"] = b.String()
		for _, f := range fields {
			if f.secret {
				secrets = append(secrets, name+"."+f.name)
			}
		}
	}

	pages["docs/guides/state-secrets-and-import.md"] = guide(secrets, imports)
	return pages, nil
}

func guide(secrets []string, imports map[string]string) string {
	sort.Strings(secrets)
	var b strings.Builder
	b.WriteString(front("State, secrets and import - terraform-provider-virtscale", "What the state of the virtscale provider holds, which attributes are sensitive, and how to import each resource."))
	b.WriteString("# State, secrets and import\n\n")
	b.WriteString("## Versions\n\nThe provider speaks protocol 6 and runs with OpenTofu 1.x and Terraform 1.x; it is tested with OpenTofu 1.12.5. " +
		"No attribute is write-only, so no feature of a recent minimum version is needed.\n\n")
	b.WriteString("## The state holds credentials\n\n" +
		"A sensitive attribute is hidden in plan and apply output, but it is stored in the state in plain text, as every provider " +
		"does. Keep the state in an encrypted backend (or use OpenTofu's state encryption) and do not print a sensitive value in an " +
		"output.\n\nThe sensitive attributes of this provider are:\n\n")
	for _, s := range secrets {
		fmt.Fprintf(&b, "- `%s`\n", s)
	}
	b.WriteString("\nThe provider's `api_key` is better kept out of configuration files: set `VIRTSCALE_API_KEY` in the environment. " +
		"`virtscale_s3_access_key.secret_access_key` is returned by the API exactly once, by the request that creates the key, so " +
		"it is in the state and nowhere else; an imported key has none. `user_data` is not supported by the API, so no instance " +
		"data is stored. The provider has no resource for IAM: members, invitations and API keys are managed by people, because no " +
		"API key scope holds `iam.*` (a key cannot mint keys, so a leaked key cannot widen its reach).\n\n")
	b.WriteString("## Import\n\nEvery resource is imported by an id. Attributes the API cannot return are left empty; the resource page says which.\n\n| Resource | Import id |\n| --- | --- |\n")
	names := sorted(imports)
	for _, n := range names {
		fmt.Fprintf(&b, "| `%s` | `%s` |\n", n, imports[n])
	}
	return b.String()
}
