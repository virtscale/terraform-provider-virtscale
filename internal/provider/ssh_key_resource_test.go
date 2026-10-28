package provider

// Acceptance tests for virtscale_ssh_key (the TF7 plan, §4 and §5 TF7-7). They
// share the TF_ACC harness of instance_resource_test.go and run against the
// in-process fake of the customer API, whose keystore routes follow the plan's
// §4 contract. Only run with TF_ACC=1 and tofu on PATH.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// Public keys that OpenSSH (ssh-keygen 10.0p2) made for these tests, and the
// fingerprints ssh-keygen -lf printed for them. The private halves were
// throwaway and are gone, so nothing here can sign in anywhere. The ECDSA
// P-256 key ends in one "=": the two low bits of its last base64 character are
// spare, so that key can be written down in two texts.
const (
	testKeyEd25519            = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJy5P70jujhUnXA7S+eGNpaimjfDmOvUKyR0se2QLF4m"
	testKeyEd25519Comment     = "tf7-ed25519@example.invalid"
	testKeyEd25519Fingerprint = "SHA256:Kirymlh+tnaM+XM+/AGm3lcaJt2h3bMTySZc4HKX8RU"

	testKeyOther            = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIF8NvSdivzDTshV7EPTO9V0tBdR0OgzEI2fpNCgkcL5"
	testKeyOtherComment     = "tf7-other@example.invalid"
	testKeyOtherFingerprint = "SHA256:5T8wL/AW6wAv7o+r67vGhy7qDTp5LZq7lPupJPoZbCo"

	testKeyECDSA256            = "ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBHvylk61e362acptiaTprbMLF011uvqyX2oppytngn/u5hqIsJNVGkKo9jkqqOr+Xbzcw4yvmGRXgcRY/l5Jis0="
	testKeyECDSA256Comment     = "tf7-ecdsa@example.invalid"
	testKeyECDSA256Fingerprint = "SHA256:DLUBy/PVtQwrvgTdlgbtd7VWj2qD6DWfavkvKKNRLsg"

	testKeyECDSA384            = "ecdsa-sha2-nistp384 AAAAE2VjZHNhLXNoYTItbmlzdHAzODQAAAAIbmlzdHAzODQAAABhBGneK8Rf50//RJiFsih3wNkLQAOhd1F49D/6lejtb9Sgi5QLTFVCuH3htwV++6wrQnHdF4NpId7qTEKKuvpfS+7nV6zigvMa4PdQSCwwcej9WdQVvSl3qMNNc4DqtAd5Mg=="
	testKeyECDSA384Comment     = "tf7-ecdsa384@example.invalid"
	testKeyECDSA384Fingerprint = "SHA256:kpoKlfpeOmK8S0fvwTVfyjGBKDJp59/yugzaUNo3kN0"

	testKeyRSA2048            = "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQC35lj8/GLhrJ9MYHehZkGVgGtAPMLk8A94t1JQIfHu628ffQdwSFSAtGSxll5qI6y58APLXDG2201C0EI8V8+SokN/EuXmJvjogcYr6yX2OKIQEMXx144BcVk1F+n/bvgKZBrtFzkjXzQNO/bfXCtdOxWS9kyNGfDjhi5mRZ8kcQNMv0v1NxyziJw6s7xvB3BMrcfUTuBy4SOr1Hr8u4srL9/Mw3vqUUA2ekkT++snVjgTSGndBRXIuakoGFxTjmkMT0PAP2ZjZzc9+jddo5yhRpa7XDVGJdicRxmWirgxtJKTes8HxG9DqYXEucKLkRDJoL3YYkFcTQtv2HxktpoL"
	testKeyRSA2048Comment     = "tf7-rsa@example.invalid"
	testKeyRSA2048Fingerprint = "SHA256:vKYq2NWJk1hrgxOvgrOfxYnUY7+q1pOOEVZE+QN15dY"

	testKeyRSA1024            = "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAAAgQC1r6HYALKgxdwi7nZeD7+f5NDdPtX5c8VG3kyn8UtC9bbBDq1NqOtc3swEIZVHCWNu+S+IF1t4uMoI082mJtaUYPsnzZFL9xJqKXuTSQbsyMQeYyRrSVusceBng0EOXFVH6fnZAs52vCRgn3FsycZzXIgGiVXrC+DIGy3bVBb0Ew=="
	testKeyRSA1024Comment     = "tf7-rsa1024@example.invalid"
	testKeyRSA1024Fingerprint = "SHA256:fzIB/jx4zXUFDiiQdnzQlY1vOPJGLDdg1XhU5Ygk8AI"
)

const sshKeyAddr = "virtscale_ssh_key.k"

// withSpareBitsFlipped returns key with the lowest bit of its last base64
// character flipped. When that character carries spare bits (the blob does not
// fill a whole number of base64 groups) the text differs and the key does not.
func withSpareBitsFlipped(t *testing.T, key string) string {
	t.Helper()
	typ, blob, ok := strings.Cut(key, " ")
	if !ok || !strings.HasSuffix(blob, "=") {
		t.Fatalf("%q has no base64 padding, so its last character has no spare bits", key)
	}
	b := []byte(strings.TrimRight(blob, "="))
	pad := blob[len(b):]
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	v := strings.IndexByte(alphabet, b[len(b)-1])
	if v < 0 {
		t.Fatalf("%q does not end in a base64 character", key)
	}
	b[len(b)-1] = alphabet[v^1]
	return typ + " " + string(b) + pad
}

// testAccSshKeyBlock is one virtscale_ssh_key; publicKeyExpr is an HCL
// expression, such as a quoted string or file(...).
func testAccSshKeyBlock(label, name, publicKeyExpr string) string {
	return fmt.Sprintf(`
resource "virtscale_ssh_key" %q {
  name       = %q
  public_key = %s
}
`, label, name, publicKeyExpr)
}

// testAccSshKeyConfig is the provider and the one key "k", whose public_key is
// the given text.
func testAccSshKeyConfig(fake *fakeAPI, name, publicKey string) string {
	return testAccProviderBlock(fake) + testAccSshKeyBlock("k", name, fmt.Sprintf("%q", publicKey))
}

// sshKeyIDFromState captures the id of virtscale_ssh_key.k into *dst.
func sshKeyIDFromState(dst *string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[sshKeyAddr]
		if !ok {
			return fmt.Errorf("%s not found in state", sshKeyAddr)
		}
		*dst = rs.Primary.ID
		return nil
	}
}

// testAccCheckSshKeyDestroy checks that no virtscale_ssh_key the test created
// is still in the keystore: the API answers 404 with the ssh_key sentence.
func testAccCheckSshKeyDestroy(t *testing.T, fake *fakeAPI) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		found := 0
		for addr, rs := range s.RootModule().Resources {
			if rs.Type != "virtscale_ssh_key" {
				continue
			}
			found++
			status, _, body := fakeHTTP(t, fake, fake.apiKey, http.MethodGet, sshKeysPath(fake.org)+"/"+rs.Primary.ID, "")
			p := decodeObject(t, body)
			if status != http.StatusNotFound || p["code"] != "not_found" ||
				p["detail"] != "no ssh_key matching that identifier is visible to this organization" {
				return fmt.Errorf("%s: after destroy the API answers %d %v %v, want 404 not_found about the ssh_key", addr, status, p["code"], p["detail"])
			}
		}
		if found == 0 {
			return fmt.Errorf("no virtscale_ssh_key in the state to check")
		}
		return nil
	}
}

// sortedVariantNames orders the names of notFoundVariants, so a test runs them
// in the same order every time.
func sortedVariantNames(variants map[string]func(*reqCtx)) []string {
	names := make([]string, 0, len(variants))
	for name := range variants {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestAccSshKeyResource_CreateAndTwoCycle: a key read from a file (the key, a
// comment, a newline: what file("~/.ssh/id_ed25519.pub") returns) imports, gets
// its id, fingerprint and type from the API, and a second plan is empty even
// though the API stores the key without the comment.
func TestAccSshKeyResource_CreateAndTwoCycle(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	text := testKeyEd25519 + " " + testKeyEd25519Comment + "\n"
	pubFile := filepath.Join(t.TempDir(), "id_ed25519.pub")
	if err := os.WriteFile(pubFile, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	config := testAccProviderBlock(fake) + testAccSshKeyBlock("k", "laptop", fmt.Sprintf("file(%q)", pubFile))
	var id string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckSshKeyDestroy(t, fake),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					sshKeyIDFromState(&id),
					resource.TestCheckResourceAttrSet(sshKeyAddr, "id"),
					resource.TestCheckResourceAttr(sshKeyAddr, "name", "laptop"),
					resource.TestCheckResourceAttr(sshKeyAddr, "key_type", "ssh-ed25519"),
					resource.TestCheckResourceAttr(sshKeyAddr, "fingerprint", testKeyEd25519Fingerprint),
					resource.TestCheckResourceAttrSet(sshKeyAddr, "created_at"),
					// The state keeps what the configuration says; the API holds the canonical form.
					resource.TestCheckResourceAttr(sshKeyAddr, "public_key", text),
					func(*terraform.State) error {
						if k, ok := fake.SSHKey(id); !ok || k.PublicKey != testKeyEd25519 || k.Name != "laptop" {
							return fmt.Errorf("the keystore holds %+v, want laptop with the key and no comment", k)
						}
						return nil
					},
				),
			},
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
	if keys := fake.SSHKeyIdempotencyKeys(); len(keys) != 1 || keys[0] == "" {
		t.Errorf("Idempotency-Keys of the key imports = %v, want one fresh key", keys)
	}
}

// TestAccSshKeyResource_TheSameKeyInAnotherFormIsAnUpdateWithoutAnAPICall: a
// change of the text that leaves the key alone (a comment, CRLF, whitespace,
// spare bits in the last base64 character) is not a replacement and not an API
// call: the plan is an in-place update that stores the configuration text. The
// comparison is of the key type and the decoded bytes, not of the text.
func TestAccSshKeyResource_TheSameKeyInAnotherFormIsAnUpdateWithoutAnAPICall(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	var id string
	var before int

	toForm := func(text string) resource.TestStep {
		return resource.TestStep{
			PreConfig: func() { before = len(fake.Calls()) },
			Config:    testAccSshKeyConfig(fake, "laptop", text),
			ConfigPlanChecks: resource.ConfigPlanChecks{
				PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(sshKeyAddr, plancheck.ResourceActionUpdate),
					// The attributes the API owns stay known through a text-only update
					// (UseStateForUnknown): without it the plan would show them as "known
					// after apply" and anything reading them, such as the example's
					// fingerprint output, would show a change.
					plancheck.ExpectKnownValue(sshKeyAddr, tfjsonpath.New("fingerprint"), knownvalue.StringExact(testKeyECDSA256Fingerprint)),
					plancheck.ExpectKnownValue(sshKeyAddr, tfjsonpath.New("key_type"), knownvalue.StringExact("ecdsa-sha2-nistp256")),
					plancheck.ExpectKnownValue(sshKeyAddr, tfjsonpath.New("id"), knownvalue.StringRegexp(regexp.MustCompile(`.+`))),
					plancheck.ExpectKnownValue(sshKeyAddr, tfjsonpath.New("created_at"), knownvalue.StringRegexp(regexp.MustCompile(`.+`))),
				},
			},
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(sshKeyAddr, "public_key", text),
				func(s *terraform.State) error {
					if got := mutating(fake.CallsSince(before)); len(got) != 0 {
						return fmt.Errorf("the update called the API: %v", got)
					}
					if rs := s.RootModule().Resources[sshKeyAddr]; rs == nil || rs.Primary.ID != id {
						return fmt.Errorf("the key was replaced: its id is no longer %s", id)
					}
					if k, _ := fake.SSHKey(id); k.PublicKey != testKeyECDSA256 {
						return fmt.Errorf("the keystore's copy changed to %q", k.PublicKey)
					}
					return nil
				},
			),
		}
	}

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckSshKeyDestroy(t, fake),
		Steps: []resource.TestStep{
			{
				Config: testAccSshKeyConfig(fake, "laptop", testKeyECDSA256),
				Check:  sshKeyIDFromState(&id),
			},
			toForm(testKeyECDSA256 + " tf7-ecdsa@example.invalid"),
			// Whitespace, tabs between the fields and CRLF at once; each form has its own
			// case in TestPublicKeysEquivalent.
			toForm("  \t" + strings.Replace(testKeyECDSA256, " ", "\t", 1) + " a comment with several words\r\n\r\n"),
			toForm(withSpareBitsFlipped(t, testKeyECDSA256)),
		},
	})
}

// TestAccSshKeyResource_ADifferentKeyOrNameIsAReplacement: another key, or
// another name, is a different resource: destroy, then create.
func TestAccSshKeyResource_ADifferentKeyOrNameIsAReplacement(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	var first, second, third string

	replaced := func(prev *string, id *string) resource.TestCheckFunc {
		return func(s *terraform.State) error {
			if err := sshKeyIDFromState(id)(s); err != nil {
				return err
			}
			if *id == *prev {
				return fmt.Errorf("the key kept id %s: it was not replaced", *id)
			}
			return nil
		}
	}

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckSshKeyDestroy(t, fake),
		Steps: []resource.TestStep{
			{
				Config: testAccSshKeyConfig(fake, "laptop", testKeyEd25519),
				Check: resource.ComposeAggregateTestCheckFunc(
					sshKeyIDFromState(&first),
					resource.TestCheckResourceAttr(sshKeyAddr, "fingerprint", testKeyEd25519Fingerprint),
				),
			},
			{
				// Another key under the same name.
				Config: testAccSshKeyConfig(fake, "laptop", testKeyOther),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(sshKeyAddr, plancheck.ResourceActionDestroyBeforeCreate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					replaced(&first, &second),
					resource.TestCheckResourceAttr(sshKeyAddr, "fingerprint", testKeyOtherFingerprint),
					resource.TestCheckResourceAttr(sshKeyAddr, "key_type", "ssh-ed25519"),
				),
			},
			{
				// The same key under another name.
				Config: testAccSshKeyConfig(fake, "desktop", testKeyOther),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(sshKeyAddr, plancheck.ResourceActionDestroyBeforeCreate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					replaced(&second, &third),
					resource.TestCheckResourceAttr(sshKeyAddr, "name", "desktop"),
				),
			},
		},
	})
	if n := fake.SSHKeyCount(); n != 0 {
		t.Errorf("%d key(s) left in the keystore after destroy", n)
	}
}

// TestAccSshKeyResource_ImportByID: a key imports by its id, and everything
// reads back as it was created.
func TestAccSshKeyResource_ImportByID(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckSshKeyDestroy(t, fake),
		Steps: []resource.TestStep{
			{Config: testAccSshKeyConfig(fake, "laptop", testKeyEd25519)},
			{
				ResourceName:            sshKeyAddr,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{},
			},
		},
	})
}

// TestAccSshKeyResource_ImportOfAnUnknownIDFails: importing an id that is not
// in the keystore is refused as a missing object (Read sees the ssh_key 404 and
// drops it), not as a failure of the API.
func TestAccSshKeyResource_ImportOfAnUnknownIDFails(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckSshKeyDestroy(t, fake),
		Steps: []resource.TestStep{
			{Config: testAccSshKeyConfig(fake, "laptop", testKeyEd25519)},
			{
				ResourceName:  sshKeyAddr,
				ImportState:   true,
				ImportStateId: testSomeUUID,
				ExpectError:   wrapSafe("Cannot import non-existent remote object"),
			},
		},
	})
}

// TestAccSshKeyResource_ImportThenACommentedKeyIsAnUpdateWithoutAnAPICall: after
// tofu import the state holds the API's canonical text, and the configuration
// holds file("~/.ssh/id_ed25519.pub") with its comment. The two differ as text
// and not as keys, so the plan is an in-place update that calls nothing and
// stores the configuration text. It must never be a replacement of the same key.
func TestAccSshKeyResource_ImportThenACommentedKeyIsAnUpdateWithoutAnAPICall(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	commented := testKeyEd25519 + " " + testKeyEd25519Comment + "\n"
	config := testAccSshKeyConfig(fake, "laptop", commented)
	var id string
	var before int

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckSshKeyDestroy(t, fake),
		Steps: []resource.TestStep{
			{
				// The key exists already, made out of band; the state does not know it
				// (the plan wants to create it). This step also fixes the configuration
				// the import below runs against.
				PreConfig: func() {
					id = importKeyVia(t, fake, "laptop", commented)["id"].(string)
				},
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				ResourceName:       sshKeyAddr,
				ImportState:        true,
				ImportStatePersist: true,
				ImportStateIdFunc:  func(*terraform.State) (string, error) { return id, nil },
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 {
						return fmt.Errorf("imported %d resources, want 1", len(states))
					}
					a := states[0].Attributes
					if a["public_key"] != testKeyEd25519 || a["name"] != "laptop" || a["fingerprint"] != testKeyEd25519Fingerprint || a["key_type"] != "ssh-ed25519" {
						return fmt.Errorf("imported attributes = %v, want the API's canonical view of the key", a)
					}
					return nil
				},
			},
			{
				PreConfig: func() { before = len(fake.Calls()) },
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(sshKeyAddr, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(sshKeyAddr, "public_key", commented),
					resource.TestCheckResourceAttrPtr(sshKeyAddr, "id", &id),
					func(*terraform.State) error {
						if got := mutating(fake.CallsSince(before)); len(got) != 0 {
							return fmt.Errorf("the update called the API: %v", got)
						}
						return nil
					},
				),
			},
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

// TestAccSshKeyResource_VanishedOutOfBand: a key deleted out of band is dropped
// from the state by Read (the API's 404 says it is the ssh_key that is gone),
// so the next plan creates it again.
func TestAccSshKeyResource_VanishedOutOfBand(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	var first, second string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckSshKeyDestroy(t, fake),
		Steps: []resource.TestStep{
			{
				Config: testAccSshKeyConfig(fake, "laptop", testKeyEd25519),
				Check:  sshKeyIDFromState(&first),
			},
			{
				PreConfig:          func() { fake.VanishSSHKey(first) },
				Config:             testAccSshKeyConfig(fake, "laptop", testKeyEd25519),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config: testAccSshKeyConfig(fake, "laptop", testKeyEd25519),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(sshKeyAddr, plancheck.ResourceActionCreate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					sshKeyIDFromState(&second),
					func(*terraform.State) error {
						if second == first {
							return fmt.Errorf("the key was not created again")
						}
						return nil
					},
				),
			},
		},
	})
}

// TestAccSshKeyResource_ReadErrorsThatAreNotTheKeyBeingGoneKeepTheResource: a
// 404 means "this key is gone" only when it carries the ssh_key sentence of the
// API's not_found helper. A mistyped endpoint, a proxy's page and the sentence
// for something else are errors, and the key stays in the state: reading them
// as "gone" would empty the state, and the next apply would import it again.
func TestAccSshKeyResource_ReadErrorsThatAreNotTheKeyBeingGoneKeepTheResource(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	config := testAccSshKeyConfig(fake, "laptop", testKeyEd25519)
	variants := notFoundVariants()

	steps := []resource.TestStep{{Config: config}}
	for _, name := range sortedVariantNames(variants) {
		respond := variants[name]
		steps = append(steps, resource.TestStep{
			PreConfig: func() {
				fake.ClearFaults()
				fake.FailNext(1000, matchRoute(http.MethodGet, "/compute/ssh-keys/"), respond)
			},
			Config:      config,
			PlanOnly:    true,
			ExpectError: wrapSafe("Unable to Read SSH Key"),
		})
	}
	steps = append(steps, resource.TestStep{
		// Faults gone, the key is still tracked: the plan is empty.
		PreConfig: fake.ClearFaults,
		Config:    config,
		PlanOnly:  true,
	})

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckSshKeyDestroy(t, fake),
		Steps:                    steps,
	})
}

// TestAccSshKeyResource_DestroyTreatsOnlyTheKeyBeingGoneAsDone: the ssh_key 404
// on the DELETE is success (the key is gone, which is what destroy wanted); the
// same status from a mistyped endpoint is an error, and the key stays tracked.
func TestAccSshKeyResource_DestroyTreatsOnlyTheKeyBeingGoneAsDone(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	var id string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: testAccSshKeyConfig(fake, "laptop", testKeyEd25519),
				Check:  sshKeyIDFromState(&id),
			},
			{
				PreConfig: func() {
					fake.FailNext(1000, matchRoute(http.MethodDelete, "/compute/ssh-keys/"), respondNoSuchRoute)
				},
				Config:      testAccProviderBlock(fake),
				ExpectError: wrapSafe("Unable to Delete SSH Key"),
			},
			{
				// The key is still there; the DELETE is processed and its answer is
				// the 404 about the key itself, as when something else deleted it first.
				PreConfig: func() {
					fake.ClearFaults()
					if _, ok := fake.SSHKey(id); !ok {
						t.Error("the refused delete removed the key")
					}
					fake.FailNextAfterHandling(1, matchRoute(http.MethodDelete, "/compute/ssh-keys/"), respondNotFoundOf("ssh_key"))
				},
				Config: testAccProviderBlock(fake),
			},
		},
	})
	if _, ok := fake.SSHKey(id); ok {
		t.Error("the key is still in the keystore")
	}
}

// TestAccSshKeyResource_TheAPIsRefusalsAreShown: whatever the API refuses
// reaches the user with its code and its own words, so a bad key still gets to
// the API and its 422 explains it. Nothing is stored.
func TestAccSshKeyResource_TheAPIsRefusalsAreShown(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config:      testAccSshKeyConfig(fake, "laptop", "this is not a key at all"),
				ExpectError: wrapSafe("invalid_public_key"),
			},
			{
				Config:      testAccSshKeyConfig(fake, "laptop", "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\n-----END OPENSSH PRIVATE KEY-----\n"),
				ExpectError: wrapSafe("this is a private key; upload the public key (the .pub file) instead. Nothing was stored."),
			},
			{
				Config:      testAccSshKeyConfig(fake, "laptop", testKeyRSA1024),
				ExpectError: wrapSafe("RSA keys must be 2048 to 16384 bits; this one has 1024"),
			},
			{
				Config:      testAccSshKeyConfig(fake, "-not a name", testKeyEd25519),
				ExpectError: wrapSafe("invalid_name"),
			},
			{
				PreConfig:   func() { importKeyVia(t, fake, "laptop", testKeyOther) },
				Config:      testAccSshKeyConfig(fake, "laptop", testKeyEd25519),
				ExpectError: wrapSafe("name_taken"),
			},
		},
	})
	if n := fake.SSHKeyCount(); n != 1 {
		t.Errorf("keystore holds %d keys, want only the one made out of band", n)
	}
}

// TestAccSshKeyResource_QuotaExceeded: the organization's key quota is enforced
// by the API, and its refusal reaches the user.
func TestAccSshKeyResource_QuotaExceeded(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.SetSSHKeyQuota(1)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: testAccProviderBlock(fake) +
					testAccSshKeyBlock("a", "first", fmt.Sprintf("%q", testKeyEd25519)) +
					testAccSshKeyBlock("b", "second", fmt.Sprintf("%q", testKeyOther)),
				ExpectError: wrapSafe("quota_exceeded"),
			},
		},
	})
}

// TestAccSshKeyResource_NotSwitchedOnIsShownAndNotRetried: while the keystore is
// off the API answers 503 not_ready, which says SSH keys are not switched on for
// this API yet. It is no hiccup: the request is sent once.
func TestAccSshKeyResource_NotSwitchedOnIsShownAndNotRetried(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	fake.SetSSHKeysOn(false)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config:      testAccSshKeyConfig(fake, "laptop", testKeyEd25519),
				ExpectError: wrapSafe("not_ready: SSH keys are not switched on yet"),
			},
		},
	})
	if n := fake.CallCount("POST compute/ssh-keys"); n != 1 {
		t.Errorf("the import was sent %d times, want exactly once: not_ready is never retried", n)
	}
}

// TestAccSshKeyResource_ScopesAreNamed: the four compute.ssh_key.* permissions
// come with compute:write (and the reads with compute:read), so a read-only key
// can plan but not import, and is told which scope to add.
func TestAccSshKeyResource_ScopesAreNamed(t *testing.T) {
	fake := newFakeAPI(t, "write-key", testOrgID)
	fake.AddKey("compute-read", "compute:read")

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckSshKeyDestroy(t, fake),
		Steps: []resource.TestStep{
			{
				Config:      testAccProviderBlockWith(fake, "compute-read", fake.org) + testAccSshKeyBlock("k", "laptop", fmt.Sprintf("%q", testKeyEd25519)),
				ExpectError: wrapSafe("this credential does not carry the 'compute.ssh_key.create' permission"),
			},
			{
				Config:      testAccProviderBlockWith(fake, "compute-read", fake.org) + testAccSshKeyBlock("k", "laptop", fmt.Sprintf("%q", testKeyEd25519)),
				ExpectError: wrapSafe("the API key needs the 'compute:write' scope"),
			},
			{
				Config: testAccProviderBlockWith(fake, "write-key", fake.org) + testAccSshKeyBlock("k", "laptop", fmt.Sprintf("%q", testKeyEd25519)),
			},
			{
				// Planning only reads, which compute:read allows.
				Config:   testAccProviderBlockWith(fake, "compute-read", fake.org) + testAccSshKeyBlock("k", "laptop", fmt.Sprintf("%q", testKeyEd25519)),
				PlanOnly: true,
			},
			{
				// Back to the key that can destroy what step 3 made.
				Config:   testAccProviderBlockWith(fake, "write-key", fake.org) + testAccSshKeyBlock("k", "laptop", fmt.Sprintf("%q", testKeyEd25519)),
				PlanOnly: true,
			},
		},
	})
}

// TestAccSshKeyResource_SurvivesALostAnswer: the import is processed but its
// answer is lost; the retry carries the same Idempotency-Key and gets the stored
// 201, so one key exists and the apply succeeds.
func TestAccSshKeyResource_SurvivesALostAnswer(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckSshKeyDestroy(t, fake),
		Steps: []resource.TestStep{
			{
				PreConfig: func() {
					fake.FailNextAfterHandling(1, matchMethod(http.MethodPost), respondGatewayRetryAfter(502, "0"))
				},
				Config: testAccSshKeyConfig(fake, "laptop", testKeyEd25519),
				Check: func(*terraform.State) error {
					if n := fake.SSHKeyCount(); n != 1 {
						return fmt.Errorf("the retried import left %d keys, want exactly 1", n)
					}
					keys := fake.SSHKeyIdempotencyKeys()
					if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
						return fmt.Errorf("Idempotency-Keys of the attempts = %v, want the same key twice", keys)
					}
					return nil
				},
			},
		},
	})
}

// TestAccSshKeyResource_TheInFlightDuplicateIsWaitedFor: the retry of a lost
// answer can meet 409 idempotency_in_progress, because the first request is
// still inside its transaction. It waits and gets the stored answer; no second
// key is imported and the apply succeeds (V4 L3, I4).
func TestAccSshKeyResource_TheInFlightDuplicateIsWaitedFor(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckSshKeyDestroy(t, fake),
		Steps: []resource.TestStep{
			{
				PreConfig: func() {
					fake.FailNextAfterHandling(1, matchMethod(http.MethodPost), respondGatewayRetryAfter(502, "0"))
					fake.InProgressFor(2)
				},
				Config: testAccSshKeyConfig(fake, "laptop", testKeyEd25519),
				Check: func(*terraform.State) error {
					if n := fake.SSHKeyCount(); n != 1 {
						return fmt.Errorf("the key was imported %d times, want exactly 1", n)
					}
					keys := fake.SSHKeyIdempotencyKeys()
					if len(keys) != 4 || keys[0] == "" || keys[0] != keys[1] || keys[1] != keys[2] || keys[2] != keys[3] {
						return fmt.Errorf("Idempotency-Keys of the attempts = %v, want one key four times (lost answer, two races, replay)", keys)
					}
					return nil
				},
			},
		},
	})
}

// TestAccSshKeyResource_GivingUpSaysHowToRecover: when every attempt of the
// import is lost, the key may exist anyway. The error says so and says what to
// do (name_taken on the next apply, then tofu import), and that recovery works:
// the next apply is name_taken, and importing the key leaves nothing to change.
func TestAccSshKeyResource_GivingUpSaysHowToRecover(t *testing.T) {
	fake := newFakeAPI(t, "test-api-key", testOrgID)
	config := testAccSshKeyConfig(fake, "laptop", testKeyEd25519)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccCheckSshKeyDestroy(t, fake),
		Steps: []resource.TestStep{
			{
				PreConfig: func() {
					fake.FailNextAfterHandling(5, matchMethod(http.MethodPost), respondGatewayRetryAfter(502, "0"))
				},
				Config: config,
				ExpectError: wrapSafe("the first request may still have created the resource: " +
					"if the next apply reports name_taken, find it in the panel or with the API " +
					"and bring it under management with tofu import"),
			},
			{
				Config:      config,
				ExpectError: wrapSafe("name_taken"),
			},
			{
				ResourceName:       sshKeyAddr,
				ImportState:        true,
				ImportStatePersist: true,
				ImportStateIdFunc:  func(*terraform.State) (string, error) { return fake.SSHKeyID("laptop"), nil },
			},
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

// --- the public key, and when two texts are one key (D2) -----------------------------

func TestParsePublicKey(t *testing.T) {
	blobOf := func(key string) string { return strings.Fields(key)[1] }

	for _, tc := range []struct {
		name, text, keyType string
		blobText            string // the base64 whose bytes are expected; "" when the text must not parse
	}{
		{"the canonical form", testKeyEd25519, "ssh-ed25519", blobOf(testKeyEd25519)},
		{"a comment", testKeyEd25519 + " " + testKeyEd25519Comment, "ssh-ed25519", blobOf(testKeyEd25519)},
		{"a comment of several words", testKeyEd25519 + " my laptop, the old one", "ssh-ed25519", blobOf(testKeyEd25519)},
		{"a newline", testKeyEd25519 + "\n", "ssh-ed25519", blobOf(testKeyEd25519)},
		{"CRLF", testKeyEd25519 + " c\r\n", "ssh-ed25519", blobOf(testKeyEd25519)},
		{"whitespace around it", " \t" + testKeyEd25519 + " \t\n\n", "ssh-ed25519", blobOf(testKeyEd25519)},
		{"tabs between the fields", strings.Replace(testKeyEd25519, " ", "\t", 1), "ssh-ed25519", blobOf(testKeyEd25519)},
		{"a key that ends in padding", testKeyECDSA256, "ecdsa-sha2-nistp256", blobOf(testKeyECDSA256)},
		{"spare bits in the last character", withSpareBitsFlipped(t, testKeyECDSA256), "ecdsa-sha2-nistp256", blobOf(testKeyECDSA256)},

		{"nothing", "", "", ""},
		{"only whitespace", " \r\n\t", "", ""},
		{"only a type", "ssh-ed25519", "", ""},
		{"base64 that is not base64", "ssh-ed25519 !!!!", "", ""},
		{"empty base64 after trimming", "ssh-ed25519  ", "", ""},
		{"two keys, two lines", testKeyEd25519 + "\n" + testKeyOther, "", ""},
		{"a key and a blank line and a comment line", testKeyEd25519 + "\n\n# note", "", ""},
		{"authorized_keys options in front", `command="ls" ` + testKeyEd25519, "", ""},
		{"a private key", "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\n-----END OPENSSH PRIVATE KEY-----", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keyType, blob, ok := parsePublicKey(tc.text)
			if tc.blobText == "" {
				if ok {
					t.Fatalf("parsePublicKey(%q) = %q, %x, true; want it not to parse", tc.text, keyType, blob)
				}
				return
			}
			if !ok || keyType != tc.keyType {
				t.Fatalf("parsePublicKey(%q) = %q, ok %v; want %q", tc.text, keyType, ok, tc.keyType)
			}
			_, want, _ := parsePublicKey(tc.keyType + " " + tc.blobText)
			if string(blob) != string(want) {
				t.Errorf("the decoded bytes differ from the key's own: %x vs %x", blob, want)
			}
		})
	}
}

// Two texts are one key when the type and the decoded blob are the same; the
// comment, whitespace, CRLF and spare base64 bits are not part of the key. When
// either side does not parse, only identical text is equal, so a bad key still
// reaches the API and its 422 is shown.
func TestPublicKeysEquivalent(t *testing.T) {
	spare := withSpareBitsFlipped(t, testKeyECDSA256)
	blob := strings.Fields(testKeyEd25519)[1]

	for _, tc := range []struct {
		name, a, b string
		want       bool
	}{
		{"the same text", testKeyEd25519, testKeyEd25519, true},
		{"with and without a comment", testKeyEd25519, testKeyEd25519 + " " + testKeyEd25519Comment, true},
		{"two different comments", testKeyEd25519 + " one", testKeyEd25519 + " two", true},
		{"with and without a newline", testKeyEd25519, testKeyEd25519 + "\n", true},
		{"LF and CRLF", testKeyEd25519 + " c\n", testKeyEd25519 + " c\r\n", true},
		{"whitespace around it", " \t" + testKeyEd25519 + "  ", testKeyEd25519, true},
		{"spare bits: one key, two base64 texts", testKeyECDSA256, spare, true},
		{"spare bits, the other way round", spare, testKeyECDSA256 + " c\n", true},
		{"two keys of one type", testKeyEd25519, testKeyOther, false},
		{"two keys of one type, with comments", testKeyEd25519 + " c", testKeyOther + " c", false},
		{"two types", testKeyEd25519, testKeyECDSA256, false},
		{"the same bytes under another type name", "ssh-ed25519 " + blob, "ssh-rsa " + blob, false},
		{"a key and a truncated key", testKeyEd25519, testKeyEd25519[:len(testKeyEd25519)-8], false},
		{"a key and nothing", testKeyEd25519, "", false},

		{"unparseable, identical", "this is not a key", "this is not a key", true},
		{"empty, identical", "", "", true},
		{"unparseable, different", "this is not a key", "this is not a key either", false},
		{"unparseable against a key", "ssh-ed25519 !!!!", testKeyEd25519, false},
		{"the first line of two keys against the key", testKeyEd25519 + "\n" + testKeyOther, testKeyEd25519, false},
		{"two keys, written differently", testKeyEd25519 + "\n" + testKeyOther, testKeyEd25519 + " c\n" + testKeyOther, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := publicKeysEquivalent(tc.a, tc.b); got != tc.want {
				t.Errorf("publicKeysEquivalent(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
			if got := publicKeysEquivalent(tc.b, tc.a); got != tc.want {
				t.Errorf("publicKeysEquivalent(%q, %q) = %v, want %v (it must be symmetric)", tc.b, tc.a, got, tc.want)
			}
		})
	}
}

// The framework asks the value, not our function: the custom type is what makes
// a Read that returns the canonical text keep the configuration's, and an apply
// that stores the canonical text match the plan.
func TestPublicKeyValue_StringSemanticEquals(t *testing.T) {
	ctx := context.Background()
	commented := NewPublicKeyValue(testKeyEd25519 + " " + testKeyEd25519Comment + "\n")
	canonical := NewPublicKeyValue(testKeyEd25519)

	if same, diags := canonical.StringSemanticEquals(ctx, commented); diags.HasError() || !same {
		t.Errorf("canonical vs commented: %v, %v; want equal", same, diags)
	}
	if same, diags := commented.StringSemanticEquals(ctx, canonical); diags.HasError() || !same {
		t.Errorf("commented vs canonical: %v, %v; want equal", same, diags)
	}
	if same, diags := canonical.StringSemanticEquals(ctx, NewPublicKeyValue(testKeyOther)); diags.HasError() || same {
		t.Errorf("one key vs another: %v, %v; want different", same, diags)
	}
	if _, diags := canonical.StringSemanticEquals(ctx, basetypes.NewStringValue(testKeyEd25519)); !diags.HasError() {
		t.Error("comparing against a plain string value must say what is wrong, not guess")
	}
}

func TestPublicKeyType_IsAStringTypeWithItsOwnValue(t *testing.T) {
	ctx := context.Background()
	typ := PublicKeyType{}

	if !typ.Equal(PublicKeyType{}) || typ.Equal(basetypes.StringType{}) {
		t.Error("PublicKeyType must equal itself and nothing else, even the plain string type")
	}
	value, diags := typ.ValueFromString(ctx, basetypes.NewStringValue("x"))
	if diags.HasError() {
		t.Fatal(diags)
	}
	key, ok := value.(PublicKeyValue)
	if !ok || key.ValueString() != "x" || !key.Type(ctx).Equal(typ) {
		t.Errorf("ValueFromString gave %T %v, want a PublicKeyValue of type PublicKeyType", value, value)
	}
	if _, ok := typ.ValueType(ctx).(PublicKeyValue); !ok {
		t.Errorf("ValueType = %T, want PublicKeyValue", typ.ValueType(ctx))
	}
	tfValue, err := NewPublicKeyValue("x").ToTerraformValue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	back, err := typ.ValueFromTerraform(ctx, tfValue)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := back.(PublicKeyValue); !ok || !back.Equal(NewPublicKeyValue("x")) {
		t.Errorf("ValueFromTerraform = %T %v, want the same PublicKeyValue back", back, back)
	}
	var _ attr.Value = NewPublicKeyNull()
	if !NewPublicKeyNull().IsNull() {
		t.Error("NewPublicKeyNull must be null")
	}
}

// The plan modifier is asked only when the text differs; it says whether the key
// does too. A value that is not known yet may be another key.
func TestReplaceWhenTheKeyChanges(t *testing.T) {
	for _, tc := range []struct {
		name        string
		plan, state types.String
		want        bool
	}{
		{"the same key with a comment", types.StringValue(testKeyEd25519 + " c\n"), types.StringValue(testKeyEd25519), false},
		{"the same key, spare bits", types.StringValue(withSpareBitsFlipped(t, testKeyECDSA256)), types.StringValue(testKeyECDSA256), false},
		{"another key", types.StringValue(testKeyOther), types.StringValue(testKeyEd25519), true},
		{"another key with the old one's comment", types.StringValue(testKeyOther + " " + testKeyEd25519Comment), types.StringValue(testKeyEd25519 + " " + testKeyEd25519Comment), true},
		{"a text that is no key", types.StringValue("nope"), types.StringValue(testKeyEd25519), true},
		{"not known yet", types.StringUnknown(), types.StringValue(testKeyEd25519), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var resp stringplanmodifier.RequiresReplaceIfFuncResponse
			replaceWhenTheKeyChanges(context.Background(), planmodifier.StringRequest{PlanValue: tc.plan, StateValue: tc.state}, &resp)
			if resp.RequiresReplace != tc.want || resp.Diagnostics.HasError() {
				t.Errorf("RequiresReplace = %v (%v), want %v", resp.RequiresReplace, resp.Diagnostics, tc.want)
			}
		})
	}
}
