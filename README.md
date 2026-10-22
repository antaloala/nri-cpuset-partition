# nri-cpuset-partition

An NRI plugin that turns the cgroup of a container holding exclusively
assigned cpus into a **cgroup v2 cpuset remote partition**, handing the
exclusive cpu reservation down from `kubepods.slice` on the way.

It implements section `[C] PROPOSED SOLUTION` of `./docs/WHAT-WHY-HOW.md`.

## Why it is needed

Kubelet's CPU Manager `static` policy pins a Guaranteed QoS pod's container that asks
for an integer amount of cpu to a fixed set of cores, but those cores stay in
the kernel's SMP load balancing domain if not having been removed from it
(e.g., at kernel bootup, adding the "domain" flag to the "isolcpus" kernel parameter).
It is still possible to dynamically remove those cpu cores from the kernel's SMP load
balancing using the cpuset remote partition support provided from cgroups v2 cpuset controller
(available from kernel 6.7 onward)

```sh
echo isolated > /sys/fs/cgroup/kubepods.slice/kubepods-pod<uid>.slice/cri-containerd-<id>.scope/cpuset.cpus.partition
```

which the kernel rejects out of the box:

```
cat /sys/fs/cgroup/kubepods.slice/kubepods-pod<uid>.slice/cri-containerd-<id>.scope/cpuset.cpus.partition
isolated invalid (Invalid cpu list in cpuset.cpus.exclusive)
```

Why is it rejected? A leaf cgroup v2 can only become a partition root over cpus
that **every one of its ancestors already lists in its cgroup's `cpuset.cpus.exclusive`**
(the "remote partition" support of kernel 6.7+). Kubelet leaves that file empty everywhere,
so the reservation has to be threaded down in the kubelet cgroups v2 hierarchy: `kubepods.slice` →
`kubepods-pod<uid>.slice` (not needed in the container .scope).
NOTE: `cpuset.cpus.exclusive` must be FIRST updated at the top of the ancestors hierarchy
(i.e., at `kubepods.slice`)

Cgroups v2 cpuset controller accepts only the following input values in `cpuset.cpus.partition`:

| value      | effect                                                                  |
|------------|-------------------------------------------------------------------------|
| `member`   | default value, the cgroup shares its parent's scheduling domain         |
| `root`     | the cpu cores get their own scheduling domain, load balancing stays on  |
| `isolated` | like `root`, and load balancing inside the partition is turned off      |

`isolated` is the equivalent of `isolcpus=domain` flags for those cores only, applied
dynamically and only to the containers that ask for it. **A container that
does not pin (to allowed cpu cores) its own threads and is given `isolated` over the
allowed cores will pile all of them onto one core**, exactly as it would under a global
`isolcpus=domain`. That is why the plugin is opt-in per pod or per container,
and why `root` value exists as the middle ground.

## Requirements

- cgroup v2 (unified hierarchy), kernel **6.7 or newer** for
  `cpuset.cpus.exclusive`.
- containerd 2.x with NRI enabled (the default from 2.0), socket at
  `/var/run/nri/nri.sock`.
- Kubelet with `cpuManagerPolicy: static`, and `reservedSystemCPUs` set.
- Go 1.26+ to build.

## The annotation

```yaml
metadata:
  annotations:
    cpuset-partition.nri.io/pod: isolated                        # pod-wide annotation
    cpuset-partition.nri.io/container.<container_name>: root     # container-scoped annotation
```

- Values: `member` (default, nothing happens), `root`, `isolated`. Anything
  else is treated as `member` and logged.
- The per-container form wins over the pod-wide one for that container; the
  other containers keep the pod-wide value.
- The annotation is **ignored** unless the CPU Manager actually gave the
  container exclusive cores, i.e. (1) the pod is Guaranteed QoS *and* (2) the 
  container asks for a whole number of cpus. The plugin checks this three ways: the
  pod's cgroup sits directly under `kubepods.slice` (how kubelet lays out Guaranteed
  pods), the cfs quota is a whole multiple of the period, and the container's
  `cpuset.cpus` holds exactly that many cores.

## What the plugin does

On every event it rebuilds the whole picture from its in-memory state and
writes it out, in this order:

1. **tear down** leaves that are no longer wanted or whose cpus changed
   (`cpuset.cpus.partition` back to `member`, `cpuset.cpus.exclusive`
   cleared), so that no live partition depends on a reservation about to be
   withdrawn;
2. **reserve** on the ancestors, parents before children — a child can only be
   granted cpus its parent already holds. Each ancestor's
   `cpuset.cpus.exclusive` is written as the *union over every live
   child reservation*, never as a read-modify-write of the current file;
3. **create** the partitions at the leaves and verify the kernel accepted
   them.

Everything runs under a single mutex, which is what makes concurrent container
events safe: two pods starting at once cannot have one's write to
`<cgoups2-mount-path>/kubepods.slice/cpuset.cpus.exclusive` drop the other's  
cores, because each recomputes the full union rather than editing what it read.

### Lifecycle

| event                                   | action                                                      |
|-----------------------------------------|-------------------------------------------------------------|
| `Synchronize`                           | adopt running containers, clear reservations left by a previous run |
| `PostCreateContainer`, `PostStartContainer` | create the partition                                    |
| `UpdateContainer`                       | dismantle the partition so the runtime's `cpuset.cpus` write cannot be rejected |
| `PostUpdateContainer`                   | rebuild it against the new cpuset                           |
| `StopContainer`                         | demote the leaf; keep the pod's reservation                 |
| last container of the pod stopped       | release the pod's reservation from the ancestors            |
| `RemoveContainer`, `StopPodSandbox`, `RemovePodSandbox` | same, as backstops                          |
| every `--resync-period` (10s)           | repair partitions kubelet knocked out, follow a changed cpuset |

A container that stops while siblings in the same pod are still running has
its partition demoted immediately, but its cores stay reserved on the
ancestors until the whole pod is gone. That is the `[C]` requirement that the
ancestors are only updated "when ALL the pod's containers have terminated"
(mimicking the way cpu-manager returns cores to the shared pool).

The exact instant at which the cpu-manager returns cores to the shared pool is
not observable from NRI, so the plugin releases on the last `StopContainer`
instead. Releasing marginally early is the safe direction: the CPU Manager
still holds the assignment at that point, so no other pod can be given those
cores, whereas releasing late risks two sibling pod slices claiming the same
cpu as exclusive, which the kernel rejects with `EINVAL`.

The periodic resync is not a safety net, it is load-bearing: a kubelet
restart, a CPU Manager reconcile or a container update rewrites `cpuset.cpus`
and invalidates the partition, and the plugin has to put it back.

## Build and deploy

```sh
make check build                         # gofmt, vet, race tests, static binary in bin/
make image-push IMAGE=...                # container image
make image-build IMAGE=...               # container image
kubectl apply -f deploy/daemonset.yaml
```

Edit the image reference in `deploy/daemonset.yaml` first. The DaemonSet runs
privileged and mounts `/sys/fs/cgroup` and `/var/run/nri` from the host.

**Start with `--dry-run`.** It logs every cgroup write it would make and
performs none of them, which is enough to confirm the plugin sees the right
containers and computes the right cpu sets on a given node.

## Verifying

```sh
# What the plugin reserved and created.
find /sys/fs/cgroup/kubepods.slice -name 'cpuset.cpus.*' \
  -exec sh -c 'for f; do printf "\n== %s\n" "${f#/sys/fs/cgroup/}"; cat "$f"; done' sh {} +

# The isolated cpus must have disappeared from the root cgroup.
cat /sys/fs/cgroup/cpuset.cpus.effective

# ... and from every other pod's effective cpuset.
cat /sys/fs/cgroup/kubepods.slice/cpuset.cpus.effective

# Load balancing is off for an isolated partition: threads of a container
# that does not pin them stay on the core they started on.
ps -eL -o pid,ppid,lwp,nlwp,psr,cgroup,comm | grep <container-id>
```

## Operational hazards

- **The shared pool can be squeezed to nothing.** Every cpu an isolated
  partition claims leaves `kubepods.slice`'s effective cpuset. With
  `reservedSystemCPUs` covering all housekeeping cores, the shared pool *is*
  the isolated range, so partitioning all of it leaves Burstable and BestEffort
  pods, including this plugin, with no cpus. Keep at least one core of the
  shared pool unpartitioned.
- **The plugin runs in that same shared pool.** On a node where the shared
  pool is made of isolated cores, the plugin itself adds (very little) noise to
  them. It is idle between container events.
- **`systemctl daemon-reload` can reset the reservations** on slices systemd
  manages, including `kubepods.slice`. The resync puts them back within one
  period.
- **`cpuset.cpus.exclusive` cannot overlap between siblings.** If a write
  fails with `EINVAL`, look for another pod slice still holding the cpu.

## Not handled

- Steering Guaranteed pods onto the isolated cores in the first place. Kubelet
  knows nothing about `isolcpus` and allocates cpus lowest-id first; if any
  housekeeping core is allocatable it will be handed out before the isolated
  ones. That needs a topology/NUMA policy or an admission layer, not this
  plugin.
- Partitions for anything other than containers the CPU Manager pinned —
  shared pool containers have nothing exclusive to carve out.
- Annotation updates on an already deployed Pod.
- cgroup v1.

## Layout

```
cmd/                        entry point, flags, reconnect loop
internal/cpuset/            cpu list parsing and set algebra
internal/cgroup/            cgroup v2 paths and cpuset file access
internal/partition/         NRI callbacks, state tracking, reconciliation
deploy/                     DaemonSet and example pods (no Helm chart in this repo)
```

The tests in `internal/partition` drive the plugin against a cgroup tree made
of plain files, so the ordering, the union of concurrent reservations and the
teardown logic can be exercised with `go test` on any machine. What they
cannot reproduce is the kernel's own validation — that needs the lab node.
