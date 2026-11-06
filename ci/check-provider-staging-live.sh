#!/bin/sh
# Guards the hardening of ci/provider-staging-live.sh and of its job in .gitlab-ci.yml (PROV-001). The job carries the
# staging API key; these lines keep a pipeline variable from sending it elsewhere. Runs in the `test` job, from the
# repository root.
set -eu
script=ci/provider-staging-live.sh
rc=0

bad() {
  echo "check-provider-staging-live: $*" >&2
  rc=1
}

# line_of PATTERN: the number of the first non-comment line matching the fixed string, or empty
line_of() {
  grep -nF -- "$1" "$script" | grep -vE '^[0-9]+:[[:space:]]*#' | head -n 1 | cut -d: -f1
}

need() { # need DESCRIPTION FIXED-STRING
  [ -n "$(line_of "$2")" ] || bad "missing in $script: $1"
}

# Stage A: the allowlist re-exec, with a fixed PATH and the marker as an argument.
need "the env -i re-exec" "exec /usr/bin/env -i"
need "the fixed PATH literal" "PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin"
need "the marker argument" '/bin/sh "$0" --clean-env'
need "the marker test" '[ "${1:-}" != "--clean-env" ]'
# Stage B: the toolchain from literals, the id_token out of the environment before any toolchain command.
need "the pinned tofu on PATH (the test's pre-check skips without it)" 'PATH="$work/bin:$PATH"'
need "CGO off" "CGO_ENABLED=0"
need "local toolchain" "GOTOOLCHAIN=local"
need "no go env file" "GOENV=off"
need "GOFLAGS" "GOFLAGS=-mod=readonly"
need "GOPROXY" "GOPROXY=https://proxy.golang.org"
need "GOSUMDB" "GOSUMDB=sum.golang.org"
need "NO_PROXY" "NO_PROXY='*'"
need "go mod verify" "go mod verify"
need "the module proxy off while the key is set" "GOPROXY=off"
need "the pinned tofu checksum check" "sha256sum -c"
need "the project gate" '[ "${CI_PROJECT_ID:-}" = 225 ]'
unset_line=$(line_of "unset BAO_ID_TOKEN")
[ -n "$unset_line" ] || bad "missing in $script: unset BAO_ID_TOKEN"
exec_line=$(line_of "exec /usr/bin/env -i")
dl_line=$(line_of "go mod download")
login_line=$(line_of "auth/jwt/login")
apt_line=$(line_of "apt-get install")
if [ -n "$unset_line" ] && [ -n "$exec_line" ] && [ -n "$dl_line" ] && [ -n "$login_line" ] && [ -n "$apt_line" ]; then
  [ "$exec_line" -lt "$unset_line" ] || bad "the env -i re-exec must come before unset BAO_ID_TOKEN"
  [ "$unset_line" -lt "$apt_line" ] || bad "unset BAO_ID_TOKEN must come before the first package or toolchain command"
  [ "$unset_line" -lt "$dl_line" ] || bad "unset BAO_ID_TOKEN must come before go mod download"
  [ "$dl_line" -lt "$login_line" ] || bad "go mod download must finish before the OpenBao login (no secret while modules are fetched)"
fi
# The first executable line after set -eu is the re-exec guard: nothing but shell builtins runs before it.
first=$(grep -vE '^[[:space:]]*(#|$)' "$script" | sed -n '2p')
[ "$first" = 'if [ "${1:-}" != "--clean-env" ]; then' ] || bad "the first command after set -eu must be the re-exec guard"
if ! grep -qE '^TOFU_ZIP_SHA256=[0-9a-f]{64}$' "$script"; then
  bad "TOFU_ZIP_SHA256 is not a pinned 64-digit hex literal"
fi
# The key scan runs after every test and before the log goes anywhere: scan, then the job log, then the artifact.
scan_line=$(line_of 'grep -qF -f - "$work/test.log"')
cat_line=$(line_of 'cat "$work/test.log"')
append_line=$(line_of 'cat "$work/test.log" >>"$work/go-test.log"')
cp_line=$(line_of 'cp "$work/go-test.log" provider-staging-live.log')
if [ -n "$scan_line" ] && [ -n "$cat_line" ] && [ -n "$append_line" ] && [ -n "$cp_line" ]; then
  [ "$scan_line" -lt "$cat_line" ] || bad "the key scan must come before the log goes to the job log"
  [ "$cat_line" -lt "$append_line" ] || bad "the log must go to the job log before it is appended to the artifact copy"
  [ "$append_line" -lt "$cp_line" ] || bad "the artifact copy must be updated after the log is appended"
else
  bad "the per-test key scan, the job log, the append and the artifact copy must all be there"
fi
need "a grep that fails to scan is a failure" '*) fail "scanning the output of $name for the API key failed'
need "the key scan found the key" '0) fail "the API key appears in the output of $name'
# A run starts only on a clean org, so that a leak of an earlier run is never the baseline.
need "the clean-start guard" '[ "$before" = "ssh_keys=0 projects=0 instances=0 buckets=0 vpcs=0" ]'
need "the after-run guard" '[ "$after" = "$before" ]'
# S3 is opt-in, and only for exactly 1.
need "the S3 opt-in for exactly 1" '[ "${PROVIDER_STAGING_LIVE_S3:-}" = 1 ]'
need "S3 off by default" 'live_s3=""'
need "the S3 variable crosses the allowlist" 'PROVIDER_STAGING_LIVE_S3="${PROVIDER_STAGING_LIVE_S3:-}"'
# The key stays on curl's stdin, in the one test command's environment and on grep's stdin: no other line may use it
# (whole lines are compared, so an allowed use cannot carry a second one).
other_key_use=$(grep -vE '^[[:space:]]*#' "$script" | grep -F '$api_key' | sed 's/^[[:space:]]*//' |
  grep -vxF -e 'code=$(printf '"'"'header = "Authorization: Bearer %s"\n'"'"' "$api_key" |' \
    -e 'VIRTSCALE_API_KEY="$api_key" \' \
    -e 'printf '"'"'%s'"'"' "$api_key" | grep -qF -f - "$work/test.log" || scan=$?' \
    -e 'case "$api_key" in' || true)
[ -z "$other_key_use" ] || bad "the API key is used where it must not be: $other_key_use"
# The CA is a committed public certificate, not a variable.
grep -qF "BAO_CA_PEM" "$script" && bad "the script must not read the CA from a variable (BAO_CA_PEM)"
[ "$(grep -c 'BEGIN CERTIFICATE' ci/bao-ca.pem)" -eq 1 ] || bad "ci/bao-ca.pem must hold exactly one certificate"
grep -q 'PRIVATE KEY' ci/bao-ca.pem && bad "ci/bao-ca.pem holds a private key"
# Every curl goes through curl_safe, which is the one line that carries -q --noproxy '*'.
if [ "$(grep -cE "^[[:space:]]+curl -q --noproxy '\*' \"\\\$@\"$" "$script")" -ne 1 ]; then
  bad "curl_safe must be defined once as: curl -q --noproxy '*' \"\$@\""
fi
if grep -vE '^[[:space:]]*#' "$script" | grep -E '(^|[^_a-z])curl ' | grep -vqE "curl -q --noproxy '\*' \"\\\$@\""; then
  bad "a bare curl call (use curl_safe)"
fi
if grep -vE '^[[:space:]]*#' "$script" | grep -qE '(^|[[:space:]])(set -x|set -o xtrace|env|printenv|declare -p)([[:space:]]|$)'; then
  bad "a command that can print the environment"
fi
# The job: defined once (a duplicate key silently wins in YAML), no cache, no before_script/after_script, no job-level
# variables, no inherited global variables, one artifact path, started as /bin/sh ci/provider-staging-live.sh.
[ "$(grep -c '^provider-staging-live:' .gitlab-ci.yml)" -eq 1 ] || bad "provider-staging-live must be defined exactly once in .gitlab-ci.yml"
job=$(awk '/^provider-staging-live:/ { f = 1; next } /^[A-Za-z.]/ { f = 0 } f' .gitlab-ci.yml)
printf '%s\n' "$job" | grep -qE '^  after_script:' && bad "provider-staging-live must not have an after_script"
printf '%s\n' "$job" | grep -qE '^  variables:' && bad "provider-staging-live must not set job-level variables"
[ "$(printf '%s\n' "$job" | awk '/^  artifacts:$/ { f = 1; next } /^  [a-z_]+:/ { f = 0 } f && /^      - /' | tr -d ' ')" = "-provider-staging-live.log" ] || bad "provider-staging-live may publish only provider-staging-live.log"
printf '%s\n' "$job" | grep -qE '^  cache:' && bad "provider-staging-live must not use a cache"
printf '%s\n' "$job" | grep -qE '^  before_script:' && bad "provider-staging-live must not have a before_script (it runs before the allowlist)"
printf '%s\n' "$job" | grep -qE '^  (default|extends|trigger):' && bad "provider-staging-live must not extend or default another job"
if ! printf '%s\n' "$job" | awk '/^  inherit:$/ { f = 1; next } f && /^    variables: false$/ { ok = 1 } { f = 0 } END { exit !ok }'; then
  bad "provider-staging-live must set inherit: variables: false"
fi
printf '%s\n' "$job" | grep -qE '^    - /bin/sh ci/provider-staging-live\.sh$' || bad "the job must run exactly: /bin/sh ci/provider-staging-live.sh"
[ "$(printf '%s\n' "$job" | awk '/^  script:$/ { f = 1; next } /^  [a-z_]+:/ { f = 0 } f' | grep -c .)" -eq 1 ] || bad "the job script must be that single line"
[ "$rc" -eq 0 ] && echo "check-provider-staging-live: clean"
exit "$rc"
