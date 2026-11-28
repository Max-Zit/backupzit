package backend

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeKeepService answers POST /v1/keep like a hardened repository: names
// containing "gone" are not there. old answers 404 for the whole request,
// as services before 0.63 do.
func fakeKeepService(t *testing.T, old bool) (*Hardened, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/keep" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		var req struct {
			Names []string `json:"names"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var resp struct {
			Extended     int      `json:"extended"`
			Missing      int      `json:"missing,omitempty"`
			MissingNames []string `json:"missing_names,omitempty"`
		}
		for _, n := range req.Names {
			if !strings.HasPrefix(n, "p/") {
				t.Errorf("name without the prefix: %s", n)
			}
			if strings.Contains(n, "gone") {
				if old {
					http.Error(w, n+": not found", http.StatusNotFound)
					return
				}
				resp.Missing++
				resp.MissingNames = append(resp.MissingNames, n)
				continue
			}
			resp.Extended++
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return &Hardened{base: srv.URL + "/v1/", prefix: "p", key: "k", client: srv.Client(), loc: "hardened://test/p", lockDays: 7}, &calls
}

// L-24 (0.63): files that are not on the hardened repository are reported,
// and the others of every batch are extended; the step does not stop.
func TestHardenedKeepReportsMissingFiles(t *testing.T) {
	h, calls := fakeKeepService(t, false)
	var names []string
	for i := range 4500 { // three batches, each with a missing file
		if i%2000 == 7 {
			names = append(names, "data/gone"+string(rune('a'+i/2000)))
			continue
		}
		names = append(names, "data/ok")
	}
	n, err := h.KeepLocked(context.Background(), names)
	if n != len(names)-3 || calls.Load() != 3 {
		t.Errorf("extended %d of %d in %d requests", n, len(names), calls.Load())
	}
	if err == nil || errors.Is(err, ErrKeepStopped) {
		t.Fatalf("err = %v, want a report of the missing files that does not stop the step", err)
	}
	if !strings.Contains(err.Error(), "3 of 4500 files") || !strings.Contains(err.Error(), "first: data/gonea") {
		t.Errorf("message: %v", err)
	}
}

// A service before 0.63 refuses the whole batch with 404: counted as failed,
// as before.
func TestHardenedKeepOldServiceMissingFile(t *testing.T) {
	h, _ := fakeKeepService(t, true)
	n, err := h.KeepLocked(context.Background(), []string{"data/ok", "data/gone", "data/ok2"})
	if n != 0 || err == nil || !strings.Contains(err.Error(), "the lock of 3 of 3 files could not be extended") {
		t.Errorf("old service: %d %v", n, err)
	}
}
