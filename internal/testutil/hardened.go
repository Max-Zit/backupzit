package testutil

import (
	"io"
	"log/slog"
	"net/http/httptest"

	"github.com/max-zit/backupzit/internal/hardened"
	"github.com/max-zit/backupzit/internal/tlsutil"
)

// HardenedServer is an in-process hardened repository for tests.
type HardenedServer struct {
	*httptest.Server
	Store       *hardened.Store
	Host        string // host:port
	Key         string
	Fingerprint string
}

// StartHardened starts a hardened repository storing files in dir.
func StartHardened(dir string, lockDays int) (*HardenedServer, error) {
	store, err := hardened.OpenStore(dir, lockDays)
	if err != nil {
		return nil, err
	}
	keys := hardened.NewKeys(dir + ".keys")
	key, err := keys.Add("test")
	if err != nil {
		return nil, err
	}
	srv := &hardened.Server{Store: store, Keys: keys, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Version: "test"}
	ts := httptest.NewTLSServer(srv.Handler())
	return &HardenedServer{Server: ts, Store: store, Host: ts.Listener.Addr().String(), Key: key,
		Fingerprint: tlsutil.FingerprintDER(ts.Certificate().Raw)}, nil
}
