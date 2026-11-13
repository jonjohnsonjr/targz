package debugui

import (
	"bytes"
	"compress/flate"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/jonjohnsonjr/targz/deflate"
)

func testTrace(t *testing.T) *deflate.Trace {
	t.Helper()
	// One final stored block containing "abc".
	trace, err := deflate.ParseRaw([]byte{0x01, 0x03, 0x00, 0xfc, 0xff, 'a', 'b', 'c'})
	if err != nil {
		t.Fatal(err)
	}
	return trace
}

func TestAPI(t *testing.T) {
	server := httptest.NewServer(Handler(testTrace(t)))
	defer server.Close()

	for _, path := range []string{
		"/api/summary",
		"/api/ops?start=0&limit=10",
		"/api/op?id=0",
		"/api/decode?id=0",
		"/api/origin?offset=1",
		"/api/uses?start=0&end=1",
		"/api/input?start=0&length=16",
		"/api/output?start=0&length=3",
	} {
		t.Run(path, func(t *testing.T) {
			response, err := http.Get(server.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %s", response.Status)
			}
			var value any
			if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWorkbench(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()
	Handler(testTrace(t)).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	for _, want := range []string{"deflate.inspect", "decoded output", "relationships / decoding", "copy map", "decode stack"} {
		if !bytes.Contains(response.Body.Bytes(), []byte(want)) {
			t.Errorf("workbench HTML does not contain %q", want)
		}
	}
}

func TestDynamicDecodeStack(t *testing.T) {
	input := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog; "), 200)
	var compressed bytes.Buffer
	w, err := flate.NewWriter(&compressed, flate.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(input); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	trace, err := deflate.ParseRaw(compressed.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	matchID := -1
	for _, op := range trace.Ops {
		if op.Kind == deflate.MatchOp && trace.Blocks[op.BlockID].Dynamic != nil {
			matchID = op.ID
			break
		}
	}
	if matchID < 0 {
		t.Fatal("fixture contains no match in a dynamic block")
	}

	request := httptest.NewRequest(http.MethodGet, "/api/decode?id="+strconv.Itoa(matchID), nil)
	response := httptest.NewRecorder()
	Handler(trace).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var view decodeView
	if err := json.NewDecoder(response.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	if len(view.Branches) != 2 || len(view.Header) != 3 {
		t.Fatalf("decode view = %#v, want length/distance branches and dynamic header", view)
	}
	for _, branch := range view.Branches {
		levels := make(map[string]bool)
		for _, stage := range branch.Stages {
			levels[stage.Level] = true
		}
		for _, level := range []string{"payload bits", "expanded lengths", "length program", "code-length tree", "HCLEN vector"} {
			if !levels[level] {
				t.Fatalf("%s branch missing %q: %#v", branch.Name, level, branch.Stages)
			}
		}
	}
}

func TestAPIErrorsAreJSON(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/origin?offset=99", nil)
	response := httptest.NewRecorder()
	Handler(testTrace(t)).ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
	}
	if got := response.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q", got)
	}
}
