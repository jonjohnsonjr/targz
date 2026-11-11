package lsp

import "fmt"

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
	return anchors
}()

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
