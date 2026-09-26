# mycrt: a container runtime and mini orchestrator

A container runtime written against Linux syscalls, and a small scheduler that
keeps containers running across a three-node cluster. There is no Docker,
containerd, runc or libcontainer underneath. The only libraries are
`golang.org/x/sys`, `vishvananda/netlink` and `bbolt`, as the brief asks.

- `mycrt` runs OCI images: namespaces, cgroups v2, overlayfs, image pulls
  from any registry, and bridge networking with NAT.
- `mycontrol` is the control plane: pods in bbolt, a scheduler, and a
  reconcile loop that replaces failed containers and dead nodes.
- `myagent` runs on every node: it starts containers with mycrt, probes
  their health and sends heartbeats.
- `myctl` is the command-line client: `myctl apply pod.json`.

## Quick start

Code is edited on the Mac and cross-compiled for the Lima VMs, where it runs
as root. `make install` builds and copies the binaries into `crt1`.

```sh
make install
limactl shell crt1

sudo mycrt run alpine:3.20 sh                         # isolated, networked shell
sudo mycrt run --memory 64m --cpus 0.5 --pids 100 alpine:3.20 sh
sudo mycrt run -d --name web nginx                    # background container
sudo mycrt ps                                         # note its IP, e.g. 10.88.0.2
curl http://10.88.0.2/
sudo mycrt stop web && sudo mycrt rm web
```

The cluster needs all three VMs:

```sh
limactl start crt2 crt3
scripts/cluster.sh up                                 # control plane on crt1, agent everywhere
limactl shell crt1 myctl apply examples/pod.json
limactl shell crt1 myctl get pods
limactl shell crt1 myctl events -f                    # watch it react
limactl stop -f crt2                                  # kill a node, watch pods move
scripts/cluster.sh down
```

Set `MYCRT_DEBUG=1` to see where container start-up time goes. Set
`MYCRT_REGISTRY_MIRROR=mirror.gcr.io` to spare Docker Hub's rate limit.

## Tests

Each stage has a script that checks the brief's "done when" line, plus more.

| Stage | Run | Checks |
|---|---|---|
| 1. Isolation | `make test-stage1` | 28 |
| 2. cgroups v2 | `make test-stage2` | 19 |
| 3. overlayfs | `make test-stage3` | 13 |
| 4. OCI images | `make test-stage4` | 24 |
| 5. Networking | `make test-stage5` | 21 |
| 6. Orchestrator | `bash test/stage6.sh` (on the Mac) | 23 |

`make unit` runs the Go tests that need no root, on the Mac. They cover image
references, the registry client against a fake registry, IP allocation, the
agent, and the control plane against a simulated cluster: node death, probe
failure, crash loops, a slow node coming back, and a control-plane restart.
The stage 4 script also runs the pull pipeline's root-only tests in the VM,
which mount a real overlay to check whiteouts.

## How it works

### Stage 1: isolation (`internal/container`)

`mycrt run` starts a child with `CLONE_NEWPID | NEWNS | NEWUTS | NEWIPC | NEWNET`.
The child is the same binary re-executed through `/proc/self/exe` with a
marker in its environment. The Go runtime is multithreaded before `main`
runs, and some namespace operations only work in a fresh process, so the
child starts life already inside its namespaces.

The child blocks reading its config from a pipe while the parent sets up the
host side. Then it makes all mounts private (systemd marks `/` shared, so
mounts would otherwise leak to the host), mounts the root filesystem, `/proc`,
a fresh `/dev`, `/sys` and `/dev/pts`, and calls `pivot_root(".", ".")`
followed by a lazy unmount of the old root. Finally it execs the command,
which becomes PID 1.

PID 1 is special in two ways, and the tests cover both. Orphans are
re-parented to it, so it must reap them. And the kernel drops any signal
sent to PID 1 that it has no handler for, so `sleep` as PID 1 ignores SIGTERM.
`--init` puts a tiny init at PID 1 that reaps and relays signals, like
`docker run --init`.

Every mount target is resolved with `openat2(RESOLVE_IN_ROOT)` (see
`internal/fsutil`). An image containing a symlink such as `/proc -> /etc`
can therefore never make mycrt mount over a host path.

### Stage 2: cgroups v2 (`internal/cgroups`)

Each container gets `/sys/fs/cgroup/mycrt/<id>` with `memory.max`, `cpu.max`
and `pids.max`. The child is created directly inside it with `clone3` and
`CLONE_INTO_CGROUP`, so it never runs for a moment without limits. It then
unshares a cgroup namespace, so inside the container
`/sys/fs/cgroup/memory.max` shows its own limit. `memory.oom.group` makes the
OOM killer take the whole container, and `cgroup.kill` stops even a container
that keeps forking.

### Stage 3: overlayfs

Image layers are read-only lower directories. Each container gets its own
`upper` and `work` directories, so any number of containers share a layer
and it is never modified. Deleting a lower file creates a whiteout, a 0:0
character device, in the upper layer. The stage 3 test shows one.

### Stage 4: OCI images (`internal/registry`, `internal/image`)

A pull resolves the name, follows a multi-arch index to the `linux/arm64`
manifest, fetches the config, then streams each layer:

```
HTTP body -> sha256 (must match the manifest) -> gunzip -> sha256 (must match
the config's diff ID) -> untar into a fresh directory -> rename into place
```

A layer is committed only if both hashes match. Docker Hub's anonymous token
dance is handled by following the `WWW-Authenticate` challenge. A HEAD request
detects an up-to-date image without using a pull from the rate limit.

Each layer is unpacked into its own directory, so OCI whiteouts (`.wh.name`,
`.wh..wh..opq`) are converted into what overlayfs reads rather than applied
by deleting files. Tar entries are resolved inside the layer with
`RESOLVE_IN_ROOT`, so `../../etc/passwd` or a path under a symlink to `/`
stays inside the layer.

### Stage 5: networking (`internal/network`)

```
container A: eth0 10.88.0.2        container B: eth0 10.88.0.3
        | veth pair                        | veth pair
        +----------- mycrt0 bridge, 10.88.0.1 -----------+
                              |
                  host routing + nftables masquerade -> internet
```

Each container gets a veth pair. The inner end is created directly inside
the container's network namespace and named `eth0`. The outer end is plugged
into the `mycrt0` bridge. Addresses come from a lease file guarded by `flock`.
nftables masquerades traffic leaving the subnet. DNS comes from
systemd-resolved's upstream list, because `127.0.0.53` is unreachable from
inside a container.

### Stage 6: the orchestrator (`internal/orch`)

The orchestrator is one reconcile loop: observe actual state, diff it against
desired state, act, repeat.

- **Desired state** is the pods in bbolt, each with the node it was placed
  on and a **generation** number that increases with every placement.
- **Actual state** is what agents report in heartbeats every 2 seconds.
  Every container is labelled with its pod and generation.
- **Each second**, the control plane schedules unplaced pods onto the ready
  node with the fewest pods. It replaces containers that exited, failed to
  start, or failed their health probe three times in a row. It moves pods
  off nodes silent for 10 seconds, and stops containers nobody wants.

Health probes run on the agent, as kubelet does, because container IPs are
private to each node's bridge. Agents pre-pull every image the cluster uses,
so a failover does not wait for a download. Containers survive an agent
restart: each has its own mycrt supervisor process, and the agent
rediscovers them from their labels.

**Split brain.** A node that is slow rather than dead gets its pods
rescheduled after the 10 second lease, so for a moment two copies run.
This design accepts that duplicate window instead of fencing. When the node
reports again, its containers carry an old generation, and the control plane
stops them. The stage 6 test freezes an agent with SIGSTOP to show both
copies and then the cleanup.

## What the tests caught

These bugs only showed up under the stage tests and are worth knowing
about. Each fix is in the code with a comment.

1. **Every container exit took ~400ms.** The exiting process was stuck in
   `wait_sb_inodes`. Tearing down an overlay mount syncs its upper
   filesystem, which here is the whole root disk. The overlay option
   `volatile` skips that. The cost is an upper layer that is unusable after
   a crash, and mycrt never reuses one.
2. **Bridge networking cost ~0.7s per container.** Moving a veth into a
   namespace and renaming it each wait for an RCU grace period. Creating the
   peer inside the namespace, already named `eth0`, avoids both. Deleting
   the veth by hand at exit was also redundant, since the kernel removes it
   with the namespace.
3. **A replacement container was unreachable for ~30s.** It reused the IP
   its predecessor had just freed, and the host's ARP cache still pointed
   at the old MAC. The fix is Docker's: derive the MAC from the IP.
4. **A power-cut node crash-looped 122 times in 3 minutes.** A file written
   and renamed without `fsync` came back empty after the VM was killed, and
   every start failed on it. All state now goes through `internal/fsync`.
   Damaged indexes are rebuilt, and the control plane backs off
   exponentially on repeated failures, like CrashLoopBackOff.
5. **SIGTERM during setup killed mycrt before cleanup.** The fix is to
   subscribe to signals before creating anything, then relay any queued
   signal once the container runs.

## Not implemented

Some of these are out of scope per the brief, and some are known gaps.

- **No user namespaces.** Root in a container is root on the host, so this
  is not a security boundary. The brief lists user namespaces as a stretch
  goal. There is also no seccomp, capability dropping or AppArmor.
- **No pseudo-terminal allocation.** Containers share the caller's
  terminal. Shells and job control work, but `tty` inside prints
  "not a tty".
- **No zstd layers.** Go's standard library cannot decode zstd. Such images
  fail with a clear error.
- **No `exec` into a running container.**
- **One control plane, no auth or TLS, node-local container IPs, and no
  service discovery**, per the brief.

## Layout

```
cmd/mycrt         runtime CLI: run, ps, stop, rm, logs, inspect, pull, images
cmd/mycontrol     control plane server
cmd/myagent       node agent
cmd/myctl         cluster CLI
internal/container  namespaces, re-exec'd init, mounts, pivot_root, mini init
internal/cgroups    cgroup v2 groups and limits
internal/image      references, manifests, content store, verified pulls, unpacking
internal/registry   OCI distribution client with bearer-token auth
internal/network    bridge, veth, NAT, IP leases, /etc files
internal/engine     container lifecycle: create, run, supervise, stop, remove
internal/orch       pod types, control plane, agent
internal/fsutil     symlink-safe path resolution inside a root
internal/fsync      crash-safe file writes
test/stageN.sh      one script per stage of the brief
scripts/            Alpine rootfs fetcher, cluster start and stop
```
