package money

import (
	"errors"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"0", 0},
		{"1", 1_000_000},
		{"12.5", 12_500_000},
		{"0.000001", 1},
		{".25", 250_000},
		{"-3.10", -3_100_000},
		{"  7.000000 ", 7_000_000},
		{"9223372036854.775807", 9223372036854775807},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if err != nil {
			t.Fatalf("Parse(%q) error: %v", c.in, err)
		}
		if got.Micros() != c.want {
			t.Errorf("Parse(%q) = %d, want %d", c.in, got.Micros(), c.want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]error{
		"":                     ErrInvalidAmount,
		".":                    ErrInvalidAmount,
		"1.":                   ErrInvalidAmount,
		"abc":                  ErrInvalidAmount,
		"1e6":                  ErrInvalidAmount,
		"1.2.3":                ErrInvalidAmount,
		"0.0000001":            ErrTooPrecise,
		"9223372036854.775808": ErrOverflow,
		"99999999999999999999": ErrOverflow,
	}
	for in, want := range cases {
		_, err := Parse(in)
		if !errors.Is(err, want) {
			t.Errorf("Parse(%q) err = %v, want %v", in, err, want)
		}
	}
}

func TestString(t *testing.T) {
	cases := map[int64]string{
		0:          "0.000000",
		1:          "0.000001",
		12_500_000: "12.500000",
		-3_100_000: "-3.100000",
	}
	for in, want := range cases {
		if got := FromMicros(in).String(); got != want {
			t.Errorf("String(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	for _, s := range []string{"0.000000", "1.234567", "1000000.000001", "-42.000000"} {
		if got := MustParse(s).String(); got != s {
			t.Errorf("round trip %q -> %q", s, got)
		}
	}
}
