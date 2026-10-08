#!/bin/bash
# Runs an e2e suite in a container. Builds both CLIs for Linux first.
#
#   ./run.sh <suite> [robot options...]
#       suites: cli account users user-access usage billing, or "fast" (all but billing)
#   ./run.sh shell       a shell in the test container, for debugging
#   ./run.sh build       rebuild the image
#
#   Variable file: config/$USER.py, or E2E_CONFIG=<file> (not needed for cli).
#   Reports: results/<suite>-<UTC time>/report.html; results/latest points to the newest.
#   Needs Docker (or E2E_RUNTIME=podman); Go on the host is optional (else a golang image builds).
#   ~/.aws is mounted into the container. Every suite but cli uses a real AWS account.
#   Examples: ./run.sh cli    ./run.sh users --test "Pause*"    ./run.sh billing --include phase1
set -euo pipefail
cd "$(dirname "$0")"
here=$PWD
repo=$(cd ../.. && pwd)
runtime=${E2E_RUNTIME:-docker}
image=bedrock-e2e:$(cat Dockerfile requirements.txt install-awscli.py | shasum | cut -c1-12)

usage() { sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//'; exit 1; }
die() { echo "error: $*" >&2; exit 1; }
build_image() { "$runtime" build -t "$image" .; }

arch() {
  local a
  a=$("$runtime" info --format '{{.Architecture}}' 2>/dev/null || "$runtime" info --format '{{.Host.Arch}}')
  case "$a" in
    x86_64|amd64) echo amd64 ;;
    aarch64|arm64) echo arm64 ;;
    *) die "unknown container architecture $a" ;;
  esac
}

build_clis() {
  local goarch build
  goarch=$(arch)
  build="go generate ./... && for c in bedrock-admin bedrock; do
    CGO_ENABLED=0 GOOS=linux GOARCH=$goarch go build -o tests/e2e/bin/\$c ./cmd/\$c || exit 1; done"
  echo "Building bedrock-admin and bedrock for linux/$goarch"
  if command -v go >/dev/null; then
    (cd "$repo" && sh -c "$build")
  else
    local v
    v=$(sed -n 's/^go \([0-9.]*\)$/\1/p' "$repo/go.mod")
    "$runtime" run --rm --user "$(id -u):$(id -g)" -e HOME=/tmp -e GOCACHE=/tmp/go-cache -e GOPATH=/tmp/go \
      -v "$repo:/src" -w /src "golang:$v" sh -c "$build"
  fi
}

suite=${1:-}; shift || true
extra=()
case "$suite" in
  cli|account|users|user-access|usage|billing) target=suites/$suite.robot ;;
  fast) target=suites; extra=(--exclude billing) ;;
  shell) target= ;;
  build) build_image; exit ;;
  *) usage ;;
esac
command -v "$runtime" >/dev/null || die "$runtime is not installed"
"$runtime" image inspect "$image" >/dev/null 2>&1 || build_image
build_clis

config=${E2E_CONFIG:-config/$USER.py}
vars=()
if [ -f "$config" ]; then
  config=$(cd "$(dirname "$config")" && pwd)/$(basename "$config")
  vars=(--variablefile /home/e2e/config.py)
elif [ "$suite" != cli ] && [ "$suite" != shell ]; then
  die "no variable file $config. Copy config/example.py to it and fill it in."
fi

mkdir -p ~/.aws results
tty=(-i); [ -t 0 ] && [ -t 1 ] && tty=(-it)
run=("$runtime" run --rm "${tty[@]}" --user "$(id -u):$(id -g)"
  -v "$repo:/work:ro"
  -v "$here/results:/work/tests/e2e/results"
  -v "$HOME/.aws:/home/e2e/.aws"
  -e TERM="${TERM:-xterm}" -e E2E_BIN=/work/tests/e2e/bin -e E2E_FIXTURES=/work/tests/fixtures)
[ ${#vars[@]} -gt 0 ] && run+=(-v "$config:/home/e2e/config.py:ro")
run+=("$image")

if [ -z "$target" ]; then
  exec "${run[@]}" bash
fi
out=results/$suite-$(date -u +%Y%m%dT%H%M%SZ)
mkdir -p "$out"
ln -sfn "$(basename "$out")" results/latest
echo "Report: $out/report.html   Follow it live: tail -f $out/debug.log"
exec "${run[@]}" robot ${vars[@]+"${vars[@]}"} ${extra[@]+"${extra[@]}"} --outputdir "$out" --console verbose \
  --debugfile debug.log "$@" "$target"
