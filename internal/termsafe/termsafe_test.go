package termsafe_test

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/grafana/grafanapi/internal/termsafe"
	"github.com/stretchr/testify/assert"
)

type corpusEntry struct {
	name  string
	input string
	cell  string
	text  string
	runes string
}

func corpus() []corpusEntry {
	return []corpusEntry{
		{"csi sgr", "\x1b[31mred\x1b[0m", "red", "red", " [31mred [0m"},
		{"osc52 bel", "\x1b]52;c;aGk=\a", "", "", " ]52;c;aGk= "},
		{"osc52 st", "\x1b]52;c;aGk=\x1b\\", "", "", " ]52;c;aGk= \\"},
		{"unterminated osc", "ok\x1b]52;c;aGk=", "ok", "ok", "ok ]52;c;aGk="},
		{"dcs", "a\x1bPq#0\x1b\\b", "ab", "ab", "a Pq#0 \\b"},
		{"nF charset", "a\x1b(Bb", "ab", "ab", "a (Bb"},
		{"osc aborted by can", "a\x1b]0;t\x18b", "ab", "ab", "a ]0;t b"},
		{"trailing esc", "a\x1b", "a", "a", "a "},
		{"unterminated csi", "a\x1b[31", "a", "a", "a [31"},
		{"c1 csi", "a\u009b31mb", "ab", "ab", "a 31mb"},
		{"c1 osc and st", "a\u009d52;c;x\u009cb", "ab", "ab", "a 52;c;x b"},
		{"raw invalid byte", "a\x9bb", "a\ufffdb", "a\ufffdb", "a\ufffdb"},
		{"rlo", "abc\u202edef", "abc def", "abc def", "abc def"},
		{"isolates", "a\u2066b\u2069", "a b ", "a b ", "a b "},
		{"tag char", "a\U000E0041b", "a b", "a b", "a b"},
		{"layout", "line1\nline2\tcol\r", "line1 line2 col ", "line1\nline2\tcol ", "line1 line2 col "},
		{"del", "a\x7fb", "a b", "a b", "a b"},
		{"nul", "\x00nul", " nul", " nul", " nul"},
		{"greek", "\u0395\u03bb\u03bb\u03b7\u03bd\u03b9\u03ba\u03ac \u00e9\u20ac", "\u0395\u03bb\u03bb\u03b7\u03bd\u03b9\u03ba\u03ac \u00e9\u20ac", "\u0395\u03bb\u03bb\u03b7\u03bd\u03b9\u03ba\u03ac \u00e9\u20ac", "\u0395\u03bb\u03bb\u03b7\u03bd\u03b9\u03ba\u03ac \u00e9\u20ac"},
		{"empty", "", "", "", ""},
		{"line separator", "a\u2028b", "a\u2028b", "a\u2028b", "a\u2028b"},
	}
}

// oldCell is a copy of the predicate alerts' cell used before it delegated to termsafe.Runes.
func oldCell(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}

		return r
	}, value)
}

func TestCell(t *testing.T) {
	for _, tc := range corpus() {
		t.Run(tc.name, func(t *testing.T) {
			got := termsafe.Cell(tc.input)

			assert.Equal(t, tc.cell, got)

			for _, r := range got {
				assert.False(t, r == '\x1b' || unicode.IsControl(r) || unicode.Is(unicode.Cf, r), "unsafe rune %U left", r)
			}
		})
	}
}

func TestText(t *testing.T) {
	for _, tc := range corpus() {
		t.Run(tc.name, func(t *testing.T) {
			got := termsafe.Text(tc.input)

			assert.Equal(t, tc.text, got)

			for _, r := range got {
				if r == '\n' || r == '\t' {
					continue
				}

				assert.False(t, unicode.IsControl(r) || unicode.Is(unicode.Cf, r), "unsafe rune %U left", r)
			}
		})
	}
}

func TestRunes(t *testing.T) {
	for _, tc := range corpus() {
		t.Run(tc.name, func(t *testing.T) {
			got := termsafe.Runes(tc.input)

			assert.Equal(t, tc.runes, got)
			assert.Equal(t, oldCell(tc.input), got, "Runes must stay byte-identical to the old alerts cell")
		})
	}
}

func TestText_MultiLineKeepsLines(t *testing.T) {
	input := "first line\nsecond \x1b]52;c;aGk=\a line\nthird line"

	got := termsafe.Text(input)

	assert.Equal(t, "first line\nsecond  line\nthird line", got)
	assert.Len(t, strings.Split(got, "\n"), 3)
	assert.NotContains(t, got, "aGk=")
}

func TestProperties(t *testing.T) {
	for _, tc := range corpus() {
		t.Run(tc.name, func(t *testing.T) {
			cell := termsafe.Cell(tc.input)

			assert.LessOrEqual(t, utf8.RuneCountInString(cell), utf8.RuneCountInString(tc.input), "Cell must never grow a value")
			assert.Equal(t, cell, termsafe.Cell(cell), "Cell must be idempotent")

			if utf8.ValidString(tc.input) {
				assert.Equal(t, utf8.RuneCountInString(tc.input), utf8.RuneCountInString(termsafe.Runes(tc.input)))
			}
		})
	}
}
