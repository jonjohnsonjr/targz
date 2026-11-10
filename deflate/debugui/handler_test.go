package debugui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		"/api/origin?offset=1",
		"/api/uses?start=0&end=1",
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
