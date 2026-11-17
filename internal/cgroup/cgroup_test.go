package cgroup

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/antaloala/nri-cpuset-partition/internal/cpuset"
)

func testManager(root string) *Manager {
	return &Manager{root: root, onWrite: func(string, string) {}}
}

func TestExpandSlice(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"kubepods.slice", "kubepods.slice"},
		{
			"kubepods-pod9b4a1c2e_0f3d_4a1b_9c7e_8d2f6a5b4c3d.slice",
			"kubepods.slice/kubepods-pod9b4a1c2e_0f3d_4a1b_9c7e_8d2f6a5b4c3d.slice",
		},
		{
			"kubepods-burstable-podabc.slice",
			"kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podabc.slice",
		},
		{"-.slice", ""},
	} {
		got, err := ExpandSlice(tc.in)
		if err != nil {
			t.Fatalf("ExpandSlice(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("ExpandSlice(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestContainerDir(t *testing.T) {
	m := testManager("/sys/fs/cgroup")

	for _, tc := range []struct{ in, want string }{
		{
			"kubepods-pod1234_5678.slice:cri-containerd:abcdef",
			"/sys/fs/cgroup/kubepods.slice/kubepods-pod1234_5678.slice/cri-containerd-abcdef.scope",
		},
		{
			"kubepods-burstable-pod1234.slice:cri-containerd:abc",
			"/sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1234.slice/cri-containerd-abc.scope",
		},
		{
			"/kubepods/pod1234/abcdef",
			"/sys/fs/cgroup/kubepods/pod1234/abcdef",
		},
	} {
		got, err := m.ContainerDir(tc.in)
		if err != nil {
			t.Fatalf("ContainerDir(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("ContainerDir(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	if _, err := m.ContainerDir("a:b"); err == nil {
		t.Error("a malformed systemd cgroups path should be rejected")
	}
	if _, err := m.ContainerDir(""); err == nil {
		t.Error("an empty cgroups path should be rejected")
	}
}

func TestAncestors(t *testing.T) {
	m := testManager("/sys/fs/cgroup")

	got, err := m.Ancestors("/sys/fs/cgroup/kubepods.slice/kubepods-podX.slice/cri-containerd-abc.scope")
	if err != nil {
		t.Fatalf("Ancestors: %v", err)
	}
	want := []string{
		"/sys/fs/cgroup/kubepods.slice",
		"/sys/fs/cgroup/kubepods.slice/kubepods-podX.slice",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Ancestors = %v, want %v", got, want)
	}

	// The cgroup root itself is never an ancestor to reserve cpus on.
	if _, err := m.Ancestors("/sys/fs/cgroup"); err == nil {
		t.Error("the cgroup root should be rejected")
	}
	if _, err := m.Ancestors("/elsewhere/kubepods.slice/x.scope"); err == nil {
		t.Error("a path outside the cgroup root should be rejected")
	}
}

func TestSortTopDown(t *testing.T) {
	in := []string{
		"/sys/fs/cgroup/kubepods.slice/kubepods-podB.slice",
		"/sys/fs/cgroup/kubepods.slice",
		"/sys/fs/cgroup/kubepods.slice/kubepods-podA.slice",
	}
	want := []string{
		"/sys/fs/cgroup/kubepods.slice",
		"/sys/fs/cgroup/kubepods.slice/kubepods-podA.slice",
		"/sys/fs/cgroup/kubepods.slice/kubepods-podB.slice",
	}
	if got := SortTopDown(in); !reflect.DeepEqual(got, want) {
		t.Errorf("SortTopDown = %v, want %v", got, want)
	}
}

func TestQoSClass(t *testing.T) {
	for _, tc := range []struct{ dir, want string }{
		{"/sys/fs/cgroup/kubepods.slice/kubepods-podX.slice", "Guaranteed"},
		{"/sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podX.slice", "Burstable"},
		{"/sys/fs/cgroup/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-podX.slice", "BestEffort"},
		{"/sys/fs/cgroup/kubepods/burstable/podX", "Burstable"},
	} {
		if got := QoSClass(tc.dir); got != tc.want {
			t.Errorf("QoSClass(%q) = %q, want %q", tc.dir, got, tc.want)
		}
	}
}

func TestWriteSkipsUnchangedValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileExclusive)
	if err := os.WriteFile(path, []byte("20-23\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var writes []string
	m := &Manager{root: dir, onWrite: func(_, value string) { writes = append(writes, value) }}

	// Same set, different spelling: no write.
	if err := m.WriteCPUSet(dir, FileExclusive, cpuset.MustParse("20,21,22,23")); err != nil {
		t.Fatalf("WriteCPUSet: %v", err)
	}
	if len(writes) != 0 {
		t.Errorf("an unchanged value should not be written, got %v", writes)
	}

	if err := m.WriteCPUSet(dir, FileExclusive, cpuset.MustParse("20-25")); err != nil {
		t.Fatalf("WriteCPUSet: %v", err)
	}
	if got, err := m.ReadCPUSet(dir, FileExclusive); err != nil || got.String() != "20-25" {
		t.Errorf("ReadCPUSet = %q, %v, want 20-25", got, err)
	}

	// Clearing a reservation has to write a newline, an empty buffer would
	// leave the file untouched.
	if err := m.WriteCPUSet(dir, FileExclusive, cpuset.CPUSet{}); err != nil {
		t.Fatalf("WriteCPUSet: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "\n" {
		t.Errorf("cleared file holds %q, want a newline", string(b))
	}
}

func TestDryRunDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FilePartition)
	if err := os.WriteFile(path, []byte("member\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var seen string
	m := &Manager{root: dir, dryRun: true, onWrite: func(_, value string) { seen = value }}

	if err := m.WriteString(dir, FilePartition, PartitionIsolated); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	if seen != PartitionIsolated {
		t.Errorf("dry run should report the write, got %q", seen)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "member\n" {
		t.Errorf("dry run wrote to the file: %q", string(b))
	}
}

func TestReadPartition(t *testing.T) {
	dir := t.TempDir()
	m := testManager(dir)

	for _, tc := range []struct {
		raw     string
		value   string
		invalid bool
	}{
		{"member\n", "member", false},
		{"isolated\n", "isolated", false},
		{"root\n", "root", false},
		{"isolated invalid (Invalid cpu list in cpuset.cpus.exclusive)\n", "isolated", true},
	} {
		if err := os.WriteFile(filepath.Join(dir, FilePartition), []byte(tc.raw), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := m.ReadPartition(dir)
		if err != nil {
			t.Fatalf("ReadPartition: %v", err)
		}
		if got.Value != tc.value || got.Invalid != tc.invalid {
			t.Errorf("ReadPartition(%q) = %+v, want value %q invalid %v", tc.raw, got, tc.value, tc.invalid)
		}
	}
}

func TestFindExclusiveReservations(t *testing.T) {
	root := t.TempDir()
	kubepods := filepath.Join(root, "kubepods.slice")
	pod := filepath.Join(kubepods, "kubepods-podX.slice")
	ctr := filepath.Join(pod, "cri-containerd-abc.scope")
	if err := os.MkdirAll(ctr, 0o755); err != nil {
		t.Fatal(err)
	}
	for dir, value := range map[string]string{
		kubepods: "20-23\n",
		pod:      "20-23\n",
		ctr:      "\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, FileExclusive), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := testManager(root).FindExclusiveReservations(kubepods)
	if err != nil {
		t.Fatalf("FindExclusiveReservations: %v", err)
	}
	want := []string{kubepods, pod}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FindExclusiveReservations = %v, want %v", got, want)
	}
}

func TestReadStringTellsAGoneCgroupFromAnOldKernel(t *testing.T) {
	root := t.TempDir()
	m := testManager(root)

	// The cgroup is still there but the file is not: an old kernel.
	if _, err := m.ReadString(root, FileExclusive); !errors.Is(err, ErrNoExclusiveFile) {
		t.Errorf("missing file in an existing cgroup: got %v, want ErrNoExclusiveFile", err)
	}

	// The whole cgroup is gone: a container scope removed as it exited.
	gone := filepath.Join(root, "cri-containerd-abc.scope")
	if _, err := m.ReadString(gone, FileExclusive); !errors.Is(err, ErrCgroupGone) {
		t.Errorf("removed cgroup: got %v, want ErrCgroupGone", err)
	}
	if _, err := m.ReadString(gone, FilePartition); !errors.Is(err, ErrCgroupGone) {
		t.Errorf("removed cgroup, other file: got %v, want ErrCgroupGone", err)
	}
}

// The scope can vanish between WriteString's read and its write. onWrite runs
// exactly in that window, so removing the cgroup from it reproduces the race.
func TestWriteStringReportsACgroupRemovedMidWrite(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "cri-containerd-abc.scope")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, FilePartition), []byte("isolated\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewManagerAt(root, false, func(string, string) {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	})

	if err := m.WriteString(dir, FilePartition, PartitionMember); !errors.Is(err, ErrCgroupGone) {
		t.Errorf("got %v, want ErrCgroupGone", err)
	}
}

func TestFindPartitionRoots(t *testing.T) {
	root := t.TempDir()
	kubepods := filepath.Join(root, "kubepods.slice")
	pod := filepath.Join(kubepods, "kubepods-podX.slice")

	for dir, value := range map[string]string{
		kubepods: "root\n", // the subtree itself: never reported
		pod:      "member\n",
		filepath.Join(pod, "cri-containerd-isolated.scope"): "isolated\n",
		filepath.Join(pod, "cri-containerd-root.scope"):     "root\n",
		filepath.Join(pod, "cri-containerd-invalid.scope"):  "isolated invalid (Invalid cpu list in cpuset.cpus.exclusive)\n",
		filepath.Join(pod, "cri-containerd-member.scope"):   "member\n",
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, FilePartition), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	found, err := testManager(root).FindPartitionRoots(kubepods)
	if err != nil {
		t.Fatalf("FindPartitionRoots: %v", err)
	}
	want := map[string]bool{
		filepath.Join(pod, "cri-containerd-isolated.scope"): false,
		filepath.Join(pod, "cri-containerd-root.scope"):     false,
		filepath.Join(pod, "cri-containerd-invalid.scope"):  true,
	}
	if len(found) != len(want) {
		t.Fatalf("found %d partitions (%v), want %d", len(found), found, len(want))
	}
	for dir, invalid := range want {
		state, ok := found[dir]
		if !ok {
			t.Errorf("%s not found", dir)
			continue
		}
		if state.Invalid != invalid {
			t.Errorf("%s invalid = %v, want %v", dir, state.Invalid, invalid)
		}
	}
}

// Both scans stay out of the Burstable and BestEffort QoS slices, in the
// systemd and the cgroupfs layouts: nothing there was set by the plugin.
func TestScansSkipNonGuaranteedQoSSlices(t *testing.T) {
	for _, layout := range []struct {
		name, kubepods, guaranteedPod string
		qosSlices                     []string
	}{
		{
			name:          "systemd",
			kubepods:      "kubepods.slice",
			guaranteedPod: "kubepods-podG.slice",
			qosSlices: []string{
				"kubepods-burstable.slice/kubepods-burstable-podB.slice",
				"kubepods-besteffort.slice/kubepods-besteffort-podE.slice",
			},
		},
		{
			name:          "cgroupfs",
			kubepods:      "kubepods",
			guaranteedPod: "podG",
			qosSlices:     []string{"burstable/podB", "besteffort/podE"},
		},
	} {
		t.Run(layout.name, func(t *testing.T) {
			root := t.TempDir()
			kubepods := filepath.Join(root, layout.kubepods)
			guaranteed := filepath.Join(kubepods, layout.guaranteedPod, "ctr.scope")

			// The same reservation and partition, once under the Guaranteed
			// pod and once under each QoS slice.
			leaves := []string{guaranteed}
			for _, qos := range layout.qosSlices {
				leaves = append(leaves, filepath.Join(kubepods, qos, "ctr.scope"))
			}
			for _, leaf := range leaves {
				if err := os.MkdirAll(leaf, 0o755); err != nil {
					t.Fatal(err)
				}
				for file, value := range map[string]string{
					FileExclusive: "10-11\n",
					FilePartition: "isolated\n",
				} {
					if err := os.WriteFile(filepath.Join(leaf, file), []byte(value), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}

			m := testManager(root)
			reservations, err := m.FindExclusiveReservations(kubepods)
			if err != nil {
				t.Fatal(err)
			}
			if want := []string{guaranteed}; !reflect.DeepEqual(reservations, want) {
				t.Errorf("FindExclusiveReservations = %v, want only %v", reservations, want)
			}

			partitions, err := m.FindPartitionRoots(kubepods)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := partitions[guaranteed]; !ok || len(partitions) != 1 {
				t.Errorf("FindPartitionRoots = %v, want only %s", partitions, guaranteed)
			}
		})
	}
}

// The QoS check is made below the pod cgroup tree only: a cgroup root whose
// own path happens to contain "burstable" must not hide the whole tree.
func TestScansIgnoreQoSWordsAboveTheSubtree(t *testing.T) {
	root := filepath.Join(t.TempDir(), "burstable-lab")
	kubepods := filepath.Join(root, "kubepods.slice")
	leaf := filepath.Join(kubepods, "kubepods-podG.slice", "ctr.scope")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leaf, FilePartition), []byte("isolated\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	partitions, err := testManager(root).FindPartitionRoots(kubepods)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := partitions[leaf]; !ok {
		t.Errorf("FindPartitionRoots = %v, want %s found", partitions, leaf)
	}
}

func TestRelPath(t *testing.T) {
	m := testManager("/sys/fs/cgroup")
	for _, tc := range []struct{ in, want string }{
		{"/sys/fs/cgroup/kubepods.slice/kubepods-podX.slice", "kubepods.slice/kubepods-podX.slice"},
		{"/sys/fs/cgroup", "."},
		// Not below the root: returned unchanged.
		{"/elsewhere/kubepods.slice", "/elsewhere/kubepods.slice"},
	} {
		if got := m.RelPath(tc.in); got != tc.want {
			t.Errorf("RelPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
