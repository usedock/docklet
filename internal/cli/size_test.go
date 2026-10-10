package cli

import "testing"

func TestParseMemoryMiB(t *testing.T) {
	cases := []struct {
		in   string
		want int
		bad  bool
	}{
		{"8gb", 8192, false}, {"8g", 8192, false}, {"8GB", 8192, false}, {"8gib", 8192, false},
		{"8192mb", 8192, false}, {"512m", 512, false}, {"4096", 4096, false}, {" 2gb ", 2048, false},
		{"1tb", 1048576, false},
		{"", 0, true}, {"gb", 0, true}, {"0", 0, true}, {"-1gb", 0, true}, {"1.5gb", 0, true},
		{"8xb", 0, true}, {"abc", 0, true}, {"8 gb gb", 0, true}, {"99999999999999999999", 0, true},
	}
	for _, c := range cases {
		got, err := ParseMemoryMiB(c.in)
		if (err != nil) != c.bad || got != c.want {
			t.Errorf("ParseMemoryMiB(%q) = %d, %v; want %d bad=%v", c.in, got, err, c.want, c.bad)
		}
	}
}

func TestParseDiskGiB(t *testing.T) {
	cases := []struct {
		in   string
		want int
		bad  bool
	}{
		{"40gb", 40, false}, {"40g", 40, false}, {"40", 40, false}, {"2048mb", 2, false}, {"1tb", 1024, false},
		{"1000mb", 0, true}, {"", 0, true}, {"0gb", 0, true}, {"-5", 0, true}, {"4 kb", 0, true}, {"x", 0, true},
	}
	for _, c := range cases {
		got, err := ParseDiskGiB(c.in)
		if (err != nil) != c.bad || got != c.want {
			t.Errorf("ParseDiskGiB(%q) = %d, %v; want %d bad=%v", c.in, got, err, c.want, c.bad)
		}
	}
}

func TestFormatMiB(t *testing.T) {
	if formatMiB(8192) != "8gb" || formatMiB(1536) != "1536mb" {
		t.Fatal("formatMiB")
	}
}
