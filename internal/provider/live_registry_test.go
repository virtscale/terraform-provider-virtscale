package provider

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

// TestLiveRegistryRelease_SshKeyProof runs a short SSH key proof with the provider as a user gets it: a release from
// registry.terraform.io, which tofu init downloads and checks against the signed SHA256SUMS, not the build of this
// checkout. It skips like the other live tests (liveSshKeyEnv).
//
// The staging API key reaches the downloaded binary, so the version is pinned exactly (like tofu in the live script):
// a new release is tested by bumping registryReleaseVersion in a reviewed change, never by a range.
const registryReleaseVersion = "0.1.0"

func TestLiveRegistryRelease_SshKeyProof(t *testing.T) {
	endpoint, apiKey, org := liveSshKeyEnv(t)
	key := newLiveSshKey(t)
	config := "\nprovider \"virtscale\" {\n}\n" + testAccSshKeyBlock("k", key.name, fmt.Sprintf("%q", key.configText))

	resource.Test(t, resource.TestCase{
		PreCheck: func() { testAccPreCheck(t) },
		ExternalProviders: map[string]resource.ExternalProvider{
			"virtscale": {Source: "registry.terraform.io/virtscale/virtscale", VersionConstraint: "= " + registryReleaseVersion},
		},
		CheckDestroy: liveSshKeyDestroyCheck(endpoint, apiKey, org),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(sshKeyAddr, "id"),
					resource.TestCheckResourceAttr(sshKeyAddr, "name", key.name),
					resource.TestCheckResourceAttr(sshKeyAddr, "key_type", "ssh-ed25519"),
					resource.TestCheckResourceAttr(sshKeyAddr, "fingerprint", key.fingerprint),
				),
			},
			{
				// The same configuration again: an empty plan is the explicit proof that nothing drifted.
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}
