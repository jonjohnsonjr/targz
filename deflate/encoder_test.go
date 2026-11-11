package deflate

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"testing"
)

func TestRewriteLiteralReencodesAffectedMemberAndCopiesOthers(t *testing.T) {
	first := gzipMember(t, bytes.Repeat([]byte("abc"), 400), "first.txt")
	second := gzipMember(t, bytes.Repeat([]byte("untouched"), 100), "second.txt")
	original := append(append([]byte(nil), first...), second...)
	trace, err := ParseGzip(original)
	if err != nil {
		t.Fatal(err)
	}

	literal := -1
	for _, op := range trace.Ops {
		if op.Kind == LiteralOp && op.Literal == 'a' {
			literal = op.ID
			break
		}
	}
	if literal < 0 {
		t.Fatal("fixture contains no literal 'a'")
	}
	rewritten, impact, err := trace.RewriteLiteral(literal, 'x')
	if err != nil {
		t.Fatal(err)
	}
	if impact.OutputBytes != 400 || impact.Members != 1 {
		t.Fatalf("impact = %#v, want 400 bytes in one member", impact)
	}

	parsed, err := ParseGzip(rewritten)
	if err != nil {
		t.Fatalf("parse rewritten gzip: %v", err)
	}
	want := append([]byte(nil), trace.Output...)
	for pos := range want {
		steps, originErr := trace.Origin(OutPos(pos))
		if originErr != nil {
			t.Fatal(originErr)
		}
		if steps[len(steps)-1].Op.ID == literal {
			want[pos] = 'x'
		}
	}
	if !bytes.Equal(parsed.Output, want) {
		t.Fatal("rewritten output does not match provenance rename")
	}

	// The affected member keeps its complete original gzip header, including
	// optional fields. The unaffected member remains entirely byte-identical.
	headerEnd := int(uint64(trace.Members[0].Header.End) / 8)
	if !bytes.Equal(rewritten[:headerEnd], original[:headerEnd]) {
		t.Fatal("affected member header changed")
	}
	secondStart := int(uint64(parsed.Members[1].Input.Start) / 8)
	if !bytes.Equal(rewritten[secondStart:], second) {
		t.Fatal("unaffected gzip member was not copied byte-for-byte")
	}
}

func TestRewriteLiteralRawRoundTrip(t *testing.T) {
	payload := bytes.Repeat([]byte("hello "), 300)
	raw := rawDeflate(t, payload, flate.DefaultCompression)
	trace, err := ParseRaw(raw)
	if err != nil {
		t.Fatal(err)
	}
	literal := -1
	for _, op := range trace.Ops {
		if op.Kind == LiteralOp && op.Literal == 'h' {
			literal = op.ID
			break
		}
	}
	if literal < 0 {
		t.Fatal("fixture contains no literal 'h'")
	}
	rewritten, _, err := trace.RewriteLiteral(literal, 'j')
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseRaw(rewritten)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(parsed.Output, []byte("jello ")) {
		t.Fatalf("rewritten output starts %q, want jello", parsed.Output[:6])
	}
}

func gzipMember(t *testing.T, payload []byte, name string) []byte {
	t.Helper()
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	w.Name = name
	w.Comment = "gzpls encoder test"
	w.Extra = []byte{1, 2, 3, 4}
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
