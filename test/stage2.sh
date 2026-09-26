#!/usr/bin/env bash
# Stage 2, cgroups v2. Done when runaway process creation inside is stopped
# at the pids limit and no stale cgroups remain after exit.
source "$(dirname "$0")/lib.sh"
bash scripts/fetch-alpine-rootfs.sh "$ROOTFS" >/dev/null || exit 1
run() { "$MYCRT" run --rootfs "$ROOTFS" "$@"; }
CG=/sys/fs/cgroup/mycrt

section "Placement and limits"
"$MYCRT" run --rootfs "$ROOTFS" --memory 64m --cpus 0.5 --pids 50 sh -c 'echo ready; exec sleep 2' >"$tmp/pl" &
p=$!
wait_for "$tmp/pl" ready
cgpath=$(cut -d: -f3 "/proc/$(init_of "$p")/cgroup")
check "host sees the container in /mycrt/<id>" grep -Eq '^/mycrt/[0-9a-f]{12}$' <<<"$cgpath"
check "memory.max is 64M" test "$(cat "/sys/fs/cgroup$cgpath/memory.max")" = 67108864
check "cpu.max is half a CPU" test "$(cat "/sys/fs/cgroup$cgpath/cpu.max")" = "50000 100000"
check "pids.max is 50" test "$(cat "/sys/fs/cgroup$cgpath/pids.max")" = 50
wait "$p"
check "cgroup is removed after exit" test ! -e "/sys/fs/cgroup$cgpath"
check "container sees its own cgroup as the root" test "$(run cat /proc/self/cgroup)" = "0::/"
check "and reads its own limit" test "$(run --memory 64m cat /sys/fs/cgroup/memory.max)" = 67108864
check "cgroup files are read-only inside" not run sh -c 'echo max > /sys/fs/cgroup/memory.max'

section "pids limit"
# A child shell starts background sleeps until fork fails. The parent then
# reads the counters with shell builtins only, since no fork can succeed.
start=$(date +%s)
out=$(run --pids 20 sh -s <<'SH'
sh -c 'i=0; while [ $i -lt 100 ]; do sleep 30 & i=$((i+1)); done' 2>/dev/null
read cur < /sys/fs/cgroup/pids.current
while read k v; do [ "$k" = max ] && hits=$v; done < /sys/fs/cgroup/pids.events
echo "current=$cur hits=$hits"
SH
)
code=$?
echo "        $out"
# At the limit: PID 1, the spawner shell and 18 sleeps. The spawner then
# exits because its fork failed, so 19 remain.
cur=$(sed -n 's/.*current=\([0-9]*\).*/\1/p' <<<"$out")
check "process count never passes pids.max" test "${cur:-99}" -le 20 -a "${cur:-0}" -ge 18
check "kernel refused forks past the limit" grep -Eq 'hits=[1-9]' <<<"$out"
check "container still exits cleanly" test "$code" = 0
check "leftover sleeps die with PID 1" test $(($(date +%s) - start)) -lt 10

section "Memory limit"
check "work under the limit succeeds" test "$(run --memory 32m awk 'BEGIN { s = "x"; for (i = 0; i < 20; i++) s = s s; print length(s) }')" = 1048576
run --memory 32m awk 'BEGIN { s = "x"; while (1) s = s s }' 2>"$tmp/err"
code=$?
check "memory hog is OOM-killed (exit 137)" test "$code" = 137
check "mycrt reports the OOM kill" grep -q OOM-killed "$tmp/err"

section "CPU limit"
# Spin one CPU for 2 seconds of wall time, then read the group's CPU time.
spin='timeout 2 sh -c "while :; do :; done" 2>/dev/null; while read k v; do case $k in usage_usec|nr_throttled) echo $k=$v;; esac; done < /sys/fs/cgroup/cpu.stat'
limited=$(run --cpus 0.5 sh -c "$spin")
free=$(run sh -c "$spin")
lim_us=$(sed -n 's/usage_usec=//p' <<<"$limited")
free_us=$(sed -n 's/usage_usec=//p' <<<"$free")
echo "        --cpus 0.5: ${lim_us}us, unlimited: ${free_us}us of CPU in 2s"
check "limited container gets about half a CPU" test "$lim_us" -gt 700000 -a "$lim_us" -lt 1300000
check "kernel throttled it" grep -Eq 'nr_throttled=[1-9]' <<<"$limited"
check "unlimited container gets a full CPU" test "$free_us" -gt 1700000

section "Cleanup"
"$MYCRT" run --rootfs "$ROOTFS" sh -c 'echo ready; exec sleep 30' >"$tmp/c" &
p=$!
wait_for "$tmp/c" ready
kill -KILL "$(init_of "$p")"
wait "$p"
check "no stale container cgroups" test -z "$(find "$CG" -mindepth 1 -maxdepth 1 -type d)"
finish
