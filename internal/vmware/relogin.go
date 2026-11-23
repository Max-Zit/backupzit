package vmware

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/vmware/govmomi/fault"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/vim25/types"
)

// sessionLost tells whether err means that the vSphere session is gone:
// expired, or ended by a restart of the host's management services or by
// a vCenter session policy. API calls then fail with NotAuthenticated and
// datastore downloads with HTTP 401.
func sessionLost(err error) bool {
	if err == nil {
		return false
	}
	if fault.Is(err, &types.NotAuthenticated{}) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "NotAuthenticated") || strings.Contains(s, "session is not authenticated") || strings.Contains(s, "401 Unauthorized")
}

// ensureSession signs in again when the session is gone. The keep-alive
// keeps it during long reads (0.55), but it can still end; before 0.57 the
// rest of the run then failed: the snapshot of the VM just read stayed and
// every later VM failed with 401 (#44).
func (c *Client) ensureSession(ctx context.Context) error {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	m := session.NewManager(c.vim)
	us, err := m.UserSession(ctx) // nil when not signed in
	if err != nil && !sessionLost(err) {
		return err
	}
	if err == nil && us != nil {
		return nil
	}
	if err := m.Login(ctx, url.UserPassword(c.conn.User, c.conn.Password)); err != nil {
		return fmt.Errorf("the vSphere session ended and signing in again failed: %w", err)
	}
	c.Relogins++
	return nil
}

// again runs fn; when it fails because the session is gone, it signs in
// again and runs fn once more.
func (c *Client) again(ctx context.Context, fn func() error) error {
	err := fn()
	if !sessionLost(err) || ctx.Err() != nil {
		return err
	}
	if lerr := c.ensureSession(ctx); lerr != nil {
		return errors.Join(err, lerr)
	}
	return fn()
}
