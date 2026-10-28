#!/bin/sh
# Guards the hardening of ci/provider-staging-live.sh and of its job in .gitlab-ci.yml (PROV-001). The job carries the
# staging API key; these lines keep a pipeline variable from sending it elsewhere. Runs in the `test` job.
set -eu
script=ci/provider-staging-live.sh
rc=0

need() { # need DESCRIPTION FIXED-STRING [FILE]
  if ! grep -qF -- "$2" "${3:-$script}"; then
    echo "check-provider-staging-live: missing in ${3:-$script}: $1" >&2
    rc=1
  fi
}

need "the unset list" "unset http_proxy HTTP_PROXY https_proxy HTTPS_PROXY all_proxy ALL_PROXY no_proxy NO_PROXY CURL_HOME LD_PRELOAD LD_LIBRARY_PATH GOPROXY GONOSUMDB GONOSUMCHECK GONOPROXY GOINSECURE GOPRIVATE GOFLAGS GOENV TF_CLI_CONFIG_FILE TF_PLUGIN_CACHE_DIR"
need "GOFLAGS" "GOFLAGS=-mod=readonly"
need "GOPROXY" "GOPROXY=https://proxy.golang.org"
need "GOSUMDB" "GOSUMDB=sum.golang.org"
need "NO_PROXY" "NO_PROXY='*'"
need "the refusal" "refusing to run: the pipeline carries variables"
need "go mod verify" "go mod verify"
need "the module proxy off while the key is set" "GOPROXY=off"
need "the pinned tofu checksum check" "sha256sum -c"
if ! grep -qE '^TOFU_ZIP_SHA256=[0-9a-f]{64}$' "$script"; then
  echo "check-provider-staging-live: TOFU_ZIP_SHA256 is not a pinned 64-digit hex literal" >&2
  rc=1
fi
# Every curl goes through curl_safe, which is the one line that carries -q --noproxy '*'.
if [ "$(grep -cE "^[[:space:]]+curl -q --noproxy '\*' \"\\\$@\"$" "$script")" -ne 1 ]; then
  echo "check-provider-staging-live: curl_safe must be defined once as: curl -q --noproxy '*' \"\$@\"" >&2
  rc=1
fi
if grep -vE '^[[:space:]]*#' "$script" | grep -E '(^|[^_a-z])curl ' | grep -vqE "curl -q --noproxy '\*' \"\\\$@\""; then
  echo "check-provider-staging-live: a bare curl call (use curl_safe)" >&2
  rc=1
fi
if grep -nE '(^|[[:space:]])(set -x|set -o xtrace|env|printenv)([[:space:]]|$)' "$script" | grep -vE '^[0-9]+:[[:space:]]*#'; then
  echo "check-provider-staging-live: a command that can print the environment" >&2
  rc=1
fi
# The job: no cache shared with unprivileged jobs, no inherited global variables.
job=$(awk '/^provider-staging-live:/ { f = 1; next } /^[A-Za-z.]/ { f = 0 } f' .gitlab-ci.yml)
if printf '%s\n' "$job" | grep -qE '^  cache:'; then
  echo "check-provider-staging-live: provider-staging-live must not use a cache" >&2
  rc=1
fi
if ! printf '%s\n' "$job" | awk '/^  inherit:$/ { f = 1; next } f && /^    variables: false$/ { ok = 1 } { f = 0 } END { exit !ok }'; then
  echo "check-provider-staging-live: provider-staging-live must set inherit: variables: false" >&2
  rc=1
fi
[ "$rc" -eq 0 ] && echo "check-provider-staging-live: clean"
exit "$rc"
