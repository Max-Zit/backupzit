package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kardianos/service"
	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/launchd"
)

// installNeedsEnrollment: the .pkg installs and starts the LaunchDaemon
// before the agent is enrolled (a code entered later, or a pending
// enrollment the service retries), like the MSI on Windows.
const installNeedsEnrollment = false

// launchdService installs and controls the agent as a system LaunchDaemon
// labelled com.backupzit.agent. kardianos/service still runs the agent
// ("service run": start, wait for SIGTERM, stop), but its launchd support
// labels the job with the service name, cannot set a restart delay or a
// stop timeout, and controls jobs with the legacy load/unload/list
// commands, whose "list" does not see the system domain from a user's
// shell (status would say "stopped" while the daemon runs).
type launchdService struct {
	service.Service
	args []string
}

// platformService replaces the library's install and control on macOS.
func platformService(s service.Service, args []string) service.Service {
	return &launchdService{Service: s, args: args}
}

// agentExitTimeOut is how long launchd lets the agent stop, in seconds,
// before it kills it: the agent waits for the running backup to finish
// writing (90 s, systemd's default stop timeout).
const agentExitTimeOut = 90

// stopWait is how long "service stop" waits for launchd to unload the job:
// a little longer than agentExitTimeOut, since launchd kills the process
// at that point and then needs a moment to drop the label.
const stopWait = (agentExitTimeOut + 10) * time.Second

// startWait is how long "service start" retries while launchd still lists
// a job that was just unloaded.
const startWait = 15 * time.Second

// agentJob is the agent's LaunchDaemon: root, started at boot and kept
// running, restarted 30 s after it ends (Restart=always, RestartSec=30
// with systemd), 90 s to stop (systemd's default) so a run can finish
// writing, and a lower CPU priority (Nice=10 on Linux). The agent writes
// its own log; launchd's stdout/stderr file only gets crash traces.
func agentJob(exe string, args []string) launchd.Job {
	return launchd.Job{
		Label:             agent.LaunchdLabel,
		ProgramArguments:  append([]string{exe}, args...),
		UserName:          "root",
		GroupName:         "wheel",
		RunAtLoad:         true,
		KeepAlive:         true,
		ThrottleInterval:  30,
		ExitTimeOut:       agentExitTimeOut,
		Nice:              10,
		WorkingDirectory:  "/",
		StandardOutPath:   agent.LaunchdLogPath,
		StandardErrorPath: agent.LaunchdLogPath,
	}
}

// Install writes the LaunchDaemon (again, over an existing one: the .pkg
// runs it at every upgrade; a loaded job reads the new definition at its
// next start).
func (s *launchdService) Install() error {
	if os.Geteuid() != 0 {
		return errors.New("installing the LaunchDaemon needs root (sudo)")
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return fmt.Errorf("locate the agent program: %w", err)
	}
	if err := rootOnlyPath(exe); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(agent.LaunchdLogPath), 0o700); err != nil {
		return err
	}
	return launchd.WritePlist(agent.LaunchdPlist, agentJob(exe, s.args))
}

// rootOnlyPath refuses a program that someone other than root could
// replace: launchd would run it as root at every boot. The program and
// every folder above it must belong to root and be writable by nobody
// else; write access for the admin group is accepted, since its members
// can become root anyway (/Applications is root:admin 0775).
func rootOnlyPath(p string) error {
	for {
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("%s: no owner information", p)
		}
		perm := fi.Mode().Perm()
		groupOK := st.Gid == 0 || st.Gid == 80 // wheel, admin
		if st.Uid != 0 || perm&0o002 != 0 || (perm&0o020 != 0 && !groupOK) {
			return fmt.Errorf("%s can be changed by users other than root; install the agent with its package (it goes to %s) before installing the service", p, agent.InstallPrefix)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return nil
		}
		p = parent
	}
}

// Uninstall stops the job and removes its definition.
func (s *launchdService) Uninstall() error {
	if err := s.Stop(); err != nil {
		return err
	}
	if err := os.Remove(agent.LaunchdPlist); err != nil {
		if os.IsNotExist(err) {
			return service.ErrNotInstalled
		}
		return err
	}
	return nil
}

// Start loads the job (it starts at once: RunAtLoad), or starts a loaded
// job that is not running. launchd.Start retries while a job that was just
// booted out is still listed, so a restart (the .pkg's postinstall at every
// self-update) does not leave the agent stopped.
func (s *launchdService) Start() error {
	st, err := launchd.SystemState(agent.LaunchdLabel)
	if err != nil {
		return err
	}
	if st == launchd.NotLoaded {
		if _, err := os.Stat(agent.LaunchdPlist); err != nil {
			return service.ErrNotInstalled
		}
	}
	return launchd.Start(agent.LaunchdLabel, agent.LaunchdPlist, startWait)
}

// Stop unloads the job: with KeepAlive, killing the process would only
// restart it. The definition stays, so the agent starts again at boot, as
// "systemctl stop" leaves an enabled unit. It returns once launchd has
// really unloaded the job (the agent may first finish writing a backup),
// so Restart and Uninstall can go on.
func (s *launchdService) Stop() error {
	st, err := launchd.SystemState(agent.LaunchdLabel)
	if err != nil {
		return err
	}
	if st == launchd.NotLoaded {
		return nil
	}
	return launchd.Bootout(agent.LaunchdLabel, stopWait)
}

func (s *launchdService) Restart() error {
	if err := s.Stop(); err != nil {
		return err
	}
	return s.Start()
}

func (s *launchdService) Status() (service.Status, error) {
	st, err := launchd.SystemState(agent.LaunchdLabel)
	if err != nil {
		return service.StatusUnknown, err
	}
	switch st {
	case launchd.Running:
		return service.StatusRunning, nil
	case launchd.Stopped:
		return service.StatusStopped, nil
	}
	if _, err := os.Stat(agent.LaunchdPlist); err == nil {
		return service.StatusStopped, nil
	}
	return service.StatusUnknown, service.ErrNotInstalled
}
