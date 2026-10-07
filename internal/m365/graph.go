// Package m365 backs up Microsoft 365 mailboxes (Exchange Online) and
// OneDrive through Microsoft Graph, with an app registration of the
// tenant (client credentials). Only reading permissions are needed:
// User.Read.All, Mail.Read and Files.Read.All.
package m365

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Endpoints; tests point them at a local server.
var (
	LoginURL = "https://login.microsoftonline.com"
	GraphURL = "https://graph.microsoft.com/v1.0"
)

// Credentials of an app registration.
type Credentials struct {
	Tenant string // directory (tenant) ID or a domain of the tenant
	Client string // application (client) ID
	Secret string
}

// Client calls Microsoft Graph as the application.
type Client struct {
	cred Credentials
	http *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
	roles   []string

	// throttled counts answers that asked to slow down (429, 503) and
	// waited the time spent waiting for them.
	throttled int
	waited    time.Duration
}

// NewClient returns a client; nothing is sent before the first call.
func NewClient(c Credentials) *Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	// Downloads may take long; an answer that does not start ends here.
	tr.ResponseHeaderTimeout = 2 * time.Minute
	return &Client{cred: c, http: &http.Client{Transport: tr}}
}

// Error is an error answer of Microsoft Graph or of the sign-in.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s: %s (HTTP %d)", e.Code, e.Message, e.Status)
	}
	return fmt.Sprintf("%s (HTTP %d)", e.Message, e.Status)
}

// IsNotFound reports a missing mailbox, drive or item.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && (e.Status == http.StatusNotFound ||
		e.Code == "MailboxNotEnabledForRESTAPI" || e.Code == "MailboxNotHostedInExchangeOnline" || e.Code == "ResourceNotFound")
}

// Throttled reports how often Microsoft asked to slow down and how long
// the client waited.
func (c *Client) Throttled() (int, time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.throttled, c.waited
}

// Roles are the application permissions in the current token.
func (c *Client) Roles(ctx context.Context) ([]string, error) {
	if _, err := c.accessToken(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.roles...), nil
}

func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Until(c.expires) > 5*time.Minute {
		return c.token, nil
	}
	form := url.Values{
		"client_id":     {c.cred.Client},
		"client_secret": {c.cred.Secret},
		"grant_type":    {"client_credentials"},
		"scope":         {"https://graph.microsoft.com/.default"},
	}
	u := LoginURL + "/" + url.PathEscape(c.cred.Tenant) + "/oauth2/v2.0/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("sign in to Microsoft 365: %w", err)
	}
	defer resp.Body.Close()
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tr); err != nil {
		return "", fmt.Errorf("sign in to Microsoft 365: HTTP %d", resp.StatusCode)
	}
	if tr.AccessToken == "" {
		// Keep the explanation, not the trace and correlation IDs.
		msg, _, _ := strings.Cut(tr.Description, "\r\n")
		msg, _, _ = strings.Cut(msg, " Trace ID:")
		return "", &Error{Status: resp.StatusCode, Code: tr.Error, Message: "sign in to Microsoft 365: " + msg}
	}
	c.token = tr.AccessToken
	c.expires = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	c.roles = tokenRoles(tr.AccessToken)
	return c.token, nil
}

// tokenRoles reads the "roles" claim (application permissions).
func tokenRoles(tok string) []string {
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var claims struct {
		Roles []string `json:"roles"`
	}
	json.Unmarshal(b, &claims)
	return claims.Roles
}

// maxTries bounds retries of throttled (429) and unavailable (5xx) calls.
const maxTries = 8

// retryWait is the pause before the next try; tests shorten it.
var retryWait = func(try int, retryAfter string) time.Duration {
	if s, err := strconv.Atoi(retryAfter); err == nil && s > 0 && s <= 300 {
		return time.Duration(s) * time.Second
	}
	return time.Duration(1<<min(try, 6)) * time.Second
}

// do sends a GET to Graph (path relative to GraphURL, or an absolute
// nextLink) and returns the response of a successful call. Throttling and
// temporary failures are retried.
func (c *Client) do(ctx context.Context, path string) (*http.Response, error) {
	u := path
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		u = GraphURL + path
	}
	for try := 0; ; try++ {
		tok, err := c.accessToken(ctx)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		if p, ok := ctx.Value(preferKey{}).(string); ok {
			req.Header.Set("Prefer", p)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil || try >= maxTries {
				return nil, err
			}
			if err := sleep(ctx, retryWait(try, "")); err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode < 300 {
			return resp, nil
		}
		gerr := readError(resp)
		resp.Body.Close()
		retry := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		if resp.StatusCode == http.StatusUnauthorized && try == 0 {
			c.mu.Lock()
			c.token = "" // expired early: sign in again
			c.mu.Unlock()
			retry = true
		}
		if !retry || try >= maxTries {
			return nil, gerr
		}
		wait := retryWait(try, resp.Header.Get("Retry-After"))
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			c.mu.Lock()
			c.throttled++
			c.waited += wait
			c.mu.Unlock()
		}
		if err := sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func readError(resp *http.Response) error {
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	json.Unmarshal(b, &body)
	e := &Error{Status: resp.StatusCode, Code: body.Error.Code, Message: body.Error.Message}
	if e.Message == "" {
		e.Message = http.StatusText(resp.StatusCode)
	}
	return e
}

// preferKey carries a Prefer header for the requests of a context.
type preferKey struct{}

// withPrefer asks Graph for times in UTC and bodies as text (events).
func withPrefer(ctx context.Context) context.Context {
	return context.WithValue(ctx, preferKey{}, `outlook.timezone="UTC", outlook.body-content-type="text"`)
}

// getJSON decodes one answer.
func (c *Client) getJSON(ctx context.Context, path string, v any) error {
	resp, err := c.do(ctx, path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

// list follows @odata.nextLink and calls fn for each page's items.
func list[T any](ctx context.Context, c *Client, path string, fn func([]T) error) error {
	for path != "" {
		var page struct {
			Value []T    `json:"value"`
			Next  string `json:"@odata.nextLink"`
		}
		if err := c.getJSON(ctx, path, &page); err != nil {
			return err
		}
		if err := fn(page.Value); err != nil {
			return err
		}
		path = page.Next
	}
	return nil
}

// listAll collects all pages.
func listAll[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	var out []T
	err := list(ctx, c, path, func(v []T) error { out = append(out, v...); return nil })
	return out, err
}

// User is an account of the tenant.
type User struct {
	ID                string `json:"id"`
	UserPrincipalName string `json:"userPrincipalName"`
	DisplayName       string `json:"displayName"`
	Mail              string `json:"mail"`
}

// Users lists the accounts of the tenant.
func (c *Client) Users(ctx context.Context) ([]User, error) {
	return listAll[User](ctx, c, "/users?$select=id,userPrincipalName,displayName,mail&$top=999")
}

// User looks up one account by user principal name, address or ID.
func (c *Client) User(ctx context.Context, name string) (User, error) {
	var u User
	err := c.getJSON(ctx, "/users/"+url.PathEscape(name)+"?$select=id,userPrincipalName,displayName,mail", &u)
	return u, err
}

// MailFolder is a folder of a mailbox.
type MailFolder struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	ChildCount  int    `json:"childFolderCount"`
	TotalCount  int    `json:"totalItemCount"`
}

// Message is the listed part of a message.
type Message struct {
	ID           string    `json:"id"`
	Subject      string    `json:"subject"`
	Received     time.Time `json:"receivedDateTime"`
	Created      time.Time `json:"createdDateTime"`
	LastModified time.Time `json:"lastModifiedDateTime"`
}

func (c *Client) mailFolders(ctx context.Context, user, parent string) ([]MailFolder, error) {
	p := "/users/" + url.PathEscape(user) + "/mailFolders"
	if parent != "" {
		p += "/" + url.PathEscape(parent) + "/childFolders"
	}
	return listAll[MailFolder](ctx, c, p+"?$top=250&$select=id,displayName,childFolderCount,totalItemCount")
}

func (c *Client) messages(ctx context.Context, user, folder string, fn func([]Message) error) error {
	return list(ctx, c, "/users/"+url.PathEscape(user)+"/mailFolders/"+url.PathEscape(folder)+
		"/messages?$top=500&$select=id,subject,receivedDateTime,createdDateTime,lastModifiedDateTime", fn)
}

// MIME returns a message in MIME format (.eml).
func (c *Client) MIME(ctx context.Context, user, id string) (io.ReadCloser, error) {
	resp, err := c.do(ctx, "/users/"+url.PathEscape(user)+"/messages/"+url.PathEscape(id)+"/$value")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// DriveItem is a file or folder of a OneDrive.
type DriveItem struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"lastModifiedDateTime"`
	Folder       *struct {
		ChildCount int `json:"childCount"`
	} `json:"folder"`
	Package *struct {
		Type string `json:"type"`
	} `json:"package"`
	File *struct{} `json:"file"`
}

// Drive returns the ID of a user's OneDrive.
func (c *Client) Drive(ctx context.Context, user string) (string, error) {
	var d struct {
		ID string `json:"id"`
	}
	err := c.getJSON(ctx, "/users/"+url.PathEscape(user)+"/drive?$select=id", &d)
	return d.ID, err
}

func (c *Client) children(ctx context.Context, drive, item string) ([]DriveItem, error) {
	return listAll[DriveItem](ctx, c, "/drives/"+url.PathEscape(drive)+"/items/"+url.PathEscape(item)+
		"/children?$top=999&$select=id,name,size,lastModifiedDateTime,folder,package,file")
}

// Content returns the content of a drive file.
func (c *Client) Content(ctx context.Context, drive, item string) (io.ReadCloser, error) {
	resp, err := c.do(ctx, "/drives/"+url.PathEscape(drive)+"/items/"+url.PathEscape(item)+"/content")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}
