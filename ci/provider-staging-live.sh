#!/bin/sh
# Live acceptance of virtscale_ssh_key against the staging customer API (PROV-001 criterion 6).
# Run by the CI job `provider-staging-live` (.gitlab-ci.yml) as `/bin/sh ci/provider-staging-live.sh`, on protected
# main, in a pipeline started through the GitLab API. The key is never exported, written outside a private temp dir,
# or printed.
#
# Environment: this file re-executes itself under `env -i` with an ALLOWLIST of variables (stage A below). Whatever a
# pipeline variable sets (proxy, Go, curl, loader, Terraform, apt, shell-startup variables, PATH, ...) is gone before
# the first real command; there is no denylist to keep complete. Everything the run needs is a literal in this file:
# the PATH, the OpenBao and API addresses, the CA certificate (ci/bao-ca.pem, a public certificate), the tofu release
# and its SHA-256.
#
# Stage B, in the clean environment:
#   1. check the pipeline is the one this job is for, write the id_token to a 0600 file and unset it (the file is
#      deleted right after the login),
#   2. install jq/unzip, the pinned tofu (checksum verified), fetch and verify the Go modules: no secret exists yet,
#   3. log in to OpenBao with the id_token (JWT role provider-staging-live, terraform-openbao: this project,
#      protected main, API pipelines only, token 300 s / 4 uses),
#   4. read kv/platform/s3-staging/test-keys (the one path the role's policy allows), keep stg-org-a's id and key in
#      shell variables, and revoke the OpenBao token,
#   5. run the live tests one after another, each in its own `go test` process with the key in that process's
#      environment and the Go module proxy switched off (SSH keys, the catalogue data sources, projects, VPCs, an
#      instance with its firewall, S3): apply, an empty second plan, an update, import, destroy,
#   6. withhold a test's log if it contains the key (checked after every test; the artifact is updated every time),
#      require `--- PASS` for every test (a failed test does not stop the others, so one run shows which families work),
#      and count the tf-live-* objects the org holds before (the run refuses to start if there are any) and after (the
#      run must leave none behind). The S3 test runs only with PROVIDER_STAGING_LIVE_S3=1.
#
# Not coverable here: the id_token is also in the environment of the runner's own shell, which any process of the
# job can read. The role limits what it opens (this project, protected main, API pipelines, the staging document).
# ci/check-provider-staging-live.sh keeps the hardening lines below in place; keep them when you edit this file.
set -eu

# --- Stage A: the allowlist. Only these variables cross; the marker is an argument, which no variable can set. ---
if [ "${1:-}" != "--clean-env" ]; then
  exec /usr/bin/env -i \
    PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin \
    HOME=/tmp \
    BAO_ID_TOKEN="${BAO_ID_TOKEN:-}" \
    CI_PROJECT_ID="${CI_PROJECT_ID:-}" \
    CI_COMMIT_REF_PROTECTED="${CI_COMMIT_REF_PROTECTED:-}" \
    CI_COMMIT_REF_NAME="${CI_COMMIT_REF_NAME:-}" \
    CI_PIPELINE_SOURCE="${CI_PIPELINE_SOURCE:-}" \
    PROVIDER_STAGING_LIVE="${PROVIDER_STAGING_LIVE:-}" \
    PROVIDER_STAGING_LIVE_S3="${PROVIDER_STAGING_LIVE_S3:-}" \
    /bin/sh "$0" --clean-env
fi

# --- Stage B: the clean environment ---
fail() {
  echo "provider-staging-live: $*" >&2
  exit 1
}

BAO_ADDR="https://bao.k8s-internal-01.virtscale.nl"
BAO_CA_FILE="ci/bao-ca.pem"
BAO_ROLE="provider-staging-live"
KEYS_PATH="kv/data/platform/s3-staging/test-keys"
ENDPOINT="http://s3-staging-customer-api.s3-staging.svc.cluster.local"
# The live tests, in the order they run. An instance is the heaviest and the staging host is small: it runs last, alone.
# The S3 test needs an object store behind the API, which staging has not, and every bucket it creates is soft-deleted and,
# while the platform's purge worker is off, never purged (it keeps its name and a retention row for good): it runs only when
# the pipeline is started with PROVIDER_STAGING_LIVE_S3=1.
TESTS_FIRST="TestLiveSshKeyResource_LifecycleProof TestLiveCatalogueProof TestLiveProjectResource_LifecycleProof TestLiveVpcResource_LifecycleProof"
TESTS_LAST="TestLiveInstanceFirewallResource_LifecycleProof"
TESTS="$TESTS_FIRST $TESTS_LAST"
live_s3=""
if [ "${PROVIDER_STAGING_LIVE_S3:-}" = 1 ]; then
  TESTS="$TESTS_FIRST TestLiveS3Resources_LifecycleProof $TESTS_LAST"
  live_s3=1
fi
# OpenTofu release archive, pinned: the version and the SHA-256 of tofu_<version>_linux_amd64.zip from the release's
# own tofu_<version>_SHA256SUMS.
TOFU_VERSION_PINNED=1.12.5
TOFU_ZIP_SHA256=dade9650e6b74fc7a8b986bd8717497d32f9e09cf82e479afef4977fa3085536
TOFU_URL="https://github.com/opentofu/opentofu/releases/download/v${TOFU_VERSION_PINNED}/tofu_${TOFU_VERSION_PINNED}_linux_amd64.zip"

# Every curl in this file goes through here: no .curlrc (-q must come first) and no proxy.
curl_safe() {
  curl -q --noproxy '*' "$@"
}

# 1. This job is for one pipeline only; refuse anything else even if the YAML rules were bypassed.
[ "${CI_PROJECT_ID:-}" = 225 ] || fail "not project 225"
[ "${CI_COMMIT_REF_PROTECTED:-}" = true ] || fail "not a protected ref"
[ "${CI_COMMIT_REF_NAME:-}" = main ] || fail "not main"
[ "${CI_PIPELINE_SOURCE:-}" = api ] || fail "not an API pipeline"
[ "${PROVIDER_STAGING_LIVE:-}" = 1 ] || fail "PROVIDER_STAGING_LIVE is not 1"
[ -n "${BAO_ID_TOKEN:-}" ] || fail "no id_token: this is not a CI job with id_tokens"
[ -f "$BAO_CA_FILE" ] || fail "$BAO_CA_FILE is missing"

umask 077
work=$(mktemp -d)
cleanup() {
  chmod -R u+w "$work" 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT
trap 'cleanup; exit 1' HUP INT TERM
printf '%s' "$BAO_ID_TOKEN" >"$work/idtoken"
unset BAO_ID_TOKEN

# The toolchain environment is set here, from literals.
mkdir -p "$work/home" "$work/bin"
# The acceptance tests' pre-check skips when `tofu` is not on PATH (TF_ACC_TERRAFORM_PATH alone is not enough), so the
# pinned tofu goes in front of the fixed PATH.
PATH="$work/bin:$PATH"
HOME="$work/home"
GOPATH="$work/gopath"
GOMODCACHE="$work/gomod"
GOCACHE="$work/gocache"
CGO_ENABLED=0
GOTOOLCHAIN=local
GOENV=off
GOFLAGS=-mod=readonly
GOPROXY=https://proxy.golang.org
GOSUMDB=sum.golang.org
NO_PROXY='*'
export PATH HOME GOPATH GOMODCACHE GOCACHE CGO_ENABLED GOTOOLCHAIN GOENV GOFLAGS GOPROXY GOSUMDB NO_PROXY

# 2. Tools and modules, before any secret exists.
if ! { apt-get update && apt-get install -y --no-install-recommends unzip ca-certificates jq; } >"$work/apt.log" 2>&1; then
  tail -n 5 "$work/apt.log" >&2
  fail "installing jq and unzip failed"
fi
curl_safe -sS -L --fail --max-time 180 -o "$work/tofu.zip" "$TOFU_URL" || fail "downloading tofu failed"
printf '%s  %s\n' "$TOFU_ZIP_SHA256" "$work/tofu.zip" | sha256sum -c - >/dev/null 2>&1 || fail "the tofu archive does not match its pinned SHA-256"
unzip -o -q "$work/tofu.zip" tofu -d "$work/bin"
chmod +x "$work/bin/tofu"
go mod download || fail "go mod download failed"
go mod verify >/dev/null || fail "go mod verify failed"

# 3. Login. The id_token goes to jq from its file and to curl on stdin, never through argv or the environment; the
# file is deleted as soon as the login has been sent, before any code from the module cache runs.
code=$(jq -n --arg role "$BAO_ROLE" --rawfile jwt "$work/idtoken" '{role: $role, jwt: $jwt}' |
  curl_safe -sS --max-time 30 --cacert "$BAO_CA_FILE" -X POST -H 'Content-Type: application/json' --data @- \
    -o "$work/login.json" -w '%{http_code}' "$BAO_ADDR/v1/auth/jwt/login") || code=000
rm -f "$work/idtoken"
[ "$code" = 200 ] || fail "OpenBao login answered HTTP $code (the status only: the body can echo the claims)"
token=$(jq -r '.auth.client_token // empty' "$work/login.json")
rm -f "$work/login.json"
[ -n "$token" ] || fail "OpenBao login returned no token"

# 4. Read the one path, then revoke the token whatever the read said. The token goes to curl as a config line on
# stdin (printf is a shell builtin), never as an argument.
code=$(printf 'header = "X-Vault-Token: %s"\n' "$token" |
  curl_safe -sS --max-time 30 --cacert "$BAO_CA_FILE" -K - -o "$work/keys.json" -w '%{http_code}' \
    "$BAO_ADDR/v1/$KEYS_PATH") || code=000
printf 'header = "X-Vault-Token: %s"\n' "$token" |
  curl_safe -sS --max-time 15 --cacert "$BAO_CA_FILE" -K - -X POST -o /dev/null "$BAO_ADDR/v1/auth/token/revoke-self" ||
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

# GET path (below the API root) into a file; the key goes to curl on stdin. Prints the HTTP status only.
api_get() {
  code=$(printf 'header = "Authorization: Bearer %s"\n' "$api_key" |
    curl_safe -sS --max-time 60 -K - -o "$2" -w '%{http_code}' "$ENDPOINT$1") || code=000
  printf '%s' "$code"
}

# How many objects named tf-live-* a list holds (a count, never names or keys). 503 means the feature is switched off, so
# nothing of it can exist; any other answer but 200 stops the run.
count_named() { # count_named PATH
  c=$(api_get "$1" "$work/list.json")
  [ "$c" != 503 ] || { echo 0; return 0; }
  [ "$c" = 200 ] || fail "listing $1 answered HTTP $c"
  jq -r '[.items[]? | select(.name | startswith("tf-live-"))] | length' "$work/list.json"
  rm -f "$work/list.json"
}

# One line with what the org holds now: SSH keys, projects, instances and buckets that are named tf-live-* (a soft-deleted
# bucket is history and not counted) and VPCs of every project named tf-live-*. Access keys have no name, so they are not
# counted here; the live S3 test checks its own key is revoked.
leftovers() {
  ssh=$(count_named "/v1/orgs/$org/compute/ssh-keys")
  projects=$(count_named "/v1/orgs/$org/projects")
  instances=$(count_named "/v1/orgs/$org/compute/instances")
  c=$(api_get "/v1/orgs/$org/s3/buckets" "$work/list.json")
  if [ "$c" = 200 ]; then
    buckets=$(jq -r '[.items[]? | select((.name | startswith("tf-live-")) and (.state | IN("deleting_soft", "purging", "purged", "deleted") | not))] | length' "$work/list.json")
  elif [ "$c" = 503 ]; then buckets=0; else fail "listing the buckets answered HTTP $c"; fi
  vpcs=0
  c=$(api_get "/v1/orgs/$org/projects" "$work/projects.json")
  if [ "$c" = 200 ]; then
    for project in $(jq -r '.items[]?.id' "$work/projects.json"); do
      c=$(api_get "/v1/projects/$project/vpcs" "$work/list.json")
      if [ "$c" = 200 ]; then
        n=$(jq -r '[.items[]? | select(.name | startswith("tf-live-"))] | length' "$work/list.json")
        vpcs=$((vpcs + n))
      elif [ "$c" != 503 ]; then fail "listing the VPCs of a project answered HTTP $c"; fi
    done
  elif [ "$c" != 503 ]; then fail "listing the projects answered HTTP $c"; fi
  rm -f "$work/list.json" "$work/projects.json"
  echo "ssh_keys=$ssh projects=$projects instances=$instances buckets=$buckets vpcs=$vpcs"
}
before=$(leftovers)
echo "the staging org before the run: $before"
# A leak of an earlier run must not become this run's baseline: the host is small and a forgotten server costs the next run.
[ "$before" = "ssh_keys=0 projects=0 instances=0 buckets=0 vpcs=0" ] ||
  fail "the staging org already holds tf-live-* objects from an earlier run: remove them first (before: $before)"

# 5. The tests, one `go test` process each. A prefix assignment gives the key to this one command and its children only,
# and GOPROXY=off keeps the Go toolchain off the network while the key is in the environment (the modules were fetched
# and verified above). A test that fails or skips is recorded and the next one still runs.
rc=0
results=""
: >"$work/go-test.log"
for name in $TESTS; do
  trc=0
  TF_ACC=1 \
    TF_ACC_TERRAFORM_PATH="$work/bin/tofu" \
    TF_ACC_PROVIDER_NAMESPACE=virtscale \
    TF_ACC_PROVIDER_HOST=registry.terraform.io \
    VIRTSCALE_LIVE=1 \
    VIRTSCALE_LIVE_S3="$live_s3" \
    VIRTSCALE_ENDPOINT="$ENDPOINT" \
    VIRTSCALE_ORGANIZATION="$org" \
    VIRTSCALE_API_KEY="$api_key" \
    GOPROXY=off \
    go test ./internal/provider -run "^${name}\$" -count=1 -v -timeout 30m >"$work/test.log" 2>&1 || trc=$?
  # 6. Withhold the log if it holds the key as clear text (grep reads the key on stdin). This catches an accidental print;
  # it is not a defence against code that encodes the key, which is why only reviewed code on protected main can run here.
  # It is done after every test, and the artifact is updated every time, so that a job that is killed by its timeout
  # still leaves the log of the tests that ran.
  # grep says 0 for a match and 1 for none; anything else (2: it could not read) is as bad as a match.
  scan=0
  printf '%s' "$api_key" | grep -qF -f - "$work/test.log" || scan=$?
  case "$scan" in
  0) fail "the API key appears in the output of $name: the log is withheld" ;;
  1) ;;
  *) fail "scanning the output of $name for the API key failed (grep status $scan): the log is withheld" ;;
  esac
  cat "$work/test.log"
  cat "$work/test.log" >>"$work/go-test.log"
  cp "$work/go-test.log" provider-staging-live.log
  if [ "$trc" -eq 0 ] && grep -q -- "--- PASS: ${name}" "$work/test.log" && ! grep -q -- "--- SKIP: ${name}" "$work/test.log"; then
    results="$results $name=PASS"
  else
    results="$results $name=FAIL"
    rc=1
  fi
done

after=$(leftovers)
echo "the staging org after the run: $after"
echo "provider-staging-live: results:$results"

[ "$rc" -eq 0 ] || fail "at least one live test failed or was skipped (results:$results)"
# Soft-deleted buckets and revoked keys stay on the API as history, which is why they are not counted; everything else
# the run made must be gone.
[ "$after" = "$before" ] || fail "the run left objects behind (before: $before; after: $after)"
echo "provider-staging-live: PASS (every live test passed; no object left behind)"
