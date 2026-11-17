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
(available from kernel 6.7 onward):

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
- Kubelet with `cpuManagerPolicy: static`, which is what gives containers
  exclusive cpus. The static policy itself requires a non-zero CPU
  reservation: either `reservedSystemCPUs` (recommended in this scope, as it lets you choose
  which cores are reserved and keep them aligned with the housekeeping cores)
  or `kubeReserved`/`systemReserved` cpu.
- Kubelet's `systemd` cgroup driver (`cgroupDriver: systemd`, the RKE2
  default on systemd hosts). This is the tested configuration. The `cgroupfs`
  driver is handled by the code, which then needs `--kubepods-cgroup=kubepods`,
  but it has never been run on a real node. If the `--kubepods-cgroup`
  directory doesn't exist, the plugin logs a warning at startup: partitions
  still work, but leftovers from a previous run aren't cleaned up.
- Go 1.26+ to build.

## The new pod annotation

```yaml
metadata:
  annotations:
    cpuset-partition.nri.io/pod: isolated                        # pod-wide annotation
    cpuset-partition.nri.io/container.<container_name>: root     # container-scoped annotation
```

- Values: `member` (nothing happens), `root`, `isolated`. Anything else is
  treated as `member` and logged as a warning.
- The per-container form wins over the pod-wide one for that container; the
  other containers keep the pod-wide value, or `member` if the pod has none.
- A pod with no `cpuset-partition.nri.io` annotation at all gets the value of
  the `--partition-value-for-non-annotated-pod` flag, which is `member` unless
  set (see below).
- The annotation is **ignored** unless the CPU Manager "static" policy  actually gave the
  container exclusive cores, i.e. (1) the pod is Guaranteed QoS *and* (2) the 
  container asks for a whole number of cpus. The plugin checks this three ways: the
  pod's cgroup sits directly under `kubepods.slice` (how kubelet lays out Guaranteed
  pods), the cfs quota is a whole multiple of the period, and the container's
  `cpuset.cpus` holds exactly that many cores.

### Pods with no annotation

`--partition-value-for-non-annotated-pod=<member|root|isolated>` sets the
partition applied to the containers that the CPU Manager gave exclusive cores
to, in pods that carry **no** `cpuset-partition.nri.io` annotation at all. The
default, `member`, keeps the plugin strictly opt-in.

- It never applies to a pod that has any `cpuset-partition.nri.io/...`
  annotation, whatever the key or the value. In such a pod, a container that gets (from
  CPU Manager `static` policy) a set of exclusively assigned CPU cores but is not "covered"
  by any annotation stays `member`. For example, a pod defining two containers (`app1` and
  `app2`) with only `cpuset-partition.nri.io/container.app1: isolated`  partitions `app1`
  and leaves `app2` alone, whatever this flag says. So the flag can't override what the pod's
  author asked for, and a pod can opt out entirely with `cpuset-partition.nri.io/pod: member`.
- The same eligibility checks apply: Burstable, BestEffort and containers asking for a fractional
  amount of CPU cores in the scope of a Guaranteed QoS pod are left alone. Since they never
  asked for a partition, they are not reported as refused either.
- The `partition created` log line says where the value came from:
  `modeSource=--partition-value-for-non-annotated-pod` or the annotation key.
- Any value other than `member`, `root` or `isolated` makes the plugin binary print
  an error and exit with status 1 at startup, before it registers with NRI or
  handles any container. The match is exact and case-sensitive, so
  `Isolated`, a value with surrounding spaces, or an empty value fail too. As
  a DaemonSet, the pod then goes into `CrashLoopBackOff`, and `kubectl logs`
  shows the error:

  ```text
  nri-cpuset-partition: invalid partition-value-for-non-annotated-pod "isolatd" (want member, root or isolated)
  ```

WARNING: Use `isolated` here with care. It applies to **every** non-annotated
Guaranteed pod with containers asking for an integer amount of CPU cores, 
including infrastructure deployment manifests you did not write. A workload that
does not pin its own threads piles them onto one core inside an isolated partition. 
`root` keeps load balancing within the partition and is the safer node-wide value.

## What the plugin does

On every event it rebuilds the whole picture from its in-memory state and
writes it out, in this order:

1. **tear down** cgroups v2 leaves that are no longer wanted or whose cpuset settings
   changed (`cpuset.cpus.partition` back to `member`, `cpuset.cpus.exclusive`
   cleared), so that no live partition depends on a reservation about to be
   withdrawn;
2. **reserve** exclusive cpus on the cgroups v2 ancestors following the required "parents
   before children" order for cpuset "remote partition" — a child becoming a partition root can
   only be granted exclusive cpus its parent already holds. Each ancestor's
   `cpuset.cpus.exclusive` is carefully written as the *union over every live child reservation*,
   never as a read-modify-write of the current file;
3. **create** the cpuset "remote partitions" at the cgroups v2 leaves and verify the kernel
   accepted them.

Everything runs under a single mutex, which is what makes concurrent container
events safe: two pods starting at once cannot have one's write to
`<cgroups2-mount-path>/kubepods.slice/cpuset.cpus.exclusive` drop the other's cores,
because each recomputes the full union rather than editing what it read. The same
applies for two containers in the scope of the same Guaranteed Qos pod (i.e., write to
`<cgroups2-mount-path>/kubepods.slice/kubepods-pod<uid>.slice/cpuset.cpus.exclusive`)

### Lifecycle

| NRI event                                   | action                                                      |
|-----------------------------------------|-------------------------------------------------------------|
| `Synchronize`                           | adopt running containers, release partitions and reservations nobody wants any more (see [below](#restarts-crashes-and-pods-that-predate-the-plugin)) |
| `PostCreateContainer`, `PostStartContainer` | create the partition                                    |
| `UpdateContainer`                       | dismantle the partition so the runtime's `cpuset.cpus` write cannot be rejected |
| `PostUpdateContainer`                   | rebuild it against the new cpuset                           |
| `StopContainer`                         | demote the leaf; keep the pod's reservation                 |
| `StopContainer` (last container of the pod stopped)       | release the pod's reservation from the ancestors            |
| `RemoveContainer`, `StopPodSandbox`, `RemovePodSandbox` | same, as backstops                          |
| every `--resync-period` (10s)           | repair partitions kubelet knocked out, follow a changed cpuset |

A container that stops while siblings in the same pod are still running has
its partition demoted immediately, but its cores stay reserved on the
ancestors until the whole pod is gone. The rational behind this is to mimick the
way the CPU Manager returns cores to the "shared pool": 
* exclusively assigned cores are removed from the ancestors' `cpuset.cpus.exclusive` value
  once **all** the pod's containers have terminated — that is, when
  the CPU Manager adds those cores back to the shared pool (not before, not
  after). This means the plugin tracks the lifecycle of the pods' containers in order to
  correctly update the ancestors' cgroups' `cpuset.cpus.exclusive` value, once all the pod's
  containers have terminated (not before, not after).

Note the exact instant at which the CPU Manager returns cores to the shared pool is not
observable from NRI, so the plugin releases on the last `StopContainer` instead. Releasing
marginally early is the safe direction: the CPU Manager still holds the assignment at that
point, so no other pod can be given those cores, whereas releasing late risks two sibling
pod slices claiming the same cpu as exclusive, which the kernel would reject with `EINVAL`.

The periodic resync is not a safety net, it is load-bearing: a kubelet
restart, a CPU Manager reconcile or a container update rewrites `cpuset.cpus`
and invalidates the partition, and the plugin has to put it back.

### Plugin restarts, crashes and pods creations that predate the plugin deployment

The plugin only receives container events while it is connected to an NRI enabled
container runtime (e.g., containerd). It misses everything that happens before it
is first deployed on a node, while it is down after a crash, or while containerd
itself is restarting. Pods keep starting and stopping in those windows. The plugin
doesn't replay events to catch up: it rebuilds its view from the current state
of the node.

Every time the plugin registers with containerd, containerd sends it the NRI
`Synchronize` event with the **complete list of pods and containers currently
on the node**, however and whenever they were created. That happens on the
first start, after any restart, and after every reconnection: when containerd
drops the connection, the plugin retries every 5 s instead of exiting, and
each successful reconnect triggers another `Synchronize`. On that event the
plugin:

1. **Re-evaluates every running container from scratch.** It reads the pod's
   annotations, the QoS class and the integer cpu request, and reads the
   assigned cores back from the container's own `cpuset.cpus`. A container
   that started while the plugin wasn't there is handled exactly as if the
   plugin had seen it start.
2. **Finds cpu cores exclusive reservations not needed any more.** It walks
   `kubepods.slice` for cgroups whose `cpuset.cpus.exclusive` is set but belong
   to no container it is now managing. Those are typically left by pods deleted
   while the plugin was down.
3. **Finds remote partitions not needed any more.** It walks `kubepods.slice`
   for cgroups whose `cpuset.cpus.partition` is `root` or `isolated`, valid or
   already invalidated by the kernel, that no running container should have.
   Those are typically left by a previous run for a container that is still
   running but no longer in scope, for example after restarting the plugin
   with a different `--partition-value-for-non-annotated-pod` value.

   NOTE: Both walks (in steps 2 and 3 above) skip the Burstable and
   BestEffort QoS slices: `kubepods-burstable.slice` and
   `kubepods-besteffort.slice` with kubelet's `systemd` cgroup driver (the
   RKE2 default on systemd hosts), or `burstable` and `besteffort` with the
   `cgroupfs` driver. Only Guaranteed pods, whose slices sit directly under
   `kubepods.slice`, ever get a reservation or a partition from this plugin,
   and a pod cannot change QoS class. So nothing under those two slices was set
   by the plugin, and it's not the plugin's to release.
4. **Runs the normal reconcile** in its usual order: demote the unwanted
   partitions to `member` first, then rewrite the ancestors'
   `cpuset.cpus.exclusive`, parents before children, then create the missing
   partitions. Demoting first means no partition is ever left depending on a
   reservation that has just been withdrawn.

Every write is skipped when the file already holds the right value. A
container that is correctly partitioned before a plugin restart is therefore
not touched at all by the restart, and an isolated workload sees no window
where its partition is invalid.

| situation | what happens at `Synchronize` |
| --- | --- |
| Annotated pod created before the plugin was deployed, or while it was down | its partition is created |
| Partitioned pod still running across a plugin crash or restart | nothing is written, the partition stays as it is |
| Partitioned pod deleted while the plugin was down | its leftover reservation on `kubepods.slice` is cleared |
| Running container partitioned by a previous run but no longer in scope | its partition is demoted to `member`, then its reservation is cleared |
| containerd restarted | the plugin reconnects, and the cases above apply |

This depends on `--cleanup-on-exit` staying off, as it is by default. With
it on, a restart first tears every partition down and then rebuilds them, so
running workloads lose their partition for a moment.

Limits:

- A pod that starts while the plugin is down **runs without its partition
  until the plugin comes back.** NRI cannot hold a container back waiting for
  a plugin that is not connected. Deploy the plugin before the workloads that
  depend on it. The chart's `system-node-critical` priority class helps it get
  scheduled first on a node.
- Steps 2 and 3 release **any** cpu reservation and/or `root`/`isolated` partition
  in the Guaranteed pods' part of `kubepods.slice` that no container should have,
  not only the ones this plugin created. Don't create partitions or exclusive
  reservations for Guaranteed pods by other means on a node running this
  plugin. The Burstable and BestEffort slices are, instead, never touched.

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

## Command-line parameters

| Flag | Default | Description |
| --- | --- | --- |
| `--partition-value-for-non-annotated-pod` | `member` | Partition (`member`, `root` or `isolated`) applied to the containers with exclusive cpus of pods that carry no `cpuset-partition.nri.io` annotation at all. Never used for a pod with any such annotation. `member` leaves them alone. Any other value (matched exactly and case-sensitively) makes the plugin print an error and exit at startup. See [Pods with no annotation](#pods-with-no-annotation). |
| `--dry-run` | `false` | Log every cgroup write as `DRY RUN: would write` instead of performing it. |
| `--log-level` | `info` | `debug`, `info`, `warn` or `error`. At `debug`, every container event and every cgroup write is logged. |
| `--resync-period` | `10s` | How often to check that the partitions are still in place and repair them. `0` disables it, but kubelet's CPU Manager rewrites `cpuset.cpus` and invalidates partitions, so leave it on. |
| `--cleanup-on-exit` | `false` | Release every partition and reservation when the plugin stops. Off by default so that restarting or upgrading the plugin doesn't disturb running workloads. |
| `--plugin-name` | `cpuset-partition` | Name the plugin registers with in NRI. |
| `--plugin-index` | `10` | NRI plugin index, which decides the order of plugins. Must be exactly two digits: containerd rejects anything else at registration, and the plugin then retries forever without running. The Helm chart sets `50`. |
| `--nri-socket` | NRI default (`/var/run/nri/nri.sock`) | Path of the NRI socket to connect to. |
| `--cgroup-root` | autodetected | Mount point of the cgroup v2 hierarchy, found from `/proc/self/mountinfo` when not set. |
| `--kubepods-cgroup` | `kubepods.slice` | Name of the pod cgroup tree below the cgroup root. Only used at startup to find and clear reservations and partitions left behind by a previous run. With kubelet's `cgroupfs` driver it is `kubepods`. If the directory doesn't exist, the plugin logs a warning at startup and skips that cleanup. |
| `--version` | | Print the version and exit. |

Flags take one or two leading dashes, and boolean flags can be given bare
(`--dry-run`) or with a value (`--dry-run=false`).

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
