#!/usr/bin/env bash
# Stage 6, orchestrator. Runs on the Mac and drives the three VMs.
# Done when killing a node brings its containers back on another node
# within 30 s, and a container failing its health probe gets replaced.
NEED_ROOT=0 source "$(dirname "$0")/lib.sh"

ctl() { limactl shell crt1 myctl "$@"; }
on() {
  local vm=$1
  shift
  limactl shell "$vm" sudo "$@"
}
# field POD KEY: a pod's status field, as the control plane reports it.
field() {
  ctl get pods -o json | python3 -c "import sys,json
p={x['spec']['name']:x['status'] for x in json.load(sys.stdin)}.get('$1',{})
print(p.get('$2',''))"
}
# wait_until SECONDS COMMAND...: poll once a second.
wait_until() {
  local deadline=$((SECONDS + $1))
  shift
  while ((SECONDS < deadline)); do
    "$@" >/dev/null 2>&1 && return 0
    sleep 1
  done
  return 1
}
now() { python3 -c 'import time; print(time.time())'; }
since() { python3 -c "import time; print(round(time.time() - $1, 1))"; }
ready_nodes() { ctl get nodes -o json | python3 -c 'import sys,json; print(sum(n["ready"] for n in json.load(sys.stdin)))'; }
all_running() { for p in "$@"; do [ "$(field "$p" phase)" = Running ] || return 1; done; }
moved_off() { [ "$(field "$1" phase)" = Running ] && [ "$(field "$1" node)" != "$2" ]; }
serves() { on "$(field "$1" node)" curl -s --max-time 3 "http://$(field "$1" ip)/" | grep -q 'Welcome to nginx'; }
node_ready() { ctl get nodes -o json | python3 -c "import sys,json; sys.exit(0 if any(n['name']=='$1' and n['ready'] for n in json.load(sys.stdin)) else 1)"; }
has_container() { on "$1" mycrt ps -a | grep -q " $2 "; }
running_on() { on "$1" mycrt ps | grep -q " $2 "; }
pod_on_worker() {
  for p in "$@"; do
    n=$(field "$p" node)
    if [ "$n" != crt1 ] && [ -n "$n" ]; then echo "$p"; return; fi
  done
}

section "Cluster"
scripts/cluster.sh down >/dev/null 2>&1
scripts/cluster.sh up
check "three nodes report in" wait_until 30 test "$(ready_nodes)" = 3

section "Scheduling"
for i in 1 2 3; do
  sed "s/\"name\": \"web\"/\"name\": \"web$i\"/" examples/pod.json | limactl shell crt1 myctl apply -
done
check "three pods Running (image pulls included)" wait_until 240 all_running web1 web2 web3
nodes=$(for p in web1 web2 web3; do field "$p" node; done | sort -u | wc -l | tr -d ' ')
check "spread one per node" test "$nodes" = 3
for p in web1 web2 web3; do
  check "$p serves from $(field "$p" node)" serves "$p"
done
ctl get pods | sed 's/^/        /'

section "Node failure"
victim=$(pod_on_worker web1 web2 web3)
vnode=$(field "$victim" node)
vgen=$(field "$victim" generation)
others=$(for p in web1 web2 web3; do [ "$p" != "$victim" ] && echo "$p=$(field "$p" generation)"; done)
echo "        killing $vnode, which runs $victim (generation $vgen)"
t0=$(now)
limactl stop -f "$vnode" >/dev/null 2>&1
wait_until 60 moved_off "$victim" "$vnode"
took=$(since "$t0")
echo "        $victim running on $(field "$victim" node) as generation $(field "$victim" generation) after ${took}s"
check "$victim is back on another node within 30s (${took}s)" python3 -c "import sys; sys.exit(0 if $took <= 30 else 1)"
check "it is a new generation" test "$(field "$victim" generation)" -gt "$vgen"
check "it serves HTTP on its new node" serves "$victim"
check "control plane declared $vnode not ready" bash -c "limactl shell crt1 myctl events | grep -q 'node/$vnode .*not ready'"
for pg in $others; do
  check "${pg%=*} was left alone" test "$(field "${pg%=*}" generation)" = "${pg#*=}"
done

section "Health probe failure"
hp=web1
[ "$(field web1 node)" = "$vnode" ] && hp=web2
hnode=$(field "$hp" node)
hgen=$(field "$hp" generation)
pid=$(on "$hnode" mycrt inspect "$hp-g$hgen" | python3 -c 'import sys,json; print(json.load(sys.stdin)["pid"])')
echo "        deleting nginx's index page inside $hp-g$hgen on $hnode, so / returns 403"
on "$hnode" nsenter -t "$pid" -m rm /usr/share/nginx/html/index.html
t0=$(now)
wait_until 60 bash -c "[ \"\$(limactl shell crt1 myctl get pods -o json | python3 -c \"import sys,json; print({x['spec']['name']:x['status'] for x in json.load(sys.stdin)}['$hp']['generation'])\")\" -gt $hgen ]"
wait_until 60 all_running "$hp"
took=$(since "$t0")
check "unhealthy container replaced and healthy again (${took}s)" all_running "$hp"
check "control plane logged the failing probe" bash -c "limactl shell crt1 myctl events | grep -q '$hp .*health probe failed'"
check "replacement serves the page again" serves "$hp"
check "the unhealthy generation was removed" wait_until 20 not has_container "$hnode" "$hp-g$hgen"

section "Dead node comes back"
limactl start "$vnode" >/dev/null 2>&1
scripts/cluster.sh agent "$vnode"
check "$vnode is ready again" wait_until 60 node_ready "$vnode"
check "its stale $victim-g$vgen is cleaned up" wait_until 30 not has_container "$vnode" "$victim-g$vgen"

section "Slow node (split brain)"
slow=$(pod_on_worker web1 web2 web3)
if [ -z "$slow" ]; then
  # Everything sits on crt1: move one pod by nudging its spec.
  sed 's/"name": "web"/"name": "web3"/; s/"64m"/"65m"/' examples/pod.json | limactl shell crt1 myctl apply -
  wait_until 60 all_running web3
  slow=$(pod_on_worker web3)
fi
snode=$(field "$slow" node)
sgen=$(field "$slow" generation)
apid=$(on "$snode" systemctl show -p MainPID --value myagent)
echo "        freezing the agent on $snode; $slow-g$sgen keeps running there"
on "$snode" kill -STOP "$apid"
check "$slow is rescheduled off the silent node" wait_until 45 moved_off "$slow" "$snode"
check "duplicate window: the old copy still runs on $snode" running_on "$snode" "$slow-g$sgen"
on "$snode" kill -CONT "$apid"
t0=$(now)
wait_until 40 not has_container "$snode" "$slow-g$sgen"
took=$(since "$t0")
check "stale copy stopped once $snode reports again (${took}s)" not has_container "$snode" "$slow-g$sgen"
check "the new copy keeps serving" serves "$slow"

section "Delete"
for p in web1 web2 web3; do ctl delete pod "$p" >/dev/null; done
none_left() { for n in crt1 crt2 crt3; do on "$n" mycrt ps -a | grep -q ' web' && return 1; done; return 0; }
check "every pod container removed from every node" wait_until 40 none_left

section "Activity log"
ctl events | tail -25 | sed 's/^/        /'
scripts/cluster.sh down >/dev/null
finish
