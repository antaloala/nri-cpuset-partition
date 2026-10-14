package cpuset

import "testing"

func TestParseAndString(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		size     int
	}{
		{"", "", 0},
		{"5", "5", 1},
		{"0-3", "0-3", 4},
		{"20-21", "20-21", 2},
		{"0-3,7", "0-3,7", 5},
		{"7,0-3", "0-3,7", 5},
		{"3,2,1,0", "0-3", 4},
		{"0-3,2-5", "0-5", 6},
		{" 20-23 , 30 ", "20-23,30", 5},
		{"20-39\n", "20-39", 20},
	} {
		set, err := Parse(tc.in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.in, err)
		}
		if got := set.String(); got != tc.want {
			t.Errorf("Parse(%q).String() = %q, want %q", tc.in, got, tc.want)
		}
		if got := set.Size(); got != tc.size {
			t.Errorf("Parse(%q).Size() = %d, want %d", tc.in, got, tc.size)
		}
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, in := range []string{"a", "1-", "-1", "3-1", "0-7:2/4", "1.5"} {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) should have failed", in)
		}
	}
}

func TestSetAlgebra(t *testing.T) {
	a, b := MustParse("20-23"), MustParse("22-25")

	if got := a.Union(b).String(); got != "20-25" {
		t.Errorf("Union = %q, want 20-25", got)
	}
	if got := a.Difference(b).String(); got != "20-21" {
		t.Errorf("Difference = %q, want 20-21", got)
	}
	if got := a.Intersection(b).String(); got != "22-23" {
		t.Errorf("Intersection = %q, want 22-23", got)
	}
	if a.IsSubsetOf(b) {
		t.Error("20-23 should not be a subset of 22-25")
	}
	if !MustParse("22-23").IsSubsetOf(a) {
		t.Error("22-23 should be a subset of 20-23")
	}
	if !MustParse("23,20,21,22").Equals(a) {
		t.Error("23,20,21,22 should equal 20-23")
	}
	if a.Equals(b) {
		t.Error("20-23 should not equal 22-25")
	}

	// The zero value has to behave as an empty set, it is what the plugin
	// writes to release a reservation.
	var zero CPUSet
	if !zero.IsEmpty() || zero.String() != "" || !zero.IsSubsetOf(a) {
		t.Error("the zero CPUSet should be a usable empty set")
	}
	if got := zero.Union(a).String(); got != "20-23" {
		t.Errorf("zero.Union = %q, want 20-23", got)
	}
}

func TestUnionDoesNotAliasOperands(t *testing.T) {
	a, b := MustParse("20-21"), MustParse("30-31")

	u := a.Union(b)
	if got := u.String(); got != "20-21,30-31" {
		t.Fatalf("Union = %q", got)
	}
	if got := a.String(); got != "20-21" {
		t.Errorf("Union mutated its receiver: %q", got)
	}
	if got := b.String(); got != "30-31" {
		t.Errorf("Union mutated its argument: %q", got)
	}
}
