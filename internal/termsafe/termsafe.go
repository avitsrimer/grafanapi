// Package termsafe neutralises terminal control in server-controlled text before it is written to a
// terminal.
//
// A log line, label value, datasource name or error message that comes back from a server is
// arbitrary input, and a terminal interprets more of it than a reader expects:
//
//   - a tab or newline inside a table cell is read by the tabwriter as a column or row terminator,
//     silently shifting or splitting the row;
//   - a bidirectional override or isolate (U+202E and friends) makes a terminal re-order the rest
//     of the line without changing a byte of the layout;
//   - an OSC sequence (for example OSC 52) can write the clipboard or the window title, and a CSI
//     sequence can clear the screen or forge earlier output;
//   - Unicode tag characters (the U+E0000 block, category Cf) are invisible, which makes them a
//     channel for text that only an agent reading the output can see.
//
// [Cell] and [Text] therefore drop every escape sequence whole (CSI, OSC, DCS, SOS, PM, APC and
// the other ESC-introduced sequences, in both the 7-bit and the 8-bit C1 form) and then replace
// each remaining control or format rune with a space. Dropping the whole sequence, rather than
// only its introducer, also removes the payload: an OSC 52 base64 blob or a forged title string
// stays readable text otherwise. A string sequence (OSC, DCS, SOS, PM, APC) with no terminator is
// dropped to the end of the value, which hides exactly what a terminal would have swallowed as
// the payload, and means an attacker cannot smuggle text through by omitting the terminator.
//
// [Runes] is the per-rune rule alone, with no sequence parsing. It exists so the alerts table
// cells, which were written against that rule, stay byte-identical; new callers use [Cell].
//
// Not covered, deliberately: U+2028/U+2029 and variation selectors, which terminals do not act on
// as control, and plain-text prompt injection, which no filter can remove. The json and yaml
// codecs must NOT use this package: they carry raw values for machines, and callers of those
// formats are told to treat the values as untrusted.
package termsafe

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	esc = '\x1b'
	bel = '\a'
	can = '\x18'
	sub = '\x1a'

	c1DCS = '\u0090'
	c1SOS = '\u0098'
	c1CSI = '\u009b'
	c1ST  = '\u009c'
	c1OSC = '\u009d'
	c1PM  = '\u009e'
	c1APC = '\u009f'

	paramLow, paramHigh   = '0', '?'
	interLow, interHigh   = ' ', '/'
	finalLow, finalHigh   = '@', '~'
	escFinalLow           = '0'
	escSingleByteSequence = 1
)

// Cell drops every escape sequence (see the package doc) and replaces every remaining control
// character (C0 including ESC, CR, LF and TAB, DEL, C1) and Unicode format character (Cf: bidi
// overrides, isolates, tag characters) with a space. Invalid UTF-8 bytes become U+FFFD. Use it
// for table cells and single-line fields.
func Cell(value string) string {
	return sanitize(value, false)
}

// Text is [Cell] for multi-line messages: '\n' and '\t' are kept, since there newlines are
// structure and a tab is inert. '\r' is still replaced, because it lets a value overwrite the
// start of the line and forge output.
func Text(value string) string {
	return sanitize(value, true)
}

// Runes replaces each control or format rune with a space and parses no sequences. It is the rule
// the alerts table cells were written against and is kept byte-identical for them; new callers
// use [Cell]. The rune count is preserved for valid UTF-8.
func Runes(value string) string {
	return strings.Map(func(r rune) rune {
		if isUnsafe(r) {
			return ' '
		}

		return r
	}, value)
}

func isUnsafe(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
}

// sanitize walks value rune by rune. At an introducer it skips the whole sequence; otherwise it
// writes the rune, replaced by a space when it is unsafe (except '\n' and '\t' under keepLayout).
// utf8.RuneError is written as U+FFFD, matching strings.Map.
func sanitize(value string, keepLayout bool) string {
	var out strings.Builder

	out.Grow(len(value))

	for i := 0; i < len(value); {
		if n := sequenceLen(value[i:]); n > 0 {
			i += n

			continue
		}

		r, width := utf8.DecodeRuneInString(value[i:])
		i += width

		switch {
		case keepLayout && (r == '\n' || r == '\t'):
			out.WriteRune(r)
		case isUnsafe(r):
			out.WriteRune(' ')
		default:
			out.WriteRune(r)
		}
	}

	return out.String()
}

// sequenceLen returns how many bytes of s form one escape sequence starting at s[0], or 0 when s
// does not start with an introducer. A raw invalid byte (such as a lone 0x9b) is never one.
func sequenceLen(s string) int {
	r, width := utf8.DecodeRuneInString(s)

	switch r {
	case esc:
		return escapeLen(s)
	case c1CSI:
		return csiLen(s, width)
	case c1OSC, c1DCS, c1SOS, c1PM, c1APC:
		return stringLen(s, width)
	default:
		return 0
	}
}

// escapeLen handles a sequence introduced by the 7-bit ESC at s[0].
func escapeLen(s string) int {
	if len(s) == escSingleByteSequence {
		return escSingleByteSequence
	}

	switch s[1] {
	case '[':
		return csiLen(s, 2)
	case ']', 'P', 'X', '^', '_':
		return stringLen(s, 2)
	}

	// nF/Fp/Fe/Fs: intermediate bytes, then one final byte (ESC ( B, ESC 7, ESC c, ESC \).
	i := escSingleByteSequence
	for i < len(s) && s[i] >= interLow && s[i] <= interHigh {
		i++
	}

	if i < len(s) && s[i] >= escFinalLow && s[i] <= finalHigh {
		return i + 1
	}

	// ESC followed by anything else: drop the ESC alone and read the next rune normally.
	return escSingleByteSequence
}

// csiLen returns the length of a CSI sequence whose parameter bytes start at s[start]: parameter
// bytes, intermediate bytes, one final byte. A rune that cannot continue the sequence ends it
// there and is left for the caller to read.
func csiLen(s string, start int) int {
	i := start
	for i < len(s) && s[i] >= paramLow && s[i] <= paramHigh {
		i++
	}

	for i < len(s) && s[i] >= interLow && s[i] <= interHigh {
		i++
	}

	if i < len(s) && s[i] >= finalLow && s[i] <= finalHigh {
		return i + 1
	}

	return i
}

// stringLen returns the length of a string sequence (OSC, DCS, SOS, PM, APC) whose payload starts
// at s[start]. It ends at BEL, ST (ESC \ or U+009C), or CAN/SUB, which abort a sequence; the
// terminator is dropped with it. An ESC that does not start ST ends the string and is left to be
// read as a new introducer. With no terminator the rest of the value is dropped.
func stringLen(s string, start int) int {
	for i := start; i < len(s); {
		r, width := utf8.DecodeRuneInString(s[i:])

		switch r {
		case bel, can, sub, c1ST:
			return i + width
		case esc:
			if i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}

			return i
		}

		i += width
	}

	return len(s)
}
