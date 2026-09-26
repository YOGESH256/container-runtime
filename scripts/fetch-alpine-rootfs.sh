#!/usr/bin/env bash
# Downloads and unpacks the Alpine minirootfs tarball, checking its sha256.
# Stages 1 to 3 run containers from this directory; stage 4 replaces it
# with real image pulls.
set -euo pipefail
dest=${1:-/var/lib/mycrt-test/alpine}
branch=${ALPINE_BRANCH:-v3.20}
arch=$(uname -m)
base="https://dl-cdn.alpinelinux.org/alpine/$branch/releases/$arch"

if [ -x "$dest/bin/busybox" ]; then
  echo "$dest already populated"
  exit 0
fi
meta=$(curl -fsSL "$base/latest-releases.yaml")
file=$(awk '/flavor: alpine-minirootfs/{f=1} f && /^ *file:/{print $2; exit}' <<<"$meta")
sum=$(awk '/flavor: alpine-minirootfs/{f=1} f && /^ *sha256:/{print $2; exit}' <<<"$meta")
[ -n "$file" ] && [ -n "$sum" ] || { echo "could not find minirootfs in $base" >&2; exit 1; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL -o "$tmp/$file" "$base/$file"
echo "$sum  $tmp/$file" | sha256sum -c --quiet
mkdir -p "$tmp/rootfs"
tar -xzf "$tmp/$file" -C "$tmp/rootfs" --numeric-owner
mkdir -p "$(dirname "$dest")"
mv "$tmp/rootfs" "$dest"
echo "unpacked $file into $dest"
