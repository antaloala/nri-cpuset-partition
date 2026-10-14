package partition

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/containerd/nri/pkg/api"

	"github.com/antaloala/nri-cpuset-partition/internal/cgroup"
	"github.com/antaloala/nri-cpuset-partition/internal/cpuset"
)

// The tests drive the plugin against a cgroup tree made of plain files. The
// kernel's own validation is obviously not there, but the ordering, the union
// of concurrent reservations and the teardown logic are all exercised for
// real, including the writes the plugin would issue on a node.

type fakeTree struct {
	root string
	t    *testing.T
}

func newFakeTree(t *testing.T) *fakeTree {
	t.Helper()
	f := &fakeTree{root: t.TempDir(), t: t}
	f.mkcgroup("kubepods.slice", "0-39")
	return f
}

// mkcgroup creates a cgroup directory below the root with the cpuset files a
// cgroup v2 cpuset controller would expose.
func (f *fakeTree) mkcgroup(rel, cpus string) string {
	f.t.Helper()
	dir := filepath.Join(f.root, rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	for file, value := range map[string]string{
		cgroup.FileCPUs:            cpus + "\n",
		cgroup.FileCPUsEffective:   cpus + "\n",
		cgroup.FileExclusive:       "\n",
		cgroup.FileExclusiveEffect: "\n",
		cgroup.FilePartition:       "member\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(value), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	return dir
}

// addContainer creates the pod slice and container scope cgroups for a
// container pinned to cpus, and returns the NRI objects describing them.
func (f *fakeTree) addContainer(podUID, podName, ctrName, ctrID, cpus string, annotations map[string]string, qos string) (*api.PodSandbox, *api.Container) {
	f.t.Helper()

	podSlice := "kubepods-pod" + podUID + ".slice"
	rel := filepath.Join("kubepods.slice", podSlice)
	switch qos {
	case "Burstable":
		podSlice = "kubepods-burstable-pod" + podUID + ".slice"
		rel = filepath.Join("kubepods.slice", "kubepods-burstable.slice", podSlice)
		f.mkcgroup(filepath.Join("kubepods.slice", "kubepods-burstable.slice"), "0-39")
	case "BestEffort":
		podSlice = "kubepods-besteffort-pod" + podUID + ".slice"
		rel = filepath.Join("kubepods.slice", "kubepods-besteffort.slice", podSlice)
		f.mkcgroup(filepath.Join("kubepods.slice", "kubepods-besteffort.slice"), "0-39")
	}
	f.mkcgroup(rel, "0-39")
	f.mkcgroup(filepath.Join(rel, "cri-containerd-"+ctrID+".scope"), cpus)

	pod := &api.PodSandbox{
		Id:          "sandbox-" + podUID,
		Uid:         podUID,
		Name:        podName,
		Namespace:   "default",
		Annotations: annotations,
		Linux:       &api.LinuxPodSandbox{CgroupParent: podSlice},
	}
	ctr := &api.Container{
		Id:           ctrID,
		PodSandboxId: pod.Id,
		Name:         ctrName,
		State:        api.ContainerState_CONTAINER_RUNNING,
		Linux: &api.LinuxContainer{
			CgroupsPath: podSlice + ":cri-containerd:" + ctrID,
			Resources: &api.LinuxResources{
				Cpu: &api.LinuxCPU{
					Cpus:   cpus,
					Quota:  api.Int64(int64(cpuCount(cpus)) * 100000),
					Period: api.UInt64(uint64(100000)),
				},
			},
		},
	}

	return pod, ctr
}

func cpuCount(cpus string) int {
	return cpuset.MustParse(cpus).Size()
}

func (f *fakeTree) read(t *testing.T, rel, file string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.root, rel, file))
	if err != nil {
		t.Fatalf("read %s/%s: %v", rel, file, err)
	}
	return trim(string(b))
}

func (f *fakeTree) newPlugin(t *testing.T) *Plugin {
	t.Helper()
	cg := cgroup.NewManagerAt(f.root, false, nil)
	return New(DefaultConfig(), cg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func podSliceRel(uid string) string {
	return filepath.Join("kubepods.slice", "kubepods-pod"+uid+".slice")
}

func ctrScopeRel(uid, ctrID string) string {
	return filepath.Join(podSliceRel(uid), "cri-containerd-"+ctrID+".scope")
}

const kubepods = "kubepods.slice"

func TestTwoContainersOfOnePodShareTheReservation(t *testing.T) {
	f := newFakeTree(t)
	p := f.newPlugin(t)
	ctx := context.Background()

	ann := map[string]string{AnnotationPod: "isolated"}
	pod, ctrA := f.addContainer("A", "dpdk", "app-a", "aaa", "20-21", ann, "Guaranteed")
	_, ctrB := f.addContainer("A", "dpdk", "app-b", "bbb", "22-23", ann, "Guaranteed")

	if err := p.PostCreateContainer(ctx, pod, ctrA); err != nil {
		t.Fatal(err)
	}
	if err := p.PostCreateContainer(ctx, pod, ctrB); err != nil {
		t.Fatal(err)
	}

	// Both ancestors must hold the union, and each leaf only its own cpus.
	if got := f.read(t, kubepods, cgroup.FileExclusive); got != "20-23" {
		t.Errorf("kubepods.slice exclusive = %q, want 20-23", got)
	}
	if got := f.read(t, podSliceRel("A"), cgroup.FileExclusive); got != "20-23" {
		t.Errorf("pod slice exclusive = %q, want 20-23", got)
	}
	for _, tc := range []struct{ ctrID, cpus string }{{"aaa", "20-21"}, {"bbb", "22-23"}} {
		rel := ctrScopeRel("A", tc.ctrID)
		if got := f.read(t, rel, cgroup.FileExclusive); got != tc.cpus {
			t.Errorf("%s exclusive = %q, want %q", tc.ctrID, got, tc.cpus)
		}
		if got := f.read(t, rel, cgroup.FilePartition); got != "isolated" {
			t.Errorf("%s partition = %q, want isolated", tc.ctrID, got)
		}
	}
}

func TestReservationIsHeldUntilThePodIsFullyStopped(t *testing.T) {
	f := newFakeTree(t)
	p := f.newPlugin(t)
	ctx := context.Background()

	ann := map[string]string{AnnotationPod: "isolated"}
	pod, ctrA := f.addContainer("A", "dpdk", "app-a", "aaa", "20-21", ann, "Guaranteed")
	_, ctrB := f.addContainer("A", "dpdk", "app-b", "bbb", "22-23", ann, "Guaranteed")
	mustCreate(t, p, pod, ctrA, ctrB)

	// First container out: its partition goes away, the reservation does not.
	if _, err := p.StopContainer(ctx, pod, ctrA); err != nil {
		t.Fatal(err)
	}
	if got := f.read(t, ctrScopeRel("A", "aaa"), cgroup.FilePartition); got != "member" {
		t.Errorf("stopped container partition = %q, want member", got)
	}
	if got := f.read(t, kubepods, cgroup.FileExclusive); got != "20-23" {
		t.Errorf("kubepods.slice exclusive = %q, want the pod to keep 20-23", got)
	}
	if got := f.read(t, ctrScopeRel("A", "bbb"), cgroup.FilePartition); got != "isolated" {
		t.Errorf("surviving container partition = %q, want isolated", got)
	}

	// Last container out: everything is released.
	if _, err := p.StopContainer(ctx, pod, ctrB); err != nil {
		t.Fatal(err)
	}
	if got := f.read(t, kubepods, cgroup.FileExclusive); got != "" {
		t.Errorf("kubepods.slice exclusive = %q, want it cleared", got)
	}
	if got := f.read(t, podSliceRel("A"), cgroup.FileExclusive); got != "" {
		t.Errorf("pod slice exclusive = %q, want it cleared", got)
	}
	if got := f.read(t, ctrScopeRel("A", "bbb"), cgroup.FilePartition); got != "member" {
		t.Errorf("partition = %q, want member", got)
	}
}

func TestConcurrentPodsDoNotOverwriteEachOther(t *testing.T) {
	f := newFakeTree(t)
	p := f.newPlugin(t)
	ctx := context.Background()

	ann := map[string]string{AnnotationPod: "isolated"}
	podA, ctrA := f.addContainer("A", "dpdk-a", "app", "aaa", "20-21", ann, "Guaranteed")
	podB, ctrB := f.addContainer("B", "dpdk-b", "app", "bbb", "30-31", ann, "Guaranteed")

	if err := p.PostCreateContainer(ctx, podA, ctrA); err != nil {
		t.Fatal(err)
	}
	if err := p.PostCreateContainer(ctx, podB, ctrB); err != nil {
		t.Fatal(err)
	}

	// The shared ancestor holds both pods' cpus, each pod slice only its own.
	if got := f.read(t, kubepods, cgroup.FileExclusive); got != "20-21,30-31" {
		t.Errorf("kubepods.slice exclusive = %q, want 20-21,30-31", got)
	}
	if got := f.read(t, podSliceRel("A"), cgroup.FileExclusive); got != "20-21" {
		t.Errorf("pod A slice exclusive = %q, want 20-21", got)
	}
	if got := f.read(t, podSliceRel("B"), cgroup.FileExclusive); got != "30-31" {
		t.Errorf("pod B slice exclusive = %q, want 30-31", got)
	}

	// One pod leaving must not take the other pod's reservation with it.
	if err := p.RemovePodSandbox(ctx, podA); err != nil {
		t.Fatal(err)
	}
	if got := f.read(t, kubepods, cgroup.FileExclusive); got != "30-31" {
		t.Errorf("kubepods.slice exclusive = %q, want 30-31", got)
	}
	if got := f.read(t, ctrScopeRel("B", "bbb"), cgroup.FilePartition); got != "isolated" {
		t.Errorf("surviving pod partition = %q, want isolated", got)
	}
}

func TestOutOfScopeContainersAreIgnored(t *testing.T) {
	ann := map[string]string{AnnotationPod: "isolated"}

	for _, tc := range []struct {
		name        string
		annotations map[string]string
		qos         string
		cpus        string
		quota       int64
	}{
		{name: "not annotated", annotations: nil, qos: "Guaranteed"},
		{name: "member", annotations: map[string]string{AnnotationPod: "member"}, qos: "Guaranteed"},
		{name: "unknown value", annotations: map[string]string{AnnotationPod: "exclusive-ish"}, qos: "Guaranteed"},
		{name: "burstable pod", annotations: ann, qos: "Burstable"},
		{name: "besteffort pod", annotations: ann, qos: "BestEffort"},
		// 1.5 cpus is not an integer request, so the CPU Manager gave it no
		// exclusive cores.
		{name: "fractional cpu", annotations: ann, qos: "Guaranteed", quota: 150000},
		// One cpu requested but the whole isolated range in the cpuset: a
		// shared pool container, nothing exclusive to carve out.
		{name: "shared pool", annotations: ann, qos: "Guaranteed", cpus: "20-39", quota: 100000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeTree(t)
			p := f.newPlugin(t)

			cpus := tc.cpus
			if cpus == "" {
				cpus = "20-21"
			}
			pod, ctr := f.addContainer("A", "pod", "app", "aaa", cpus, tc.annotations, tc.qos)
			if tc.quota != 0 {
				ctr.Linux.Resources.Cpu.Quota = api.Int64(tc.quota)
			}

			if err := p.PostCreateContainer(context.Background(), pod, ctr); err != nil {
				t.Fatal(err)
			}
			if got := f.read(t, kubepods, cgroup.FileExclusive); got != "" {
				t.Errorf("kubepods.slice exclusive = %q, want no reservation", got)
			}
			if got := len(p.applied); got != 0 {
				t.Errorf("%d partitions applied, want 0", got)
			}
		})
	}
}

func TestContainerAnnotationOverridesThePodOne(t *testing.T) {
	f := newFakeTree(t)
	p := f.newPlugin(t)
	ctx := context.Background()

	ann := map[string]string{
		AnnotationPod:                 "isolated",
		AnnotationCtrPrefix + "app-b": "root",
		AnnotationCtrPrefix + "app-c": "member",
	}
	pod, ctrA := f.addContainer("A", "pod", "app-a", "aaa", "20-21", ann, "Guaranteed")
	_, ctrB := f.addContainer("A", "pod", "app-b", "bbb", "22-23", ann, "Guaranteed")
	_, ctrC := f.addContainer("A", "pod", "app-c", "ccc", "24-25", ann, "Guaranteed")
	_ = ctx
	mustCreate(t, p, pod, ctrA, ctrB, ctrC)

	for _, tc := range []struct{ ctrID, want string }{
		{"aaa", "isolated"}, // from the pod annotation
		{"bbb", "root"},     // overridden for this container
		{"ccc", "member"},   // opted out for this container
	} {
		if got := f.read(t, ctrScopeRel("A", tc.ctrID), cgroup.FilePartition); got != tc.want {
			t.Errorf("%s partition = %q, want %q", tc.ctrID, got, tc.want)
		}
	}
	// The opted-out container's cpus are not reserved anywhere.
	if got := f.read(t, kubepods, cgroup.FileExclusive); got != "20-23" {
		t.Errorf("kubepods.slice exclusive = %q, want 20-23", got)
	}
}

func TestResyncRepairsAPartitionKubeletKnockedOut(t *testing.T) {
	f := newFakeTree(t)
	p := f.newPlugin(t)
	ctx := context.Background()

	ann := map[string]string{AnnotationPod: "isolated"}
	pod, ctr := f.addContainer("A", "pod", "app", "aaa", "20-21", ann, "Guaranteed")
	mustCreate(t, p, pod, ctr)

	// What a CPU Manager reconcile does to us: the cgroup is rewritten and
	// the partition falls back to an invalid state.
	scope := filepath.Join(f.root, ctrScopeRel("A", "aaa"))
	write(t, scope, cgroup.FilePartition, "isolated invalid (Invalid cpu list in cpuset.cpus.exclusive)")
	write(t, scope, cgroup.FileExclusive, "")

	p.Resync(ctx)

	if got := f.read(t, ctrScopeRel("A", "aaa"), cgroup.FilePartition); got != "isolated" {
		t.Errorf("partition = %q, want it repaired to isolated", got)
	}
	if got := f.read(t, ctrScopeRel("A", "aaa"), cgroup.FileExclusive); got != "20-21" {
		t.Errorf("exclusive = %q, want 20-21", got)
	}
}

func TestResyncFollowsANewCPUAssignment(t *testing.T) {
	f := newFakeTree(t)
	p := f.newPlugin(t)
	ctx := context.Background()

	ann := map[string]string{AnnotationPod: "isolated"}
	pod, ctr := f.addContainer("A", "pod", "app", "aaa", "20-21", ann, "Guaranteed")
	mustCreate(t, p, pod, ctr)

	// The CPU Manager moved the container to another pair of cores.
	scope := filepath.Join(f.root, ctrScopeRel("A", "aaa"))
	write(t, scope, cgroup.FileCPUs, "30-31")

	p.Resync(ctx)

	if got := f.read(t, kubepods, cgroup.FileExclusive); got != "30-31" {
		t.Errorf("kubepods.slice exclusive = %q, want it to follow to 30-31", got)
	}
	if got := f.read(t, ctrScopeRel("A", "aaa"), cgroup.FileExclusive); got != "30-31" {
		t.Errorf("leaf exclusive = %q, want 30-31", got)
	}
	if got := f.read(t, ctrScopeRel("A", "aaa"), cgroup.FilePartition); got != "isolated" {
		t.Errorf("partition = %q, want isolated", got)
	}
}

func TestSynchronizeAdoptsRunningContainersAndDropsStaleReservations(t *testing.T) {
	f := newFakeTree(t)
	p := f.newPlugin(t)
	ctx := context.Background()

	ann := map[string]string{AnnotationPod: "isolated"}
	pod, ctr := f.addContainer("A", "pod", "app", "aaa", "20-21", ann, "Guaranteed")

	// A reservation a previous run of the plugin left behind for a pod that
	// no longer exists.
	stale := f.mkcgroup(filepath.Join(kubepods, "kubepods-podGONE.slice"), "0-39")
	write(t, stale, cgroup.FileExclusive, "36-37")
	write(t, filepath.Join(f.root, kubepods), cgroup.FileExclusive, "36-37")

	if _, err := p.Synchronize(ctx, []*api.PodSandbox{pod}, []*api.Container{ctr}); err != nil {
		t.Fatal(err)
	}

	if got := f.read(t, kubepods, cgroup.FileExclusive); got != "20-21" {
		t.Errorf("kubepods.slice exclusive = %q, want only the live pod's 20-21", got)
	}
	if got := f.read(t, filepath.Join(kubepods, "kubepods-podGONE.slice"), cgroup.FileExclusive); got != "" {
		t.Errorf("stale reservation = %q, want it cleared", got)
	}
	if got := f.read(t, ctrScopeRel("A", "aaa"), cgroup.FilePartition); got != "isolated" {
		t.Errorf("adopted container partition = %q, want isolated", got)
	}
}

func TestUpdateDismantlesAndRebuildsThePartition(t *testing.T) {
	f := newFakeTree(t)
	p := f.newPlugin(t)
	ctx := context.Background()

	ann := map[string]string{AnnotationPod: "isolated"}
	pod, ctr := f.addContainer("A", "pod", "app", "aaa", "20-21", ann, "Guaranteed")
	mustCreate(t, p, pod, ctr)

	if _, err := p.UpdateContainer(ctx, pod, ctr, ctr.Linux.Resources); err != nil {
		t.Fatal(err)
	}
	if got := f.read(t, ctrScopeRel("A", "aaa"), cgroup.FilePartition); got != "member" {
		t.Errorf("partition during the update = %q, want member", got)
	}

	// The runtime applies the new cpuset, then PostUpdateContainer runs.
	write(t, filepath.Join(f.root, ctrScopeRel("A", "aaa")), cgroup.FileCPUs, "24-27")
	ctr.Linux.Resources.Cpu.Cpus = "24-27"
	ctr.Linux.Resources.Cpu.Quota = api.Int64(int64(400000))
	if err := p.PostUpdateContainer(ctx, pod, ctr); err != nil {
		t.Fatal(err)
	}

	if got := f.read(t, ctrScopeRel("A", "aaa"), cgroup.FilePartition); got != "isolated" {
		t.Errorf("partition after the update = %q, want isolated", got)
	}
	if got := f.read(t, kubepods, cgroup.FileExclusive); got != "24-27" {
		t.Errorf("kubepods.slice exclusive = %q, want 24-27", got)
	}
}

func TestConcurrentEventsAreSerialised(t *testing.T) {
	f := newFakeTree(t)
	p := f.newPlugin(t)
	ctx := context.Background()

	ann := map[string]string{AnnotationPod: "isolated"}
	type ev struct {
		pod *api.PodSandbox
		ctr *api.Container
	}
	var events []ev
	for _, spec := range []struct{ uid, ctrID, cpus string }{
		{"A", "aaa", "20-21"}, {"B", "bbb", "22-23"}, {"C", "ccc", "24-25"},
		{"D", "ddd", "26-27"}, {"E", "eee", "28-29"}, {"F", "fff", "30-31"},
	} {
		pod, ctr := f.addContainer(spec.uid, "pod-"+spec.uid, "app", spec.ctrID, spec.cpus, ann, "Guaranteed")
		events = append(events, ev{pod, ctr})
	}

	done := make(chan struct{})
	for _, e := range events {
		go func(e ev) {
			defer func() { done <- struct{}{} }()
			if err := p.PostCreateContainer(ctx, e.pod, e.ctr); err != nil {
				t.Error(err)
			}
		}(e)
	}
	for range events {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for the container events")
		}
	}

	// No update may have dropped another one's cpus.
	if got := f.read(t, kubepods, cgroup.FileExclusive); got != "20-31" {
		t.Errorf("kubepods.slice exclusive = %q, want 20-31", got)
	}
}

//
// helpers
//

func mustCreate(t *testing.T, p *Plugin, pod *api.PodSandbox, ctrs ...*api.Container) {
	t.Helper()
	for _, ctr := range ctrs {
		if err := p.PostCreateContainer(context.Background(), pod, ctr); err != nil {
			t.Fatal(err)
		}
	}
}

func write(t *testing.T, dir, file, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(value+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func trim(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}

func TestSynchronizeDoesNotDisturbAnAlreadyIsolatedContainer(t *testing.T) {
	f := newFakeTree(t)

	ann := map[string]string{AnnotationPod: "isolated"}
	pod, ctr := f.addContainer("A", "pod", "app", "aaa", "20-21", ann, "Guaranteed")

	// The state a previous run of the plugin left behind: the reservation is
	// threaded down and the leaf is already an isolated partition.
	write(t, filepath.Join(f.root, kubepods), cgroup.FileExclusive, "20-21")
	write(t, filepath.Join(f.root, podSliceRel("A")), cgroup.FileExclusive, "20-21")
	scope := filepath.Join(f.root, ctrScopeRel("A", "aaa"))
	write(t, scope, cgroup.FileExclusive, "20-21")
	write(t, scope, cgroup.FilePartition, "isolated")

	var writes []string
	cg := cgroup.NewManagerAt(f.root, false, func(path, value string) {
		writes = append(writes, path+"="+value)
	})
	p := New(DefaultConfig(), cg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if _, err := p.Synchronize(context.Background(), []*api.PodSandbox{pod}, []*api.Container{ctr}); err != nil {
		t.Fatal(err)
	}

	// Taking the container back over must be a no-op, not a tear down and
	// rebuild: a cleared cpuset.cpus.exclusive would invalidate the live
	// partition for as long as it takes to put it back.
	if len(writes) != 0 {
		t.Errorf("adopting a running partition wrote %v, want no writes", writes)
	}
	if got := f.read(t, ctrScopeRel("A", "aaa"), cgroup.FilePartition); got != "isolated" {
		t.Errorf("partition = %q, want isolated", got)
	}
}

func TestResyncReleasesAPodWhoseCgroupVanished(t *testing.T) {
	f := newFakeTree(t)
	p := f.newPlugin(t)
	ctx := context.Background()

	ann := map[string]string{AnnotationPod: "isolated"}
	podA, ctrA := f.addContainer("A", "pod-a", "app", "aaa", "20-21", ann, "Guaranteed")
	podB, ctrB := f.addContainer("B", "pod-b", "app", "bbb", "30-31", ann, "Guaranteed")
	mustCreate(t, p, podA, ctrA)
	mustCreate(t, p, podB, ctrB)

	// The pod went away without the plugin ever seeing a stop event.
	if err := os.RemoveAll(filepath.Join(f.root, podSliceRel("A"))); err != nil {
		t.Fatal(err)
	}

	p.Resync(ctx)

	if got := f.read(t, kubepods, cgroup.FileExclusive); got != "30-31" {
		t.Errorf("kubepods.slice exclusive = %q, want only the surviving pod's 30-31", got)
	}
	if _, ok := p.applied["aaa"]; ok {
		t.Error("the vanished container is still recorded as applied")
	}
	if got := f.read(t, ctrScopeRel("B", "bbb"), cgroup.FilePartition); got != "isolated" {
		t.Errorf("surviving partition = %q, want isolated", got)
	}
}
