# Shared helpers for the stage tests. They run as root inside the VM.
set -u
cd "$(dirname "${BASH_SOURCE[0]}")/.."
MYCRT=${MYCRT:-/usr/local/bin/mycrt}
ROOTFS=${ROOTFS:-/var/lib/mycrt-test/alpine}
PASS=0
FAIL=0
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

if [ "${NEED_ROOT:-1}" = 1 ] && [ "$(id -u)" != 0 ]; then
  echo "run as root" >&2
  exit 1
fi

section() { printf '\n== %s\n' "$1"; }

# check NAME COMMAND...: runs the command and records PASS or FAIL.
check() {
  local name=$1
  shift
  if "$@" >/dev/null 2>&1; then
    printf '  PASS  %s\n' "$name"
    PASS=$((PASS + 1))
  else
    printf '  FAIL  %s\n' "$name"
    FAIL=$((FAIL + 1))
  fi
}

not() { ! "$@"; }

# wait_for FILE PATTERN [SECONDS]: poll until PATTERN appears in FILE.
wait_for() {
  local i
  for ((i = 0; i < ${3:-30} * 10; i++)); do
    grep -q "$2" "$1" 2>/dev/null && return 0
    sleep 0.1
  done
  return 1
}

# init_of MYCRT_PID: print the host PID of the container's PID 1, the child
# of mycrt that is process 1 inside its own PID namespace.
init_of() {
  local i c
  for ((i = 0; i < 100; i++)); do
    for c in $(pgrep -P "$1"); do
      if awk '/^NSpid:/ { exit !(NF == 3 && $3 == 1) }' "/proc/$c/status" 2>/dev/null; then
        echo "$c"
        return 0
      fi
    done
    sleep 0.05
  done
  return 1
}

finish() {
  printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
  [ "$FAIL" -eq 0 ]
}
