package agent

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/tlsutil"
)

// LOW 3: when the console stored a finished update or uninstall but its
// answer was lost, the repeated report is refused (404 for the finished
// run, 401 for the agent removed by its uninstall). The installer still
// runs, once; a refusal without a lost answer before does not start it.
func TestReportAfterLostAnswer(t *testing.T) {
	orig := reportRetryWait
	reportRetryWait = 10 * time.Millisecond
	defer func() { reportRetryWait = orig }()
	cert, err := tlsutil.LoadOrCreateCert(t.TempDir(), []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	// answers: what the console does for each attempt; "lost" reads the
	// report and cuts the connection without an answer.
	serve := func(t *testing.T, answers ...string) (*Agent, *atomic.Int32) {
		var n atomic.Int32
		s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			i := int(n.Add(1)) - 1
			if i >= len(answers) {
				i = len(answers) - 1
			}
			switch answers[i] {
			case "lost":
				c, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = c.Close()
				}
			case "ok":
				_ = json.NewEncoder(w).Encode(struct{}{})
			case "404":
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(api.Error{Error: "run not found or not running"})
			case "401":
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(api.Error{Error: "unknown agent"})
			}
		}))
		s.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
		s.StartTLS()
		t.Cleanup(s.Close)
		a := New(&Config{ServerURL: s.URL, Fingerprint: tlsutil.CertFingerprint(cert), AgentUUID: "u", Secret: "s"}, slog.New(slog.DiscardHandler), "test")
		return a, &n
	}
	ok := api.RunResult{Status: api.StatusSuccess}
	for _, tc := range []struct {
		name    string
		kind    string
		answers []string
		after   int32
	}{
		{"answered", api.KindAgentUpdate, []string{"ok"}, 1},
		{"update: answer lost, then finished", api.KindAgentUpdate, []string{"lost", "404"}, 1},
		{"uninstall: answer lost, then agent removed", api.KindAgentUninstall, []string{"lost", "401"}, 1},
		{"uninstall: answer lost, then finished", api.KindAgentUninstall, []string{"lost", "lost", "404"}, 1},
		{"refused without a lost answer", api.KindAgentUpdate, []string{"404"}, 0},
		{"answer lost, then accepted", api.KindAgentUpdate, []string{"lost", "ok"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, n := serve(t, tc.answers...)
			var after atomic.Int32
			done := make(chan struct{})
			go func() {
				defer close(done)
				a.report(context.Background(), api.Run{ID: 7, Kind: tc.kind}, ok, func() { after.Add(1) })
			}()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("report did not end")
			}
			if got := after.Load(); got != tc.after {
				t.Errorf("installer started %d times, want %d (%d reports)", got, tc.after, n.Load())
			}
		})
	}
}

// A report that never left the machine (no connection) is not taken for
// one that may have arrived.
func TestMayHaveArrived(t *testing.T) {
	dial := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	read := &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}
	for err, want := range map[error]bool{
		dial:                                  false,
		&wrapped{dial}:                        false,
		read:                                  true,
		io.EOF:                                true,
		context.DeadlineExceeded:              true,
		errors.New("server: HTTP 502"):        false,
		errors.New("server: unknown agent"):   false,
		errors.New("server: invalid request"): false,
	} {
		if got := mayHaveArrived(err); got != want {
			t.Errorf("%v: %v, want %v", err, got, want)
		}
	}
}

type wrapped struct{ err error }

func (w *wrapped) Error() string { return "Post \"https://console/api\": " + w.err.Error() }
func (w *wrapped) Unwrap() error { return w.err }
