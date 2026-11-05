// Package qr is a small QR Code encoder written for `cerberus pair`: byte
// mode, error correction level M, versions 1 to 10 (up to 213 bytes), with
// the eight masks scored by the ISO/IEC 18004 penalty rules. It has no
// dependencies and draws the symbol as text with Unicode half blocks, so a
// phone can scan the pairing URL straight off a terminal.
//
// The construction follows the standard: data codewords (mode 0100, an
// 8- or 16-bit length, the bytes, a terminator and 0xEC/0x11 padding),
// Reed–Solomon error correction over GF(256) with the polynomial 0x11D,
// block interleaving, function patterns, the zig-zag codeword placement,
// masking, and the BCH-coded format and version information.
package qr

import (
	"errors"
	"strings"
)

// MaxVersion is the largest version this encoder produces.
const MaxVersion = 10

// ErrTooLong is returned when the data does not fit version 10 at level M.
var ErrTooLong = errors.New("qr: data too long (at most 213 bytes)")

// block layout for error correction level M: EC codewords per block and the
// data codewords of each block, for versions 1..10.
type layout struct {
	ecPerBlock int
	blocks     []int // data codewords per block
}

var layoutsM = [MaxVersion + 1]layout{
	1:  {10, []int{16}},
	2:  {16, []int{28}},
	3:  {26, []int{44}},
	4:  {18, []int{32, 32}},
	5:  {24, []int{43, 43}},
	6:  {16, []int{27, 27, 27, 27}},
	7:  {18, []int{31, 31, 31, 31}},
	8:  {22, []int{38, 38, 39, 39}},
	9:  {22, []int{36, 36, 36, 37, 37}},
	10: {26, []int{43, 43, 43, 43, 44}},
}

// alignment pattern centre coordinates per version.
var alignment = [MaxVersion + 1][]int{
	2: {6, 18}, 3: {6, 22}, 4: {6, 26}, 5: {6, 30}, 6: {6, 34},
	7: {6, 22, 38}, 8: {6, 24, 42}, 9: {6, 26, 46}, 10: {6, 28, 50},
}

// Code is an encoded symbol. Modules[y][x] is true for a dark module.
type Code struct {
	Version int
	Mask    int
	Size    int
	Modules [][]bool
}

// dataCodewords is the number of data codewords of version v at level M.
func dataCodewords(v int) int {
	n := 0
	for _, b := range layoutsM[v].blocks {
		n += b
	}
	return n
}

// Capacity is how many bytes version v holds at level M (0 if out of range).
func Capacity(v int) int {
	if v < 1 || v > MaxVersion {
		return 0
	}
	return (dataCodewords(v)*8 - 4 - countBits(v)) / 8
}

func countBits(v int) int {
	if v <= 9 {
		return 8
	}
	return 16
}

// Encode encodes data in the smallest version that fits, choosing the mask
// with the lowest penalty.
func Encode(data []byte) (*Code, error) {
	return encode(data, -1)
}

// EncodeMask is Encode with a fixed mask (0..7); tests use it to compare
// against reference symbols.
func EncodeMask(data []byte, mask int) (*Code, error) {
	if mask < 0 || mask > 7 {
		return nil, errors.New("qr: mask must be 0..7")
	}
	return encode(data, mask)
}

func encode(data []byte, mask int) (*Code, error) {
	v := 0
	for i := 1; i <= MaxVersion; i++ {
		if len(data) <= Capacity(i) {
			v = i
			break
		}
	}
	if v == 0 {
		return nil, ErrTooLong
	}
	cw := interleave(v, Codewords(data, v))
	base := newSymbol(v)
	base.drawCodewords(cw)
	if mask >= 0 {
		base.applyMask(mask)
		base.drawFormat(mask)
		return base.code(mask), nil
	}
	best, bestScore := -1, 0
	var bestSym *symbol
	for m := 0; m < 8; m++ {
		s := base.clone()
		s.applyMask(m)
		s.drawFormat(m)
		if p := s.penalty(); best < 0 || p < bestScore {
			best, bestScore, bestSym = m, p, s
		}
	}
	return bestSym.code(best), nil
}

// Codewords returns the data codewords (before error correction) of data
// in version v: mode, length, bytes, terminator and padding.
func Codewords(data []byte, v int) []byte {
	var bb bitBuffer
	bb.put(0b0100, 4)
	bb.put(len(data), countBits(v))
	for _, b := range data {
		bb.put(int(b), 8)
	}
	capBits := dataCodewords(v) * 8
	bb.put(0, min(4, capBits-bb.n))
	if r := bb.n % 8; r != 0 {
		bb.put(0, 8-r)
	}
	for pad := 0xEC; bb.n < capBits; pad ^= 0xEC ^ 0x11 {
		bb.put(pad, 8)
	}
	return bb.bytes
}

type bitBuffer struct {
	bytes []byte
	n     int
}

func (b *bitBuffer) put(v, bits int) {
	for i := bits - 1; i >= 0; i-- {
		if b.n%8 == 0 {
			b.bytes = append(b.bytes, 0)
		}
		if (v>>i)&1 == 1 {
			b.bytes[b.n/8] |= 0x80 >> (b.n % 8)
		}
		b.n++
	}
}

// interleave splits the data into blocks, appends each block's EC
// codewords and interleaves them column by column.
func interleave(v int, data []byte) []byte {
	l := layoutsM[v]
	var blocks, ecs [][]byte
	off := 0
	for _, n := range l.blocks {
		blk := data[off : off+n]
		off += n
		blocks = append(blocks, blk)
		ecs = append(ecs, ReedSolomon(blk, l.ecPerBlock))
	}
	var out []byte
	maxLen := l.blocks[len(l.blocks)-1]
	for i := 0; i < maxLen; i++ {
		for _, b := range blocks {
			if i < len(b) {
				out = append(out, b[i])
			}
		}
	}
	for i := 0; i < l.ecPerBlock; i++ {
		for _, e := range ecs {
			out = append(out, e[i])
		}
	}
	return out
}

// ---------- Reed–Solomon over GF(256), polynomial 0x11D ----------

func gfMul(x, y byte) byte {
	var z byte
	for i := 7; i >= 0; i-- {
		hi := z & 0x80
		z <<= 1
		if hi != 0 {
			z ^= 0x1D
		}
		if (y>>i)&1 != 0 {
			z ^= x
		}
	}
	return z
}

// generator returns the coefficients (highest degree first, leading 1
// omitted) of prod_{i<degree} (x - α^i).
func generator(degree int) []byte {
	res := make([]byte, degree)
	res[degree-1] = 1
	root := byte(1)
	for i := 0; i < degree; i++ {
		for j := range res {
			res[j] = gfMul(res[j], root)
			if j+1 < len(res) {
				res[j] ^= res[j+1]
			}
		}
		root = gfMul(root, 2)
	}
	return res
}

// ReedSolomon returns the n error-correction codewords of data.
func ReedSolomon(data []byte, n int) []byte {
	gen := generator(n)
	res := make([]byte, n)
	for _, b := range data {
		factor := b ^ res[0]
		copy(res, res[1:])
		res[n-1] = 0
		for i := range res {
			res[i] ^= gfMul(gen[i], factor)
		}
	}
	return res
}

// ---------- the symbol ----------

type symbol struct {
	v, size int
	mod     [][]bool
	fn      [][]bool // function modules (not data, not masked)
}

func newSymbol(v int) *symbol {
	size := 17 + 4*v
	s := &symbol{v: v, size: size, mod: grid(size), fn: grid(size)}
	for i := 0; i < size; i++ {
		s.set(6, i, i%2 == 0)
		s.set(i, 6, i%2 == 0)
	}
	s.finder(3, 3)
	s.finder(size-4, 3)
	s.finder(3, size-4)
	pos := alignment[v]
	for i, y := range pos {
		for j, x := range pos {
			if (i == 0 && j == 0) || (i == 0 && j == len(pos)-1) || (i == len(pos)-1 && j == 0) {
				continue
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					s.set(x+dx, y+dy, max(abs(dx), abs(dy)) != 1)
				}
			}
		}
	}
	s.drawFormat(0) // reserves the area; redrawn after masking
	s.drawVersion()
	return s
}

func grid(n int) [][]bool {
	g := make([][]bool, n)
	for i := range g {
		g[i] = make([]bool, n)
	}
	return g
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// set draws a function module at column x, row y.
func (s *symbol) set(x, y int, dark bool) {
	s.mod[y][x] = dark
	s.fn[y][x] = true
}

func (s *symbol) finder(cx, cy int) {
	for dy := -4; dy <= 4; dy++ {
		for dx := -4; dx <= 4; dx++ {
			x, y := cx+dx, cy+dy
			if x < 0 || y < 0 || x >= s.size || y >= s.size {
				continue
			}
			d := max(abs(dx), abs(dy))
			s.set(x, y, d != 2 && d != 4)
		}
	}
}

// FormatBits is the 15-bit format information for level M and mask.
func FormatBits(mask int) int {
	data := 0<<3 | mask // level M = 00
	rem := data
	for i := 0; i < 10; i++ {
		rem = (rem << 1) ^ ((rem >> 9) * 0x537)
	}
	return (data<<10 | rem) ^ 0x5412
}

// VersionBits is the 18-bit version information (versions 7 and up).
func VersionBits(v int) int {
	rem := v
	for i := 0; i < 12; i++ {
		rem = (rem << 1) ^ ((rem >> 11) * 0x1F25)
	}
	return v<<12 | rem
}

func bit(x, i int) bool { return (x>>i)&1 != 0 }

func (s *symbol) drawFormat(mask int) {
	bits := FormatBits(mask)
	for i := 0; i <= 5; i++ {
		s.set(8, i, bit(bits, i))
	}
	s.set(8, 7, bit(bits, 6))
	s.set(8, 8, bit(bits, 7))
	s.set(7, 8, bit(bits, 8))
	for i := 9; i < 15; i++ {
		s.set(14-i, 8, bit(bits, i))
	}
	for i := 0; i < 8; i++ {
		s.set(s.size-1-i, 8, bit(bits, i))
	}
	for i := 8; i < 15; i++ {
		s.set(8, s.size-15+i, bit(bits, i))
	}
	s.set(8, s.size-8, true) // the dark module
}

func (s *symbol) drawVersion() {
	if s.v < 7 {
		return
	}
	bits := VersionBits(s.v)
	for i := 0; i < 18; i++ {
		a, b := s.size-11+i%3, i/3
		s.set(a, b, bit(bits, i))
		s.set(b, a, bit(bits, i))
	}
}

// drawCodewords places the bits in the two-column zig-zag, right to left.
func (s *symbol) drawCodewords(cw []byte) {
	i := 0
	for right := s.size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < s.size; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				y := vert
				if (right+1)&2 == 0 {
					y = s.size - 1 - vert
				}
				if !s.fn[y][x] && i < len(cw)*8 {
					s.mod[y][x] = (cw[i>>3]>>(7-uint(i&7)))&1 != 0
					i++
				}
			}
		}
	}
}

func maskBit(m, x, y int) bool {
	switch m {
	case 0:
		return (x+y)%2 == 0
	case 1:
		return y%2 == 0
	case 2:
		return x%3 == 0
	case 3:
		return (x+y)%3 == 0
	case 4:
		return (x/3+y/2)%2 == 0
	case 5:
		return x*y%2+x*y%3 == 0
	case 6:
		return (x*y%2+x*y%3)%2 == 0
	default:
		return ((x+y)%2+x*y%3)%2 == 0
	}
}

func (s *symbol) applyMask(m int) {
	for y := 0; y < s.size; y++ {
		for x := 0; x < s.size; x++ {
			if !s.fn[y][x] && maskBit(m, x, y) {
				s.mod[y][x] = !s.mod[y][x]
			}
		}
	}
}

func (s *symbol) clone() *symbol {
	c := &symbol{v: s.v, size: s.size, mod: grid(s.size), fn: s.fn}
	for y := range s.mod {
		copy(c.mod[y], s.mod[y])
	}
	return c
}

func (s *symbol) code(mask int) *Code {
	return &Code{Version: s.v, Mask: mask, Size: s.size, Modules: s.mod}
}

// penalty scores the symbol with the four ISO/IEC 18004 rules.
func (s *symbol) penalty() int {
	n := s.size
	p := 0
	at := func(x, y int, col bool) bool {
		if col {
			return s.mod[x][y]
		}
		return s.mod[y][x]
	}
	finderA := []bool{true, false, true, true, true, false, true, false, false, false, false}
	finderB := []bool{false, false, false, false, true, false, true, true, true, false, true}
	for _, col := range []bool{false, true} {
		for y := 0; y < n; y++ {
			run := 1
			for x := 1; x <= n; x++ {
				if x < n && at(x, y, col) == at(x-1, y, col) {
					run++
					continue
				}
				if run >= 5 {
					p += 3 + run - 5
				}
				run = 1
			}
			for x := 0; x+11 <= n; x++ {
				a, b := true, true
				for k := 0; k < 11; k++ {
					m := at(x+k, y, col)
					a = a && m == finderA[k]
					b = b && m == finderB[k]
				}
				if a {
					p += 40
				}
				if b {
					p += 40
				}
			}
		}
	}
	dark := 0
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if s.mod[y][x] {
				dark++
			}
			if x+1 < n && y+1 < n {
				c := s.mod[y][x]
				if c == s.mod[y][x+1] && c == s.mod[y+1][x] && c == s.mod[y+1][x+1] {
					p += 3
				}
			}
		}
	}
	total := n * n
	k := (abs(dark*20-total*10)+total-1)/total - 1
	p += k * 10
	return p
}

// ---------- text output ----------

// HalfBlocks draws the symbol with a quiet zone of quiet modules, two module
// rows per text line. With lightOnDark (the usual dark terminal), light
// modules are printed as filled blocks so the symbol reads dark-on-light;
// otherwise dark modules are filled.
func (c *Code) HalfBlocks(quiet int, lightOnDark bool) string {
	dark := func(x, y int) bool {
		x, y = x-quiet, y-quiet
		if x < 0 || y < 0 || x >= c.Size || y >= c.Size {
			return false
		}
		return c.Modules[y][x]
	}
	full := c.Size + 2*quiet
	filled := func(x, y int) bool {
		if y >= full {
			return false // below the last row: background
		}
		return dark(x, y) != lightOnDark
	}
	var b strings.Builder
	for y := 0; y < full; y += 2 {
		for x := 0; x < full; x++ {
			top, bot := filled(x, y), filled(x, y+1)
			switch {
			case top && bot:
				b.WriteString("█")
			case top:
				b.WriteString("▀")
			case bot:
				b.WriteString("▄")
			default:
				b.WriteString(" ")
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// String is the symbol as rows of '#' (dark) and '.' (light), no quiet zone.
func (c *Code) String() string {
	var b strings.Builder
	for _, row := range c.Modules {
		for _, m := range row {
			if m {
				b.WriteByte('#')
			} else {
				b.WriteByte('.')
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}
