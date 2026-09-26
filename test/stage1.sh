#!/usr/bin/env bash
# Stage 1, isolation. Done when ps inside shows the shell as PID 1, the
# hostname differs from the host, and zombies get reaped.
source "$(dirname "$0")/lib.sh"
bash scripts/fetch-alpine-rootfs.sh "$ROOTFS" >/dev/null || exit 1

run() { "$MYCRT" run --rootfs "$ROOTFS" "$@"; }

section "PID namespace"
# "; true" stops sh from exec'ing ps in its place, so sh stays PID 1.
out=$(run sh -c 'ps -o pid,comm; true')
echo "$out" | sed 's/^/        /'
check "shell is PID 1" grep -Eq '^ *1 +sh$' <<<"$out"
check "only the container's processes are visible" test "$(wc -l <<<"$out")" -eq 3

section "UTS namespace"
check "hostname is set inside" test "$(run --hostname box1 hostname)" = box1
check "default hostname is the container ID" grep -Eq '^[0-9a-f]{12}$' <<<"$(run hostname)"
check "host hostname is unchanged" test "$(hostname)" != box1

section "Mount namespace"
check "sees the image's files" run test -e /etc/alpine-release
check "does not see the host's files" not run test -e /etc/lsb-release
before=$(wc -l </proc/self/mountinfo)
run sh -c 'mount -t tmpfs scratch /mnt && echo ready && sleep 1' >"$tmp/m" &
wait_for "$tmp/m" ready
check "container mounts stay invisible to the host" test "$(wc -l </proc/self/mountinfo)" = "$before"
wait
check "no rootfs mount left behind" not grep -q "$ROOTFS" /proc/self/mountinfo

section "Network namespace"
links=$(run --net none ip -o link)
check "only a loopback interface" test "$(grep -c . <<<"$links")" = 1
check "loopback is up" run --net none ping -c1 -W1 127.0.0.1

section "IPC namespace"
qid=$(ipcmk -Q | awk '{print $NF}')
check "host message queue is invisible" test "$(run cat /proc/sysvipc/msg | wc -l)" = 1
ipcrm -q "$qid"

section "Zombie reaping"
out=$(run sh -c '(sleep 0.2 &); sleep 1; ps -o stat,comm; true')
check "shell as PID 1 reaps orphans" not grep -q '^Z' <<<"$out"
# A PID 1 that never calls wait() collects zombies. That is the problem
# --init exists for. The orphan below is re-parented to PID 1, which is
# `sleep 2` after the exec, so it stays a zombie until the container ends.
"$MYCRT" run --rootfs "$ROOTFS" sh -c '(sleep 0.2 &); echo ready; exec sleep 3' >"$tmp/z1" &
p=$!
wait_for "$tmp/z1" ready
sleep 0.6
init=$(init_of "$p")
check "non-reaping PID 1 leaves a zombie" grep -q Z <<<"$(ps -o stat= --ppid "$init")"
wait "$p"
"$MYCRT" run --rootfs "$ROOTFS" --init sh -c '(sleep 0.2 &); echo ready; exec sleep 3' >"$tmp/z2" &
p=$!
wait_for "$tmp/z2" ready
sleep 0.6
init=$(init_of "$p")
check "--init shows mycrt-init as PID 1" test "$(ps -o comm= -p "$init")" = mycrt-init
check "--init reaps the orphan" not grep -q Z <<<"$(ps -o stat= --ppid "$init")"
wait "$p"

section "Exit codes and signals"
run sh -c 'exit 7'
check "exit code passes through" test $? = 7
run sh -c 'nosuchcmd'
check "missing command inside shell gives 127" test $? = 127
run /no/such/binary 2>/dev/null
check "missing binary gives 127" test $? = 127
"$MYCRT" run --rootfs "$ROOTFS" sh -c 'echo ready; exec sleep 5' >"$tmp/k" &
p=$!
wait_for "$tmp/k" ready
kill -KILL "$(init_of "$p")"
wait "$p"
check "killed PID 1 gives 128+9" test $? = 137
"$MYCRT" run --rootfs "$ROOTFS" sh -c 'trap "echo got-term; exit 3" TERM; echo ready; while :; do sleep 0.1; done' >"$tmp/out" &
p=$!
wait_for "$tmp/out" ready
kill -TERM "$p"
wait "$p"
code=$?
check "SIGTERM to mycrt reaches PID 1" grep -q got-term "$tmp/out"
check "PID 1's exit code is returned" test "$code" = 3
"$MYCRT" run --rootfs "$ROOTFS" sh -c 'echo ready; exec sleep 30' >"$tmp/s" &
p=$!
wait_for "$tmp/s" ready
kill -TERM "$p"
sleep 0.5
check "PID 1 without a handler ignores SIGTERM" kill -0 "$p"
kill -KILL "$(init_of "$p")"
wait "$p"
"$MYCRT" run --rootfs "$ROOTFS" --init sh -c 'echo ready; exec sleep 30' >"$tmp/i" &
p=$!
wait_for "$tmp/i" ready
kill -TERM "$p"
wait "$p"
check "--init relays SIGTERM to the command" test $? = $((128 + 15))
# A stop request that lands while the container is still being set up is
# queued and relayed once it runs, and nothing is left behind.
"$MYCRT" run --rootfs "$ROOTFS" --init sleep 30 &
p=$!
sleep 0.05
kill -TERM "$p"
wait "$p"
check "SIGTERM during startup still stops the container" test $? = $((128 + 15))

section "Cleanup"
check "no container processes left" not pgrep -f mycrt-init
check "no container directories left" test -z "$(ls -A /var/lib/mycrt/containers)"
check "no stale cgroups" test -z "$(find /sys/fs/cgroup/mycrt -mindepth 1 -maxdepth 1 -type d)"
finish
