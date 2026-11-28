package hardened

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// L-24 (0.63): a file that is not on the repository no longer ends Keep.
// The files after it in the same request are extended too, and the answer
// names the missing one.
func TestKeepGoesOnPastMissingFiles(t *testing.T) {
	ts := newTestServer(t)
	s := ts.srv.Store
	now := time.Now()
	s.now = func() time.Time { return now }
	names := []string{"pc1/data/aa", "pc1/data/bb", "pc1/data/cc"}
	for _, n := range names {
		if err := s.Save(n, strings.NewReader("pack "+n)); err != nil {
			t.Fatal(err)
		}
	}
	// Three days later every file's lock ends within the period.
	now = now.Add(3 * 24 * time.Hour)
	ask := []string{"pc1/data/aa", "pc1/data/gone1", "pc1/data/bb", "pc1/data/gone2", "pc1/data/cc"}
	n, missing, err := s.Keep(ask)
	if err != nil || n != 3 {
		t.Fatalf("keep: %d extended, %v", n, err)
	}
	if strings.Join(missing, ",") != "pc1/data/gone1,pc1/data/gone2" {
		t.Errorf("missing: %v", missing)
	}
	want := now.Add(7 * 24 * time.Hour)
	for _, name := range names {
		fi, err := os.Stat(s.path(name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.ModTime().Before(want) {
			t.Errorf("%s retained until %v, want after %v", name, fi.ModTime(), want)
		}
	}

	// Through the service: 200 with the count and the names, not 404.
	now = now.Add(5 * 24 * time.Hour)
	tok := ts.key("pc1", Limits{Path: "pc1"})
	body, _ := json.Marshal(KeepRequest{Names: ask})
	w := ts.do("POST", "/v1/keep", tok, body)
	if w.Code != http.StatusOK {
		t.Fatalf("keep request: %d %s", w.Code, w.Body)
	}
	var kr KeepResponse
	if err := json.Unmarshal(w.Body.Bytes(), &kr); err != nil {
		t.Fatal(err)
	}
	if kr.Extended != 3 || kr.Missing != 2 || len(kr.MissingNames) != 2 || kr.MissingNames[0] != "pc1/data/gone1" {
		t.Errorf("answer: %+v", kr)
	}
}

// The answer lists at most maxMissingNames names, but counts all of them.
func TestKeepAnswerClipsMissingNames(t *testing.T) {
	ts := newTestServer(t)
	tok := ts.key("pc1", Limits{Path: "pc1"})
	var ask []string
	for i := range 3 * maxMissingNames {
		ask = append(ask, "pc1/data/missing"+strings.Repeat("x", i+1))
	}
	body, _ := json.Marshal(KeepRequest{Names: ask})
	w := ts.do("POST", "/v1/keep", tok, body)
	var kr KeepResponse
	if err := json.Unmarshal(w.Body.Bytes(), &kr); err != nil || w.Code != http.StatusOK {
		t.Fatalf("keep request: %d %s", w.Code, w.Body)
	}
	if kr.Missing != len(ask) || len(kr.MissingNames) != maxMissingNames {
		t.Errorf("answer: %d missing, %d names", kr.Missing, len(kr.MissingNames))
	}
}
