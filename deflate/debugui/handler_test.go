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
		"/api/producer?offset=1",
		"/api/input-anchor?bit=48",
		"/api/decode?id=0",
		"/api/block?id=0",
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

func TestStoredBlockHeader(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/block?id=0", nil)
	response := httptest.NewRecorder()
	Handler(testTrace(t)).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var view blockDetailView
	if err := json.NewDecoder(response.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	levels := make(map[string]bool)
	for _, field := range view.Fields {
		levels[field.Level] = true
	}
	for _, level := range []string{"BFINAL", "BTYPE", "alignment", "LEN", "NLEN"} {
		if !levels[level] {
			t.Fatalf("stored header missing %q: %#v", level, view.Fields)
		}
	}
}

func TestWorkbench(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()
	Handler(testTrace(t)).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	for _, want := range []string{"inside deflate", "bits DEFLATE reads", "text those bits produce", "why this step works", "viewAscii", "linkScroll", "inspectRelation", "inspectOrigin", "inspectDecode", "copy earlier output", "how the bits get meaning", "block-boundary", "pageInput(direction)", "pageOutput(direction)"} {
		if !bytes.Contains(response.Body.Bytes(), []byte(want)) {
			t.Errorf("workbench HTML does not contain %q", want)
		}
	}
}

func TestWindowsIncludeScrollAnchors(t *testing.T) {
	for _, path := range []string{
		"/api/input?start=40&length=24",
		"/api/output?start=0&length=3",
	} {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			response := httptest.NewRecorder()
			Handler(testTrace(t)).ServeHTTP(response, request)
			var value struct {
				Anchors [][5]uint64 `json:"anchors"`
			}
			if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
				t.Fatal(err)
			}
			if len(value.Anchors) == 0 {
				t.Fatal("window has no scroll anchors")
			}
			anchor := value.Anchors[0]
			if anchor[1] != 40 || anchor[2] != 64 || anchor[3] != 0 || anchor[4] != 3 {
				t.Fatalf("anchor = %v, want input [40,64) and output [0,3)", anchor)
			}
		})
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
	blockRequest := httptest.NewRequest(http.MethodGet, "/api/block?id="+strconv.Itoa(view.BlockID), nil)
	blockResponse := httptest.NewRecorder()
	Handler(trace).ServeHTTP(blockResponse, blockRequest)
	var blockView blockDetailView
	if err := json.NewDecoder(blockResponse.Body).Decode(&blockView); err != nil {
		t.Fatal(err)
	}
	if len(blockView.Fields) != 5 || blockView.Runs == 0 {
		t.Fatalf("dynamic block header = %#v, want five primary fields and length runs", blockView)
	}
	wantLayers := []string{"hclen", "codelen", "program", "litlen-lengths", "distance-lengths", "litlen-tree", "distance-tree"}
	if len(blockView.Layers) != len(wantLayers) {
		t.Fatalf("dynamic layers = %d, want %d", len(blockView.Layers), len(wantLayers))
	}
	for i, want := range wantLayers {
		if got := blockView.Layers[i].ID; got != want {
			t.Fatalf("layer %d = %q, want %q", i, got, want)
		}
		if len(blockView.Layers[i].Items) == 0 {
			t.Fatalf("layer %q has no items", want)
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
