package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	dsschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
)

// secretish names an attribute that holds a credential. An identifier (access_key_id, api_key_id) is the public half
// of a key and is not one.
func secretish(name string) bool {
	if strings.HasSuffix(name, "_id") {
		return false
	}
	for _, word := range []string{"secret", "token", "password", "private_key", "api_key"} {
		if strings.Contains(name, word) {
			return true
		}
	}
	return false
}

// schemaAttr is an attribute reduced to what the guard looks at: the three schema kinds have their own attribute types.
type schemaAttr struct {
	name      string
	sensitive bool
}

// unprotected lists the attributes whose name says they hold a credential and which are not sensitive.
func unprotected(where string, attrs []schemaAttr) []string {
	var bad []string
	for _, a := range attrs {
		if secretish(a.name) && !a.sensitive {
			bad = append(bad, where+"."+a.name)
		}
	}
	return bad
}

func resourceAttrs(s rschema.Schema) []schemaAttr {
	var out []schemaAttr
	for name, a := range s.Attributes {
		out = append(out, schemaAttr{name, a.IsSensitive()})
	}
	return out
}

func dataSourceAttrs(s dsschema.Schema) []schemaAttr {
	var out []schemaAttr
	for name, a := range s.Attributes {
		out = append(out, schemaAttr{name, a.IsSensitive()})
	}
	return out
}

// Every attribute that can hold a credential is sensitive, in every resource, data source and the provider block.
func TestEveryCredentialAttributeIsSensitive(t *testing.T) {
	ctx := context.Background()
	p := New("test")()

	var bad []string
	var ps provider.SchemaResponse
	p.Schema(ctx, provider.SchemaRequest{}, &ps)
	for name, a := range ps.Schema.Attributes {
		bad = append(bad, unprotected("provider", []schemaAttr{{name, a.IsSensitive()}})...)
	}
	for _, newResource := range p.(provider.Provider).Resources(ctx) {
		r := newResource()
		var meta resource.MetadataResponse
		r.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "virtscale"}, &meta)
		var rs resource.SchemaResponse
		r.Schema(ctx, resource.SchemaRequest{}, &rs)
		bad = append(bad, unprotected(meta.TypeName, resourceAttrs(rs.Schema))...)
	}
	for _, newDataSource := range p.(provider.Provider).DataSources(ctx) {
		d := newDataSource()
		var meta datasource.MetadataResponse
		d.Metadata(ctx, datasource.MetadataRequest{ProviderTypeName: "virtscale"}, &meta)
		var ds datasource.SchemaResponse
		d.Schema(ctx, datasource.SchemaRequest{}, &ds)
		bad = append(bad, unprotected(meta.TypeName, dataSourceAttrs(ds.Schema))...)
	}
	if len(bad) > 0 {
		t.Errorf("credential attributes that are not sensitive: %q", bad)
	}
}

// The guard has teeth: a schema with an unprotected credential is reported.
func TestTheSchemaGuardCatchesAnUnprotectedCredential(t *testing.T) {
	s := rschema.Schema{Attributes: map[string]rschema.Attribute{
		"name":          rschema.StringAttribute{Computed: true},
		"access_key_id": rschema.StringAttribute{Computed: true},
		"api_token":     rschema.StringAttribute{Computed: true},
		"password":      rschema.StringAttribute{Computed: true, Sensitive: true},
	}}
	bad := unprotected("virtscale_example", resourceAttrs(s))
	if len(bad) != 1 || bad[0] != "virtscale_example.api_token" {
		t.Errorf("unprotected = %q, want only api_token", bad)
	}
}

func TestTheAccessKeySecretIsSensitive(t *testing.T) {
	var rs resource.SchemaResponse
	NewS3AccessKeyResource().Schema(context.Background(), resource.SchemaRequest{}, &rs)
	if a, ok := rs.Schema.Attributes["secret_access_key"]; !ok || !a.IsSensitive() {
		t.Errorf("virtscale_s3_access_key.secret_access_key must exist and be sensitive")
	}
}
