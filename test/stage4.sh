#!/usr/bin/env bash
# Stage 4, OCI images. Done when `mycrt run alpine:3.20 sh` and
# `mycrt run nginx` work from a cold cache.
source "$(dirname "$0")/lib.sh"
echo "registry: ${MYCRT_REGISTRY_MIRROR:-Docker Hub}"

section "alpine:3.20 from a cold cache"
rm -rf /var/lib/mycrt/images
out=$("$MYCRT" run alpine:3.20 sh -c 'cat /etc/alpine-release; ps -o pid,comm; true' 2>"$tmp/pull")
sed 's/^/        /' "$tmp/pull"
check "image was pulled" grep -q "pulled alpine:3.20" "$tmp/pull"
check "alpine 3.20 userland runs" grep -Eq '^3\.20\.' <<<"$out"
check "shell is PID 1" grep -Eq '^ *1 +sh$' <<<"$out"
out=$(printf 'grep PRETTY /etc/os-release\nexit\n' | script -qec "$MYCRT run alpine:3.20 sh" /dev/null)
check "interactive shell works on a terminal" grep -q 'Alpine Linux' <<<"$out"

section "Local cache"
"$MYCRT" run alpine:3.20 true 2>"$tmp/second"
check "second run uses the cache, no pull" test ! -s "$tmp/second"
"$MYCRT" pull alpine:3.20 2>"$tmp/uptodate" >/dev/null
check "pull of a current image is a cheap HEAD check" grep -q "up to date" "$tmp/uptodate"
check "images lists it" grep -q '^alpine:3.20 ' <<<"$("$MYCRT" images)"

section "nginx from a cold cache"
"$MYCRT" run nginx >"$tmp/nginx" 2>&1 &
p=$!
for _ in $(seq 240); do grep -q "start worker processes" "$tmp/nginx" && break; sleep 0.5; done
grep -E "pulled|layer" "$tmp/nginx" | sed 's/^/        /'
init=$(init_of "$p")
check "entrypoint script configured nginx" grep -q "Configuration complete" "$tmp/nginx"
check "nginx serves its welcome page" bash -c "nsenter -t $init -n curl -s http://127.0.0.1/ | grep -q 'Welcome to nginx'"
check "workers dropped to the image's nginx user (uid 101, host 100101)" grep -qx ' *100101' <<<"$(ps -o uid= --ppid "$init" | head -1)"
kill -TERM "$p"
wait "$p"
code=$?
check "SIGTERM became nginx's stop signal, SIGQUIT" grep -q "SIGQUIT" "$tmp/nginx"
check "nginx shut down gracefully (exit 0)" test "$code" = 0

section "Image config and overrides"
check "image Env is applied" grep -q '^NGINX_VERSION=' <<<"$("$MYCRT" run nginx env)"
check "-e overrides image Env" grep -qx 'NGINX_VERSION=x' <<<"$("$MYCRT" run -e NGINX_VERSION=x nginx env)"
check "arguments replace Cmd, entrypoint still runs" grep -q 'nginx version' <<<"$("$MYCRT" run nginx nginx -v 2>&1)"
check "--entrypoint replaces the entrypoint" test "$("$MYCRT" run --entrypoint /bin/echo nginx hello)" = hello
check "-u switches user by name" test "$("$MYCRT" run -u nginx nginx id -u)" = 101
check "-w sets the working directory" test "$("$MYCRT" run -w /usr/share nginx pwd)" = /usr/share
check "bad image name is rejected" not "$MYCRT" run 'Not/Valid' true

section "Verification (Go tests, fake registry)"
out=$(bin/image.test -test.run 'TestPull' -test.v 2>&1)
check "whiteouts converted, and overlay hides the files" grep -q -- '--- PASS: TestPullUnpacksAndConvertsWhiteouts' <<<"$out"
check "corrupted layer rejected, nothing committed" grep -q -- '--- PASS: TestPullRejectsCorruptLayer' <<<"$out"
check "wrong diff ID rejected" grep -q -- '--- PASS: TestPullRejectsWrongDiffID' <<<"$out"

section "Cleanup"
check "no container directories left" test -z "$(ls -A /var/lib/mycrt/containers)"
check "no stale cgroups" test -z "$(find /sys/fs/cgroup/mycrt -mindepth 1 -maxdepth 1 -type d)"
finish
