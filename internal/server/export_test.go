package server

// EnableUpdateHelper pretends the root update helper is installed (tests).
func (s *Server) EnableUpdateHelper() { s.upd.helper = true }

// PackageFormat is the package type the update installs on this machine.
var PackageFormat = packageFormat
