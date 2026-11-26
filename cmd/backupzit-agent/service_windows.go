//go:build windows

package main

import (
	"time"
	"unsafe"

	"github.com/kardianos/service"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// installNeedsEnrollment: "service install" is for an enrolled agent (the
// MSI installs the service itself).
const installNeedsEnrollment = true

// preshutdownTimeout is how long Windows waits for the service after the
// pre-shutdown notice before it shuts down anyway.
const preshutdownTimeout = 3 * time.Minute

// platformService: kardianos/service installs and controls the service;
// running it under the service manager goes through winHandler, which also
// accepts the pre-shutdown notice (#74). With only the shutdown notice,
// Windows gave the agent a few seconds, often after the network was gone:
// a running backup was cut off without a word to the console. The
// pre-shutdown notice comes before that, while the network still works,
// so the agent stops the run, unlocks the repository and reports.
func platformService(s service.Service, _ []string, p *program) service.Service {
	return &winService{Service: s, p: p}
}

type winService struct {
	service.Service
	p *program // the program the handler starts and stops
}

func (w *winService) Run() error {
	if is, err := svc.IsWindowsService(); err != nil || !is || w.p == nil {
		return w.Service.Run() // in the foreground
	}
	return svc.Run(serviceName, &winHandler{s: w, p: w.p})
}

type winHandler struct {
	s service.Service
	p *program
}

func (h *winHandler) Execute(_ []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const accepts = svc.AcceptStop | svc.AcceptShutdown | svc.AcceptPreShutdown
	changes <- svc.Status{State: svc.StartPending}
	if err := h.p.Start(h.s); err != nil {
		return true, 1
	}
	setPreshutdownTimeout()
	changes <- svc.Status{State: svc.Running, Accepts: accepts}
	for c := range r {
		switch c.Cmd {
		case svc.Interrogate:
			changes <- c.CurrentStatus
		case svc.Stop, svc.Shutdown, svc.PreShutdown:
			h.stop(changes)
			return false, 0
		}
	}
	return false, 0
}

// stop stops the agent and keeps telling the service manager that it is
// still stopping, so Windows waits for it (up to preshutdownTimeout).
func (h *winHandler) stop(changes chan<- svc.Status) {
	done := make(chan struct{})
	go func() {
		_ = h.p.Stop(h.s)
		close(done)
	}()
	for checkpoint := uint32(1); ; checkpoint++ {
		changes <- svc.Status{State: svc.StopPending, CheckPoint: checkpoint, WaitHint: 20000}
		select {
		case <-done:
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// serviceConfigPreshutdownInfo is SERVICE_PRESHUTDOWN_INFO.
type serviceConfigPreshutdownInfo struct{ PreshutdownTimeout uint32 }

// setPreshutdownTimeout sets how long Windows waits after the pre-shutdown
// notice (the default of some Windows versions is too short to stop a run
// and report it). Best effort: the service runs as LocalSystem.
func setPreshutdownTimeout() {
	m, err := mgr.Connect()
	if err != nil {
		return
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return
	}
	defer s.Close()
	info := serviceConfigPreshutdownInfo{PreshutdownTimeout: uint32(preshutdownTimeout / time.Millisecond)}
	_ = windows.ChangeServiceConfig2(s.Handle, windows.SERVICE_CONFIG_PRESHUTDOWN_INFO, (*byte)(unsafe.Pointer(&info))) // #nosec G103 -- structure passed to the Windows service API
}
