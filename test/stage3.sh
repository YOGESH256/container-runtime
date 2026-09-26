#!/usr/bin/env bash
# Stage 3, overlayfs. Done when the same rootfs runs twice with independent
# writes and the layers stay untouched.
source "$(dirname "$0")/lib.sh"
bash scripts/fetch-alpine-rootfs.sh "$ROOTFS" >/dev/null || exit 1
run() { "$MYCRT" run --rootfs "$ROOTFS" "$@"; }
CONTAINERS=/var/lib/mycrt/containers

# Fingerprint of the lower layer: every path's metadata plus file contents.
snapshot() {
  (cd "$ROOTFS" && find . -printf '%p %y %s %m %U %G %T@ %l\n' | sort
   cd "$ROOTFS" && find . -type f -exec sha256sum {} + | sort) | sha256sum
}
before=$(snapshot)

section "Independent writes"
"$MYCRT" run --rootfs "$ROOTFS" sh -c 'echo A > /etc/motd; sleep 1; cat /etc/motd' >"$tmp/a" &
"$MYCRT" run --rootfs "$ROOTFS" sh -c 'echo B > /etc/motd; sleep 1; cat /etc/motd' >"$tmp/b" &
wait
check "container A reads back its own write" test "$(cat "$tmp/a")" = A
check "container B reads back its own write" test "$(cat "$tmp/b")" = B
run sh -c 'rm /etc/alpine-release && touch /new-file && rm -rf /etc/apk'
check "a new container starts from the pristine image" run test -e /etc/alpine-release -a ! -e /new-file -a -d /etc/apk

section "Layers untouched"
check "deleted file still exists in the rootfs" test -e "$ROOTFS/etc/alpine-release"
check "created file did not leak into the rootfs" test ! -e "$ROOTFS/new-file"
check "rootfs is byte-for-byte unchanged" test "$(snapshot)" = "$before"

section "Inside the upper layer"
"$MYCRT" run --rootfs "$ROOTFS" sh -c 'echo hi > /etc/motd; rm /etc/alpine-release; rm -rf /etc/apk; mkdir /etc/apk; echo ready; sleep 2' >"$tmp/u" &
p=$!
wait_for "$tmp/u" ready
upper=$(ls -d "$CONTAINERS"/*/upper)
echo "        upper layer: $upper"
(cd "$upper" && find . -mindepth 1 | sort | sed 's/^/          /')
check "modified file was copied up" test "$(cat "$upper/etc/motd")" = hi
check "untouched files were not copied" test ! -e "$upper/bin/busybox"
check "deleted file is a whiteout, a 0:0 char device" test "$(stat -c '%F %t:%T' "$upper/etc/alpine-release")" = "character special file 0:0"
check "recreated directory is marked opaque" python3 -c "import os,sys; sys.exit(os.getxattr('$upper/etc/apk', 'trusted.overlay.opaque') != b'y')"
check "overlay mount is invisible to the host" not grep -q overlay /proc/self/mountinfo
wait "$p"

section "Cleanup"
check "container directories are removed after exit" test -z "$(ls -A "$CONTAINERS")"
check "no stale cgroups" test -z "$(find /sys/fs/cgroup/mycrt -mindepth 1 -maxdepth 1 -type d)"
finish
