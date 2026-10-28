#!/bin/sh
# Live acceptance of virtscale_ssh_key against the staging customer API (PROV-001 criterion 6).
# Run by the CI job `provider-staging-live` (.gitlab-ci.yml), on protected main, in a pipeline started through
# the GitLab API. The key is never exported, written outside a private temp dir, or printed:
#
#   0. refuse to run when the pipeline carries variables that can redirect traffic or change the toolchain,
#      drop the rest of that class, and pin the proxy, module and curl settings below,
#   1. install the pinned tofu (checksum verified) and fetch + verify the Go modules, all before any secret exists,
#   2. log in to OpenBao with the job's id_token (JWT role provider-staging-live, terraform-openbao: this project,
#      protected main, API pipelines only, token 300 s / 4 uses),
#   3. read kv/platform/s3-staging/test-keys (the one path the role's policy allows), keep stg-org-a's id and key
#      in shell variables, and revoke the OpenBao token,
#   4. run TestLiveSshKeyResource_LifecycleProof with the key in that one process's environment and the Go module
#      proxy switched off: apply, an empty second plan, import by id, destroy,
#   5. withhold the test log if it contains the key or the id_token, require `--- PASS` for the test, and count the
#      tf-live-* keys the org holds before and after (the run must leave none behind).
#
# The addresses below are literals on purpose: a pipeline variable can override a job's `variables:` block but not
# a line of this script, so a trigger cannot point the id_token or the key at another host.
# ci/check-provider-staging-live.sh greps this file for the hardening lines; keep them when you edit it.
set -eu

fail() {
  echo "provider-staging-live: $*" >&2
  exit 1
}

# 0. Variables a trigger could use to send the key elsewhere or to swap the toolchain: refuse, by name only.
unsafe=""
for name in $(awk 'BEGIN { for (k in ENVIRON) print k }'); do
  case "$name" in
  http_proxy | HTTP_PROXY | https_proxy | HTTPS_PROXY | all_proxy | ALL_PROXY | no_proxy | NO_PROXY | \
    CURL_* | SSL_CERT_FILE | SSL_CERT_DIR | LD_PRELOAD | LD_LIBRARY_PATH | \
    GOPROXY | GOSUMDB | GONOSUMDB | GONOSUMCHECK | GONOPROXY | GOINSECURE | GOPRIVATE | GOFLAGS | GOENV | GOMODCACHE | \
    TF_* | VIRTSCALE_*)
    unsafe="$unsafe $name"
    ;;
  esac
done
[ -z "$unsafe" ] || fail "refusing to run: the pipeline carries variables that can redirect traffic or change the toolchain:$unsafe"

unset http_proxy HTTP_PROXY https_proxy HTTPS_PROXY all_proxy ALL_PROXY no_proxy NO_PROXY CURL_HOME LD_PRELOAD LD_LIBRARY_PATH GOPROXY GONOSUMDB GONOSUMCHECK GONOPROXY GOINSECURE GOPRIVATE GOFLAGS GOENV TF_CLI_CONFIG_FILE TF_PLUGIN_CACHE_DIR
GOFLAGS=-mod=readonly
GOPROXY=https://proxy.golang.org
GOSUMDB=sum.golang.org
GOTOOLCHAIN=local
NO_PROXY='*'
export GOFLAGS GOPROXY GOSUMDB GOTOOLCHAIN NO_PROXY

BAO_ADDR="https://bao.k8s-internal-01.virtscale.nl"
BAO_ROLE="provider-staging-live"
KEYS_PATH="kv/data/platform/s3-staging/test-keys"
ENDPOINT="http://s3-staging-customer-api.s3-staging.svc.cluster.local"
TEST_NAME="TestLiveSshKeyResource_LifecycleProof"
# OpenTofu release archive, pinned: the version and the SHA-256 of tofu_<version>_linux_amd64.zip from the release's
# own tofu_<version>_SHA256SUMS.
TOFU_VERSION_PINNED=1.12.5
TOFU_ZIP_SHA256=dade9650e6b74fc7a8b986bd8717497d32f9e09cf82e479afef4977fa3085536
TOFU_URL="https://github.com/opentofu/opentofu/releases/download/v${TOFU_VERSION_PINNED}/tofu_${TOFU_VERSION_PINNED}_linux_amd64.zip"

# Every curl in this file goes through here: no .curlrc (-q must come first) and no proxy, whatever the environment says.
curl_safe() {
  curl -q --noproxy '*' "$@"
}

[ -n "${BAO_ID_TOKEN:-}" ] || fail "no id_token: this is not a CI job with id_tokens"
[ -n "${BAO_CA_PEM:-}" ] || fail "BAO_CA_PEM (the cluster CA certificate, a protected CI variable) is not set"

umask 077
work=$(mktemp -d)
cleanup() {
  chmod -R u+w "$work" 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT
trap 'cleanup; exit 1' HUP INT TERM
printf '%s\n' "$BAO_CA_PEM" >"$work/ca.pem"
printf '%s' "$BAO_ID_TOKEN" >"$work/idtoken"
unset BAO_CA_PEM
GOMODCACHE="$work/gomod"
GOCACHE="$work/gocache"
export GOMODCACHE GOCACHE

# 1. Toolchain, before any secret exists. The Go modules are fetched, then verified against go.sum.
mkdir -p "$work/bin"
curl_safe -sS -L --fail --max-time 180 -o "$work/tofu.zip" "$TOFU_URL" || fail "downloading tofu failed"
printf '%s  %s\n' "$TOFU_ZIP_SHA256" "$work/tofu.zip" | sha256sum -c - >/dev/null 2>&1 || fail "the tofu archive does not match its pinned SHA-256"
unzip -o -q "$work/tofu.zip" tofu -d "$work/bin"
chmod +x "$work/bin/tofu"
go mod download || fail "go mod download failed"
go mod verify >/dev/null || fail "go mod verify failed"

# 2. Login. The id_token goes to jq through its environment and to curl on stdin, never through argv.
code=$(jq -n --arg role "$BAO_ROLE" '{role: $role, jwt: env.BAO_ID_TOKEN}' |
  curl_safe -sS --max-time 30 --cacert "$work/ca.pem" -X POST -H 'Content-Type: application/json' --data @- \
    -o "$work/login.json" -w '%{http_code}' "$BAO_ADDR/v1/auth/jwt/login") || code=000
unset BAO_ID_TOKEN CI_JOB_TOKEN CI_JOB_JWT CI_JOB_JWT_V1 CI_JOB_JWT_V2 CI_BUILD_TOKEN CI_REGISTRY_PASSWORD CI_DEPLOY_PASSWORD CI_REPOSITORY_URL
[ "$code" = 200 ] || fail "OpenBao login answered HTTP $code (the status only: the body can echo the claims)"
token=$(jq -r '.auth.client_token // empty' "$work/login.json")
rm -f "$work/login.json"
[ -n "$token" ] || fail "OpenBao login returned no token"

# 3. Read the one path, then revoke the token whatever the read said. The token goes to curl as a config line on
# stdin (printf is a shell builtin), never as an argument.
code=$(printf 'header = "X-Vault-Token: %s"\n' "$token" |
  curl_safe -sS --max-time 30 --cacert "$work/ca.pem" -K - -o "$work/keys.json" -w '%{http_code}' \
    "$BAO_ADDR/v1/$KEYS_PATH") || code=000
printf 'header = "X-Vault-Token: %s"\n' "$token" |
  curl_safe -sS --max-time 15 --cacert "$work/ca.pem" -K - -X POST -o /dev/null "$BAO_ADDR/v1/auth/token/revoke-self" ||
  echo "provider-staging-live: revoke-self failed (the token ends on its own within 5 minutes)" >&2
unset token
[ "$code" = 200 ] || fail "reading the staging key document answered HTTP $code"
org=$(jq -r '.data.data.S3CP_STAGING_ORG_A_ID // empty' "$work/keys.json")
api_key=$(jq -r '.data.data.S3CP_STAGING_ORG_A_KEY // empty' "$work/keys.json")
rm -f "$work/keys.json"
[ -n "$org" ] || fail "the key document has no S3CP_STAGING_ORG_A_ID"
case "$api_key" in
vsk_?*) ;;
*) fail "the key document has no usable S3CP_STAGING_ORG_A_KEY" ;;
esac

# How many tf-live-* keys the org holds right now (the whole list; a count, never names or keys).
count_live_keys() {
  c=$(printf 'header = "Authorization: Bearer %s"\n' "$api_key" |
    curl_safe -sS --max-time 30 -K - -o "$work/list.json" -w '%{http_code}' "$ENDPOINT/v1/orgs/$org/compute/ssh-keys") || c=000
  [ "$c" = 200 ] || fail "listing the org's SSH keys answered HTTP $c"
  jq -r '[.items[]? | select(.name | startswith("tf-live-"))] | length' "$work/list.json"
  rm -f "$work/list.json"
}
before=$(count_live_keys)
echo "tf-live-* keys in the staging org before the run: $before"

# 4. The test. A prefix assignment gives the key to this one command and its children only, and GOPROXY=off keeps the
# Go toolchain off the network while the key is in the environment (the modules were fetched and verified above).
rc=0
TF_ACC=1 \
  TF_ACC_TERRAFORM_PATH="$work/bin/tofu" \
  TF_ACC_PROVIDER_NAMESPACE=virtscale \
  TF_ACC_PROVIDER_HOST=registry.terraform.io \
  VIRTSCALE_LIVE=1 \
  VIRTSCALE_ENDPOINT="$ENDPOINT" \
  VIRTSCALE_ORGANIZATION="$org" \
  VIRTSCALE_API_KEY="$api_key" \
  GOPROXY=off \
  go test ./internal/provider -run "^${TEST_NAME}\$" -count=1 -v >"$work/go-test.log" 2>&1 || rc=$?

# 5. Withhold the log if it holds the key or the id_token (grep reads the key on stdin, the id_token from a 0600 file).
if printf '%s' "$api_key" | grep -qF -f - "$work/go-test.log" || grep -qF -f "$work/idtoken" "$work/go-test.log"; then
  fail "a secret appears in the test output: the log is withheld"
fi
cat "$work/go-test.log"
cp "$work/go-test.log" provider-staging-live.log

after=$(count_live_keys)
echo "tf-live-* keys in the staging org after the run: $after"

[ "$rc" -eq 0 ] || fail "go test exited with status $rc"
grep -q -- "--- PASS: ${TEST_NAME}" "$work/go-test.log" || fail "the log has no '--- PASS: ${TEST_NAME}' (a skip also exits 0)"
if grep -q -- "--- SKIP: ${TEST_NAME}" "$work/go-test.log"; then fail "the live test was skipped"; fi
[ "$after" = "$before" ] || fail "the run left tf-live-* keys behind ($before before, $after after)"
echo "provider-staging-live: PASS (apply, empty second plan, import, destroy; no key left behind)"
