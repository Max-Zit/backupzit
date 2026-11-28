package localipc

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// MED 16: interactive users may talk to the agent's pipe, but not add
// server instances of their own to it (FILE_APPEND_DATA is
// FILE_CREATE_PIPE_INSTANCE for a pipe), and the client opens it with no
// more than they are granted.
func TestPipeSDDLNoInstancesForUsers(t *testing.T) {
	sd, err := windows.SecurityDescriptorFromString(pipeSDDL)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)) // #nosec G103 -- the SID inside the ACE
		if !sid.IsWellKnown(windows.WinInteractiveSid) {
			continue
		}
		found = true
		if ace.Mask&windows.FILE_APPEND_DATA != 0 {
			t.Errorf("interactive users may create pipe instances (mask %#x)", ace.Mask)
		}
		if ace.Mask&clientAccess != clientAccess {
			t.Errorf("mask %#x does not grant what the client asks for (%#x)", ace.Mask, clientAccess)
		}
	}
	if !found {
		t.Error("no entry for interactive users")
	}
}

// testPipe serves a pipe of its own under the test's (non-elevated)
// account. That account may not create further instances (pipeSDDL), so
// each test pipe answers one connection: its first instance.
func testPipe(t *testing.T) (*pipeListener, context.Context) {
	t.Helper()
	orig := pipeName
	pipeName = `\\.\pipe\backupzit-test-` + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	t.Cleanup(func() { pipeName = orig })
	l, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go Serve(ctx, l, func(context.Context, Request) Response {
		return Response{OK: true, Status: &Status{Version: "test"}}
	})
	return l.(*pipeListener), ctx
}

func TestPipeServerCheck(t *testing.T) {
	tok := windows.GetCurrentProcessToken()
	if tok.IsElevated() {
		t.Skip("elevated: the test's pipes belong to Administrators, and it may create instances")
	}

	t.Run("pipe of another account is refused", func(t *testing.T) {
		_, ctx := testPipe(t)
		if _, err := Call(ctx, Request{Cmd: CmdStatus}); err == nil || !strings.Contains(err.Error(), "not created by the BackupZit agent service") {
			t.Errorf("a pipe the agent service did not create was accepted: %v", err)
		}
	})

	t.Run("users cannot add an instance; the client's access is enough", func(t *testing.T) {
		iu, err := windows.CreateWellKnownSid(windows.WinInteractiveSid)
		if err != nil {
			t.Fatal(err)
		}
		if member, err := tok.IsMember(iu); err != nil || !member {
			t.Skip("not an interactive logon: the pipe's users entry does not apply")
		}
		tu, err := tok.GetTokenUser()
		if err != nil {
			t.Fatal(err)
		}
		orig := trustedPipeOwner
		trustedPipeOwner = func(sid *windows.SID) bool { return sid.Equals(tu.User.Sid) }
		defer func() { trustedPipeOwner = orig }()
		l, ctx := testPipe(t)
		// While the pipe exists (its first instance waits for a client).
		if h, err := l.create(false); err == nil {
			_ = windows.CloseHandle(h)
			t.Error("a user could add a server instance to the pipe")
		} else if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			t.Errorf("second instance: %v", err)
		}
		resp, err := Call(ctx, Request{Cmd: CmdStatus})
		if err != nil || resp.Status == nil || resp.Status.Version != "test" {
			t.Fatalf("status through the pipe: %+v %v", resp, err)
		}
	})
}
