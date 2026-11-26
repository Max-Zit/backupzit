//go:build !darwin && !windows

package main

import "github.com/kardianos/service"

// installNeedsEnrollment: "service install" is for an enrolled agent (the
// packages install the service themselves).
const installNeedsEnrollment = true

// platformService: kardianos/service installs and controls the service.
func platformService(s service.Service, _ []string, _ *program) service.Service { return s }
