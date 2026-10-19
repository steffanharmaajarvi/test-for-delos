package money

import (
	"encoding/json"
	"errors"
	"math/big"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want Amount
		err  error
	}{
		{"100", 100_000_000, nil},
		{"0.5", 500_000, nil},
		{"12.345678", 12_345_678, nil},
		{"0.000001", 1, nil},
		{"0", 0, nil},
		{"1.0000001", 0, ErrTooPrecise},
		{"-1", 0, ErrInvalidFormat},
		{"+1", 0, ErrInvalidFormat},
		{"1e6", 0, ErrInvalidFormat},
		{".5", 0, ErrInvalidFormat},
		{"5.", 0, ErrInvalidFormat},
		{"", 0, ErrInvalidFormat},
		{"1,5", 0, ErrInvalidFormat},
		{"9223372036854.775807", 9223372036854775807, nil},
		{"9223372036854.775808", 0, ErrOutOfRange},
		{"99999999999999999999", 0, ErrOutOfRange},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if !errors.Is(err, c.err) {
			t.Errorf("Parse(%q) err = %v, want %v", c.in, err, c.err)
			continue
		}
		if got != c.want {
			t.Errorf("Parse(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestString(t *testing.T) {
	cases := map[Amount]string{
		0:          "0.000000",
		1:          "0.000001",
		99_000_000: "99.000000",
		12_345_678: "12.345678",
		-1_500_000: "-1.500000",
	}
	for in, want := range cases {
		if got := in.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", in, got, want)
		}
	}
}

func TestMulBpsRoundsDown(t *testing.T) {
	cases := []struct {
		a    Amount
		bps  int
		want Amount
	}{
		{100_000_000, 100, 1_000_000}, // 1% of 100
		{1, 100, 0},                   // 1% of 0.000001 rounds to zero
		{199, 100, 1},
		{12_345_678, 250, 308_641}, // 2.5% of 12.345678 = 0.30864195
		{9_000_000_000_000_000_000, 10_000, 9_000_000_000_000_000_000}, // no overflow
	}
	for _, c := range cases {
		if got := c.a.MulBps(c.bps); got != c.want {
			t.Errorf("%d.MulBps(%d) = %d, want %d", c.a, c.bps, got, c.want)
		}
	}
}

func TestJSONRejectsNumbers(t *testing.T) {
	var a Amount
	if err := json.Unmarshal([]byte(`100.5`), &a); err == nil {
		t.Fatal("expected error for JSON number")
	}
	if err := json.Unmarshal([]byte(`"100.5"`), &a); err != nil || a != 100_500_000 {
		t.Fatalf("got %d, %v", a, err)
	}
	b, _ := json.Marshal(Amount(1))
	if string(b) != `"0.000001"` {
		t.Fatalf("marshal = %s", b)
	}
}

func TestFromBaseUnits(t *testing.T) {
	if a, err := FromBaseUnits(big.NewInt(42)); err != nil || a != 42 {
		t.Fatalf("got %d, %v", a, err)
	}
	huge := new(big.Int).Lsh(big.NewInt(1), 200)
	if _, err := FromBaseUnits(huge); !errors.Is(err, ErrOutOfRange) {
		t.Fatalf("want ErrOutOfRange, got %v", err)
	}
	if _, err := FromBaseUnits(big.NewInt(-1)); !errors.Is(err, ErrOutOfRange) {
		t.Fatalf("want ErrOutOfRange, got %v", err)
	}
}
