// Package cpuset implements parsing, formatting and set algebra for the Linux
// cpu list format ("0-3,7") used by the cgroup v2 cpuset controller.
package cpuset

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// maxCPU bounds what Parse accepts, so that a corrupted or unexpected cgroup
// file cannot make us allocate an enormous set.
const maxCPU = 1 << 16

// CPUSet is an immutable set of cpu ids. The zero value is a usable empty set.
type CPUSet struct {
	m map[int]struct{}
}

// New builds a CPUSet out of the given cpu ids.
func New(cpus ...int) CPUSet {
	s := CPUSet{m: make(map[int]struct{}, len(cpus))}
	for _, c := range cpus {
		s.m[c] = struct{}{}
	}
	return s
}

// Parse reads a cpu list such as "0-3,7" or "" (the empty set).
func Parse(s string) (CPUSet, error) {
	out := CPUSet{m: map[int]struct{}{}}

	s = strings.TrimSpace(s)
	if s == "" {
		return out, nil
	}

	for _, tok := range strings.Split(s, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}

		lo, hi, isRange := strings.Cut(tok, "-")
		first, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil {
			return CPUSet{}, fmt.Errorf("invalid cpu list %q: %w", s, err)
		}
		last := first
		if isRange {
			last, err = strconv.Atoi(strings.TrimSpace(hi))
			if err != nil {
				return CPUSet{}, fmt.Errorf("invalid cpu list %q: %w", s, err)
			}
		}

		if first < 0 || last < first || last > maxCPU {
			return CPUSet{}, fmt.Errorf("invalid cpu range %q in cpu list %q", tok, s)
		}
		for c := first; c <= last; c++ {
			out.m[c] = struct{}{}
		}
	}

	return out, nil
}

// MustParse is Parse for compile-time constants and tests.
func MustParse(s string) CPUSet {
	set, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return set
}

// String renders the set in the canonical kernel cpu list format, collapsing
// consecutive ids into ranges. The empty set renders as "".
func (s CPUSet) String() string {
	cpus := s.List()
	if len(cpus) == 0 {
		return ""
	}

	var (
		b     strings.Builder
		start = cpus[0]
		prev  = cpus[0]
	)
	flush := func() {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		if start == prev {
			fmt.Fprintf(&b, "%d", start)
		} else {
			fmt.Fprintf(&b, "%d-%d", start, prev)
		}
	}

	for _, c := range cpus[1:] {
		if c == prev+1 {
			prev = c
			continue
		}
		flush()
		start, prev = c, c
	}
	flush()

	return b.String()
}

// List returns the cpu ids in ascending order.
func (s CPUSet) List() []int {
	cpus := make([]int, 0, len(s.m))
	for c := range s.m {
		cpus = append(cpus, c)
	}
	sort.Ints(cpus)
	return cpus
}

// Size is the number of cpus in the set.
func (s CPUSet) Size() int { return len(s.m) }

// IsEmpty reports whether the set has no cpus.
func (s CPUSet) IsEmpty() bool { return len(s.m) == 0 }

// Contains reports whether cpu is a member of the set.
func (s CPUSet) Contains(cpu int) bool {
	_, ok := s.m[cpu]
	return ok
}

// Union returns the set of cpus present in either s or o.
func (s CPUSet) Union(o CPUSet) CPUSet {
	out := CPUSet{m: make(map[int]struct{}, len(s.m)+len(o.m))}
	for c := range s.m {
		out.m[c] = struct{}{}
	}
	for c := range o.m {
		out.m[c] = struct{}{}
	}
	return out
}

// Difference returns the cpus of s that are not in o.
func (s CPUSet) Difference(o CPUSet) CPUSet {
	out := CPUSet{m: make(map[int]struct{}, len(s.m))}
	for c := range s.m {
		if !o.Contains(c) {
			out.m[c] = struct{}{}
		}
	}
	return out
}

// Intersection returns the cpus present in both s and o.
func (s CPUSet) Intersection(o CPUSet) CPUSet {
	out := CPUSet{m: map[int]struct{}{}}
	for c := range s.m {
		if o.Contains(c) {
			out.m[c] = struct{}{}
		}
	}
	return out
}

// IsSubsetOf reports whether every cpu of s is also in o.
func (s CPUSet) IsSubsetOf(o CPUSet) bool {
	for c := range s.m {
		if !o.Contains(c) {
			return false
		}
	}
	return true
}

// Equals reports whether s and o hold exactly the same cpus.
func (s CPUSet) Equals(o CPUSet) bool {
	return len(s.m) == len(o.m) && s.IsSubsetOf(o)
}
