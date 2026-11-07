# Releasing

GitLab is the source of truth. It push-mirrors to `github.com/virtscale/terraform-provider-virtscale`; when a `v*` tag
arrives there, GitHub Actions ([.github/workflows/release.yml](.github/workflows/release.yml)) builds the provider with
GoReleaser ([.goreleaser.yml](.goreleaser.yml)), signs the checksum file with the release GPG key and creates the GitHub
release. registry.terraform.io and registry.opentofu.org pick the release up from GitHub.

## Cut a release

1. Main's pipeline is green. It includes `release-snapshot`, which runs `goreleaser check` and an unsigned snapshot build.
2. Tag the head of `main` on GitLab with `vMAJOR.MINOR.PATCH` (annotated: `git tag -a v0.1.0 -m "v0.1.0" && git push origin v0.1.0`).
3. Wait for the mirror to push the tag, then watch the **Release** run on GitHub. It ends with a release holding 13 zips,
   `..._SHA256SUMS`, `..._SHA256SUMS.sig` and `..._manifest.json`.
4. Check that both registries list the version, then run `tofu init` with `version = "<it>"` in an empty directory.

Never move or re-push a tag after the run started. A bad release is fixed by a new patch version.

## Secrets

Only GitHub holds them, as repository secrets: `GPG_PRIVATE_KEY` (ASCII-armored private key) and `PASSPHRASE`.
`GITHUB_TOKEN` is the workflow's own. None of them goes into GitLab CI variables, git, a log or a chat. GitLab's side is
one mirror credential (GitHub deploy key or token with write access to this one repository, set under Settings >
Repository > Mirroring repositories, direction Push). The backup of the private key and its passphrase belongs in OpenBao.

## First-time setup (once)

1. GitHub organization `virtscale` and a public repository `terraform-provider-virtscale`; the GitLab push mirror.
2. A dedicated signing key on a trusted machine, never reused: `gpg --batch --quick-generate-key "<name> <address>" rsa4096 sign 2y`.
   Note its fingerprint (`gpg --list-secret-keys --keyid-format long`).
3. Store `gpg --armor --export-secret-keys <fingerprint>` and the passphrase as the two GitHub secrets, then delete the
   exported file.
4. Register the public key (`gpg --armor --export <fingerprint>`) and the provider with both registries, following their
   current docs: registry.terraform.io (Publish > Provider, signed in as an owner of the GitHub organization, plus the
   key under the namespace's signing keys) and registry.opentofu.org (a request on github.com/opentofu/registry).

## Rotate the GPG key

Do it before the key expires, or at once if it leaked.

1. Generate the new key as in the setup. Add its public key to **both** registries; leave the old one listed, so releases
   signed with it keep verifying.
2. Replace `GPG_PRIVATE_KEY` and `PASSPHRASE` in GitHub, delete the exported file, back the new key up in OpenBao.
3. Cut a patch release. Download its `_SHA256SUMS` and `.sig` and run `gpg --verify` with only the new public key imported.
4. Retire the old key once nothing needs it. If it leaked: revoke it (`gpg --gen-revoke`), remove it from the registries
   and publish a new version; versions signed with a leaked key cannot be trusted.

## Maintenance

- GoReleaser's version lives in `.gitlab-ci.yml` (`GORELEASER_VERSION` and `GORELEASER_SHA256`, the line for
  `goreleaser_Linux_x86_64.tar.gz` in that release's `checksums.txt`) and in the workflow (`version:`). Change them together.
- The workflow's actions are pinned to commit SHAs; the comment names the tag. To update one, take the commit of the
  new tag (`git ls-remote https://github.com/<owner>/<repo> 'refs/tags/<tag>*'`: for an annotated tag use the `^{}` line)
  and change the comment with it.
- The protocol version in `terraform-registry-manifest.json` (`6.0`) is what `main.go` serves: plugin framework
  `providerserver.Serve` with no `ProtocolVersion` is protocol 6. Change it only if that changes.
