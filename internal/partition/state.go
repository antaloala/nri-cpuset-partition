package partition

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/containerd/nri/pkg/api"

	"github.com/antaloala/nri-cpuset-partition/internal/cgroup"
	"github.com/antaloala/nri-cpuset-partition/internal/cpuset"
)

// The annotation this plugin reacts to, in its pod-wide and per-container
// forms, e.g.
//
//	cpuset-partition.nri.io/pod: isolated
//	cpuset-partition.nri.io/container.dpdk-app: isolated
const (
	AnnotationDomain    = "cpuset-partition.nri.io"
	AnnotationPod       = AnnotationDomain + "/pod"
	AnnotationCtrPrefix = AnnotationDomain + "/container."
	qosClassGuaranteed  = "Guaranteed"
)

// target is one container the plugin has to turn into a cpuset partition.
type target struct {
	PodID     string
	PodUID    string
	PodName   string
	Namespace string
	CtrID     string
	CtrName   string

	// Mode is the requested cpuset.cpus.partition value, "root" or
	// "isolated"; containers resolving to "member" never become a target.
	Mode string
	// CPUs are the cores the CPU Manager exclusively assigned to the
	// container, as read back from its cgroup.
	CPUs cpuset.CPUSet
	// Dir is the container's cgroup directory and Ancestors lists every
	// cgroup between the cgroup root and Dir, outermost first.
	Dir       string
	Ancestors []string
}

func (t *target) describe() string {
	return fmt.Sprintf("%s/%s/%s", t.Namespace, t.PodName, t.CtrName)
}

// snapshot copies a target. What has been applied to the cgroups must not
// alias what the plugin currently wants, or a cpuset that changed underneath
// us would compare equal to itself and never be re-applied.
func (t *target) snapshot() *target {
	copied := *t
	return &copied
}

// podState tracks one pod: the containers we manage for it, the containers of
// it that are still running, and the cpus whose reservation has to be held
// until the pod is done.
type podState struct {
	ID        string
	UID       string
	Name      string
	Namespace string
	Dir       string
	Ancestors []string

	// running holds the ids of every container of the pod that has not been
	// stopped yet, managed by this plugin or not. The pod's reservation is
	// released only once this is empty, which is the plugin's rendering of
	// "when ALL the pod's containers have terminated".
	running map[string]bool
	// managed holds the containers we have turned into partitions.
	managed map[string]*target
	// retired holds the cpus of managed containers that have already been
	// stopped. They stay reserved on the ancestors until the whole pod is
	// gone, so that a surviving sibling container never sees the reservation
	// it depends on shrink underneath it.
	retired cpuset.CPUSet
}

// reserved is everything the pod currently needs reserved on its ancestors.
func (p *podState) reserved() cpuset.CPUSet {
	out := p.retired
	for _, t := range p.managed {
		out = out.Union(t.CPUs)
	}
	return out
}

// partitionMode resolves the partition value requested for a container.
//
// Only annotations at kubernetes level are looked at: they are the ones a Kubernetes
// manifest sets (so CRI container annotations are ignored). A per-container partition
// annotation wins over the pod-wide one (see NOTE(2) of the design), and any value
// that is not "root" or "isolated" degrades to "member", which means the plugin leaves
// the container untouched.
//
// foundPartitionAnnotation is the annotation key that decided, empty when not found;
// originalMode is that annotation's value as written, so a typo can be reported as
// such (instead of as a request for "member").
func partitionMode(pod *api.PodSandbox, ctr *api.Container) (selectedMode, foundPartitionAnnotation, originalMode string) {
	annotations := pod.GetAnnotations()

	for _, triedPartitionAnnotation := range []string{AnnotationCtrPrefix + ctr.GetName(), AnnotationPod} {
		originalMode, ok := annotations[triedPartitionAnnotation]
		if !ok {
			continue
		}
		switch normalizedMode := strings.ToLower(strings.TrimSpace(originalMode)); normalizedMode {
		case cgroup.PartitionRoot, cgroup.PartitionIsolated:
			return normalizedMode, triedPartitionAnnotation, originalMode
		default:
			// Explicit "member" and anything unrecognised both mean
			// "nothing to do"; evaluate tells the two apart in its reason.
			return cgroup.PartitionMember, triedPartitionAnnotation, originalMode
		}
	}

	return cgroup.PartitionMember, "", ""
}

// isAnnotated reports whether the pod carries a partition annotation that
// applies to this container, whatever its value.
func isAnnotated(pod *api.PodSandbox, ctr *api.Container) bool {
	_, source, _ := partitionMode(pod, ctr)
	return source != ""
}

// requestedCPUs recovers the integer number of cpus a container asked for. It
// reports false when the request is not a whole number of cpus, in which case
// the CPU Manager did not assign it exclusive cores.
//
// The cfs quota is used when set. Since Kubernetes 1.33 the kubelet
// (DisableCPUQuotaWithExclusiveCPUs, on by default) leaves containers with
// exclusive cpus unthrottled, quota -1, so the cpu shares are the fallback:
// the kubelet derives them as milliCPU*1024/1000 from the request, which in a
// Guaranteed pod equals the limit, and they are a multiple of 1024 exactly
// when the request is a whole number of cpus.
func requestedCPUs(ctr *api.Container) (int, bool) {
	cpu := ctr.GetLinux().GetResources().GetCpu()
	quota, period := cpu.GetQuota().GetValue(), int64(cpu.GetPeriod().GetValue())
	if quota > 0 && period > 0 {
		if quota%period != 0 {
			return 0, false
		}
		return int(quota / period), true
	}
	if shares := cpu.GetShares().GetValue(); shares > 0 && shares%1024 == 0 {
		return int(shares / 1024), true
	}
	return 0, false
}

// evaluate decides whether a container must be turned into a cpuset partition
// and returns the target describing the work. It returns a nil target plus the
// reason when the container is out of scope.
//
// refused is set when the container asked for a partition ("root" or
// "isolated", or a value meant as one) and cannot have it: a misconfiguration
// the user has to be told about. It is not set for containers that did not ask,
// for an explicit "member", nor for a reason that clears up on its own.
func (p *Plugin) evaluate(pod *api.PodSandbox, ctr *api.Container) (t *target, reason string, refused bool, err error) {
	decidedPartitionMode, foundAnnotation, originalValue := partitionMode(pod, ctr)
	if decidedPartitionMode == cgroup.PartitionMember {
		switch {
		case foundAnnotation == "":
			return nil, "not annotated", false, nil
		case strings.EqualFold(strings.TrimSpace(originalValue), cgroup.PartitionMember):
			return nil, fmt.Sprintf("%s requests %q", foundAnnotation, cgroup.PartitionMember), false, nil
		default:
			return nil, fmt.Sprintf("%s has unsupported value %q (want member, root or isolated)", foundAnnotation, originalValue), true, nil
		}
	}

	dir, err := p.cg.ContainerDir(ctr.GetLinux().GetCgroupsPath())
	if err != nil {
		return nil, "", false, err
	}
	ancestors, err := p.cg.Ancestors(dir)
	if err != nil {
		return nil, "", false, err
	}
	if len(ancestors) < 2 {
		return nil, "", false, fmt.Errorf("unexpected cgroup layout for %s: %s", ctr.GetName(), dir)
	}

	// NOTE(1): the annotation is ignored unless the CPU Manager actually gave
	// the container exclusive cores, which takes a Guaranteed pod asking for a
	// whole number of cpus.
	podDir := filepath.Dir(dir)
	if qos := cgroup.QoSClass(podDir); qos != qosClassGuaranteed {
		return nil, "pod is " + qos + " QoS", true, nil
	}
	want, haveQuota := requestedCPUs(ctr)
	if !haveQuota {
		return nil, "container does not request an integer amount of cpu", true, nil
	}

	cpus, err := p.assignedCPUs(dir, ctr)
	if err != nil {
		return nil, "", false, err
	}
	if cpus.IsEmpty() {
		// Transient: the next event normally finds the cpuset in place.
		return nil, "no cpuset assigned (yet)", false, nil
	}
	// With the static policy the container's cpuset is exactly the cores it
	// was given. A wider cpuset means it is running out of the shared pool,
	// so there is nothing exclusive to carve out.
	if cpus.Size() != want {
		return nil, fmt.Sprintf("cpuset %s does not match the %d exclusive cpu(s) requested", cpus, want), true, nil
	}

	return &target{
		PodID:     pod.GetId(),
		PodUID:    pod.GetUid(),
		PodName:   pod.GetName(),
		Namespace: pod.GetNamespace(),
		CtrID:     ctr.GetId(),
		CtrName:   ctr.GetName(),
		Mode:      decidedPartitionMode,
		CPUs:      cpus,
		Dir:       dir,
		Ancestors: ancestors,
	}, "", false, nil
}

// assignedCPUs reads the cores the CPU Manager pinned the container to. The
// cgroup is authoritative once it exists; the resources NRI reported are the
// fallback for the window before the runtime has created it.
func (p *Plugin) assignedCPUs(dir string, ctr *api.Container) (cpuset.CPUSet, error) {
	if p.cg.Exists(dir) {
		cpus, err := p.cg.ReadCPUSet(dir, cgroup.FileCPUs)
		if err == nil && !cpus.IsEmpty() {
			return cpus, nil
		}
	}

	cpus, err := cpuset.Parse(ctr.GetLinux().GetResources().GetCpu().GetCpus())
	if err != nil {
		return cpuset.CPUSet{}, fmt.Errorf("container %s: %w", ctr.GetName(), err)
	}

	return cpus, nil
}
