package deflate

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"fmt"
	"testing"
)

func rawDeflate(t testing.TB, data []byte, level int) []byte {
	t.Helper()
	var compressed bytes.Buffer
	w, err := flate.NewWriter(&compressed, level)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func TestParseRawMatchesStandardLibrary(t *testing.T) {
	inputs := [][]byte{
		nil,
		[]byte("hello, deflate"),
		bytes.Repeat([]byte("abc"), 1000),
		bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog\n"), 2000),
	}
	levels := []int{
		flate.NoCompression,
		flate.HuffmanOnly,
		flate.BestSpeed,
		flate.DefaultCompression,
		flate.BestCompression,
	}
	for inputIndex, input := range inputs {
		for _, level := range levels {
			t.Run(fmt.Sprintf("input-%d/level-%d", inputIndex, level), func(t *testing.T) {
				compressed := rawDeflate(t, input, level)
				trace, err := ParseRaw(compressed)
				if err != nil {
					t.Fatalf("ParseRaw: %v\ncompressed: %x", err, compressed)
				}
				if !bytes.Equal(trace.Output, input) {
					t.Fatalf("output mismatch: got %d bytes, want %d", len(trace.Output), len(input))
				}
				if len(trace.Blocks) == 0 {
					t.Fatal("trace has no blocks")
				}
				if !trace.Blocks[len(trace.Blocks)-1].Final {
					t.Fatal("last block is not final")
				}
				assertTraceInvariants(t, trace)
			})
		}
	}
}

func TestOverlappingMatchProvenance(t *testing.T) {
	input := bytes.Repeat([]byte("abc"), 400)
	trace, err := ParseRaw(rawDeflate(t, input, flate.BestCompression))
	if err != nil {
		t.Fatal(err)
	}

	var match *Op
	for i := range trace.Ops {
		op := &trace.Ops[i]
		if op.Kind == MatchOp && op.Length > op.Distance {
			match = op
			break
		}
	}
	if match == nil {
		t.Fatal("standard library produced no overlapping match")
	}

	selected := match.Output.End - 1
	steps, err := trace.Origin(selected)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) < 2 {
		t.Fatalf("origin path has %d steps, want at least match and literal", len(steps))
	}
	first := steps[0]
	if first.Op.ID != match.ID {
		t.Fatalf("first origin op = %d, want match %d", first.Op.ID, match.ID)
	}
	seed, _ := match.Seed()
	if !seed.Contains(first.SeedSource) {
		t.Fatalf("normalized source %d is outside seed [%d,%d)", first.SeedSource, seed.Start, seed.End)
	}
	last := steps[len(steps)-1]
	if last.Op.Kind != LiteralOp && last.Op.Kind != StoredOp {
		t.Fatalf("origin terminates at %s, want literal or stored", last.Op.Kind)
	}
	if trace.Output[selected] != trace.Output[last.Output] {
		t.Fatalf("origin byte %#x != selected byte %#x", trace.Output[last.Output], trace.Output[selected])
	}

	uses := trace.Uses(seed)
	found := false
	for _, use := range uses {
		if use.Op.ID == match.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Uses(%v) did not return match %d", seed, match.ID)
	}
}

func TestParseConcatenatedGzip(t *testing.T) {
	var compressed bytes.Buffer
	w := gzip.NewWriter(&compressed)
	w.Name = "first.txt"
	if _, err := w.Write([]byte("first member\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	w.Reset(&compressed)
	w.Name = "second.txt"
	if _, err := w.Write(bytes.Repeat([]byte("second member\n"), 100)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	trace, err := ParseGzip(compressed.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(trace.Members) != 2 {
		t.Fatalf("members = %d, want 2", len(trace.Members))
	}
	if trace.Members[0].Name != "first.txt" || trace.Members[1].Name != "second.txt" {
		t.Fatalf("member names = %q, %q", trace.Members[0].Name, trace.Members[1].Name)
	}
	want := append([]byte("first member\n"), bytes.Repeat([]byte("second member\n"), 100)...)
	if !bytes.Equal(trace.Output, want) {
		t.Fatalf("output mismatch: got %d bytes, want %d", len(trace.Output), len(want))
	}
	assertTraceInvariants(t, trace)
}

func TestParseGzipRejectsCorruptTrailer(t *testing.T) {
	var compressed bytes.Buffer
	w := gzip.NewWriter(&compressed)
	if _, err := w.Write([]byte("checksummed")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b := compressed.Bytes()
	b[len(b)-8] ^= 1
	if _, err := ParseGzip(b); err == nil {
		t.Fatal("ParseGzip accepted a corrupt CRC")
	}
}

func TestStoredByteOrigin(t *testing.T) {
	input := []byte("stored bytes")
	trace, err := ParseRaw(rawDeflate(t, input, flate.NoCompression))
	if err != nil {
		t.Fatal(err)
	}
	steps, err := trace.Origin(3)
	if err != nil {
		t.Fatal(err)
	}
	last := steps[len(steps)-1]
	if last.Op.Kind != StoredOp {
		t.Fatalf("origin kind = %s, want stored", last.Op.Kind)
	}
	if last.StoredInput.Len() != 8 {
		t.Fatalf("stored input span = %d bits, want 8", last.StoredInput.Len())
	}
}

func TestBitStringUsesConsumptionOrder(t *testing.T) {
	trace, err := ParseRaw([]byte{0x01, 0x00, 0x00, 0xff, 0xff})
	if err != nil {
		t.Fatal(err)
	}
	bits, err := trace.BitString(BitRange{Start: 0, End: 8})
	if err != nil {
		t.Fatal(err)
	}
	if bits != "10000000" {
		t.Fatalf("bits = %q, want logical LSB-first order", bits)
	}
}

func TestReverseUseIndexMatchesNaiveScan(t *testing.T) {
	input := bytes.Repeat([]byte("abracadabra banana bandana\n"), 200)
	trace, err := ParseRaw(rawDeflate(t, input, flate.BestCompression))
	if err != nil {
		t.Fatal(err)
	}
	for start := OutPos(0); start < OutPos(len(input)); start += 37 {
		query := OutRange{Start: start, End: minOut(start+19, OutPos(len(input)))}
		got := trace.Uses(query)
		var want []int
		for _, op := range trace.Ops {
			seed, ok := op.Seed()
			if ok && maxOut(seed.Start, query.Start) < minOut(seed.End, query.End) {
				want = append(want, op.ID)
			}
		}
		if len(got) != len(want) {
			t.Fatalf("Uses(%v) returned %d matches, want %d", query, len(got), len(want))
		}
		for i := range want {
			if got[i].Op.ID != want[i] {
				t.Fatalf("Uses(%v)[%d] = op %d, want op %d", query, i, got[i].Op.ID, want[i])
			}
		}
	}
}

func TestProducersPartitionRange(t *testing.T) {
	input := bytes.Repeat([]byte("abc"), 400)
	trace, err := ParseRaw(rawDeflate(t, input, flate.BestCompression))
	if err != nil {
		t.Fatal(err)
	}
	query := OutRange{Start: 1, End: OutPos(len(input) - 1)}
	productions := trace.Producers(query)
	if len(productions) < 2 {
		t.Fatalf("Producers(%v) returned %d operation(s), want several", query, len(productions))
	}
	next := query.Start
	for _, production := range productions {
		if production.Intersection.Start != next {
			t.Fatalf("gap before %v; next byte was %d", production.Intersection, next)
		}
		if !production.Op.Output.Contains(production.Intersection.Start) || production.Intersection.End > production.Op.Output.End {
			t.Fatalf("intersection %v is outside op %d output %v", production.Intersection, production.Op.ID, production.Op.Output)
		}
		next = production.Intersection.End
	}
	if next != query.End {
		t.Fatalf("productions end at %d, want %d", next, query.End)
	}
}

func assertTraceInvariants(t testing.TB, trace *Trace) {
	t.Helper()
	var output OutPos
	for _, op := range trace.Ops {
		if op.Output.Start != output {
			t.Fatalf("op %d output starts at %d, want %d", op.ID, op.Output.Start, output)
		}
		if op.Output.End <= op.Output.Start {
			t.Fatalf("op %d has empty output", op.ID)
		}
		if op.Input.End <= op.Input.Start {
			t.Fatalf("op %d has empty input", op.ID)
		}
		output = op.Output.End
	}
	if output != OutPos(len(trace.Output)) {
		t.Fatalf("operations end at %d, output has %d bytes", output, len(trace.Output))
	}
	for _, block := range trace.Blocks {
		if block.Input.End <= block.Input.Start {
			t.Fatalf("block %d has empty input", block.ID)
		}
		if block.Header.End <= block.Header.Start {
			t.Fatalf("block %d has empty header", block.ID)
		}
	}
}

func FuzzParseRaw(f *testing.F) {
	for _, seed := range [][]byte{
		nil,
		[]byte("a"),
		[]byte("abcabcabcabc"),
		bytes.Repeat([]byte{0, 1, 2, 3, 4}, 100),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > 1<<20 {
			t.Skip()
		}
		compressed := rawDeflate(t, input, flate.DefaultCompression)
		trace, err := ParseRaw(compressed)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(trace.Output, input) {
			t.Fatalf("output mismatch: got %d bytes, want %d", len(trace.Output), len(input))
		}
		assertTraceInvariants(t, trace)
	})
}
