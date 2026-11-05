package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = &PlansDataSource{}
	_ datasource.DataSourceWithConfigure = &PlansDataSource{}
)

// PlansDataSource implements the virtscale_plans data source: the Managed VPS plans of the price book.
type PlansDataSource struct {
	clientDataSource
}

// NewPlansDataSource is the datasource.DataSource factory registered by the provider.
func NewPlansDataSource() datasource.DataSource {
	return &PlansDataSource{}
}

type plansModel struct {
	ID    types.String `tfsdk:"id"`
	Plans []planModel  `tfsdk:"plans"`
}

type planModel struct {
	ID                types.String `tfsdk:"id"`
	Name              types.String `tfsdk:"name"`
	VCPUs             types.Int64  `tfsdk:"vcpus"`
	RAMMB             types.Int64  `tfsdk:"ram_mb"`
	DiskGB            types.Int64  `tfsdk:"disk_gb"`
	MonthlyPriceCents types.Int64  `tfsdk:"monthly_price_cents"`
	Currency          types.String `tfsdk:"currency"`
	Status            types.String `tfsdk:"status"`
	ManagementLevel   types.String `tfsdk:"management_level"`
	BackupPolicy      types.String `tfsdk:"backup_policy"`
}

// Metadata sets the data source's type name to <provider type name>_plans.
func (d *PlansDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_plans"
}

// Schema declares the virtscale_plans attributes.
func (d *PlansDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	str := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, Description: desc}
	}
	num := func(desc string) schema.Int64Attribute {
		return schema.Int64Attribute{Computed: true, Description: desc}
	}
	resp.Schema = schema.Schema{
		Description: "The Managed VPS plans of the price book, cheapest first.",
		Attributes: map[string]schema.Attribute{
			"id": str("Always plans."),
			"plans": schema.ListNestedAttribute{
				Computed:    true,
				Description: "The plans, in the API's order.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"id":                  str("The plan id."),
					"name":                str("The plan's display name."),
					"vcpus":               num("Virtual CPUs."),
					"ram_mb":              num("Memory, in megabytes."),
					"disk_gb":             num("Disk, in gigabytes."),
					"monthly_price_cents": num("The monthly price in the smallest unit of currency."),
					"currency":            str("ISO 4217, lower case: eur."),
					"status":              str("active or retired."),
					"management_level":    str("full or business."),
					"backup_policy":       str("A label such as daily-7d; null when no backups are included."),
				}},
			},
		},
	}
}

// Read lists the plans.
func (d *PlansDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	plans, err := d.client.ListPlans(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Unable to List Plans", apiErrorDetail(err))
		return
	}
	m := plansModel{ID: types.StringValue("plans"), Plans: make([]planModel, 0, len(plans))}
	for _, p := range plans {
		m.Plans = append(m.Plans, planModel{
			ID: types.StringValue(p.ID), Name: types.StringValue(p.Name), VCPUs: types.Int64Value(int64(p.VCPUs)),
			RAMMB: types.Int64Value(int64(p.RAMMB)), DiskGB: types.Int64Value(int64(p.DiskGB)),
			MonthlyPriceCents: types.Int64Value(int64(p.MonthlyPriceCents)), Currency: types.StringValue(p.Currency),
			Status: types.StringValue(p.Status), ManagementLevel: types.StringValue(p.ManagementLevel),
			BackupPolicy: nullable(p.BackupPolicy),
		})
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
