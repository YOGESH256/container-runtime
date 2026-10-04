#!/usr/bin/env bash
# Stage 5, networking. Done when a container can wget the internet and two
# containers can ping each other.
source "$(dirname "$0")/lib.sh"
"$MYCRT" pull alpine:3.20 >/dev/null 2>&1 || { echo "cannot pull alpine:3.20"; exit 1; }
run() { "$MYCRT" run "$@"; }

section "Addressing"
out=$(run alpine:3.20 sh -c 'ip -4 -o addr show eth0; ip route; ip -o link | wc -l')
ip=$(grep -oE '10\.88\.[0-9]+\.[0-9]+/16' <<<"$out")
check "eth0 has an address in 10.88.0.0/16" test -n "$ip"
check "default route via the bridge, 10.88.0.1" grep -q 'default via 10.88.0.1 dev eth0' <<<"$out"
check "container sees only lo and eth0" test "$(tail -1 <<<"$out")" = 2
check "own hostname resolves via /etc/hosts" run --hostname web1 alpine:3.20 ping -c1 -W1 web1

section "Internet access (NAT)"
check "DNS lookup works" run alpine:3.20 nslookup example.com
out=$(run alpine:3.20 wget -qO- http://example.com)
check "wget fetches a page from the internet" grep -q 'Example Domain' <<<"$out"
check "--net none has no way out" not run --net none alpine:3.20 wget -qO- -T 3 http://example.com

section "Container to container"
"$MYCRT" run alpine:3.20 sh -c 'ip -4 -o addr show eth0; sleep 6' >"$tmp/a" &
pa=$!
wait_for "$tmp/a" 'inet '
ipa=$(grep -oE '10\.88\.[0-9]+\.[0-9]+' "$tmp/a" | head -1)
out=$(run alpine:3.20 sh -c "ip -4 -o addr show eth0; ping -c2 -W1 $ipa")
ipb=$(grep -oE '10\.88\.[0-9]+\.[0-9]+' <<<"$out" | head -1)
echo "        A=$ipa  B=$ipb"
check "two running containers get different addresses" test -n "$ipa" -a -n "$ipb" -a "$ipa" != "$ipb"
check "B pings A" grep -q '2 packets received' <<<"$out"
check "the host reaches A too" ping -c1 -W1 "$ipa"
check "A's veth end is plugged into mycrt0" grep -q "master mycrt0" <<<"$(ip -o link show type veth)"
wait "$pa"

section "Address reuse"
# Keep another container up so the bridge keeps its carrier and the host's
# neighbor cache is not flushed, then hand one container's IP to the next.
keeper=$("$MYCRT" run -d alpine:3.20 sleep 60)
first=$("$MYCRT" run -d alpine:3.20 sleep 60)
ip1=$("$MYCRT" inspect "$first" | python3 -c 'import sys,json; print(json.load(sys.stdin)["ip"])')
ping -c1 -W1 "$ip1" >/dev/null
"$MYCRT" rm -f "$first" >/dev/null
second=$("$MYCRT" run -d alpine:3.20 sleep 60)
ip2=$("$MYCRT" inspect "$second" | python3 -c 'import sys,json; print(json.load(sys.stdin)["ip"])')
check "the replacement reuses the freed address ($ip2)" test "$ip1" = "$ip2"
check "and is reachable at once: MAC follows IP" ping -c1 -W1 "$ip2"
check "MAC is 02:42 plus the IP's bytes" grep -q "lladdr 02:42:0a:58:" <<<"$(ip neigh show "$ip2")"
"$MYCRT" rm -f "$keeper" "$second" >/dev/null

section "Serving from a container"
"$MYCRT" run nginx >"$tmp/nginx" 2>&1 &
pn=$!
wait_for "$tmp/nginx" 'start worker processes'
init=$(init_of "$pn")
nip=$(nsenter -t "$init" -n ip -4 -o addr show eth0 | grep -oE 'inet [0-9.]+' | cut -d' ' -f2)
check "host curls nginx at its container IP ($nip)" bash -c "curl -s --max-time 3 http://$nip/ | grep -q 'Welcome to nginx'"
check "another container fetches it too" bash -c "'$MYCRT' run alpine:3.20 wget -qO- http://$nip/ | grep -q 'Welcome to nginx'"
kill -TERM "$pn"
wait "$pn"

section "Published ports"
hostip=$(ip -4 -o addr show eth0 | awk '{print $4}' | cut -d/ -f1)
web=$("$MYCRT" run -d -p 8080:80 nginx)
wait_for /dev/null x 1; sleep 1
check "ps shows the mapping" grep -q '8080:80/tcp' <<<"$("$MYCRT" ps)"
check "reachable on localhost" bash -c "curl -s --max-time 3 http://127.0.0.1:8080/ | grep -q 'Welcome to nginx'"
check "reachable on the host's own address ($hostip)" bash -c "curl -s --max-time 3 http://$hostip:8080/ | grep -q 'Welcome to nginx'"
check "another container reaches it through the host (hairpin NAT)" bash -c "'$MYCRT' run alpine:3.20 wget -qO- -T 3 http://$hostip:8080/ | grep -q 'Welcome to nginx'"
check "a second container cannot take the same host port" not "$MYCRT" run -d -p 8080:80 nginx
"$MYCRT" rm -f "$web" >/dev/null
check "port is closed after removal" not curl -s --max-time 2 http://127.0.0.1:8080/
check "its nftables table is gone" not nft list table ip "mycrt_$web"

section "Cleanup"
sleep 0.5
check "no veth pairs left on the host" test -z "$(ip -o link show type veth)"
check "every IP lease released" test "$(tr -d ' \n' </var/lib/mycrt/network/leases.json)" = "{}"
check "bridge and NAT rule persist for the next container" bash -c "ip link show mycrt0 >/dev/null && nft list table ip mycrt | grep -q masquerade"
check "no container directories left" test -z "$(ls -A /var/lib/mycrt/containers)"
check "no stale cgroups" test -z "$(find /sys/fs/cgroup/mycrt -mindepth 1 -maxdepth 1 -type d)"
finish
