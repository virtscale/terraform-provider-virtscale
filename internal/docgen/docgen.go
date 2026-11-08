// Package docgen writes the documentation of the provider (docs/) from its schemas and the examples/ directory. The
// pages are what the schemas say, so they cannot drift from them: a test regenerates them and compares (`go generate
// ./...` rewrites them). The guides in docs/guides that a person wrote are not generated: Generate only lists them in the
// index, from the front matter of each. It is used by a test and by tools/gendocs only.
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
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"

	vsprovider "gitlab.virtscale.nl/virtscale/virtscale-platform/terraform-provider-virtscale/internal/provider"
)

// field is one attribute reduced to what a page says about it.
type field struct {
	name                                 string
	typ, desc                            string
	required, optional, computed, secret bool
	replaces                             bool
	validation                           []string // what the validators of the attribute say they accept
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

// validatorTexts is what the validators of an attribute or block say they accept, in the order they are declared.
func validatorTexts(a any) []string {
	ctx := context.Background()
	var out []string
	add := func(d string) {
		if d = oneLine(d); d != "" {
			out = append(out, d)
		}
	}
	if x, ok := a.(interface{ StringValidators() []validator.String }); ok {
		for _, v := range x.StringValidators() {
			add(v.Description(ctx))
		}
	}
	if x, ok := a.(interface{ Int64Validators() []validator.Int64 }); ok {
		for _, v := range x.Int64Validators() {
			add(v.Description(ctx))
		}
	}
	if x, ok := a.(interface{ BoolValidators() []validator.Bool }); ok {
		for _, v := range x.BoolValidators() {
			add(v.Description(ctx))
		}
	}
	if x, ok := a.(interface{ MapValidators() []validator.Map }); ok {
		for _, v := range x.MapValidators() {
			add(v.Description(ctx))
		}
	}
	if x, ok := a.(interface{ ListValidators() []validator.List }); ok {
		for _, v := range x.ListValidators() {
			add(v.Description(ctx))
		}
	}
	if x, ok := a.(interface{ SetValidators() []validator.Set }); ok {
		for _, v := range x.SetValidators() {
			add(v.Description(ctx))
		}
	}
	return out
}

func toField(name string, a attribute) field {
	f := field{name: name, typ: typeName(a.GetType()), desc: a.GetDescription(), required: a.IsRequired(), optional: a.IsOptional(),
		computed: a.IsComputed(), secret: a.IsSensitive(), replaces: forcesReplacement(a), validation: validatorTexts(a)}
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
				validation: validatorTexts(b), nested: resourceFields(b.NestedObject.Attributes, nil)})
		case rschema.ListNestedBlock:
			out = append(out, field{name: name, typ: "Block List", desc: b.Description, optional: true, nestedTitle: name,
				validation: validatorTexts(b), nested: resourceFields(b.NestedObject.Attributes, nil)})
		}
	}
	return out
}

func dataSourceFields(attrs map[string]dsschema.Attribute) []field {
	var out []field
	for _, name := range sorted(attrs) {
		f := toField(name, attrs[name])
		switch a := attrs[name].(type) {
		case dsschema.ListNestedAttribute:
			f.typ, f.nestedTitle, f.nested = "Attributes List", name, dataSourceFields(a.NestedObject.Attributes)
		case dsschema.SingleNestedAttribute:
			f.typ, f.nestedTitle, f.nested = "Attributes", name, dataSourceFields(a.Attributes)
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
	if len(f.validation) > 0 {
		s += " Validation: " + strings.Join(f.validation, "; ") + "."
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

	// the provider page (index.md); the lists of resources and data sources are added when they are known
	var ps provider.SchemaResponse
	p.Schema(ctx, provider.SchemaRequest{}, &ps)
	var index strings.Builder
	index.WriteString(front("Provider: virtscale", ps.Schema.Description))
	index.WriteString("# virtscale Provider\n\n" + ps.Schema.Description + "\n\n## Example Usage\n\n```terraform\n" + read(root, "examples/provider/provider.tf") + "```\n\n## Schema\n\n")
	renderFields(&index, providerFields(ps.Schema.Attributes), 0)
	type entry struct{ name, link, summary string }
	var resourceEntries, dataSourceEntries []entry

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
		link := "resources/" + strings.TrimPrefix(name, "virtscale_") + ".md"
		pages["docs/"+link] = b.String()
		resourceEntries = append(resourceEntries, entry{name, link, firstSentence(rs.Schema.Description)})
		if id := importID(importSh); id != "" {
			imports[name] = id
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
		link := "data-sources/" + strings.TrimPrefix(name, "virtscale_") + ".md"
		pages["docs/"+link] = b.String()
		dataSourceEntries = append(dataSourceEntries, entry{name, link, firstSentence(ds.Schema.Description)})
		for _, f := range fields {
			if f.secret {
				secrets = append(secrets, name+"."+f.name)
			}
		}
	}

	byName := func(es []entry) {
		sort.Slice(es, func(i, j int) bool { return es[i].name < es[j].name })
	}
	byName(resourceEntries)
	byName(dataSourceEntries)
	guides, err := guideEntries(root)
	if err != nil {
		return nil, err
	}
	index.WriteString("## Guides\n\n")
	for _, g := range guides {
		fmt.Fprintf(&index, "- [%s](guides/%s): %s\n", g.title, g.file, g.summary)
	}
	index.WriteString("\n## Resources\n\n")
	for _, e := range resourceEntries {
		fmt.Fprintf(&index, "- [`%s`](%s): %s\n", e.name, e.link, e.summary)
	}
	index.WriteString("\n## Data Sources\n\n")
	for _, e := range dataSourceEntries {
		fmt.Fprintf(&index, "- [`%s`](%s): %s\n", e.name, e.link, e.summary)
	}
	pages["docs/index.md"] = index.String()

	pages["docs/guides/"+secretsGuideFile] = guide(secrets, imports)
	return pages, nil
}

// guideEntry is a guide as the index lists it.
type guideEntry struct{ file, title, summary string }

// frontMatter reads the page_title (without the provider suffix) and the description of a page's front matter, the two
// things the index shows of a guide. ok is false when either is missing.
func frontMatter(page string) (title, description string, ok bool) {
	rest, found := strings.CutPrefix(page, "---\n")
	if !found {
		return "", "", false
	}
	head, _, found := strings.Cut(rest, "\n---\n")
	if !found {
		return "", "", false
	}
	lines := strings.Split(head, "\n")
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "page_title:"):
			title = strings.TrimSuffix(strings.Trim(strings.TrimSpace(strings.TrimPrefix(l, "page_title:")), `"`), pageTitleSuffix)
		case strings.TrimSpace(l) == "description: |-":
			var text []string
			for _, d := range lines[i+1:] {
				if !strings.HasPrefix(d, "  ") { // the block ends at the first line that is not indented
					break
				}
				text = append(text, strings.TrimSpace(d))
			}
			description = strings.Join(text, " ")
		}
	}
	return title, description, title != "" && description != ""
}

// guideEntries lists the guides for the index: the generated one and every page a person wrote in docs/guides, by file
// name. A hand-written page whose front matter lacks a title or a description is an error, because the registry needs both.
func guideEntries(root string) ([]guideEntry, error) {
	entries := []guideEntry{{secretsGuideFile, secretsGuideTitle, secretsGuideDescription}}
	files, err := filepath.Glob(filepath.Join(root, "docs", "guides", "*.md"))
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		file := filepath.Base(f)
		if file == secretsGuideFile {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		title, description, ok := frontMatter(string(b))
		if !ok {
			return nil, fmt.Errorf("docs/guides/%s: the front matter needs a page_title and a description", file)
		}
		entries = append(entries, guideEntry{file, title, description})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].file < entries[j].file })
	return entries, nil
}

// firstSentence is the description up to its first full stop, on one line.
func firstSentence(desc string) string {
	d := oneLine(desc)
	if i := strings.Index(d, ". "); i >= 0 {
		return d[:i+1]
	}
	return d
}

// importID is the id placeholder of the `tofu import <address> <id>` line of an import.sh: everything after the address.
func importID(script string) string {
	for _, l := range strings.Split(script, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(l), "tofu import "); ok {
			if _, id, ok := strings.Cut(rest, " "); ok {
				return strings.TrimSpace(id)
			}
		}
	}
	return ""
}

// The generated guide: its file in docs/guides, and the title and description of its front matter (the index lists them too).
const (
	secretsGuideFile        = "state-secrets-and-import.md"
	secretsGuideTitle       = "State, secrets and import"
	secretsGuideDescription = "What the state of the virtscale provider holds, which attributes are sensitive, and how to import each resource."
	pageTitleSuffix         = " - terraform-provider-virtscale"
)

func guide(secrets []string, imports map[string]string) string {
	sort.Strings(secrets)
	var b strings.Builder
	b.WriteString(front(secretsGuideTitle+pageTitleSuffix, secretsGuideDescription))
	b.WriteString("# State, secrets and import\n\n")
	b.WriteString("## Versions\n\nThe provider speaks protocol 6 and is built and tested with OpenTofu 1.12.5. Terraform 1.x speaks the same " +
		"protocol but is not tested. No attribute is write-only, so no feature of a recent minimum version is needed.\n\n")
	b.WriteString("## Sensitive attributes\n\nA sensitive attribute is hidden in plan and apply output. The sensitive attributes of this provider are:\n\n")
	for _, s := range secrets {
		fmt.Fprintf(&b, "- `%s`\n", s)
	}
	b.WriteString("\n## Where a secret ends up\n\n" +
		"- **The state.** The sensitive attributes of a resource are stored in the state in plain text, as every provider does. " +
		"Keep the state in an encrypted backend (or use OpenTofu's state encryption, OpenTofu 1.7 or newer) and do not print a " +
		"sensitive value in an output without `sensitive = true`. `virtscale_s3_access_key.secret_access_key` is returned by the " +
		"API exactly once, by the request that creates the key, so it is in the state and nowhere else; an imported key has none.\n" +
		"- **Plans and JSON output.** A saved plan (`tofu plan -out`) holds every planned value and the values of the input " +
		"variables in plain text, and `tofu output -json` and `tofu show -json` print sensitive values in clear text: keep them as " +
		"safe as the state.\n" +
		"- **The provider configuration.** `api_key` is not stored in the state, but it is better kept out of configuration files " +
		"and variables: set `VIRTSCALE_API_KEY` in the environment.\n" +
		"- **Instance data.** `virtscale_instance.user_data` is cloud-init text, and cloud-init is where bootstrap secrets go: " +
		"it is sensitive, so plan and apply output hide it, but the state holds it as text, like every sensitive attribute. " +
		"The API never returns it, so an imported instance has none.\n" +
		"- **IAM.** The provider has no resource for IAM: members, invitations and API keys are managed by people, because no " +
		"API key scope holds `iam.*` (a key cannot mint keys, so a leaked key cannot widen its reach).\n\n")
	b.WriteString("## Import\n\nEvery resource is imported by the id shown below; a VPC is addressed through its project, so its id has two parts. " +
		"Attributes the API cannot return are left empty; the resource page says which.\n\n| Resource | Import id |\n| --- | --- |\n")
	names := sorted(imports)
	for _, n := range names {
		fmt.Fprintf(&b, "| `%s` | `%s` |\n", n, imports[n])
	}
	return b.String()
}
