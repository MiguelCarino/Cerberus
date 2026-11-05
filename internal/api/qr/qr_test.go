package qr

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// testdata/reference.json holds symbols made by an independent encoder
// (Kazuhiko Arase's qrcode-generator 1.4.4, MIT, byte mode, level M) with
// the mask forced, for every version 1..10: each at its exact capacity and
// one byte past the previous version's.
func TestReferenceSymbols(t *testing.T) {
	raw, err := os.ReadFile("testdata/reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var refs []struct {
		Data string   `json:"data"`
		Mask int      `json:"mask"`
		Size int      `json:"size"`
		Rows []string `json:"rows"`
	}
	if err := json.Unmarshal(raw, &refs); err != nil {
		t.Fatal(err)
	}
	versions := map[int]bool{}
	for _, ref := range refs {
		c, err := EncodeMask([]byte(ref.Data), ref.Mask)
		if err != nil {
			t.Fatalf("%q: %v", ref.Data, err)
		}
		versions[c.Version] = true
		want := strings.Join(ref.Rows, "\n") + "\n"
		if c.Size != ref.Size || c.String() != want {
			t.Errorf("len %d mask %d: version %d size %d differs from the reference (size %d)\ngot:\n%s\nwant:\n%s",
				len(ref.Data), ref.Mask, c.Version, c.Size, ref.Size, c, want)
		}
	}
	if len(versions) != MaxVersion {
		t.Fatalf("reference covers versions %v, want 1..10", versions)
	}
}

// The worked example of Thonky's QR tutorial: version 1-M data codewords of
// "HELLO WORLD" (alphanumeric) and their 10 EC codewords.
func TestReedSolomonKnownVector(t *testing.T) {
	data := []byte{32, 91, 11, 120, 209, 114, 220, 77, 67, 64, 236, 17, 236, 17, 236, 17}
	want := []byte{196, 35, 39, 119, 235, 215, 231, 226, 93, 23}
	if got := ReedSolomon(data, 10); !bytes.Equal(got, want) {
		t.Fatalf("EC = %v, want %v", got, want)
	}
}

// Format strings for level M from the specification's table, and the
// version 7 and 10 information strings.
func TestFormatAndVersionBits(t *testing.T) {
	wantFormat := []string{"101010000010010", "101000100100101", "101111001111100", "101101101001011",
		"100010111111001", "100000011001110", "100111110010111", "100101010100000"}
	for m, w := range wantFormat {
		if got := bin(FormatBits(m), 15); got != w {
			t.Errorf("format M mask %d = %s, want %s", m, got, w)
		}
	}
	if got := bin(VersionBits(7), 18); got != "000111110010010100" {
		t.Errorf("version 7 = %s", got)
	}
	if got := bin(VersionBits(10), 18); got != "001010010011010011" {
		t.Errorf("version 10 = %s", got)
	}
}

func bin(v, n int) string {
	var b strings.Builder
	for i := n - 1; i >= 0; i-- {
		if (v>>i)&1 == 1 {
			b.WriteByte('1')
		} else {
			b.WriteByte('0')
		}
	}
	return b.String()
}

func TestCapacityAndLimits(t *testing.T) {
	want := []int{0, 14, 26, 42, 62, 84, 106, 122, 152, 180, 213}
	for v := 1; v <= MaxVersion; v++ {
		if Capacity(v) != want[v] {
			t.Errorf("capacity v%d = %d, want %d", v, Capacity(v), want[v])
		}
	}
	if _, err := Encode(bytes.Repeat([]byte("x"), 214)); err != ErrTooLong {
		t.Fatalf("214 bytes: err = %v", err)
	}
	if _, err := EncodeMask([]byte("x"), 8); err == nil {
		t.Fatal("mask 8 accepted")
	}
	c, err := Encode(nil)
	if err != nil || c.Version != 1 {
		t.Fatalf("empty: %v %+v", err, c)
	}
}

// Encode picks the mask with the lowest penalty among the eight.
func TestAutoMaskIsLowestPenalty(t *testing.T) {
	data := []byte("http://127.0.0.1:7340/?token=cdv-00112233445566778899aabbccddeeff")
	c, err := Encode(data)
	if err != nil {
		t.Fatal(err)
	}
	best := -1
	for m := 0; m < 8; m++ {
		fixed, _ := EncodeMask(data, m)
		s := &symbol{size: fixed.Size, mod: fixed.Modules}
		if p := s.penalty(); best < 0 || p < best {
			best = p
		}
	}
	got := (&symbol{size: c.Size, mod: c.Modules}).penalty()
	if got != best {
		t.Fatalf("auto mask %d penalty %d, best %d", c.Mask, got, best)
	}
}

func TestHalfBlocks(t *testing.T) {
	c, _ := EncodeMask([]byte("HELLO WORLD"), 0)
	out := c.HalfBlocks(2, false)
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != (21+4+1)/2 {
		t.Fatalf("%d lines", len(lines))
	}
	for _, l := range lines {
		if n := len([]rune(l)); n != 25 {
			t.Fatalf("line width %d", n)
		}
	}
	// Row 2 (first symbol row) and 3: the finder's top edge is dark over dark.
	if r := []rune(lines[1]); r[2] != '█' || r[0] != ' ' {
		t.Fatalf("line 1 = %q", lines[1])
	}
	inv := c.HalfBlocks(2, true)
	if r := []rune(strings.Split(inv, "\n")[1]); r[2] != ' ' || r[0] != '█' {
		t.Fatalf("inverted line 1 = %q", string(r))
	}
}
