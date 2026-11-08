package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = &InstanceTypesDataSource{}
	_ datasource.DataSourceWithConfigure = &InstanceTypesDataSource{}
)

// InstanceTypesDataSource implements the virtscale_instance_types data source: what a server can be.
type InstanceTypesDataSource struct {
	clientDataSource
}

// NewInstanceTypesDataSource is the datasource.DataSource factory registered by the provider.
func NewInstanceTypesDataSource() datasource.DataSource {
	return &InstanceTypesDataSource{}
}

type instanceTypesModel struct {
	ID            types.String        `tfsdk:"id"`
	Region        types.String        `tfsdk:"region"`
	InstanceTypes []instanceTypeModel `tfsdk:"instance_types"`
}

type instanceTypeModel struct {
	ID       types.String `tfsdk:"id"`
	VRN      types.String `tfsdk:"vrn"`
	Name     types.String `tfsdk:"name"`
	Family   types.String `tfsdk:"family"`
	CPUKind  types.String `tfsdk:"cpu_kind"`
	VCPUs    types.Int64  `tfsdk:"vcpus"`
	MemoryMB types.Int64  `tfsdk:"memory_mb"`
	DiskGB   types.Int64  `tfsdk:"disk_gb"`
	Regions  types.List   `tfsdk:"regions"`
	Status   types.String `tfsdk:"status"`
}

// Metadata sets the data source's type name to <provider type name>_instance_types.
func (d *InstanceTypesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_instance_types"
}

// Schema declares the virtscale_instance_types attributes.
func (d *InstanceTypesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	str := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, Description: desc}
	}
	num := func(desc string) schema.Int64Attribute {
		return schema.Int64Attribute{Computed: true, Description: desc}
	}
	resp.Schema = schema.Schema{
		Description: "The public instance types, in the order the API lists them (by sort order, then id). The catalogue is empty in production today, so the list is empty there and an instance is sized with vcpus, memory_mb, disk_gb and storage_pool instead.",
		Attributes: map[string]schema.Attribute{
			"region": schema.StringAttribute{Optional: true, Description: "Keep only the types that can be ordered in this region now."},
			"id":     str("Always instance-types."),
			"instance_types": schema.ListNestedAttribute{
				Computed:    true,
				Description: "The instance types, in the API's order.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"id":        str("The type id, for example g1-small."),
					"vrn":       str("vrn:virtscale:instance-type:<id>."),
					"name":      str("The type's display name."),
					"family":    str("general, cpu or memory."),
					"cpu_kind":  str("shared or dedicated."),
					"vcpus":     num("Virtual CPUs."),
					"memory_mb": num("Memory, in megabytes."),
					"disk_gb":   num("Boot disk, in gigabytes."),
					"regions":   schema.ListAttribute{Computed: true, ElementType: types.StringType, Description: "The region ids where the type can be ordered now."},
					"status":    str("active, coming_soon, maintenance or retired."),
				}},
			},
		},
	}
}

// Read lists the instance types, for one region when region is set.
func (d *InstanceTypesDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m instanceTypesModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	types_, err := d.client.ListInstanceTypes(ctx, m.Region.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unable to List Instance Types", apiErrorDetail(err))
		return
	}
	m.ID = types.StringValue("instance-types")
	m.InstanceTypes = make([]instanceTypeModel, 0, len(types_))
	for _, it := range types_ {
		regions := it.Regions
		if regions == nil {
			regions = []string{}
		}
		list, diags := types.ListValueFrom(ctx, types.StringType, regions)
		resp.Diagnostics.Append(diags...)
		m.InstanceTypes = append(m.InstanceTypes, instanceTypeModel{
			ID: types.StringValue(it.ID), VRN: types.StringValue(it.VRN), Name: types.StringValue(it.Name),
			Family: types.StringValue(it.Family), CPUKind: types.StringValue(it.CPUKind),
			VCPUs: types.Int64Value(int64(it.VCPUs)), MemoryMB: types.Int64Value(int64(it.MemoryMB)),
			DiskGB: types.Int64Value(int64(it.DiskGB)), Regions: list, Status: types.StringValue(it.Status),
		})
	}
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
