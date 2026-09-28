package provider

import (
	"context"
	"fmt"
	"sort"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"gitlab.virtscale.nl/virtscale/virtscale-platform/terraform-provider-virtscale/internal/client"
)

// Ensure ImagesDataSource satisfies the expected data source interfaces.
var (
	_ datasource.DataSource              = &ImagesDataSource{}
	_ datasource.DataSourceWithConfigure = &ImagesDataSource{}
)

// ImagesDataSource implements the virtscale_images data source.
type ImagesDataSource struct {
	client *client.Client
}

// NewImagesDataSource is the datasource.DataSource factory registered by the
// provider.
func NewImagesDataSource() datasource.DataSource {
	return &ImagesDataSource{}
}

// imagesDataSourceModel mirrors the virtscale_images schema.
type imagesDataSourceModel struct {
	ID     types.String `tfsdk:"id"`
	Region types.String `tfsdk:"region"`
	Images []imageModel `tfsdk:"images"`
}

// imageModel mirrors one entry of the images list.
type imageModel struct {
	ID        types.String `tfsdk:"id"`
	Name      types.String `tfsdk:"name"`
	OSDistro  types.String `tfsdk:"os_distro"`
	OSVersion types.String `tfsdk:"os_version"`
	MinDiskGB types.Int64  `tfsdk:"min_disk_gb"`
	MinRAMMB  types.Int64  `tfsdk:"min_ram_mb"`
}

// Metadata sets the data source's type name to <provider type name>_images.
func (d *ImagesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_images"
}

// Schema declares the virtscale_images attributes.
func (d *ImagesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "The catalogue of images available to boot a virtscale_instance from, in a given region.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Set to region.",
			},
			"region": schema.StringAttribute{
				Required:    true,
				Description: "The region id to list images for.",
			},
			"images": schema.ListNestedAttribute{
				Computed:    true,
				Description: "The images available in region, ordered by name.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id": schema.StringAttribute{
							Computed:    true,
							Description: "The image id, for use as a virtscale_instance's image attribute.",
						},
						"name": schema.StringAttribute{
							Computed:    true,
							Description: "The image's display name.",
						},
						"os_distro": schema.StringAttribute{
							Computed:    true,
							Description: "The OS distribution, e.g. debian. Null when unknown.",
						},
						"os_version": schema.StringAttribute{
							Computed:    true,
							Description: "The OS version, e.g. 12. Null when unknown.",
						},
						"min_disk_gb": schema.Int64Attribute{
							Computed:    true,
							Description: "The minimum boot disk size, in gigabytes, this image requires.",
						},
						"min_ram_mb": schema.Int64Attribute{
							Computed:    true,
							Description: "The minimum memory, in megabytes, this image requires.",
						},
					},
				},
			},
		},
	}
}

// Configure receives the shared *client.Client the provider built.
func (d *ImagesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}

	c, ok := req.ProviderData.(*client.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Data Source Configure Type",
			fmt.Sprintf("Expected *client.Client, got: %T. Please report this issue.", req.ProviderData),
		)
		return
	}
	d.client = c
}

// Read lists images for region and sets the result, sorted by name for a
// deterministic plan.
func (d *ImagesDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data imagesDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	region := data.Region.ValueString()

	images, err := d.client.ListImages(ctx, region)
	if err != nil {
		resp.Diagnostics.AddError("Unable to List Images", apiErrorDetail(err))
		return
	}

	sort.Slice(images, func(i, j int) bool { return images[i].Name < images[j].Name })

	data.ID = types.StringValue(region)
	data.Images = make([]imageModel, 0, len(images))
	for _, img := range images {
		m := imageModel{
			ID:        types.StringValue(img.ID),
			Name:      types.StringValue(img.Name),
			MinDiskGB: types.Int64Value(int64(img.MinDiskGB)),
			MinRAMMB:  types.Int64Value(int64(img.MinRAMMB)),
		}

		if img.OSDistro != nil {
			m.OSDistro = types.StringValue(*img.OSDistro)
		} else {
			m.OSDistro = types.StringNull()
		}

		if img.OSVersion != nil {
			m.OSVersion = types.StringValue(*img.OSVersion)
		} else {
			m.OSVersion = types.StringNull()
		}

		data.Images = append(data.Images, m)
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
