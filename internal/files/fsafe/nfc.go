package fsafe

import (
	"strings"
	"unicode/utf8"
)

// NFC composes a base letter followed by one combining mark into the single
// precomposed character, for the Latin letters of Latin-1 Supplement and
// Latin Extended-A (the accents of Western, Central and Northern European
// languages: "e"+U+0301 → "é", "n"+U+0303 → "ñ", "s"+U+030C → "š"…).
//
// Limit: this is not full Unicode normalisation (the stdlib has no
// decomposition data and Cerberus takes no extra dependencies). Several
// stacked marks, Vietnamese double accents, Greek, Cyrillic and Hangul
// composition are left as they are. Text already in NFC, which is what
// browsers send on every platform except some macOS paths, passes through
// unchanged.
func NFC(s string) string {
	if !hasMark(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	prev, havePrev := rune(0), false
	for _, r := range s {
		if havePrev {
			if c, ok := compose[[2]rune{prev, r}]; ok {
				prev = c
				continue
			}
			b.WriteRune(prev)
		}
		prev, havePrev = r, true
	}
	if havePrev {
		b.WriteRune(prev)
	}
	return b.String()
}

// hasMark is a fast check for any combining mark of the table's range.
func hasMark(s string) bool {
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if r >= 0x0300 && r <= 0x0328 {
			return true
		}
		i += n
	}
	return false
}

// compositions lists, per combining mark, the base letters and their
// precomposed forms, aligned character by character.
var compositions = []struct {
	mark            rune
	bases, composed string
}{
	{0x0300, "AEIOUaeiou", "ÀÈÌÒÙàèìòù"},                             // grave
	{0x0301, "AEIOUYaeiouyCcLlNnRrSsZz", "ÁÉÍÓÚÝáéíóúýĆćĹĺŃńŔŕŚśŹź"}, // acute
	{0x0302, "AEIOUaeiouCcGgHhJjSsWwYy", "ÂÊÎÔÛâêîôûĈĉĜĝĤĥĴĵŜŝŴŵŶŷ"}, // circumflex
	{0x0303, "ANOanoIiUu", "ÃÑÕãñõĨĩŨũ"},                             // tilde
	{0x0308, "AEIOUaeiouyY", "ÄËÏÖÜäëïöüÿŸ"},                         // diaeresis
	{0x030A, "AaUu", "ÅåŮů"},                                         // ring above
	{0x0327, "CcGgKkLlNnRrSsTt", "ÇçĢģĶķĻļŅņŖŗŞşŢţ"},                 // cedilla
	{0x030C, "CcDdEeNnRrSsTtZz", "ČčĎďĚěŇňŘřŠšŤťŽž"},                 // caron
	{0x0304, "AaEeIiOoUu", "ĀāĒēĪīŌōŪū"},                             // macron
	{0x0306, "AaEeGgIiOoUu", "ĂăĔĕĞğĬĭŎŏŬŭ"},                         // breve
	{0x0328, "AaEeIiUu", "ĄąĘęĮįŲų"},                                 // ogonek
	{0x0307, "CcEeGgIZz", "ĊċĖėĠġİŻż"},                               // dot above
	{0x030B, "OoUu", "ŐőŰű"},                                         // double acute
}

var compose = func() map[[2]rune]rune {
	m := map[[2]rune]rune{}
	for _, c := range compositions {
		bases, composed := []rune(c.bases), []rune(c.composed)
		if len(bases) != len(composed) {
			panic("fsafe: composition table misaligned for mark " + string(c.mark))
		}
		for i, b := range bases {
			m[[2]rune{b, c.mark}] = composed[i]
		}
	}
	return m
}()
