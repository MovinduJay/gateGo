package buildinfo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandlerReturnsBuildIdentity(t *testing.T) {
	previousVersion, previousCommit, previousDate := Version, Commit, Date
	Version, Commit, Date = "1.2.3", "abc123", "2026-09-23T00:00:00Z"
	t.Cleanup(func() { Version, Commit, Date = previousVersion, previousCommit, previousDate })

	rr := httptest.NewRecorder()
	Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://gateway/version", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var got Info
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != "1.2.3" || got.Commit != "abc123" || got.BuildDate != "2026-09-23T00:00:00Z" || got.GoVersion == "" {
		t.Fatalf("unexpected build info: %+v", got)
	}
}

func TestHandlerRejectsOtherMethods(t *testing.T) {
	rr := httptest.NewRecorder()
	Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "http://gateway/version", nil))
	if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("status=%d allow=%q", rr.Code, rr.Header().Get("Allow"))
	}
}
