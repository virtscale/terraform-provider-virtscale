package provider

// Live acceptance test for virtscale_ssh_key.
//
// The proof is the resource's whole lifecycle: an apply imports one freshly
// generated ed25519 key, a second plan is empty, the key imports again by its
// id, and destroy removes it. The key, its fingerprint and its canonical text
// are computed by the test itself, so the checks do not trust the API's own
// arithmetic.
//
// To run it against a real API, set all five variables and run the test:
// TF_ACC=1, VIRTSCALE_LIVE=1, VIRTSCALE_API_KEY, VIRTSCALE_ORGANIZATION and
// VIRTSCALE_ENDPOINT. Without them it skips (see liveSshKeyEnv).
//
// The same code runs in CI against the in-process fake: the "AgainstTheFake"
// callers exercise the lifecycle proof and the live proof body respectively,
// so a mistake in this file is caught without a real API.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"gitlab.virtscale.nl/virtscale/virtscale-platform/terraform-provider-virtscale/internal/client"
)

// liveSshKey is one throwaway key generated for a lifecycle proof. The private
// half is discarded; only these texts are kept.
type liveSshKey struct {
	name          string // "tf-live-" and 8 random lowercase hex characters
	configText    string // the text the configuration holds: type, blob, comment, newline
	canonicalText string // the API's stored form: "<type> <base64>"
	fingerprint   string // "SHA256:" plus the unpadded base64 of the blob's SHA-256
}

// newLiveSshKey generates a fresh ed25519 key and the texts and fingerprint
// that go with it. The wire blob is built by hand: the type name and the
// 32-byte public key, each as a uint32 big-endian length followed by its bytes.
func newLiveSshKey(t *testing.T) liveSshKey {
	t.Helper()

	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("generate a random name suffix: %v", err)
	}
	name := "tf-live-" + hex.EncodeToString(suffix[:])

	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate an ed25519 key: %v", err)
	}

	const keyType = "ssh-ed25519"
	blob := make([]byte, 0, 4+len(keyType)+4+len(publicKey))
	blob = binary.BigEndian.AppendUint32(blob, uint32(len(keyType)))
	blob = append(blob, keyType...)
	blob = binary.BigEndian.AppendUint32(blob, uint32(len(publicKey)))
	blob = append(blob, publicKey...)

	b64 := base64.StdEncoding.EncodeToString(blob)
	sum := sha256.Sum256(blob)

	return liveSshKey{
		name:          name,
		configText:    keyType + " " + b64 + " " + name + "@example.invalid\n",
		canonicalText: keyType + " " + b64,
		fingerprint:   "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]),
	}
}

// runSshKeyLifecycleProof runs the three steps of the proof against whatever
// providerBlock configures: create, an empty second plan, and import by id.
// terraform-plugin-testing destroys everything afterwards and calls
// destroyCheck.
func runSshKeyLifecycleProof(t *testing.T, providerBlock string, destroyCheck resource.TestCheckFunc) {
	t.Helper()

	key := newLiveSshKey(t)
	config := providerBlock + testAccSshKeyBlock("k", key.name, fmt.Sprintf("%q", key.configText))

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             destroyCheck,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(sshKeyAddr, "id"),
					resource.TestCheckResourceAttrSet(sshKeyAddr, "created_at"),
					resource.TestCheckResourceAttr(sshKeyAddr, "name", key.name),
					resource.TestCheckResourceAttr(sshKeyAddr, "key_type", "ssh-ed25519"),
					resource.TestCheckResourceAttr(sshKeyAddr, "fingerprint", key.fingerprint),
					// The state keeps the configuration's text, comment and newline included.
					resource.TestCheckResourceAttr(sshKeyAddr, "public_key", key.configText),
				),
			},
			{
				// The same configuration again: an empty plan is the explicit proof
				// that nothing about the key drifted.
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// Import by the id in the state. public_key is not compared: the state
				// holds the configuration text with its comment and newline, the API
				// the canonical "<type> <base64>" form.
				ResourceName:            sshKeyAddr,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"public_key"},
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 {
						return fmt.Errorf("imported %d resources, want 1", len(states))
					}
					a := states[0].Attributes
					if a["public_key"] != key.canonicalText {
						return fmt.Errorf("the imported state does not hold the canonical public key")
					}
					if a["id"] == "" {
						return fmt.Errorf("the imported state has no id")
					}
					return nil
				},
			},
		},
	})
}

// liveSshKeyEnv returns the three connection values for a live run, or skips
// the test. It names the variables that are not set, never their values.
func liveSshKeyEnv(t *testing.T) (endpoint, apiKey, org string) {
	t.Helper()

	var missing []string
	if os.Getenv("TF_ACC") != "1" {
		missing = append(missing, "TF_ACC=1")
	}
	if os.Getenv("VIRTSCALE_LIVE") != "1" {
		missing = append(missing, "VIRTSCALE_LIVE=1")
	}
	endpoint = os.Getenv("VIRTSCALE_ENDPOINT")
	apiKey = os.Getenv("VIRTSCALE_API_KEY")
	org = os.Getenv("VIRTSCALE_ORGANIZATION")
	for _, name := range []string{"VIRTSCALE_ENDPOINT", "VIRTSCALE_API_KEY", "VIRTSCALE_ORGANIZATION"} {
		if os.Getenv(name) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Skipf("this live acceptance test talks to a real VirtScale API and is skipped: set %s", strings.Join(missing, ", "))
	}
	return endpoint, apiKey, org
}

// runLiveSshKeyProof runs the lifecycle proof with an empty provider block, so
// the provider reads its credentials and endpoint from the environment. The
// API key therefore never enters the Terraform configuration.
func runLiveSshKeyProof(t *testing.T, endpoint, apiKey, org string) {
	t.Helper()
	providerBlock := `
provider "virtscale" {
}
`
	runSshKeyLifecycleProof(t, providerBlock, liveSshKeyDestroyCheck(endpoint, apiKey, org))
}

// liveSshKeyDestroyCheck reads every key the run left in state back from the
// API and requires it to be gone. Errors name the resource address and what
// happened, never the key or the API key.
func liveSshKeyDestroyCheck(endpoint, apiKey, org string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		c, err := client.New(endpoint, apiKey, org)
		if err != nil {
			return fmt.Errorf("build the API client for the destroy check: %v", err)
		}

		found := 0
		for addr, rs := range s.RootModule().Resources {
			if rs.Type != "virtscale_ssh_key" {
				continue
			}
			found++

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_, err := c.GetSshKey(ctx, rs.Primary.ID)
			cancel()

			if client.IsNotFound(err, client.ResourceSshKey) {
				continue
			}
			if err == nil {
				return fmt.Errorf("%s is still in the keystore after destroy", addr)
			}
			if apiErr, ok := err.(*client.APIError); ok {
				return fmt.Errorf("%s: the read after destroy failed with status %d (code %q)", addr, apiErr.Status, apiErr.Code)
			}
			return fmt.Errorf("%s: the read after destroy failed with a %T error", addr, err)
		}
		if found == 0 {
			return fmt.Errorf("no virtscale_ssh_key in the state to check")
		}
		return nil
	}
}

// TestAccSshKeyResource_LifecycleProofAgainstTheFake runs the lifecycle proof
// against the in-process fake. It is nothing live: it proves the test code
// itself in CI, under TF_ACC like the other acceptance tests.
func TestAccSshKeyResource_LifecycleProofAgainstTheFake(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	runSshKeyLifecycleProof(t, testAccProviderBlock(fake), testAccCheckSshKeyDestroy(t, fake))
}

// TestLiveSshKeyResource_LifecycleProof talks to a real API. It skips unless
// TF_ACC=1, VIRTSCALE_LIVE=1 and the endpoint, API key and organization are
// all set.
func TestLiveSshKeyResource_LifecycleProof(t *testing.T) {
	endpoint, apiKey, org := liveSshKeyEnv(t)
	runLiveSshKeyProof(t, endpoint, apiKey, org)
}

// TestAccSshKeyResource_LiveProofBodyAgainstTheFake proves the live body works
// without a real API: it points the environment at the fake and then runs the
// very same empty-provider-block path. The provider runs in the test process,
// so it reads the variables set here itself.
func TestAccSshKeyResource_LiveProofBodyAgainstTheFake(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	t.Setenv("VIRTSCALE_API_KEY", fake.apiKey)
	t.Setenv("VIRTSCALE_ORGANIZATION", fake.org)
	t.Setenv("VIRTSCALE_ENDPOINT", fake.URL())
	runLiveSshKeyProof(t, fake.URL(), fake.apiKey, fake.org)
}
