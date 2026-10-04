package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/tlsutil"
)

// HardenedPort is the default port of a hardened repository.
const HardenedPort = "8500"

// Hardened stores the repository on a backupzit hardened repository
// (backupzit-repo): write-once, immutable storage on a Linux server.
//
// Location: hardened://host[:port]/path. The access key and the pinned
// certificate fingerprint come from Options.
type Hardened struct {
	base     string // https://host:port/v1/
	prefix   string
	key      string
	client   *http.Client
	loc      string
	lockDays int
	asOf     int64
}

// OpenHardened connects to the repository service and checks the key.
func OpenHardened(ctx context.Context, u *url.URL, opts Options) (*Hardened, error) {
	if opts.HardenedKey == "" {
		return nil, errors.New("hardened repository: access key is required")
	}
	if opts.HardenedFingerprint == "" {
		return nil, errors.New("hardened repository: certificate fingerprint is required (shown by: backupzit-repo fingerprint)")
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), HardenedPort)
	}
	prefix := strings.Trim(u.Path, "/")
	if prefix == "" {
		return nil, errors.New("hardened repository: path missing in location (hardened://host/path)")
	}
	tr := &http.Transport{
		TLSClientConfig:       tlsutil.PinnedConfig(opts.HardenedFingerprint),
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 10 * time.Minute,
		MaxIdleConnsPerHost:   8,
	}
	h := &Hardened{base: "https://" + host + "/v1/", prefix: prefix, key: opts.HardenedKey,
		client: &http.Client{Transport: tr}, loc: "hardened://" + host + "/" + prefix}
	if !opts.AsOf.IsZero() {
		h.asOf = opts.AsOf.Unix()
		h.loc += "@" + opts.AsOf.UTC().Format(time.RFC3339)
	}
	var info struct {
		LockDays int `json:"lock_days"`
	}
	if err := h.getJSON(ctx, "info", &info); err != nil {
		return nil, fmt.Errorf("hardened repository %s: %w", host, err)
	}
	h.lockDays = info.LockDays
	return h, nil
}

func (h *Hardened) Location() string { return h.loc }
func (h *Hardened) Close() error     { h.client.CloseIdleConnections(); return nil }
func (h *Hardened) LockDays() int    { return h.lockDays }

func (h *Hardened) name(n string) string { return h.prefix + "/" + n }

// path returns the request path below /v1/ for kind ("files", "list").
func (h *Hardened) path(kind, name string) string {
	p := kind + "/" + name
	if h.asOf > 0 {
		p += "?asof=" + strconv.FormatInt(h.asOf, 10)
	}
	return p
}

func (h *Hardened) do(ctx context.Context, method, path string, body []byte, hdr map[string]string) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, h.base+path, rd)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+h.key)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := h.client.Do(req)
		if err != nil {
			if errors.Is(err, tlsutil.ErrFingerprintMismatch) || ctx.Err() != nil {
				return nil, err
			}
			// All requests are safe to repeat: uploading identical content
			// again succeeds.
			lastErr = err
			continue
		}
		if resp.StatusCode >= 500 {
			lastErr = statusError(resp)
			continue
		}
		return resp, nil
	}
	return nil, lastErr
}

func statusError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return errors.New("access key rejected")
	case http.StatusNotFound:
		return ErrNotFound
	}
	return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
}

func (h *Hardened) getJSON(ctx context.Context, path string, v any) error {
	resp, err := h.do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return statusError(resp)
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

func (h *Hardened) Save(ctx context.Context, name string, data []byte) error {
	if h.asOf > 0 {
		return ErrReadOnly
	}
	resp, err := h.do(ctx, http.MethodPut, h.path("files", h.name(name)), data, map[string]string{"Content-Type": "application/octet-stream"})
	if err != nil {
		return fmt.Errorf("save %s: %w", name, err)
	}
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("save %s: %w", name, statusError(resp))
	}
	resp.Body.Close()
	return nil
}

func (h *Hardened) get(ctx context.Context, name string, hdr map[string]string, want int) ([]byte, error) {
	resp, err := h.do(ctx, http.MethodGet, h.path("files", h.name(name)), nil, hdr)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", name, err)
	}
	if resp.StatusCode != want {
		return nil, fmt.Errorf("load %s: %w", name, statusError(resp))
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (h *Hardened) Load(ctx context.Context, name string) ([]byte, error) {
	return h.get(ctx, name, nil, http.StatusOK)
}

func (h *Hardened) LoadRange(ctx context.Context, name string, offset int64, length int) ([]byte, error) {
	if offset < 0 {
		size, err := h.Size(ctx, name)
		if err != nil {
			return nil, err
		}
		offset += size
	}
	if length <= 0 {
		return []byte{}, nil
	}
	b, err := h.get(ctx, name, map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", offset, offset+int64(length)-1)}, http.StatusPartialContent)
	if err != nil {
		return nil, err
	}
	if len(b) != length {
		return nil, fmt.Errorf("read %s@%d+%d: %w", name, offset, length, io.ErrUnexpectedEOF)
	}
	return b, nil
}

func (h *Hardened) Size(ctx context.Context, name string) (int64, error) {
	resp, err := h.do(ctx, http.MethodHead, h.path("files", h.name(name)), nil, nil)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.ContentLength, nil
	case http.StatusNotFound:
		return 0, fmt.Errorf("%s: %w", name, ErrNotFound)
	}
	return 0, fmt.Errorf("size %s: %s", name, resp.Status)
}

func (h *Hardened) List(ctx context.Context, dir string) ([]string, error) {
	var names []string
	if err := h.getJSON(ctx, h.path("list", h.name(dir)), &names); err != nil {
		return nil, fmt.Errorf("list %s: %w", dir, err)
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, strings.TrimPrefix(n, h.prefix+"/"))
	}
	return out, nil
}

// Remove deletes name; the service only hides files that are still
// retained.
func (h *Hardened) Remove(ctx context.Context, name string) error {
	if h.asOf > 0 {
		return ErrReadOnly
	}
	resp, err := h.do(ctx, http.MethodDelete, h.path("files", h.name(name)), nil, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusNoContent {
		err := statusError(resp)
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%s: %w", name, ErrNotFound)
		}
		return fmt.Errorf("remove %s: %w", name, err)
	}
	resp.Body.Close()
	return nil
}

// KeepLocked asks the service to extend the retention of the named files.
func (h *Hardened) KeepLocked(ctx context.Context, names []string) (int, error) {
	if h.asOf > 0 {
		return 0, ErrReadOnly
	}
	total := 0
	for len(names) > 0 {
		batch := names[:min(len(names), 2000)]
		names = names[len(batch):]
		full := make([]string, len(batch))
		for i, n := range batch {
			full[i] = h.name(n)
		}
		body, _ := json.Marshal(map[string][]string{"names": full})
		resp, err := h.do(ctx, http.MethodPost, "keep", body, map[string]string{"Content-Type": "application/json"})
		if err != nil {
			return total, err
		}
		if resp.StatusCode != http.StatusOK {
			return total, statusError(resp)
		}
		var kr struct {
			Extended int `json:"extended"`
		}
		err = json.NewDecoder(resp.Body).Decode(&kr)
		resp.Body.Close()
		if err != nil {
			return total, err
		}
		total += kr.Extended
	}
	return total, nil
}
