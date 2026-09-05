#!/usr/bin/env bash
# Run the test suite under the race detector in a Linux container.
#
# Why this exists: the race detector requires cgo and a working C toolchain,
# and the Windows development machine's gcc is broken. Rather than fix a
# machine, run the detector where it works. Everything the suite needs -- Go
# and git -- is in the image; nothing is installed on the host.
#
# Usage:
#   scripts/test-race.sh                 # whole suite, race detector on
#   scripts/test-race.sh ./internal/...  # a subset
#   scripts/test-race.sh -run TestFoo ./internal/pipeline
#
# Runs from Git Bash on Windows, and from a shell on Linux/macOS.
set -euo pipefail

cd "$(dirname "$0")/.."

IMAGE=firstpass-test

# On Windows, Docker Desktop needs a drive-letter path; `pwd -W` gives one.
# Elsewhere plain $PWD is right.
if command -v cygpath >/dev/null 2>&1 || [ -n "${MSYSTEM:-}" ]; then
	HOST_SRC="$(pwd -W)"
	# Git Bash rewrites anything that looks like a Unix path in a command's
	# arguments, so `-w /src` reaches docker as `C:/Program Files/Git/src`.
	# Disabling that for this script is the documented escape hatch.
	export MSYS_NO_PATHCONV=1
	export MSYS2_ARG_CONV_EXCL="*"
else
	HOST_SRC="$PWD"
fi

if ! docker info >/dev/null 2>&1; then
	echo "docker is not running. Start Docker Desktop and retry." >&2
	exit 1
fi

echo "==> building $IMAGE"
docker build -q -f Dockerfile.test -t "$IMAGE" . >/dev/null

# Default to the whole tree, but only when the caller named no packages. A
# plain `${@:-./...}` gets this wrong: passing just a flag (`-count=1`) counts
# as "arguments present", so the default drops out and `go test` falls back to
# the current directory, which holds no Go files.
ARGS=("$@")
has_pkg=0
for a in "$@"; do
	case "$a" in
	./* | . | all) has_pkg=1 ;;
	esac
done
if [ "$has_pkg" -eq 0 ]; then
	ARGS+=("./...")
fi

# Named volumes keep the module and build caches between runs, so a re-run is
# seconds rather than a full rebuild. They are the container's own caches; the
# host's GOPATH is never touched.
echo "==> go test -race ${ARGS[*]}"
exec docker run --rm \
	-v "${HOST_SRC}:/src" \
	-v firstpass-gomod:/go/pkg/mod \
	-v firstpass-gocache:/root/.cache/go-build \
	-w /src \
	"$IMAGE" \
	go test -race "${ARGS[@]}"
