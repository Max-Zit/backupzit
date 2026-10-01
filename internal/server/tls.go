package server

import "github.com/backupzit/backupzit/internal/tlsutil"

// Certificate helpers, kept here for the server command.
var (
	LoadOrCreateCert = tlsutil.LoadOrCreateCert
	CertFingerprint  = tlsutil.CertFingerprint
)
