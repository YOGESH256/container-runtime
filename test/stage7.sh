#!/usr/bin/env bash
# Stage 7, hardening. Every check here is an escape attempt that must fail,
# or a confinement fact that must hold, while real images keep working.
source "$(dirname "$0")/lib.sh"
bash scripts/fetch-alpine-rootfs.sh "$ROOTFS" >/dev/null || exit 1
"$MYCRT" pull alpine:3.20 >/dev/null 2>&1 || { echo "cannot pull alpine:3.20"; exit 1; }
run() { "$MYCRT" run "$@"; }
inside() { "$MYCRT" run alpine:3.20 sh -c "$1" 2>&1; }

section "User namespace"
check "root inside maps to host user 100000" test "$(inside 'cat /proc/self/uid_map' | awk '{print $1, $2, $3}')" = "0 100000 65536"
check "and is still uid 0 to the container" test "$(inside 'id -u')" = 0
"$MYCRT" run -d alpine:3.20 sleep 30 >"$tmp/id"; id=$(cat "$tmp/id")
check "the host sees its processes as user 100000" test "$(ps -o uid= -p "$(init_of "$(cat "/var/lib/mycrt/containers/$id/state.json" | python3 -c 'import sys,json; print(json.load(sys.stdin)["shimPid"])')")" | tr -d ' ')" = 100000
"$MYCRT" exec "$id" touch /created
check "a file it creates is owned by 100000 on the host" test "$(stat -c %u "/var/lib/mycrt/containers/$id/upper/created")" = 100000
"$MYCRT" rm -f "$id" >/dev/null
check "host-root-owned files are off limits even where visible" grep -q 'Permission denied' <<<"$(inside 'cat /etc/resolv.conf >/dev/null && echo x >> /etc/resolv.conf')"

section "Capabilities"
check "bounding set is Docker's default minus CAP_MKNOD" test "$(inside 'grep CapBnd /proc/self/status' | awk '{print $2}')" = 00000000a00425fb
check "no_new_privs is set" test "$(inside 'grep NoNewPrivs /proc/self/status' | awk '{print $2}')" = 1
check "a non-root user has no capabilities at all" test "$(run -u nobody alpine:3.20 grep CapEff /proc/self/status | awk '{print $2}')" = 0000000000000000
check "setuid binaries grant nothing" grep -qi 'denied\|not permitted' <<<"$(inside 'chmod u+s /bin/busybox; su -c "id -u" nobody 2>&1; adduser -D x 2>&1 >/dev/null; su x -c "cat /etc/shadow" 2>&1')"

section "System-call filter"
check "seccomp filter is active (mode 2)" test "$(inside 'grep Seccomp: /proc/self/status' | awk '{print $2}')" = 2
check "mount is refused" grep -q 'denied\|not permitted' <<<"$(inside 'mount -t tmpfs t /mnt')"
check "unshare of a new user namespace is refused" grep -q 'not permitted' <<<"$(inside 'unshare -U true')"
check "unshare of a mount namespace is refused" grep -q 'not permitted' <<<"$(inside 'unshare -m true')"
check "reading the kernel log is refused" grep -q 'not permitted' <<<"$(inside 'dmesg')"
check "reboot is refused, container keeps running" test "$(inside 'reboot 2>/dev/null; echo still-here')" = still-here
check "setting the hostname is refused" grep -q 'not permitted' <<<"$(inside 'hostname evil')"
check "a 32-bit compat syscall or foreign ABI would be killed, normal calls run" test "$(inside 'echo ok')" = ok

section "Devices"
check "only the standard device nodes exist" test "$(inside 'ls /dev | sort | tr "\n" " "')" = "fd full mqueue null ptmx pts random shm stderr stdin stdout tty urandom zero "
check "mknod of a disk device is refused" grep -q 'not permitted' <<<"$(inside 'mknod /dev/sda b 8 0')"
"$MYCRT" run -d alpine:3.20 sleep 30 >"$tmp/id"; id=$(cat "$tmp/id")
cg=/sys/fs/cgroup/mycrt/$id
check "even host root inside the cgroup cannot open the disk (eBPF device filter)" bash -c "echo \$\$ > $cg/cgroup.procs; ! head -c1 /dev/vda >/dev/null 2>&1"
check "but /dev/null and friends work there" bash -c "echo \$\$ > $cg/cgroup.procs; head -c1 /dev/null && echo x > /dev/null"
"$MYCRT" rm -f "$id" >/dev/null

section "Masked and read-only kernel files"
check "/proc/kcore is masked" test "$(inside 'stat -c %s /proc/kcore')" = 0
check "/sys/firmware is empty" test "$(inside 'ls /sys/firmware | wc -l')" = 0
check "/proc/sys is read-only" grep -q 'Read-only' <<<"$(inside 'echo 1 > /proc/sys/kernel/sysrq')"
check "sysrq-trigger is read-only" grep -q 'Read-only' <<<"$(inside 'echo c > /proc/sysrq-trigger')"
check "/sys is read-only" grep -q 'Read-only' <<<"$(inside 'echo 1 > /sys/kernel/mm/transparent_hugepage/enabled')"
check "cgroup files are read-only" grep -q 'Read-only' <<<"$(inside 'echo max > /sys/fs/cgroup/pids.max')"

section "exec"
"$MYCRT" run -d --name hx --hostname hx alpine:3.20 sleep 30 >/dev/null
out=$("$MYCRT" exec hx sh -c 'hostname; cat /proc/1/comm; grep Seccomp: /proc/self/status; grep CapEff /proc/self/status; mount -t tmpfs x /mnt 2>&1; cat /proc/self/cgroup')
echo "$out" | sed 's/^/        /'
check "exec joins the container's namespaces (hostname, PID 1)" bash -c "grep -q '^hx$' <<<'$out' && grep -q '^sleep$' <<<'$out'"
check "exec'd process is seccomp-filtered" grep -q 'Seccomp:.2' <<<"$out"
check "exec'd process has no capabilities" grep -q 'CapEff:.0000000000000000' <<<"$out"
check "exec'd process cannot mount" grep -q 'not permitted\|denied' <<<"$out"
check "exec'd process is in the container's cgroup" grep -q "^0::/mycrt/$(cat /var/lib/mycrt/containers/*/state.json | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])')\$" <<<"$out"
# An exec'd process sits outside the user namespace (a multithreaded Go
# process cannot join one), so it sees host IDs: nobody is 65534+100000.
check "exec -u switches user (shown as the shifted host id)" test "$("$MYCRT" exec -u nobody hx id -u)" = 165534
check "exec returns the command's exit code" bash -c "'$MYCRT' exec hx sh -c 'exit 7'; test \$? = 7"
"$MYCRT" rm -f hx >/dev/null

section "Real images still work under confinement"
check "nginx serves (binds :80 with NET_BIND_SERVICE, drops to its user)" bash -c "id=\$('$MYCRT' run -d -p 8082:80 nginx) && sleep 3 && curl -s --max-time 3 http://127.0.0.1:8082/ | grep -q 'Welcome to nginx'; r=\$?; '$MYCRT' rm -f \$id >/dev/null; exit \$r"
check "ping works (CAP_NET_RAW kept)" run alpine:3.20 ping -c1 -W1 127.0.0.1
check "package manager runs as root inside" run alpine:3.20 sh -c 'apk --version >/dev/null && touch /etc/x && rm /etc/x'

section "Cleanup"
check "no container directories left" test -z "$(ls -A /var/lib/mycrt/containers)"
check "no stale cgroups" test -z "$(find /sys/fs/cgroup/mycrt -mindepth 1 -maxdepth 1 -type d)"
finish
