package lsp

import (
	"fmt"
	"strings"
)

// rfcAnchor gives a stable semantic name to a location in the vendored RFC.
// TargetRange keeps enough surrounding prose for an editor preview while
// Selection is the exact token on which Go to Definition should place the
// cursor.
type rfcAnchor struct {
	TargetRange lspRange
	Selection   lspRange
}

var rfcAnchors = func() map[string]rfcAnchor {
	anchors := map[string]rfcAnchor{
		"fixed-lit-0-143":   rfcLineAnchor(654, 670, 662, 21, 28),
		"fixed-lit-144-255": rfcLineAnchor(654, 670, 664, 19, 28),
		"fixed-lit-256-279": rfcLineAnchor(654, 670, 666, 19, 28),
		"fixed-lit-280-287": rfcLineAnchor(654, 670, 668, 19, 28),
		"fixed-distance":    rfcLineAnchor(678, 688, 684, 9, 28),

		"term-bfinal":         rfcLineAnchor(489, 519, 510, 9, 15),
		"term-btype":          rfcLineAnchor(489, 519, 513, 9, 14),
		"term-literal-length": rfcLineAnchor(520, 551, 521, 31, 45),
		"term-distance":       rfcLineAnchor(520, 551, 521, 50, 58),
		"term-code-length":    rfcLineAnchor(689, 711, 697, 47, 72),
		"term-hlit":           rfcLineAnchor(722, 726, 723, 23, 27),
		"term-hdist":          rfcLineAnchor(722, 726, 724, 23, 28),
		"term-hclen":          rfcLineAnchor(722, 737, 725, 23, 28),
		"term-len":            rfcLineAnchor(575, 587, 585, 9, 12),
		"term-nlen":           rfcLineAnchor(575, 587, 585, 56, 60),
		"repeat-extra-16":     rfcLineAnchor(700, 706, 702, 32, 38),
		"repeat-extra-17":     rfcLineAnchor(707, 709, 708, 24, 30),
		"repeat-extra-18":     rfcLineAnchor(709, 711, 710, 24, 30),
		"section-3.2.1":       rfcLineAnchor(312, 370, 312, 6, 11),
		"section-3.2.2":       rfcLineAnchor(371, 488, 371, 6, 11),
		"section-3.2.3":       rfcLineAnchor(489, 574, 489, 6, 11),
		"section-3.2.4":       rfcLineAnchor(575, 587, 575, 6, 11),
		"section-3.2.5":       rfcLineAnchor(588, 653, 588, 6, 11),
		"section-3.2.6":       rfcLineAnchor(654, 688, 654, 6, 11),
		"section-3.2.7":       rfcLineAnchor(689, 759, 689, 6, 11),
	}
	// RFC 1951 §3.2.7 splits the mandated HCLEN permutation over two
	// physical lines. These coordinates select each symbol token, rather
	// than merely dropping the cursor at the start of the paragraph.
	order := []struct {
		line, start, end int
	}{
		{735, 59, 61}, {735, 63, 65}, {735, 67, 69},
		{736, 18, 19}, {736, 21, 22}, {736, 24, 25}, {736, 27, 28},
		{736, 30, 31}, {736, 33, 35}, {736, 37, 38}, {736, 40, 42},
		{736, 44, 45}, {736, 47, 49}, {736, 51, 52}, {736, 54, 56},
		{736, 58, 59}, {736, 61, 63}, {736, 65, 66}, {736, 68, 70},
	}
	for field, token := range order {
		anchors[hclenRFCAnchor(field)] = rfcLineAnchor(689, 750, token.line, token.start, token.end)
	}
	for symbol := 257; symbol <= 285; symbol++ {
		row, start := 0, 0
		switch {
		case symbol <= 266:
			row, start = 625+(symbol-257), 19
		case symbol <= 276:
			row, start = 625+(symbol-267), 39
		default:
			row, start = 625+(symbol-277), 59
		}
		anchors[lengthExtraRFCAnchor(symbol)] = rfcLineAnchor(622, 635, row, start, start+1)
	}
	for symbol := 0; symbol <= 29; symbol++ {
		row, start := 643+(symbol%10), 19
		if symbol >= 20 {
			start = 56
		} else if symbol >= 10 {
			start = 35
		}
		width := 1
		if symbol >= 22 {
			start--
			width = 2
		}
		anchors[distanceExtraRFCAnchor(symbol)] = rfcLineAnchor(640, 653, row, start, start+width)
	}
	return anchors
}()

type rfcTerm struct {
	Text   string
	Anchor string
}

// Ordered longest-first so a cursor on "literal/length alphabet" resolves
// the concept, rather than a shorter word embedded in it.
var rfcTerms = []rfcTerm{
	{"alphabet for code lengths", "term-code-length"},
	{"code length huffman", "term-code-length"},
	{"code length alphabet", "term-code-length"},
	{"literal/length alphabet", "term-literal-length"},
	{"literal/length", "term-literal-length"},
	{"distance alphabet", "term-distance"},
	{"3.2.1", "section-3.2.1"},
	{"3.2.2", "section-3.2.2"},
	{"3.2.3", "section-3.2.3"},
	{"3.2.4", "section-3.2.4"},
	{"3.2.5", "section-3.2.5"},
	{"3.2.6", "section-3.2.6"},
	{"3.2.7", "section-3.2.7"},
	{"bfinal", "term-bfinal"},
	{"btype", "term-btype"},
	{"hdist", "term-hdist"},
	{"hclen", "term-hclen"},
	{"hlit", "term-hlit"},
}

func (d *document) rfcTermAt(at position) (lspRange, string, bool) {
	if at.Line < 0 || at.Line >= len(d.rfcLines) {
		return lspRange{}, "", false
	}
	line := strings.ToLower(d.rfcLines[at.Line])
	for _, term := range rfcTerms {
		for from := 0; from <= len(line)-len(term.Text); {
			relative := strings.Index(line[from:], term.Text)
			if relative < 0 {
				break
			}
			start := from + relative
			origin := lspRange{
				Start: position{Line: at.Line, Character: start},
				End:   position{Line: at.Line, Character: start + len(term.Text)},
			}
			if containsPosition(origin, at) {
				return origin, term.Anchor, true
			}
			from = start + 1
		}
	}
	return lspRange{}, "", false
}

func rfcLineAnchor(contextStart, contextEnd, line, start, end int) rfcAnchor {
	return rfcAnchor{
		TargetRange: lspRange{Start: position{Line: contextStart}, End: position{Line: contextEnd}},
		Selection: lspRange{
			Start: position{Line: line, Character: start},
			End:   position{Line: line, Character: end},
		},
	}
}

func fixedLiteralRFCAnchor(symbol int) string {
	switch {
	case symbol <= 143:
		return "fixed-lit-0-143"
	case symbol <= 255:
		return "fixed-lit-144-255"
	case symbol <= 279:
		return "fixed-lit-256-279"
	default:
		return "fixed-lit-280-287"
	}
}

func hclenRFCAnchor(field int) string { return fmt.Sprintf("hclen-order-%d", field) }
func lengthExtraRFCAnchor(symbol int) string {
	return fmt.Sprintf("length-extra-%d", symbol)
}
func distanceExtraRFCAnchor(symbol int) string {
	return fmt.Sprintf("distance-extra-%d", symbol)
}
func codeLengthExtraRFCAnchor(symbol int) string {
	return fmt.Sprintf("repeat-extra-%d", symbol)
}
