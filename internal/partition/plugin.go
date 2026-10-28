// Package partition implements the NRI plugin that turns the cgroup of a
// container holding exclusively assigned cpus into a cgroup v2 cpuset
// partition ("root" or "isolated"), handing the exclusive reservation down
// from the kubepods cgroup on the way.
//
// The kernel only lets a leaf cgroup become a partition root if every one of
// its ancestors already lists the cpus in cpuset.cpus.exclusive; kubelet
// leaves that file empty, so without this plugin a write of "isolated" to a
// container's cpuset.cpus.partition is rejected with
//
//	isolated invalid (Invalid cpu list in cpuset.cpus.exclusive)
package partition

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"

	"github.com/antaloala/nri-cpuset-partition/internal/cgroup"
	"github.com/antaloala/nri-cpuset-partition/internal/cpuset"
)

// Config holds the plugin's tunables. The same fields can be supplied by NRI
// as a JSON configuration blob.
type Config struct {
	// KubepodsCgroup is the name of the pod tree below the cgroup root. It is
	// only used at startup, to find and adopt reservations left behind by a
	// previous run of the plugin.
	KubepodsCgroup string `json:"kubepodsCgroup"`
	// ResyncPeriod is how often the plugin re-checks that its partitions are
	// still in place. Kubelet's CPU Manager reconcile loop rewrites
	// cpuset.cpus behind our back and knocks partitions back to invalid, so
	// this is a correctness mechanism, not just a safety net.
	ResyncPeriod time.Duration `json:"resyncPeriod"`
	// CleanupOnExit tears every partition down when the plugin stops. Off by
	// default: a plugin restart or upgrade must not disturb running
	// workloads.
	CleanupOnExit bool `json:"cleanupOnExit"`
}

// DefaultConfig returns the configuration the plugin runs with unless told
// otherwise.
func DefaultConfig() Config {
	return Config{
		KubepodsCgroup: "kubepods.slice",
		ResyncPeriod:   10 * time.Second,
		CleanupOnExit:  false,
	}
}

// Plugin is the NRI plugin.
//
// Every exported entry point takes mu for its whole duration and then runs a
// full reconcile from the in-memory state. Concurrent container events
// therefore cannot interleave their read-modify-write of a shared ancestor's
// cpuset.cpus.exclusive: each one recomputes that file from the complete set
// of live reservations, so no update can drop another one's cpus.
type Plugin struct {
	cfg Config
	cg  *cgroup.Manager
	log *slog.Logger

	mu sync.Mutex
	// pods is indexed by pod sandbox id.
	pods map[string]*podState
	// applied records what was last successfully written for a container, so
	// that a changed or vanished target can be torn down before the ancestor
	// reservations shrink.
	applied map[string]*target
	// touched is every ancestor cgroup the plugin has written a reservation
	// to, so it knows what to clear once the last reservation is released.
	touched map[string]bool
}

// New creates a plugin instance.
func New(cfg Config, cg *cgroup.Manager, log *slog.Logger) *Plugin {
	return &Plugin{
		cfg:     cfg,
		cg:      cg,
		log:     log,
		pods:    map[string]*podState{},
		applied: map[string]*target{},
		touched: map[string]bool{},
	}
}

//
// NRI entry points.
//

// Configure accepts an optional JSON configuration from NRI and subscribes to
// the events the plugin implements (returning 0 leaves the mask to the stub).
func (p *Plugin) Configure(ctx context.Context, config, runtime, version string) (api.EventMask, error) {
	if config != "" {
		cfg := p.cfg
		if err := json.Unmarshal([]byte(config), &cfg); err != nil {
			return 0, fmt.Errorf("invalid plugin configuration: %w", err)
		}
		p.mu.Lock()
		p.cfg = cfg
		p.mu.Unlock()
	}

	p.log.Info("configured", "runtime", runtime, "version", version,
		"resyncPeriod", p.cfg.ResyncPeriod, "dryRun", p.cg.DryRun())

	return 0, nil
}

// Synchronize rebuilds the plugin's view of the node. It runs at startup and
// whenever the plugin reconnects to the runtime, and it is what lets the
// plugin adopt the partitions of containers that were already running.
func (p *Plugin) Synchronize(ctx context.Context, pods []*api.PodSandbox, containers []*api.Container) ([]*api.ContainerUpdate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	byID := make(map[string]*api.PodSandbox, len(pods))
	for _, pod := range pods {
		byID[pod.GetId()] = pod
	}

	p.pods = map[string]*podState{}

	for _, ctr := range containers {
		switch ctr.GetState() {
		case api.ContainerState_CONTAINER_CREATED, api.ContainerState_CONTAINER_RUNNING, api.ContainerState_CONTAINER_PAUSED:
		default:
			continue
		}
		pod, ok := byID[ctr.GetPodSandboxId()]
		if !ok {
			p.log.Warn("container without a sandbox, skipped", "container", ctr.GetName())
			continue
		}
		p.trackLocked(pod, ctr)
	}

	p.adoptStaleReservationsLocked()

	p.log.Info("synchronized", "pods", len(p.pods), "containers", len(containers))
	p.reconcileLocked(ctx)

	return nil, nil
}

// PostCreateContainer is the first event for a new container. Its cgroup does
// not exist yet: containerd's CRI only creates the runc task, and with it the
// cgroup, in StartContainer. The cpus NRI reports are enough to reserve them
// on the ancestors here; the leaf is partitioned at PostStartContainer.
func (p *Plugin) PostCreateContainer(ctx context.Context, pod *api.PodSandbox, ctr *api.Container) error {
	p.onContainerEvent(ctx, "PostCreateContainer", pod, ctr)
	return nil
}

// PostStartContainer is where the partition is normally created: the
// container's cgroup now exists, holding the cpus the CPU Manager assigned.
func (p *Plugin) PostStartContainer(ctx context.Context, pod *api.PodSandbox, ctr *api.Container) error {
	p.onContainerEvent(ctx, "PostStartContainer", pod, ctr)
	return nil
}

// UpdateContainer runs before the runtime applies new resources. A write of
// cpuset.cpus into a live partition root is rejected unless the new cpus are
// already reserved on the ancestors, so the partition is dismantled here and
// rebuilt in PostUpdateContainer against whatever cpuset the update left.
func (p *Plugin) UpdateContainer(ctx context.Context, pod *api.PodSandbox, ctr *api.Container, _ *api.LinuxResources) ([]*api.ContainerUpdate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if applied, ok := p.applied[ctr.GetId()]; ok {
		p.log.Debug("dismantling partition for a resource update", "container", applied.describe())
		p.demoteLocked(applied)
		delete(p.applied, ctr.GetId())
	}

	return nil, nil
}

// PostUpdateContainer rebuilds the partition after a resource update, which is
// also the event kubelet's CPU Manager reconcile loop generates when it
// rewrites a container's cpuset.
func (p *Plugin) PostUpdateContainer(ctx context.Context, pod *api.PodSandbox, ctr *api.Container) error {
	p.onContainerEvent(ctx, "PostUpdateContainer", pod, ctr)
	return nil
}

// StopContainer retires a container's partition.
//
// The leaf is demoted straight away, because the workload is going away and
// leaving an isolated partition behind would keep its cpus out of the parent's
// effective cpuset for no reason. The reservation on the ancestors is only
// given back once every container of the pod has stopped.
func (p *Plugin) StopContainer(ctx context.Context, pod *api.PodSandbox, ctr *api.Container) ([]*api.ContainerUpdate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.stopContainerLocked(ctx, pod.GetId(), ctr.GetId())
	p.reconcileLocked(ctx)

	return nil, nil
}

// RemoveContainer is the backstop for containers that never produced a usable
// StopContainer event.
func (p *Plugin) RemoveContainer(ctx context.Context, pod *api.PodSandbox, ctr *api.Container) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.stopContainerLocked(ctx, pod.GetId(), ctr.GetId())
	p.reconcileLocked(ctx)

	return nil
}

// StopPodSandbox releases everything the pod still holds.
func (p *Plugin) StopPodSandbox(ctx context.Context, pod *api.PodSandbox) error {
	return p.dropPod(ctx, pod.GetId())
}

// RemovePodSandbox releases everything the pod still holds.
func (p *Plugin) RemovePodSandbox(ctx context.Context, pod *api.PodSandbox) error {
	return p.dropPod(ctx, pod.GetId())
}

// Shutdown is called when the runtime goes away.
func (p *Plugin) Shutdown(ctx context.Context) {
	p.log.Info("runtime is shutting down")
	if p.cfg.CleanupOnExit {
		p.Cleanup(ctx)
	}
}

// Cleanup tears down every partition the plugin created and releases all
// reservations. It is only used when the plugin is told to clean up on exit.
func (p *Plugin) Cleanup(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.log.Info("releasing all partitions")
	p.pods = map[string]*podState{}
	p.reconcileLocked(ctx)
}

// Resync re-reads the state of every managed container and repairs whatever
// drifted, then reconciles. Call it periodically.
func (p *Plugin) Resync(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for id, pod := range p.pods {
		// A pod whose cgroup is gone will never send another event; drop it
		// rather than hold its reservation forever.
		if pod.Dir != "" && !p.cg.Exists(pod.Dir) {
			p.log.Info("pod cgroup is gone, releasing reservation",
				"pod", pod.Namespace+"/"+pod.Name, "cpus", pod.reserved().String())
			delete(p.pods, id)
			for ctrID := range pod.managed {
				delete(p.applied, ctrID)
			}
			continue
		}

		for id, t := range pod.managed {
			if !p.cg.Exists(t.Dir) {
				p.log.Debug("cgroup is gone, retiring", "container", t.describe())
				p.retireLocked(pod, id)
				continue
			}

			// The CPU Manager may have handed the container a different set
			// of cores; the reservation has to follow.
			if cpus, err := p.cg.ReadCPUSet(t.Dir, cgroup.FileCPUs); err == nil && !cpus.IsEmpty() && !cpus.Equals(t.CPUs) {
				p.log.Info("cpuset changed underneath us", "container", t.describe(),
					"from", t.CPUs.String(), "to", cpus.String())
				t.CPUs = cpus
			}

			// A kubelet restart or a container update can knock the partition
			// back to member or to invalid; forget that it was applied so the
			// reconcile below programs it again.
			if _, ok := p.applied[id]; !ok {
				continue
			}
			state, err := p.cg.ReadPartition(t.Dir)
			if err != nil {
				p.log.Warn("cannot read partition state", "container", t.describe(), "err", err)
				continue
			}
			if state.Invalid || state.Value != t.Mode {
				p.log.Info("partition drifted, repairing", "container", t.describe(), "state", state.Raw)
				delete(p.applied, id)
			}
		}
	}

	p.reconcileLocked(ctx)
}

//
// Bookkeeping.
//

func (p *Plugin) onContainerEvent(ctx context.Context, event string, pod *api.PodSandbox, ctr *api.Container) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Kubelet's cpu-manager "static" policy rewrites cpuset.cpus of every container being
	// assigned the shared pool each time an exclusive allocation of cores changes, so most
	// container events are about containers that never asked for a partition. Only
	// annotated containers get this per-event line; the others still get one
	// "out of scope" line from trackLocked, with the reason.
	if isAnnotated(pod, ctr) {
		p.log.Debug("container event", "event", event, "pod", pod.GetName(), "container", ctr.GetName())
	}
	p.trackLocked(pod, ctr)
	p.reconcileLocked(ctx)
}

// trackLocked records a container and, when it is in scope, the partition work
// it needs.
func (p *Plugin) trackLocked(pod *api.PodSandbox, ctr *api.Container) {
	ps, ok := p.pods[pod.GetId()]
	if !ok {
		ps = &podState{
			ID:        pod.GetId(),
			UID:       pod.GetUid(),
			Name:      pod.GetName(),
			Namespace: pod.GetNamespace(),
			running:   map[string]bool{},
			managed:   map[string]*target{},
			retired:   cpuset.CPUSet{},
		}
		p.pods[pod.GetId()] = ps
	}
	ps.running[ctr.GetId()] = true

	t, reason, err := p.evaluate(pod, ctr)
	if err != nil {
		p.log.Error("cannot evaluate container", "pod", pod.GetName(), "container", ctr.GetName(), "err", err)
		return
	}
	if t == nil {
		if _, wasManaged := ps.managed[ctr.GetId()]; wasManaged {
			// It used to be in scope and no longer is: drop it so the
			// reconcile tears the partition down.
			p.log.Info("container left the plugin's scope", "container", ctr.GetName(), "reason", reason)
			delete(ps.managed, ctr.GetId())
		} else {
			p.log.Debug("container out of scope", "pod", pod.GetName(), "container", ctr.GetName(), "reason", reason)
		}
		return
	}

	if ps.Dir == "" {
		ps.Dir = t.Ancestors[len(t.Ancestors)-1]
		ps.Ancestors = t.Ancestors
	}
	ps.managed[ctr.GetId()] = t
}

// stopContainerLocked marks a container as stopped. The pod's reservation is
// released once this was its last running container.
func (p *Plugin) stopContainerLocked(ctx context.Context, podID, ctrID string) {
	ps, ok := p.pods[podID]
	if !ok {
		return
	}

	delete(ps.running, ctrID)
	p.retireLocked(ps, ctrID)

	if len(ps.running) == 0 {
		p.log.Info("last container of the pod stopped, releasing reservation",
			"pod", ps.Namespace+"/"+ps.Name, "cpus", ps.reserved().String())
		delete(p.pods, podID)
	}
}

// retireLocked demotes a managed container's partition and parks its cpus in
// the pod's retained reservation.
func (p *Plugin) retireLocked(ps *podState, ctrID string) {
	t, ok := ps.managed[ctrID]
	if !ok {
		return
	}

	if applied, ok := p.applied[ctrID]; ok {
		p.demoteLocked(applied)
		delete(p.applied, ctrID)
	}
	ps.retired = ps.retired.Union(t.CPUs)
	delete(ps.managed, ctrID)
}

func (p *Plugin) dropPod(ctx context.Context, podID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	ps, ok := p.pods[podID]
	if !ok {
		return nil
	}
	for id := range ps.managed {
		p.retireLocked(ps, id)
	}
	delete(p.pods, podID)
	p.reconcileLocked(ctx)

	return nil
}

//
// Reconciliation.
//

// reconcileLocked drives the cgroup state towards the plugin's bookkeeping. It
// is idempotent, and it is the only place that writes to cgroup files.
//
// The order matters:
//
//  1. tear down leaves that are no longer wanted, so that no live partition
//     depends on a reservation that step 2 is about to withdraw;
//  2. write every ancestor's cpuset.cpus.exclusive, parents before children,
//     because a child can only be granted cpus its parent already holds;
//  3. create the partitions at the leaves.
func (p *Plugin) reconcileLocked(ctx context.Context) {
	desired := map[string]*target{}
	for _, ps := range p.pods {
		for id, t := range ps.managed {
			desired[id] = t
		}
	}

	// 1. stale or changed leaves.
	for id, a := range p.applied {
		t, ok := desired[id]
		if ok && t.Mode == a.Mode && t.Dir == a.Dir && t.CPUs.Equals(a.CPUs) {
			continue
		}
		p.demoteLocked(a)
		delete(p.applied, id)
	}

	// 2. reservations on the ancestors.
	want := map[string]cpuset.CPUSet{}
	for dir := range p.touched {
		want[dir] = cpuset.CPUSet{}
	}
	for _, ps := range p.pods {
		reserved := ps.reserved()
		if reserved.IsEmpty() {
			continue
		}
		for _, dir := range ps.Ancestors {
			want[dir] = want[dir].Union(reserved)
		}
	}
	for _, dir := range cgroup.SortTopDown(keys(want)) {
		set := want[dir]
		if !p.cg.Exists(dir) {
			delete(p.touched, dir)
			continue
		}
		if err := p.cg.WriteCPUSet(dir, cgroup.FileExclusive, set); err != nil {
			p.log.Error("cannot reserve cpus", "cgroup", dir, "cpus", set.String(), "err", err)
			continue
		}
		if set.IsEmpty() {
			delete(p.touched, dir)
			continue
		}
		p.touched[dir] = true
		p.verifyReservationLocked(dir, set)
	}

	// 3. the partitions themselves.
	for id, t := range desired {
		if _, ok := p.applied[id]; ok {
			continue
		}
		if !p.cg.Exists(t.Dir) {
			// Expected at PostCreateContainer, before the runtime has created
			// the cgroup; PostStartContainer, or the next resync, retries.
			p.log.Debug("container cgroup not created yet, partition deferred",
				"container", t.describe(), "cgroup", t.Dir)
			continue
		}
		if err := p.programLocked(t); err != nil {
			p.log.Error("cannot create partition", "container", t.describe(),
				"cpus", t.CPUs.String(), "mode", t.Mode, "err", err)
			continue
		}
		p.applied[id] = t.snapshot()
		p.log.Info("partition created", "container", t.describe(),
			"cpus", t.CPUs.String(), "mode", t.Mode, "cgroup", t.Dir)
	}
}

// verifyReservationLocked warns when the kernel did not actually grant the
// cpus we asked an ancestor to hold, which is the failure that later shows up
// as "invalid (Invalid cpu list in cpuset.cpus.exclusive)" at the leaf.
func (p *Plugin) verifyReservationLocked(dir string, want cpuset.CPUSet) {
	if p.cg.DryRun() {
		return
	}
	effective, err := p.cg.ReadCPUSet(dir, cgroup.FileExclusiveEffect)
	if err != nil {
		p.log.Warn("cannot read effective reservation", "cgroup", dir, "err", err)
		return
	}
	if !want.IsSubsetOf(effective) {
		p.log.Warn("kernel granted less than requested", "cgroup", dir,
			"requested", want.String(), "effective", effective.String(),
			"hint", "the parent cgroup may not hold these cpus, or a sibling already claims them")
	}
}

// programLocked gives a container's cgroup its exclusive cpus and turns it
// into a partition.
func (p *Plugin) programLocked(t *target) error {
	if !p.cg.Exists(t.Dir) {
		return fmt.Errorf("cgroup %s does not exist", t.Dir)
	}
	// The leaf's own cpuset.cpus.exclusive is left empty: for the partition
	// root itself the kernel takes cpuset.cpus (what the CPU Manager wrote) as
	// the implicit exclusive set. Only the ancestors need it written, which
	// reconcileLocked has already done.
	if err := p.cg.WriteString(t.Dir, cgroup.FilePartition, t.Mode); err != nil {
		return err
	}
	if p.cg.DryRun() {
		return nil
	}

	state, err := p.cg.ReadPartition(t.Dir)
	if err != nil {
		return err
	}
	if state.Invalid || state.Value != t.Mode {
		return fmt.Errorf("kernel reports %q", state.Raw)
	}

	return nil
}

// demoteLocked puts a container's cgroup back to a plain member cpuset.
func (p *Plugin) demoteLocked(t *target) {
	if !p.cg.Exists(t.Dir) {
		return // the container is gone and took its cgroup with it
	}
	if err := p.cg.WriteString(t.Dir, cgroup.FilePartition, cgroup.PartitionMember); err != nil {
		p.log.Warn("cannot demote partition", "container", t.describe(), "err", err)
	}
	if err := p.cg.WriteCPUSet(t.Dir, cgroup.FileExclusive, cpuset.CPUSet{}); err != nil {
		p.log.Warn("cannot clear exclusive cpus", "container", t.describe(), "err", err)
	}
	p.log.Info("partition released", "container", t.describe(), "cpus", t.CPUs.String())
}

// adoptStaleReservationsLocked finds reservations a previous run of the plugin
// left on the pod cgroup tree, so that the reconcile can clear the ones no
// running container needs any more.
//
// Call it after the containers have been tracked: the cgroup of a container
// the plugin is taking back over must not be adopted, or the reconcile would
// clear its exclusive cpus, invalidating a partition it is about to rebuild
// anyway.
func (p *Plugin) adoptStaleReservationsLocked() {
	subtree := filepath.Join(p.cg.Root(), p.cfg.KubepodsCgroup)
	if !p.cg.Exists(subtree) {
		p.log.Debug("no pod cgroup tree to scan", "cgroup", subtree)
		return
	}

	dirs, err := p.cg.FindExclusiveReservations(subtree)
	if err != nil {
		p.log.Warn("cannot scan for stale reservations", "cgroup", subtree, "err", err)
		return
	}

	live := map[string]bool{}
	for _, ps := range p.pods {
		for _, t := range ps.managed {
			live[t.Dir] = true
		}
	}

	adopted := 0
	for _, dir := range dirs {
		if live[dir] {
			continue
		}
		p.touched[dir] = true
		adopted++
	}
	if adopted > 0 {
		p.log.Info("adopted existing cpu reservations", "count", adopted)
	}
}

func keys(m map[string]cpuset.CPUSet) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// compile-time check that the plugin implements the NRI interfaces it relies
// on; a mismatch with the vendored NRI version then fails the build instead of
// silently unsubscribing the plugin from an event.
var (
	_ stub.ConfigureInterface           = (*Plugin)(nil)
	_ stub.SynchronizeInterface         = (*Plugin)(nil)
	_ stub.PostCreateContainerInterface = (*Plugin)(nil)
	_ stub.PostStartContainerInterface  = (*Plugin)(nil)
	_ stub.UpdateContainerInterface     = (*Plugin)(nil)
	_ stub.PostUpdateContainerInterface = (*Plugin)(nil)
	_ stub.StopContainerInterface       = (*Plugin)(nil)
	_ stub.RemoveContainerInterface     = (*Plugin)(nil)
	_ stub.StopPodInterface             = (*Plugin)(nil)
	_ stub.RemovePodInterface           = (*Plugin)(nil)
	_ stub.ShutdownInterface            = (*Plugin)(nil)
)
