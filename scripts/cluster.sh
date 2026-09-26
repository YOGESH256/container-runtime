#!/usr/bin/env bash
# Runs the demo cluster from the Mac: the control plane on crt1 and a node
# agent on every VM, each as a systemd unit so its logs land in journald.
#
#   scripts/cluster.sh up            build, install binaries, start everything
#   scripts/cluster.sh agent crt2    (re)start one node's agent
#   scripts/cluster.sh down          stop everything and remove pod containers
#   scripts/cluster.sh status        nodes and pods as the control plane sees them
set -euo pipefail
cd "$(dirname "$0")/.."
NODES=(${NODES:-crt1 crt2 crt3})
CP_NODE=${CP_NODE:-crt1}

ip_of() { limactl shell "$1" -- ip -4 -o addr show eth0 | awk '{print $4}' | cut -d/ -f1; }
cp_url() { echo "http://$(ip_of "$CP_NODE"):7070"; }

# start_unit NODE UNIT COMMAND...: (re)start COMMAND as a transient unit.
# KillMode=process: stopping the agent must not kill container supervisors.
start_unit() {
  local node=$1 unit=$2
  shift 2
  limactl shell "$node" sudo bash -c "systemctl stop $unit 2>/dev/null; systemctl reset-failed $unit 2>/dev/null; true"
  limactl shell "$node" sudo systemd-run --quiet --unit="$unit" --collect -p KillMode=process "$@"
}

install_bins() {
  for n in "${NODES[@]}"; do
    # sync: a node killed right after an install must not reboot with a
    # half-written binary still sitting in its page cache.
    limactl shell "$n" sudo sh -c 'install -m 0755 bin/mycrt bin/myagent bin/mycontrol bin/myctl /usr/local/bin/ && sync'
  done
}

start_agent() {
  start_unit "$1" myagent /usr/local/bin/myagent --name "$1" --control-plane "$(cp_url)"
}

case "${1:-}" in
up)
  make build >/dev/null
  install_bins
  start_unit "$CP_NODE" mycontrol /usr/local/bin/mycontrol --listen :7070
  for n in "${NODES[@]}"; do start_agent "$n"; done
  echo "control plane at $(cp_url); agents on ${NODES[*]}"
  ;;
agent)
  start_agent "$2"
  ;;
down)
  for n in "${NODES[@]}"; do
    limactl shell "$n" sudo bash -c '
      systemctl stop myagent 2>/dev/null || true
      for c in $(mycrt ps -a --json 2>/dev/null | python3 -c "import sys,json; [print(c[\"id\"]) for c in json.load(sys.stdin) if \"orch.pod\" in (c.get(\"labels\") or {})]"); do
        mycrt rm -f "$c" >/dev/null
      done' || true
  done
  limactl shell "$CP_NODE" sudo bash -c 'systemctl stop mycontrol 2>/dev/null; rm -f /var/lib/mycontrol/state.db; true'
  echo "cluster stopped, pod containers removed, control plane state wiped"
  ;;
status)
  limactl shell "$CP_NODE" myctl get nodes
  echo
  limactl shell "$CP_NODE" myctl get pods
  ;;
*)
  sed -n '2,9p' "$0" | sed 's/^# \{0,1\}//'
  exit 2
  ;;
esac
