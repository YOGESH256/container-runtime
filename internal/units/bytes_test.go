package units

import "testing"

func TestParseBytes(t *testing.T) {
	good := map[string]int64{
		"1048576": 1 << 20, "64m": 64 << 20, "64M": 64 << 20, "1.5g": 3 << 29,
		"512KiB": 512 << 10, "2 GB": 2 << 30, "0": 0,
	}
	for in, want := range good {
		if got, err := ParseBytes(in); err != nil || got != want {
			t.Errorf("ParseBytes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "m", "12x", "-1m", "1..2k"} {
		if _, err := ParseBytes(in); err == nil {
			t.Errorf("ParseBytes(%q) succeeded, want error", in)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	for n, want := range map[int64]string{64 << 20: "64M", 2 << 30: "2G", 1536: "1536B", 3 << 10: "3K"} {
		if got := FormatBytes(n); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
