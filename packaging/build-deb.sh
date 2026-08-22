#!/bin/bash
# Build vtessera .deb packages.
#
# Requires: dpkg-buildpackage, debhelper, cargo, rustc, dh-cargo.
# Run from the repo root on a Debian/Ubuntu machine.
#
# Usage:
#   ./packaging/build-deb.sh          # builds all 4 .deb files
#   ./packaging/build-deb.sh --clean   # clean build tree after

set -euo pipefail

cd "$(dirname "$0")/.."

# Ensure Cargo.lock is present (required for --locked builds).
if [ ! -f Cargo.lock ]; then
    echo "ERROR: Cargo.lock not found. Run cargo generate-lockfile first." >&2
    exit 1
fi

echo "Building vtessera .deb packages..."
echo "  Source: $(pwd)"
echo "  Target: $(pwd)/packaging/debian/"

dpkg-buildpackage -b -uc -us --jobs=auto

echo ""
echo "Built packages:"
ls -1 ../vtessera*.deb 2>/dev/null || ls -1 ../*.deb 2>/dev/null || echo "(check parent directory)"

if [ "${1:-}" = "--clean" ]; then
    echo "Cleaning build tree..."
    dh clean --buildsystem=cargo 2>/dev/null || true
    rm -rf debian/vtessera debian/vtessera-node debian/vtessera-offer-index debian/vtessera-settle
fi
