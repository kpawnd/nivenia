package platform

import "testing"

func TestMajorVersion_Valid(t *testing.T) {
	cases := []struct {
		input string
		want  int
	}{
		{"14.3.1", 14},
		{"12.0", 12},
		{"15", 15},
		{"14.0.0.0", 14},
		{" 13.1 ", 13},
	}
	for _, c := range cases {
		got, err := majorVersion(c.input)
		if err != nil {
			t.Errorf("majorVersion(%q): unexpected error %v", c.input, err)
			continue
		}
		if got != c.want {
			t.Errorf("majorVersion(%q): got %d, want %d", c.input, got, c.want)
		}
	}
}

func TestMajorVersion_Invalid(t *testing.T) {
	cases := []string{
		"",
		"abc",
		"x.y.z",
		"   ",
	}
	for _, c := range cases {
		_, err := majorVersion(c)
		if err == nil {
			t.Errorf("majorVersion(%q): expected error, got nil", c)
		}
	}
}

func TestSupportedRange(t *testing.T) {
	if MinSupportedMajor >= MaxSupportedMajor {
		t.Errorf("MinSupportedMajor (%d) must be < MaxSupportedMajor (%d)", MinSupportedMajor, MaxSupportedMajor)
	}
}
