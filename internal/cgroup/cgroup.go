// Package cgroup gives the plugin the little bit of cgroup v2 it needs:
// locating the unified hierarchy, turning the cgroup paths NRI reports into
// filesystem paths, and reading/writing the cpuset partition files.
package cgroup

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/antaloala/nri-cpuset-partition/internal/cpuset"
)

// The cpuset controller files this plugin touches.
const (
	FileCPUs             = "cpuset.cpus"
	FileCPUsEffective    = "cpuset.cpus.effective"
	FileExclusive        = "cpuset.cpus.exclusive"
	FileExclusiveEffect  = "cpuset.cpus.exclusive.effective"
	FilePartition        = "cpuset.cpus.partition"
	fileCgroupController = "cgroup.controllers"
)

// Partition values accepted in cpuset.cpus.partition.
const (
	PartitionMember   = "member"
	PartitionRoot     = "root"
	PartitionIsolated = "isolated"
)

// ErrNoExclusiveFile is returned when the kernel does not expose
// cpuset.cpus.exclusive, i.e. it predates the 6.7 remote partition support.
var ErrNoExclusiveFile = errors.New("cpuset.cpus.exclusive not present (kernel 6.7+ required)")

// Manager performs the cgroup v2 reads and writes. A Manager in dry-run mode
// logs through its writeHook instead of touching the filesystem, which lets
// the plugin be exercised on a node without changing its state.
type Manager struct {
	root    string
	dryRun  bool
	onWrite func(path, value string)
}

// NewManager locates the cgroup v2 mount point (or uses root when non-empty)
// and verifies that it is a unified hierarchy with the cpuset controller.
func NewManager(root string, dryRun bool, onWrite func(path, value string)) (*Manager, error) {
	if root == "" {
		detected, err := detectUnifiedMount()
		if err != nil {
			return nil, err
		}
		root = detected
	}

	controllers, err := os.ReadFile(filepath.Join(root, fileCgroupController))
	if err != nil {
		return nil, fmt.Errorf("%s does not look like a cgroup v2 mount: %w", root, err)
	}
	if !strings.Contains(string(controllers), "cpuset") {
		return nil, fmt.Errorf("cpuset controller not available at %s (have: %s)",
			root, strings.TrimSpace(string(controllers)))
	}

	return NewManagerAt(root, dryRun, onWrite), nil
}

// NewManagerAt returns a Manager rooted at an arbitrary directory, without
// checking that it is a cgroup v2 mount. Use NewManager in production; this is
// for tests and for inspecting a captured cgroup tree.
func NewManagerAt(root string, dryRun bool, onWrite func(path, value string)) *Manager {
	if onWrite == nil {
		onWrite = func(string, string) {}
	}
	return &Manager{root: root, dryRun: dryRun, onWrite: onWrite}
}

// Root is the cgroup v2 mount point.
func (m *Manager) Root() string { return m.root }

// DryRun reports whether writes are suppressed.
func (m *Manager) DryRun() bool { return m.dryRun }

func detectUnifiedMount() (string, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "", fmt.Errorf("cannot read mountinfo: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		// ... <mountpoint at field 4> ... - <fstype> <source> <options>
		line := scanner.Text()
		pre, post, ok := strings.Cut(line, " - ")
		if !ok {
			continue
		}
		if fields := strings.Fields(post); len(fields) == 0 || fields[0] != "cgroup2" {
			continue
		}
		fields := strings.Fields(pre)
		if len(fields) < 5 {
			continue
		}
		return unescapeMountPath(fields[4]), nil
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("cannot scan mountinfo: %w", err)
	}

	return "", errors.New("no cgroup2 mount found; this plugin requires a unified cgroup hierarchy")
}

var mountPathEscapes = strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)

func unescapeMountPath(p string) string { return mountPathEscapes.Replace(p) }

// ContainerDir turns the cgroups path NRI reports for a container into a
// filesystem path. It understands both the systemd form
// ("kubepods-pod<uid>.slice:cri-containerd:<id>") and the cgroupfs form
// ("/kubepods/pod<uid>/<id>").
func (m *Manager) ContainerDir(cgroupsPath string) (string, error) {
	if cgroupsPath == "" {
		return "", errors.New("empty cgroups path")
	}

	if !strings.Contains(cgroupsPath, ":") {
		return filepath.Join(m.root, filepath.Clean("/"+cgroupsPath)), nil
	}

	parts := strings.Split(cgroupsPath, ":")
	if len(parts) != 3 {
		return "", fmt.Errorf("malformed systemd cgroups path %q", cgroupsPath)
	}
	slicePath, err := ExpandSlice(parts[0])
	if err != nil {
		return "", err
	}

	return filepath.Join(m.root, slicePath, parts[1]+"-"+parts[2]+".scope"), nil
}

// ExpandSlice turns a systemd slice unit name into its path below the cgroup
// root: "kubepods-burstable-podX.slice" lives at
// "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podX.slice".
//
// Kubelet replaces the dashes of a pod UID with underscores precisely so that
// the slice name keeps one dash per hierarchy level, which is what makes this
// naive split correct for the paths we care about.
func ExpandSlice(name string) (string, error) {
	name = strings.TrimPrefix(name, "/")
	if name == "" || name == "-.slice" {
		return "", nil
	}
	if !strings.HasSuffix(name, ".slice") {
		// Not a slice: a plain path, e.g. when the cgroupfs driver is in use.
		return filepath.Clean("/" + name), nil
	}

	parts := strings.Split(strings.TrimSuffix(name, ".slice"), "-")
	elems := make([]string, 0, len(parts))
	for i := range parts {
		if parts[i] == "" {
			return "", fmt.Errorf("malformed slice name %q", name)
		}
		elems = append(elems, strings.Join(parts[:i+1], "-")+".slice")
	}

	return filepath.Join(elems...), nil
}

// Ancestors lists the cgroup directories between the cgroup root (exclusive)
// and dir (exclusive), outermost first. The root itself is left out: it has no
// cpuset.cpus.exclusive and needs none, as it implicitly owns every cpu.
func (m *Manager) Ancestors(dir string) ([]string, error) {
	rel, err := filepath.Rel(m.root, dir)
	if err != nil {
		return nil, fmt.Errorf("cgroup %s is not below %s: %w", dir, m.root, err)
	}
	if rel == "." || strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("cgroup %s is not below %s", dir, m.root)
	}

	parts := strings.Split(rel, string(filepath.Separator))
	out := make([]string, 0, len(parts)-1)
	for i := 0; i < len(parts)-1; i++ {
		out = append(out, filepath.Join(m.root, filepath.Join(parts[:i+1]...)))
	}

	return out, nil
}

// SortTopDown orders cgroup directories parents-before-children, which is the
// order in which an exclusive cpu reservation has to be handed down.
func SortTopDown(dirs []string) []string {
	out := append([]string(nil), dirs...)
	sort.Slice(out, func(i, j int) bool {
		di, dj := strings.Count(out[i], "/"), strings.Count(out[j], "/")
		if di != dj {
			return di < dj
		}
		return out[i] < out[j]
	})
	return out
}

// Exists reports whether a cgroup directory is still present. Container
// cgroups come and go under us, so most operations tolerate a missing one.
func (m *Manager) Exists(dir string) bool {
	st, err := os.Stat(dir)
	return err == nil && st.IsDir()
}

// ReadString reads a cgroup file and trims the trailing newline.
func (m *Manager) ReadString(dir, file string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && file == FileExclusive {
			return "", fmt.Errorf("%s: %w", dir, ErrNoExclusiveFile)
		}
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// ReadCPUSet reads a cgroup file holding a cpu list.
func (m *Manager) ReadCPUSet(dir, file string) (cpuset.CPUSet, error) {
	s, err := m.ReadString(dir, file)
	if err != nil {
		return cpuset.CPUSet{}, err
	}
	set, err := cpuset.Parse(s)
	if err != nil {
		return cpuset.CPUSet{}, fmt.Errorf("%s/%s: %w", dir, file, err)
	}
	return set, nil
}

// WriteString writes a value to a cgroup file. It first reads the file back
// and skips the write when the value already matches, so that a periodic
// resync does not keep poking the kernel for nothing.
func (m *Manager) WriteString(dir, file, value string) error {
	path := filepath.Join(dir, file)

	current, err := m.ReadString(dir, file)
	if err != nil {
		return err
	}
	if current == value {
		return nil
	}

	m.onWrite(path, value)
	if m.dryRun {
		return nil
	}

	// cgroup files take a single write; an empty buffer would be a no-op, so
	// clearing a value means writing a bare newline.
	payload := value
	if payload == "" {
		payload = "\n"
	}
	if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
		return fmt.Errorf("write %q to %s: %w", value, path, err)
	}

	return nil
}

// WriteCPUSet writes a cpu list to a cgroup file.
func (m *Manager) WriteCPUSet(dir, file string, set cpuset.CPUSet) error {
	return m.WriteString(dir, file, set.String())
}

// PartitionState is what cpuset.cpus.partition reports: the requested value
// plus, when the kernel refused it, the reason.
type PartitionState struct {
	Value   string // member, root or isolated
	Invalid bool
	Raw     string
}

// ReadPartition reports the partition state of a cgroup.
func (m *Manager) ReadPartition(dir string) (PartitionState, error) {
	raw, err := m.ReadString(dir, FilePartition)
	if err != nil {
		return PartitionState{}, err
	}
	value, _, _ := strings.Cut(raw, " ")
	return PartitionState{
		Value:   value,
		Invalid: strings.Contains(raw, "invalid"),
		Raw:     raw,
	}, nil
}

// QoSClass derives the pod QoS class from its cgroup path. Kubelet nests
// Burstable and BestEffort pods under their own slice, while Guaranteed pods
// sit directly below the kubepods root.
func QoSClass(podDir string) string {
	switch lower := strings.ToLower(podDir); {
	case strings.Contains(lower, "besteffort"):
		return "BestEffort"
	case strings.Contains(lower, "burstable"):
		return "Burstable"
	default:
		return "Guaranteed"
	}
}

// FindExclusiveReservations walks a cgroup subtree and returns every directory
// that currently has a non-empty cpuset.cpus.exclusive. It is used at startup
// to adopt reservations left behind by a previous run of the plugin.
func (m *Manager) FindExclusiveReservations(subtree string) ([]string, error) {
	var found []string

	err := filepath.WalkDir(subtree, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil // the tree moves under us; skip what disappeared
			}
			return err
		}
		if !d.IsDir() {
			return nil
		}
		set, err := m.ReadCPUSet(path, FileExclusive)
		if err != nil {
			return nil //nolint:nilerr // a cgroup without the file is simply not interesting
		}
		if !set.IsEmpty() {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", subtree, err)
	}

	return found, nil
}
