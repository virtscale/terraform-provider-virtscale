package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"gitlab.virtscale.nl/virtscale/virtscale-platform/terraform-provider-virtscale/internal/client"
)

var (
	_ datasource.DataSource              = &ComputeLimitsDataSource{}
	_ datasource.DataSourceWithConfigure = &ComputeLimitsDataSource{}
)

// ComputeLimitsDataSource implements the virtscale_compute_limits data source: the size grid of an instance
// (GET compute/limits).
type ComputeLimitsDataSource struct {
	clientDataSource
}

// NewComputeLimitsDataSource is the datasource.DataSource factory registered by the provider.
func NewComputeLimitsDataSource() datasource.DataSource {
	return &ComputeLimitsDataSource{}
}

type computeLimitsModel struct {
	ID         types.String `tfsdk:"id"`
	VCPUsSteps []int64      `tfsdk:"vcpus_steps"`
	MemoryGB   rangeModel   `tfsdk:"memory_gb"`
	DiskGB     rangeModel   `tfsdk:"disk_gb"`
}

type rangeModel struct {
	Min  int64 `tfsdk:"min"`
	Max  int64 `tfsdk:"max"`
	Step int64 `tfsdk:"step"`
}

// Metadata sets the data source's type name to <provider type name>_compute_limits.
func (d *ComputeLimitsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_compute_limits"
}

// Schema declares the virtscale_compute_limits attributes; all of them are computed.
func (d *ComputeLimitsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	rng := func(desc string) schema.SingleNestedAttribute {
		num := func(desc string) schema.Int64Attribute {
			return schema.Int64Attribute{Computed: true, Description: desc}
		}
		return schema.SingleNestedAttribute{Computed: true, Description: desc, Attributes: map[string]schema.Attribute{
			"min":  num("The smallest value, inclusive."),
			"max":  num("The largest value, inclusive."),
			"step": num("Values are min plus a whole number of steps."),
		}}
	}
	resp.Schema = schema.Schema{
		Description: "The size grid a new instance must sit on: the vCPU counts, and the memory and disk ranges with their steps. " +
			"Give an instance sizes from it (vcpus, memory_mb, disk_gb); a size off the grid is refused with 422 invalid_spec. " +
			"The grid says what is valid, not what your quota has room for.",
		Attributes: map[string]schema.Attribute{
			"id":          schema.StringAttribute{Computed: true, Description: "Always compute-limits."},
			"vcpus_steps": schema.ListAttribute{Computed: true, ElementType: types.Int64Type, Description: "The vCPU counts an instance may have, for example 1, 2, 4, 8, 16, 32."},
			"memory_gb":   rng("Memory in whole gigabytes. The instance's memory_mb is this times 1024."),
			"disk_gb":     rng("Disk in gigabytes."),
		},
	}
}

// Read asks for the grid.
func (d *ComputeLimitsDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	l, err := d.client.GetComputeLimits(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Unable to Read Compute Limits", apiErrorDetail(err))
		return
	}
	m := computeLimitsModel{
		ID: types.StringValue("compute-limits"), VCPUsSteps: make([]int64, 0, len(l.VCPUsSteps)),
		MemoryGB: toRangeModel(l.MemoryGB), DiskGB: toRangeModel(l.DiskGB),
	}
	for _, v := range l.VCPUsSteps {
		m.VCPUsSteps = append(m.VCPUsSteps, int64(v))
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func toRangeModel(r client.Range) rangeModel {
	return rangeModel{Min: int64(r.Min), Max: int64(r.Max), Step: int64(r.Step)}
}
