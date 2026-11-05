package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = &RegionsDataSource{}
	_ datasource.DataSourceWithConfigure = &RegionsDataSource{}
)

// RegionsDataSource implements the virtscale_regions data source: every region this organization may see.
type RegionsDataSource struct {
	clientDataSource
}

// NewRegionsDataSource is the datasource.DataSource factory registered by the provider.
func NewRegionsDataSource() datasource.DataSource {
	return &RegionsDataSource{}
}

type regionsModel struct {
	ID      types.String  `tfsdk:"id"`
	Regions []regionModel `tfsdk:"regions"`
}

type regionModel struct {
	ID         types.String        `tfsdk:"id"`
	Name       types.String        `tfsdk:"name"`
	Datacenter types.String        `tfsdk:"datacenter"`
	Status     types.String        `tfsdk:"status"`
	Access     types.String        `tfsdk:"access"`
	Entitled   types.Bool          `tfsdk:"entitled"`
	Default    types.Bool          `tfsdk:"default"`
	DiskTypes  []regionDiskTypeRow `tfsdk:"disk_types"`
}

type regionDiskTypeRow struct {
	DiskType types.String `tfsdk:"disk_type"`
	Status   types.String `tfsdk:"status"`
}

// Metadata sets the data source's type name to <provider type name>_regions.
func (d *RegionsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_regions"
}

// Schema declares the virtscale_regions attributes.
func (d *RegionsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	str := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, Description: desc}
	}
	resp.Schema = schema.Schema{
		Description: "The regions this organization may see, in the order the API lists them.",
		Attributes: map[string]schema.Attribute{
			"id": str("Always regions."),
			"regions": schema.ListNestedAttribute{
				Computed:    true,
				Description: "The regions, in the API's order.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"id":         str("The region id, for a resource's region attribute."),
					"name":       str("The region's display name."),
					"datacenter": str("The datacenter the region is in."),
					"status":     str("active, coming_soon, maintenance or retired."),
					"access":     str("public: every organization may use it. restricted: only selected organizations."),
					"entitled":   schema.BoolAttribute{Computed: true, Description: "Whether this organization may use the region."},
					"default":    schema.BoolAttribute{Computed: true, Description: "Whether this is the default region, the one a panel preselects."},
					"disk_types": schema.ListNestedAttribute{
						Computed:    true,
						Description: "The disk types a new instance may name in this region, and whether each is offered yet.",
						NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
							"disk_type": str("ssd or hdd."),
							"status":    str("active, coming_soon, maintenance or retired."),
						}},
					},
				}},
			},
		},
	}
}

// Read lists the regions.
func (d *RegionsDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	regions, err := d.client.ListRegions(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Unable to List Regions", apiErrorDetail(err))
		return
	}
	m := regionsModel{ID: types.StringValue("regions"), Regions: make([]regionModel, 0, len(regions))}
	for _, r := range regions {
		row := regionModel{
			ID: types.StringValue(r.ID), Name: types.StringValue(r.Name), Datacenter: types.StringValue(r.Datacenter),
			Status: types.StringValue(r.Status), Access: types.StringValue(r.Access),
			Entitled: types.BoolValue(r.Entitled), Default: types.BoolValue(r.Default),
			DiskTypes: make([]regionDiskTypeRow, 0, len(r.DiskTypes)),
		}
		for _, dt := range r.DiskTypes {
			row.DiskTypes = append(row.DiskTypes, regionDiskTypeRow{DiskType: types.StringValue(dt.DiskType), Status: types.StringValue(dt.Status)})
		}
		m.Regions = append(m.Regions, row)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
