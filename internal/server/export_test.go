package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/netcfg"
	"github.com/max-zit/backupzit/internal/update"
)

// EnableUpdateHelper pretends the root update helper is installed (tests).
func (s *Server) EnableUpdateHelper() { s.upd.helper = true }

// PackageFormat is the package type the update installs on this machine.
var PackageFormat = packageFormat

// QueueDueSQLLogs is queueDueSQLLogs for tests.
func (s *Store) QueueDueSQLLogs(ctx context.Context, now time.Time) ([]int64, error) {
	return s.queueDueSQLLogs(ctx, now)
}

// APIRun is what the agent gets for a run (tests).
func (s *Server) APIRun(ctx context.Context, runID int64) (*api.Run, error) {
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	return s.toAPIRun(ctx, &run)
}

// UseNetSystem replaces the system whose network settings the console shows.
func UseNetSystem(s *netcfg.System) func() {
	saved := netSystem
	netSystem = s
	return func() { netSystem = saved }
}

// SetAvailableUpdate pretends the release check found version (tests).
func (s *Server) SetAvailableUpdate(version string) {
	s.upd.mu.Lock()
	s.upd.available = &update.Manifest{Version: version}
	s.upd.mu.Unlock()
}

// SetOrphanGrace changes how long a run may be missing from the agent's
// list before it is failed (tests); it returns the old value.
func SetOrphanGrace(d time.Duration) time.Duration {
	old := orphanGrace
	orphanGrace = d
	return old
}

// ChainAfterRun is chainAfterRun for tests.
func (s *Store) ChainAfterRun(ctx context.Context, runID int64) ([]int64, error) {
	return s.chainAfterRun(ctx, runID)
}

// SetLog replaces the console log (tests that read it).
func (s *Server) SetLog(l *slog.Logger) { s.log = l }

// AgentRepoURLs are the repositories agent removal treats as the agent's.
func (s *Store) AgentRepoURLs(ctx context.Context, agentID int64) []string {
	repos, _ := s.agentRepos(ctx, agentID)
	var out []string
	for _, r := range repos {
		out = append(out, r.URL)
	}
	return out
}

// TrustChatTLS makes the webhook client trust the test server's
// certificate (as http.DefaultTransport did before A18); it returns a
// function that restores it.
func TrustChatTLS(c *http.Client) func() {
	tr := chatClient.Transport.(*http.Transport)
	old := tr.TLSClientConfig
	tr.TLSClientConfig = c.Transport.(*http.Transport).TLSClientConfig.Clone()
	return func() { tr.TLSClientConfig = old }
}
