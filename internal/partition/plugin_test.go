package partition

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
					Shares: api.UInt64(uint64(cpuCount(cpus)) * 1024),
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
	return f.newPluginWith(t, DefaultConfig())
}

func (f *fakeTree) newPluginWith(t *testing.T, cfg Config) *Plugin {
	t.Helper()
	cg := cgroup.NewManagerAt(f.root, false, nil)
	return New(cfg, cg, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
		if got := f.read(t, rel, cgroup.FileExclusive); got != "" {
			t.Errorf("%s exclusive = %q, want it left empty", tc.ctrID, got)
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
		shares      uint64
	}{
		{name: "not annotated", annotations: nil, qos: "Guaranteed"},
		{name: "member", annotations: map[string]string{AnnotationPod: "member"}, qos: "Guaranteed"},
		{name: "unknown value", annotations: map[string]string{AnnotationPod: "exclusive-ish"}, qos: "Guaranteed"},
		{name: "burstable pod", annotations: ann, qos: "Burstable"},
		{name: "besteffort pod", annotations: ann, qos: "BestEffort"},
		// 1.5 cpus is not an integer request, so the CPU Manager gave it no
		// exclusive cores.
		{name: "fractional cpu", annotations: ann, qos: "Guaranteed", quota: 150000},
		// The same 1.5 cpus with the quota dropped by the kubelet: only the
		// shares are left, and 1536 is not a whole number of cpus either.
		{name: "fractional cpu without quota", annotations: ann, qos: "Guaranteed", quota: -1, shares: 1536},
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
			if tc.shares != 0 {
				ctr.Linux.Resources.Cpu.Shares = api.UInt64(tc.shares)
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

// Since Kubernetes 1.33 the kubelet sends no cfs quota for containers with
// exclusive cpus (DisableCPUQuotaWithExclusiveCPUs); the request must then be
// recovered from the cpu shares.
func TestExclusiveContainerWithoutQuotaIsPartitioned(t *testing.T) {
	f := newFakeTree(t)
	p := f.newPlugin(t)

	ann := map[string]string{AnnotationPod: "isolated"}
	pod, ctr := f.addContainer("A", "pod", "app", "aaa", "20-25", ann, "Guaranteed")
	ctr.Linux.Resources.Cpu.Quota = api.Int64(-1)
	mustCreate(t, p, pod, ctr)

	if got := f.read(t, ctrScopeRel("A", "aaa"), cgroup.FilePartition); got != "isolated" {
		t.Errorf("partition = %q, want isolated", got)
	}
	if got := f.read(t, kubepods, cgroup.FileExclusive); got != "20-25" {
		t.Errorf("kubepods.slice exclusive = %q, want 20-25", got)
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

func TestPartitionModeAndReason(t *testing.T) {
	f := newFakeTree(t)
	p := f.newPlugin(t)

	for _, tc := range []struct {
		name        string
		podAnn      map[string]string // refers to Kubernetes pod annotations
		ctrAnn      map[string]string // refers to CRI container annotations (not set at k8s level)
		wantMode    string
		wantReason  string
		wantRefused bool
	}{
		{
			name:       "not annotated",
			wantMode:   "member",
			wantReason: "not annotated",
		},
		{
			name:       "explicit member",
			podAnn:     map[string]string{AnnotationPod: "member"},
			wantMode:   "member",
			wantReason: AnnotationPod + ` requests "member"`,
		},
		{
			name:        "typo",
			podAnn:      map[string]string{AnnotationPod: "isolatd"},
			wantMode:    "member",
			wantReason:  AnnotationPod + ` has unsupported value "isolatd" (want member, root or isolated)`,
			wantRefused: true,
		},
		{
			name:     "case and spaces are tolerated",
			podAnn:   map[string]string{AnnotationPod: " Isolated "},
			wantMode: "isolated",
		},
		{
			name:     "container annotation wins",
			podAnn:   map[string]string{AnnotationPod: "isolated", AnnotationCtrPrefix + "app": "root"},
			wantMode: "root",
		},
		{
			// Kubernetes cannot set CRI container annotations: they are ignored.
			name:       "only CRI container annotations",
			ctrAnn:     map[string]string{AnnotationPod: "isolated"},
			wantMode:   "member",
			wantReason: "not annotated",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := &api.PodSandbox{Annotations: tc.podAnn}
			ctr := &api.Container{Name: "app", Annotations: tc.ctrAnn}

			if mode, _, _ := partitionMode(pod, ctr, cgroup.PartitionMember); mode != tc.wantMode {
				t.Errorf("mode = %q, want %q", mode, tc.wantMode)
			}
			if tc.wantReason == "" {
				return
			}
			_, reason, refused, _ := p.evaluate(pod, ctr)
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
			if refused != tc.wantRefused {
				t.Errorf("refused = %v, want %v", refused, tc.wantRefused)
			}
		})
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
	if got := f.read(t, ctrScopeRel("A", "aaa"), cgroup.FileExclusive); got != "" {
		t.Errorf("exclusive = %q, want it left empty", got)
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

// containerd creates a container's cgroup only when it starts it, so the
// partition can only be made at PostStartContainer; PostCreateContainer just
// reserves the cpus on the ancestors.
func TestPartitionIsCreatedOnceTheCgroupExists(t *testing.T) {
	f := newFakeTree(t)
	p := f.newPlugin(t)
	ctx := context.Background()

	ann := map[string]string{AnnotationPod: "isolated"}
	pod, ctr := f.addContainer("A", "pod", "app", "aaa", "10-11", ann, "Guaranteed")
	scope := ctrScopeRel("A", "aaa")
	if err := os.RemoveAll(filepath.Join(f.root, scope)); err != nil {
		t.Fatal(err)
	}

	if err := p.PostCreateContainer(ctx, pod, ctr); err != nil {
		t.Fatal(err)
	}
	if got := f.read(t, kubepods, cgroup.FileExclusive); got != "10-11" {
		t.Errorf("kubepods.slice exclusive = %q, want 10-11 reserved ahead of the start", got)
	}
	if _, ok := p.applied["aaa"]; ok {
		t.Fatal("partition recorded as applied before the cgroup existed")
	}

	f.mkcgroup(scope, "10-11")
	if err := p.PostStartContainer(ctx, pod, ctr); err != nil {
		t.Fatal(err)
	}
	if got := f.read(t, scope, cgroup.FilePartition); got != "isolated" {
		t.Errorf("partition = %q, want isolated", got)
	}
}

// --partition-value-for-non-annotated-pod reaches the containers with
// exclusive cpus of pods that carry no cpuset-partition.nri.io annotation at
// all, and nothing else: in a pod with any such annotation, even one only for
// other containers, an uncovered container stays "member"; and out-of-scope
// containers are neither partitioned nor reported as refused.
func TestNonAnnotatedPodPartition(t *testing.T) {
	for _, tc := range []struct {
		name        string
		annotations map[string]string
		qos         string
		quota       int64
		wantMode    string
		wantSource  string
		wantReason  string
	}{
		{
			name:       "non-annotated pod gets the value",
			qos:        "Guaranteed",
			wantMode:   "isolated",
			wantSource: sourceNonAnnotatedPod,
		},
		{
			// The pod's author chose which containers get a partition.
			name:        "pod annotated only for another container",
			annotations: map[string]string{AnnotationCtrPrefix + "other": "isolated"},
			qos:         "Guaranteed",
			wantMode:    "member",
			wantReason:  "pod's " + AnnotationDomain + " annotations do not cover this container",
		},
		{
			// A mistyped key still makes the pod an annotated one.
			name:        "pod with an unrecognised annotation key",
			annotations: map[string]string{AnnotationDomain + "/containr.app": "isolated"},
			qos:         "Guaranteed",
			wantMode:    "member",
			wantReason:  "pod's " + AnnotationDomain + " annotations do not cover this container",
		},
		{
			name:        "explicit member",
			annotations: map[string]string{AnnotationPod: "member"},
			qos:         "Guaranteed",
			wantMode:    "member",
		},
		{
			// A mistyped opt-out must not turn into an isolated partition.
			name:        "typo in the value",
			annotations: map[string]string{AnnotationPod: "membr"},
			qos:         "Guaranteed",
			wantMode:    "member",
		},
		{
			name:        "annotation for the container",
			annotations: map[string]string{AnnotationCtrPrefix + "app": "root"},
			qos:         "Guaranteed",
			wantMode:    "root",
			wantSource:  AnnotationCtrPrefix + "app",
		},
		{
			name:     "non-annotated burstable pod is left alone",
			qos:      "Burstable",
			wantMode: "member",
		},
		{
			name:     "non-annotated fractional cpu is left alone",
			qos:      "Guaranteed",
			quota:    150000,
			wantMode: "member",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeTree(t)
			cfg := DefaultConfig()
			cfg.NonAnnotatedPodPartition = cgroup.PartitionIsolated
			p := f.newPluginWith(t, cfg)

			pod, ctr := f.addContainer("A", "pod", "app", "aaa", "10-11", tc.annotations, tc.qos)
			if tc.quota != 0 {
				ctr.Linux.Resources.Cpu.Quota = api.Int64(tc.quota)
			}

			target, reason, refused, err := p.evaluate(pod, ctr)
			if err != nil {
				t.Fatal(err)
			}
			// Out-of-scope containers reached only by the flag are not a user
			// request, so they must not be reported as refused.
			if target == nil && refused && tc.annotations == nil {
				t.Error("a container of a non-annotated pod was reported as refused")
			}
			if tc.wantReason != "" && reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}

			if err := p.PostStartContainer(context.Background(), pod, ctr); err != nil {
				t.Fatal(err)
			}

			applied, ok := p.applied["aaa"]
			if tc.wantMode == cgroup.PartitionMember {
				if ok {
					t.Fatalf("container partitioned as %q, want it left alone", applied.Mode)
				}
				if got := f.read(t, kubepods, cgroup.FileExclusive); got != "" {
					t.Errorf("kubepods.slice exclusive = %q, want no reservation", got)
				}
				return
			}
			if !ok {
				t.Fatalf("container not partitioned, want %q", tc.wantMode)
			}
			if applied.Mode != tc.wantMode || applied.ModeSource != tc.wantSource {
				t.Errorf("partition = %q from %q, want %q from %q",
					applied.Mode, applied.ModeSource, tc.wantMode, tc.wantSource)
			}
		})
	}
}

func TestConfigValidate(t *testing.T) {
	for _, value := range []string{"member", "root", "isolated"} {
		cfg := DefaultConfig()
		cfg.NonAnnotatedPodPartition = value
		if err := cfg.Validate(); err != nil {
			t.Errorf("partition value %q rejected: %v", value, err)
		}
	}
	// Values are matched exactly: unlike annotations, a flag is set once by
	// an operator, and a wrong one has to stop the plugin, not be tolerated.
	for _, value := range []string{"", "Isolated", "isolatd", "invalid"} {
		cfg := DefaultConfig()
		cfg.NonAnnotatedPodPartition = value
		if err := cfg.Validate(); err == nil {
			t.Errorf("partition value %q accepted, want an error", value)
		}
	}
}

func TestConfigureRejectsAnInvalidNonAnnotatedPodPartition(t *testing.T) {
	f := newFakeTree(t)
	p := f.newPlugin(t)
	ctx := context.Background()

	if _, err := p.Configure(ctx, `{"nonAnnotatedPodPartition": "isolatd"}`, "containerd", "v2"); err == nil {
		t.Fatal("an invalid nonAnnotatedPodPartition was accepted")
	}
	if got := p.cfg.NonAnnotatedPodPartition; got != cgroup.PartitionMember {
		t.Errorf("partition value = %q after a rejected configuration, want it unchanged", got)
	}

	if _, err := p.Configure(ctx, `{"nonAnnotatedPodPartition": "root"}`, "containerd", "v2"); err != nil {
		t.Fatal(err)
	}
	if got := p.cfg.NonAnnotatedPodPartition; got != cgroup.PartitionRoot {
		t.Errorf("partition value = %q, want root", got)
	}
}

// A previous run partitioned a non-annotated pod's container through
// --partition-value-for-non-annotated-pod=isolated, and the plugin restarts
// with the default "member": the partition must be released, and released
// before the ancestors' reservation it depends on is withdrawn.
func TestSynchronizeReleasesAPartitionNoContainerWants(t *testing.T) {
	f := newFakeTree(t)
	pod, ctr := f.addContainer("A", "pod", "app", "aaa", "10-11", nil, "Guaranteed")
	write(t, filepath.Join(f.root, kubepods), cgroup.FileExclusive, "10-11")
	write(t, filepath.Join(f.root, podSliceRel("A")), cgroup.FileExclusive, "10-11")
	write(t, filepath.Join(f.root, ctrScopeRel("A", "aaa")), cgroup.FilePartition, "isolated")

	var writes []string
	cg := cgroup.NewManagerAt(f.root, false, func(path, value string) {
		rel, _ := filepath.Rel(f.root, path)
		writes = append(writes, rel+"="+value)
	})
	p := New(DefaultConfig(), cg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if _, err := p.Synchronize(context.Background(), []*api.PodSandbox{pod}, []*api.Container{ctr}); err != nil {
		t.Fatal(err)
	}

	if got := f.read(t, ctrScopeRel("A", "aaa"), cgroup.FilePartition); got != "member" {
		t.Errorf("leaf partition = %q, want member", got)
	}
	for _, rel := range []string{kubepods, podSliceRel("A")} {
		if got := f.read(t, rel, cgroup.FileExclusive); got != "" {
			t.Errorf("%s exclusive = %q, want it cleared", rel, got)
		}
	}

	leaf := filepath.Join(ctrScopeRel("A", "aaa"), cgroup.FilePartition) + "=member"
	ancestor := filepath.Join(kubepods, cgroup.FileExclusive) + "="
	if indexOf(writes, leaf) < 0 || indexOf(writes, ancestor) < 0 || indexOf(writes, leaf) > indexOf(writes, ancestor) {
		t.Errorf("writes %v: want %q before %q", writes, leaf, ancestor)
	}
}

// The kernel may already have invalidated a leftover partition; it is
// released all the same.
func TestSynchronizeReleasesAnInvalidOrphanPartition(t *testing.T) {
	f := newFakeTree(t)
	pod, ctr := f.addContainer("A", "pod", "app", "aaa", "10-11", nil, "Guaranteed")
	write(t, filepath.Join(f.root, ctrScopeRel("A", "aaa")), cgroup.FilePartition,
		"isolated invalid (Invalid cpu list in cpuset.cpus.exclusive)")

	p := f.newPlugin(t)
	if _, err := p.Synchronize(context.Background(), []*api.PodSandbox{pod}, []*api.Container{ctr}); err != nil {
		t.Fatal(err)
	}
	if got := f.read(t, ctrScopeRel("A", "aaa"), cgroup.FilePartition); got != "member" {
		t.Errorf("leaf partition = %q, want member", got)
	}
}

// Releasing an orphan must not disturb a partition that is still wanted.
func TestSynchronizeKeepsWantedPartitionsNextToAnOrphan(t *testing.T) {
	f := newFakeTree(t)
	ann := map[string]string{AnnotationPod: "isolated"}
	podA, ctrA := f.addContainer("A", "wanted", "app", "aaa", "10-11", ann, "Guaranteed")
	podB, ctrB := f.addContainer("B", "orphan", "app", "bbb", "20-21", nil, "Guaranteed")

	write(t, filepath.Join(f.root, kubepods), cgroup.FileExclusive, "10-11,20-21")
	write(t, filepath.Join(f.root, podSliceRel("A")), cgroup.FileExclusive, "10-11")
	write(t, filepath.Join(f.root, podSliceRel("B")), cgroup.FileExclusive, "20-21")
	write(t, filepath.Join(f.root, ctrScopeRel("A", "aaa")), cgroup.FilePartition, "isolated")
	write(t, filepath.Join(f.root, ctrScopeRel("B", "bbb")), cgroup.FilePartition, "isolated")

	p := f.newPlugin(t)
	if _, err := p.Synchronize(context.Background(),
		[]*api.PodSandbox{podA, podB}, []*api.Container{ctrA, ctrB}); err != nil {
		t.Fatal(err)
	}

	if got := f.read(t, ctrScopeRel("A", "aaa"), cgroup.FilePartition); got != "isolated" {
		t.Errorf("wanted partition = %q, want isolated", got)
	}
	if got := f.read(t, ctrScopeRel("B", "bbb"), cgroup.FilePartition); got != "member" {
		t.Errorf("orphan partition = %q, want member", got)
	}
	if got := f.read(t, kubepods, cgroup.FileExclusive); got != "10-11" {
		t.Errorf("kubepods.slice exclusive = %q, want only the wanted pod's 10-11", got)
	}
	if got := f.read(t, podSliceRel("B"), cgroup.FileExclusive); got != "" {
		t.Errorf("orphan pod slice exclusive = %q, want it cleared", got)
	}
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

// The QoS class is read from the part of the cgroup path below the cgroup
// root: a root whose own path contains "burstable" (here, as a test's
// temporary directory could) must not make a Guaranteed pod look Burstable.
func TestQoSIsReadBelowTheCgroupRoot(t *testing.T) {
	f := &fakeTree{root: filepath.Join(t.TempDir(), "burstable-lab"), t: t}
	f.mkcgroup("kubepods.slice", "0-39")
	p := f.newPlugin(t)

	ann := map[string]string{AnnotationPod: "isolated"}
	pod, ctr := f.addContainer("A", "pod", "app", "aaa", "10-11", ann, "Guaranteed")
	if err := p.PostStartContainer(context.Background(), pod, ctr); err != nil {
		t.Fatal(err)
	}

	if got := f.read(t, ctrScopeRel("A", "aaa"), cgroup.FilePartition); got != "isolated" {
		t.Errorf("partition = %q, want isolated", got)
	}
}

// A missing pod cgroup tree (typically kubelet's cgroupfs driver with the
// default --kubepods-cgroup) must be reported at WARN level, not hidden in a
// debug line, as the startup cleanup silently does nothing without it.
func TestCheckPodTreeWarnsWhenTheTreeIsMissing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		kubepods string
		wantOK   bool
	}{
		{name: "tree present", kubepods: "kubepods.slice", wantOK: true},
		{name: "cgroupfs node with the default name", kubepods: "kubepods.slice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.wantOK {
				if err := os.MkdirAll(filepath.Join(root, tc.kubepods), 0o755); err != nil {
					t.Fatal(err)
				}
			} else if err := os.MkdirAll(filepath.Join(root, "kubepods"), 0o755); err != nil {
				t.Fatal(err)
			}

			var logs bytes.Buffer
			cfg := DefaultConfig()
			cfg.KubepodsCgroup = tc.kubepods
			p := New(cfg, cgroup.NewManagerAt(root, false, nil),
				slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

			if got := p.CheckPodTree(); got != tc.wantOK {
				t.Errorf("CheckPodTree() = %v, want %v", got, tc.wantOK)
			}
			warned := strings.Contains(logs.String(), "level=WARN")
			if warned == tc.wantOK {
				t.Errorf("warning logged = %v, want %v; log: %s", warned, !tc.wantOK, logs.String())
			}
			if !tc.wantOK && !strings.Contains(logs.String(), "--kubepods-cgroup=kubepods") {
				t.Errorf("warning does not say how to fix it; log: %s", logs.String())
			}
		})
	}
}
