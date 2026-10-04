package server

import "github.com/max-zit/backupzit/internal/tlsutil"

// Certificate helpers, kept here for the server command.
var (
	LoadOrCreateCert = tlsutil.LoadOrCreateCert
	CertFingerprint  = tlsutil.CertFingerprint
)
