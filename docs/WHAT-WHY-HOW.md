# WHAT's the issue? / WHY do we need this NRI plugin? / HOW is it expected to work?

## [A]. Assumptions

- **RKE2 >= v1.35.4+rke2r1** (see the [release notes](https://github.com/rancher/rke2/releases/tag/v1.35.4%2Brke2r1)), which implies:
  - containerd >= v2.2.3
  - runc >= v1.4.2
- **Operating system**: SUSE Linux Micro 6.2 RT (`6.12.0-160000.38-rt`) or a newer `PREEMPT_RT` kernel.
- **Go** >= 1.26.0

## [B]. Problem description

### B.1 Housekeeping cores are needed for network interrupts

We need a non-negligible number of Housekeeping (HK) cores to handle network-related interrupts.

> **IMPORTANT:** this does **not** apply to DPDK workloads, as these workloads poll the NIC RX queues
> directly and therefore avoid IRQ handling.
> The number of HK cores required from DPDK-based workloads is then much lower than what is required from non-DPDK networking workloads

- When using RSS to distribute/load balance the IRQ/MSI-X vectors (each RX queue has its own
  MSI-X vector), several HK cores are needed to manage those IRQs/MSI-Xs.
- ... but having many network-intensive workloads to run also requires enough, properly isolated,
cores to exclusively assign to those workloads ...

### B.2 The `static` CPU Manager policy

We set the kubelet CPU Manager policy to `static` so that certain workloads get exclusively
assigned cores. The two conditions required by the `static` policy are:

1. The task is triggered from a container defined in the scope of a **Guaranteed** QoS pod.
2. That container requests an **integer** amount of CPU.

> **IMPORTANT:** all this **also** applies to DPDK workloads.

- The `static` policy removes the `reservedSystemCPUs` cores (set in the kubelet config) from
  being exclusively assigned to containers that meet the two aforementioned conditions.
- The `static` policy does **not** remove the `reservedSystemCPUs` cores from the *shared pool*,
  so those cores can still run tasks triggered from pods' containers that do not fulfil
  (1) **and** (2) above.
- Ensuring that operating system daemons and Kubernetes daemons are restricted to the cores
defined in `reservedSystemCPUs` is not something that Kubernetes manages; it is handled
outside of Kubernetes.

### B.3 `strict-cpu-reservation`

We also plan to enable `strict-cpu-reservation` CPU Manager Policy option to ensure that
the cores reserved for the operating system and Kubernetes daemons are not used to run
any pod workloads.

> **IMPORTANT:** all this **also** applies to DPDK workloads.

- As said above, the `static` policy does **not** remove the `reservedSystemCPUs` cores from the
  shared pool; `strict-cpu-reservation` does, resulting in all pods' containers' cgroups having
  those cores removed from their `cpuset.cpus` (except for the pod sandbox — `pause` — container,
  which does not consume CPU cycles anyway).
- A most tasks triggered from pods' containers do not invoke (directly or indirectly) the
  `sched_setaffinity()` syscall: we therefore **should not** set the `domain` flag in the
  `isolcpus` kernel argument when also enabling the `strict-cpu-reservation`
  `cpuManagerPolicyOption` on the kubelet side: doing so would restrict all tasks triggered from
  non self-pinning-capable workloads to the first isolated core, overloading that core (as already
  experienced in several test).
- Removing an isolated core from the SMP balancing and scheduling algorithm should be done
  (a) dynamically, (b) only for those cores exclusively assigned to pods' containers by the
  `static` policy of the kubelet CPU Manager and (c) when explictly requested for a given pod's
  container. This is done with:

  ```sh
  echo isolated > cpuset.cpus.partition
  ```

  on the pod's container cgroup (i.e., using the cgroup v2 cpuset ["remote partition"](https://docs.kernel.org/admin-guide/cgroup-v2.html#:~:text=partition%20is%20created.-,cpuset.cpus.partition,-A%20read%2Dwrite)
  support). It also requires previously updating the ancestors' cgroups' `cpuset.cpus.exclusive` value
  — updating it first in `/sys/fs/cgroup/kubepods.slice` and, after that, in
  `/sys/fs/cgroup/kubepods.slice/kubepods-pod<uid>.slice` — to include the cores exclusively
  assigned to that pod's container.

  If this is not done, reading back `cpuset.cpus.partition` after setting `isolated` reports:

  ```text
  isolated invalid (Invalid cpu list in cpuset.cpus.exclusive)
  ```

  This error happens because the container cgroup's `cpuset.cpus.exclusive` cores (that
  is, the range implicitly covered by `cpuset.cpus` when `cpuset.cpus.exclusive` is left empty)
  is not equal to, nor a subset of, the ancestor cgroup's `cpuset.cpus.exclusive.effective`.

## [C]. Proposed solution

An NRI plugin with the behaviour described below.

### C.1 Applying the partition setting

- The plugin applies `echo isolated > cpuset.cpus.partition` on a container's cgroup AFTER
  having updated the ancestors' cgroups' `cpuset.cpus.exclusive`: adding
  the cores exclusively assigned to that container — for those pods' containers that are
  exclusively assigned a set of cores by the kubelet `static` CPU Manager policy.
- It must also remove those exclusively assigned cores from the ancestors'
  `cpuset.cpus.exclusive` value when **all** the pod's containers have terminated — that is, when
  the CPU Manager adds those cores back to the shared pool (not before, not
  after). This means the plugin must track the lifecycle of the pods' containers in order to
  correctly update the ancestors' cgroups' `cpuset.cpus.exclusive` value when all the pod's
  containers have terminated (not before, not after).

### C.2 Annotations

The plugin acts only when the involved pod carries a new Pod annotation asking for it. The
possibilities for this new `cpuset-partition.nri.io` annotation are:

```yaml
# pod-level annotation [optional]:
#   Applies the indicated value to all the containers in this Guaranteed QoS pod
#   that ask for an integer amount of CPU.
cpuset-partition.nri.io/pod: <annotation_value>

# container-level annotation [optional]:
#   Applies the indicated value to the container with the given name.
#   Other containers in this pod that request an integer amount of CPU, but lack
#   their own specific annotation, adopt the value set via the pod-level annotation;
#   if the latter is also absent, the system falls back to the default "member"
#   value (the "member" value indicates that the NRI plugin should take no action).
cpuset-partition.nri.io/container.<container_name>: <annotation_value>
```

- **NOTE (1):** if this annotation is set on a pod that is not Guaranteed QoS, or refers to a
  container (in a Guaranteed QoS pod) that does not request an integer amount of CPU — and so is
  not exclusively assigned a set of cores by the CPU Manager — it is ignored by the plugin.
- **NOTE (2):** if both the pod and the container annotation forms are present in a Guaranteed QoS
  pod, the container annotation value takes precedence for the indicated container. The pod
  annotation value is still used for all the other containers in that pod that ask for an integer
  amount of CPU (and so are exclusively assigned a set of cores by the CPU Manager "static" policy)
  but are missing "their" `cpuset-partition.nri.io/container.<container_name>` annotation.

Supported annotation values (see <https://kernel-internals.org/sched/cpuset/>):

| Value | Meaning |
| --- | --- |
| `member` (default) | Normal cpuset; shares the parent's scheduling domain. |
| `root` | Carves out CPUs into a dedicated scheduling domain. |
| `isolated` | Like `root`, but also disables load balancing within the partition. |

Any other value is ignored by the plugin, which then assumes the default `member` value for that
annotation.

### C.3 Thread safety and lifecycle handling

The plugin must be thread-safe and must correctly handle the lifecycle of pods' containers when
updating the ancestors' `cpuset.cpus.exclusive` value, taking special care in the following
cases:

- Two or more containers **from the same Guaranteed QoS pod** that ask for an integer amount of
  CPU and request a `root` or `isolated` value: the writes to the two common ancestors'
  `cpuset.cpus.exclusive` must be protected, so that in the end they include the exclusively
  assigned cores of all these containers.
- Two or more **Guaranteed QoS pods**, each including containers that ask for an integer amount
  of CPU and request a `root` or `isolated` value: the writes to the their common
  `/sys/fs/cgroup/kubepods.slice` ancestor's `cpuset.cpus.exclusive` must be protected, so that
  in the end they include the exclusively assigned cores to all these containers.
- The same applies when all the containers of two or more Guaranteed QoS pods reach `terminated`
  at almost the same moment: the updates to the  common `/sys/fs/cgroup/kubepods.slice`
  ancestor's `cpuset.cpus.exclusive` must be done carefully so that, in the end, the exclusively
  assigned cores to the containers of each of these pods are all removed.

> **In general:** updates to the ancestors' cgroups' `cpuset.cpus.exclusive` must be done
> atomically, to avoid one update overriding other concurrent updates.

### C.4 Deployment

The plugin is to be deployed as a **DaemonSet** in a Kubernetes cluster (RKE2-based, using
containerd > 2.0 as the container runtime).
