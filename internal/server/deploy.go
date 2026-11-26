package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/masterzen/winrm"
	"golang.org/x/crypto/ssh"

	"github.com/max-zit/backupzit/internal/api"
)

// Installing agents from the console: the console connects to the machine
// (SSH for Linux, WinRM for Windows) with credentials that are used once and
// never stored. The machine downloads the package from the console through a
// one-time link, checks its SHA-256, installs it and enrolls.

// deployRequest is what the "Install an agent" form asks for.
type deployRequest struct {
	Host          string
	Port          int
	OS            string // linux | windows
	User          string
	Password      string // login password (SSH or WinRM); on Linux also sudo's, unless SudoPassword is given
	Key           string // SSH private key (PEM), Linux
	KeyPassphrase string // passphrase of Key, only used to open the key
	SudoPassword  string // Linux: the password sudo asks for, when it differs from Password or a key is used
	HostKey       string // expected SSH host key fingerprint (SHA256:...), optional
	By            string // console user
}

// secrets are the request's values that must never appear in the
// installation log or in an error message.
func (q *deployRequest) secrets() []string {
	var out []string
	for _, s := range []string{q.Password, q.KeyPassphrase, q.SudoPassword} {
		for _, v := range []string{s, strings.TrimSpace(s)} {
			if v != "" {
				out = append(out, v)
			}
		}
	}
	// Longest first, so a secret that contains another one is hidden whole.
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// redact replaces every secret in s.
func redact(s string, secrets []string) string {
	for _, x := range secrets {
		s = strings.ReplaceAll(s, x, "[hidden]")
	}
	return s
}

// deployment is one installation, shown on its own page while it runs.
type deployment struct {
	ID       string
	Host     string
	OS       string
	By       string
	Started  time.Time
	mu       sync.Mutex
	log      []string
	done     bool
	err      string
	hostname string
	agentID  int64
	secrets  []string // set before the installation starts, read-only afterwards
}

// logf adds a line to the installation log, with every password and
// passphrase of the request hidden.
func (d *deployment) logf(format string, a ...any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.log = append(d.log, time.Now().Format("15:04:05")+"  "+redact(fmt.Sprintf(format, a...), d.secrets))
}

// deployView is a snapshot for the page.
type deployView struct {
	ID, Host, OS, By, Err, Hostname string
	Started                         time.Time
	Log                             []string
	Done                            bool
	AgentID                         int64
}

func (d *deployment) view() deployView {
	d.mu.Lock()
	defer d.mu.Unlock()
	return deployView{ID: d.ID, Host: d.Host, OS: d.OS, By: d.By, Err: d.err, Hostname: d.hostname, Started: d.Started,
		Log: append([]string(nil), d.log...), Done: d.done, AgentID: d.agentID}
}

// deployFile is a package offered through a one-time link.
type deployFile struct {
	path    string
	expires time.Time
	uses    int
}

type deployer struct {
	mu    sync.Mutex
	runs  map[string]*deployment
	files map[string]*deployFile
}

func randomID(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Server) deployState() *deployer {
	s.deployOnce.Do(func() {
		s.deploys = &deployer{runs: map[string]*deployment{}, files: map[string]*deployFile{}}
	})
	return s.deploys
}

// offerFile makes path downloadable for 30 minutes and returns the link
// token.
func (dp *deployer) offerFile(path string) string {
	tok := randomID(24)
	dp.mu.Lock()
	defer dp.mu.Unlock()
	for k, f := range dp.files {
		if time.Now().After(f.expires) {
			delete(dp.files, k)
		}
	}
	dp.files[tok] = &deployFile{path: path, expires: time.Now().Add(30 * time.Minute)}
	return tok
}

// handleDeployDownload serves GET /api/deploy/{token}/{name}: the package of
// a running installation, without authentication but only through its
// unguessable one-time link.
func (s *Server) handleDeployDownload(w http.ResponseWriter, r *http.Request) {
	dp := s.deployState()
	dp.mu.Lock()
	f, ok := dp.files[r.PathValue("token")]
	if ok && (time.Now().After(f.expires) || f.uses >= 3 || filepath.Base(f.path) != r.PathValue("name")) {
		ok = false
	}
	if ok {
		f.uses++
	}
	dp.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, f.path)
}

// agentPackage picks the newest agent package of a format (deb, rpm, msi,
// legacy-msi) from the downloads.
func (s *Server) agentPackage(format string) (path, sum string, err error) {
	if s.DistDir == "" {
		return "", "", errors.New("no agent packages are offered (downloads folder not set)")
	}
	entries, err := os.ReadDir(s.DistDir)
	if err != nil {
		return "", "", err
	}
	type cand struct {
		name string
		mod  time.Time
	}
	var cands []cand
	for _, e := range entries {
		n := strings.ToLower(e.Name())
		if !strings.HasPrefix(n, "backupzit-agent") {
			continue
		}
		var f string
		switch {
		case strings.HasSuffix(n, "-legacy.msi"):
			f = "legacy-msi"
		case strings.HasSuffix(n, ".msi"):
			f = "msi"
		case strings.HasSuffix(n, "_amd64.deb"):
			f = "deb"
		case strings.HasSuffix(n, ".x86_64.rpm"):
			f = "rpm"
		}
		if f != format {
			continue
		}
		if fi, err := e.Info(); err == nil && fi.Mode().IsRegular() {
			cands = append(cands, cand{e.Name(), fi.ModTime()})
		}
	}
	if len(cands) == 0 {
		return "", "", fmt.Errorf("no %s agent package in the downloads folder", format)
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mod.After(cands[j].mod) })
	path = filepath.Join(s.DistDir, cands[0].name)
	fh, err := os.Open(path) // #nosec G304 -- file of the console's own data or installer directory, not named by a request
	if err != nil {
		return "", "", err
	}
	defer fh.Close()
	h := sha256.New()
	if _, err := io.Copy(h, fh); err != nil {
		return "", "", err
	}
	return path, hex.EncodeToString(h.Sum(nil)), nil
}

func (q *deployRequest) validate() error {
	q.Host = strings.TrimSpace(q.Host)
	q.User = strings.TrimSpace(q.User)
	if q.Host == "" || strings.ContainsAny(q.Host, " /\\@") {
		return errors.New("enter the host name or IP address of the machine")
	}
	if q.OS != "linux" && q.OS != "windows" {
		return errors.New("choose Linux or Windows")
	}
	if q.User == "" {
		return errors.New("enter the user name for the connection")
	}
	if q.Password == "" && (q.OS == "windows" || strings.TrimSpace(q.Key) == "") {
		return errors.New("enter the password" + map[bool]string{true: " or an SSH key", false: ""}[q.OS == "linux"])
	}
	if q.OS == "windows" {
		q.Key, q.KeyPassphrase, q.SudoPassword = "", "", ""
	}
	if strings.TrimSpace(q.Key) == "" {
		q.KeyPassphrase = ""
	}
	// sudo reads its password up to the end of the line.
	if strings.ContainsAny(q.Password+q.SudoPassword, "\r\n") {
		return errors.New("the password must not contain a line break")
	}
	if q.Port == 0 {
		q.Port = map[string]int{"linux": 22, "windows": 5985}[q.OS]
	}
	if q.Port < 1 || q.Port > 65535 {
		return errors.New("invalid port")
	}
	return nil
}

// startDeploy begins an installation in the background.
func (s *Server) startDeploy(q deployRequest, consoleURL string) (*deployment, error) {
	if err := q.validate(); err != nil {
		return nil, err
	}
	d := &deployment{ID: randomID(9), Host: q.Host, OS: q.OS, By: q.By, Started: time.Now(), secrets: q.secrets()}
	dp := s.deployState()
	dp.mu.Lock()
	dp.runs[d.ID] = d
	dp.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		err := s.deploy(ctx, d, q, consoleURL)
		msg := ""
		if err != nil {
			msg = redact(err.Error(), d.secrets)
		}
		d.mu.Lock()
		d.done = true
		d.err = msg
		d.mu.Unlock()
		if err != nil {
			s.log.Warn("agent installation failed", "host", q.Host, "err", msg)
			d.logf("FAILED: %s", msg)
		}
	}()
	return d, nil
}

func (s *Server) deploy(ctx context.Context, d *deployment, q deployRequest, consoleURL string) error {
	tok, _, err := s.store.CreateEnrollmentToken(ctx, 24*time.Hour)
	if err != nil {
		return err
	}
	code := api.EncodeEnrollCode(consoleURL, tok, s.CertFingerprint)
	since := time.Now().Add(-time.Second)
	var hostname string
	if q.OS == "linux" {
		hostname, err = s.deployLinux(ctx, d, q, consoleURL, code)
	} else {
		hostname, err = s.deployWindows(ctx, d, q, consoleURL, code)
	}
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.hostname = hostname
	d.mu.Unlock()
	d.mu.Lock()
	known := d.agentID != 0
	d.mu.Unlock()
	if known {
		d.logf("done")
		return nil
	}
	d.logf("waiting for the agent on %s to enroll", hostname)
	for i := 0; i < 60; i++ {
		agents, err := s.store.ListAgents(ctx)
		if err == nil {
			for _, a := range agents {
				if strings.EqualFold(a.Hostname, hostname) && a.EnrolledAt.After(since) {
					d.mu.Lock()
					d.agentID = a.ID
					d.mu.Unlock()
					d.logf("agent %s enrolled — done", a.Hostname)
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return errors.New("the package was installed but the agent did not enroll within 2 minutes; check that the machine reaches " + consoleURL)
}

// knownAgent reports whether an enrollment found on the machine belongs to
// an agent this console still has; then it is kept, otherwise the machine
// is enrolled again.
func (s *Server) knownAgent(ctx context.Context, d *deployment, uuid string) bool {
	uuid = strings.TrimSpace(uuid)
	if uuid == "" {
		return false
	}
	agents, err := s.store.ListAgents(ctx)
	if err != nil {
		return false
	}
	for _, a := range agents {
		if a.UUID == uuid {
			d.logf("the machine is already enrolled as agent %s; the enrollment is kept", a.Hostname)
			d.mu.Lock()
			d.agentID = a.ID
			d.mu.Unlock()
			return true
		}
	}
	d.logf("the machine has an enrollment this console does not know (removed agent or another console); it is enrolled again")
	return false
}

// ---- Linux over SSH

func (s *Server) deployLinux(ctx context.Context, d *deployment, q deployRequest, consoleURL, code string) (string, error) {
	var auth []ssh.AuthMethod
	if k := strings.TrimSpace(q.Key); k != "" {
		signer, err := parseDeployKey(k, q.KeyPassphrase)
		if err != nil {
			return "", err
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if q.Password != "" {
		pw := q.Password
		auth = append(auth, ssh.Password(pw), ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
			a := make([]string, len(qs))
			for i := range a {
				a[i] = pw
			}
			return a, nil
		}))
	}
	want := strings.TrimSpace(q.HostKey)
	cfg := &ssh.ClientConfig{User: q.User, Auth: auth, Timeout: 20 * time.Second,
		// Prefer ed25519 so the fingerprint matches what the docs tell
		// operators to read ("ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub").
		HostKeyAlgorithms: []string{
			ssh.KeyAlgoED25519,
			ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521,
			ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256,
		},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			fp := ssh.FingerprintSHA256(key)
			if want != "" && fp != want {
				return fmt.Errorf("host key %s does not match the expected %s", fp, want)
			}
			if want == "" {
				d.logf("host key %s (not checked; enter it in the form to verify the machine)", fp)
			} else {
				d.logf("host key %s verified", fp)
			}
			return nil
		}}
	addr := net.JoinHostPort(q.Host, strconv.Itoa(q.Port))
	d.logf("connecting to %s as %s (SSH)", addr, q.User)
	c, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return "", err
	}
	defer c.Close()
	run := func(cmd, stdin string) (string, error) {
		sess, err := c.NewSession()
		if err != nil {
			return "", err
		}
		defer sess.Close()
		// The SSH library copies stdout and stderr in two goroutines; a
		// plain shared bytes.Buffer lost output (its ReadFrom is not safe
		// for two at once), so both go through one lock.
		var out lockedOutput
		sess.Stdout, sess.Stderr = &out, &out
		sess.Stdin = strings.NewReader(stdin)
		done := make(chan error, 1)
		go func() { done <- sess.Run(cmd) }()
		select {
		case err = <-done:
		case <-ctx.Done():
			return out.String(), ctx.Err()
		}
		return strings.TrimSpace(out.String()), err
	}
	info, err := run("sh -s", "hostname; uname -m; if command -v dpkg >/dev/null 2>&1; then echo deb; elif command -v rpm >/dev/null 2>&1; then echo rpm; else echo none; fi; id -u")
	if err != nil {
		return "", fmt.Errorf("detect the system: %v: %s", err, info)
	}
	f := strings.Fields(info)
	if len(f) < 4 {
		return "", fmt.Errorf("unexpected answer: %q", info)
	}
	hostname, arch, format, uid := f[0], f[1], f[2], f[3]
	d.logf("machine %s, %s, packages: %s", hostname, arch, format)
	if arch != "x86_64" {
		return "", fmt.Errorf("the agent is available for x86_64 (64-bit Intel/AMD) machines, not %s", arch)
	}
	if format == "none" {
		return "", errors.New("neither dpkg nor rpm found: install the agent by hand")
	}
	asRoot, err := linuxSudo(d, q, uid, run)
	if err != nil {
		return "", err
	}
	uuid, err := asRoot(`sed -n 's/.*"agent_uuid": *"\([^"]*\)".*/\1/p' /etc/backupzit/agent.json 2>/dev/null || true`)
	if err != nil {
		uuid = "" // the answer is an error message (a wrong sudo password, for one), not an enrollment
	}
	known := s.knownAgent(ctx, d, uuid)
	enroll := "backupzit-agent enroll --force --code '" + code + "'"
	if known {
		enroll = "true"
	}
	path, sum, err := s.agentPackage(format)
	if err != nil {
		return "", err
	}
	tok := s.deployState().offerFile(path)
	name := filepath.Base(path)
	url := strings.TrimRight(consoleURL, "/") + "/api/deploy/" + tok + "/" + name
	install := `dpkg -i "$t/` + name + `" || (DEBIAN_FRONTEND=noninteractive apt-get install -f -y && dpkg -i "$t/` + name + `")`
	if format == "rpm" {
		install = `(command -v dnf >/dev/null 2>&1 && dnf install -y "$t/` + name + `") || rpm -U --replacepkgs "$t/` + name + `"`
	}
	script := `set -e
t=$(mktemp -d)
trap 'rm -rf "$t"' EXIT
if command -v curl >/dev/null 2>&1; then curl -fsSk -o "$t/` + name + `" '` + url + `'
else wget -q --no-check-certificate -O "$t/` + name + `" '` + url + `'; fi
echo '` + sum + `  '"$t/` + name + `" | sha256sum -c - >/dev/null
echo "package verified"
{ ` + install + `; } >"$t/install.log" 2>&1 || { cat "$t/install.log"; exit 1; }
echo "package installed"
` + enroll + `
systemctl restart backupzit-agent
echo "agent started"
`
	d.logf("installing %s (SHA-256 %s…)%s", name, sum[:16], map[bool]string{true: " with sudo", false: ""}[uid != "0"])
	out, err := asRoot(script)
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.Contains(l, code) {
			d.logf("  %s", l)
		}
	}
	if err != nil {
		if uid != "0" && strings.Contains(out, "sudo") {
			return "", fmt.Errorf("installation failed: %v (the user needs sudo rights)", err)
		}
		return "", fmt.Errorf("installation failed: %v", err)
	}
	return hostname, nil
}

// lockedOutput collects a command's stdout and stderr in one buffer. It has
// only Write, so io.Copy cannot use the buffer's ReadFrom from two goroutines.
type lockedOutput struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (o *lockedOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.b.Write(p)
}

func (o *lockedOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.b.String()
}

// parseDeployKey opens the SSH private key with its own passphrase. The
// login or sudo password is never tried on the key.
func parseDeployKey(key, passphrase string) (ssh.Signer, error) {
	if passphrase != "" {
		signer, err := ssh.ParsePrivateKeyWithPassphrase([]byte(key), []byte(passphrase))
		if err == nil {
			return signer, nil
		}
		// A key without a passphrase, with one entered anyway.
		if signer, err2 := ssh.ParsePrivateKey([]byte(key)); err2 == nil {
			return signer, nil
		}
		return nil, fmt.Errorf("SSH key: %v (check the key passphrase)", err)
	}
	signer, err := ssh.ParsePrivateKey([]byte(key))
	var missing *ssh.PassphraseMissingError
	if errors.As(err, &missing) {
		return nil, errors.New("the SSH key is protected by a passphrase: enter it in Key passphrase")
	}
	if err != nil {
		return nil, fmt.Errorf("SSH key: %w", err)
	}
	return signer, nil
}

// tempScript is the only shape of answer from the machine's mktemp that is
// put into a command line.
var tempScript = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)

// linuxSudo decides how scripts run as root on the machine and returns that
// runner (#67). Root needs nothing. Otherwise sudo is first asked without a
// password (sudo -n true): when that works, no password is ever sent. Only
// when sudo asks for one does the console answer it, and then sudo alone reads
// the input: the script waits in a file only the user can read, and its own
// input is /dev/null, so a password can never be run as a command.
func linuxSudo(d *deployment, q deployRequest, uid string, run func(cmd, stdin string) (string, error)) (func(script string) (string, error), error) {
	if uid == "0" {
		return func(script string) (string, error) { return run("sh -s", script) }, nil
	}
	out, err := run("sudo -n true", "")
	if err == nil {
		d.logf("sudo works without a password; no password is sent")
		return func(script string) (string, error) { return run("sudo -n sh -s", script) }, nil
	}
	low := strings.ToLower(out)
	if !strings.Contains(low, "password") && !strings.Contains(low, "authentication") && !strings.Contains(low, "terminal") {
		if out == "" {
			out = err.Error()
		}
		return nil, fmt.Errorf("the user %s cannot use sudo on the machine: %s", q.User, out)
	}
	pw := q.SudoPassword
	if pw == "" {
		pw = q.Password
	}
	if pw == "" {
		return nil, errors.New("sudo on the machine asks for a password: enter it in Sudo password, or allow the user sudo without a password")
	}
	d.logf("sudo asks for a password; the console gives it to sudo only")
	return func(script string) (string, error) {
		f, err := run(`sh -c 'umask 077; f=$(mktemp) && cat >"$f" && echo "$f"'`, "exec </dev/null\n"+script)
		if err != nil {
			return f, fmt.Errorf("prepare the script: %v", err)
		}
		if i := strings.LastIndexByte(f, '\n'); i >= 0 {
			f = strings.TrimSpace(f[i+1:])
		}
		if !tempScript.MatchString(f) || strings.Contains(f, "..") {
			return "", fmt.Errorf("unexpected temporary file name %q", f)
		}
		return run(`sh -c 'rc=0; sudo -S -p "" sh `+f+` || rc=$?; rm -f `+f+`; exit $rc'`, pw+"\n")
	}, nil
}

// ---- Windows over WinRM

func (s *Server) deployWindows(ctx context.Context, d *deployment, q deployRequest, consoleURL, code string) (string, error) {
	enc, err := winrm.NewEncryption("ntlm")
	if err != nil {
		return "", err
	}
	params := *winrm.DefaultParameters
	params.TransportDecorator = func() winrm.Transporter { return enc }
	https := q.Port == 5986
	ep := winrm.NewEndpoint(q.Host, q.Port, https, true, nil, nil, nil, 60*time.Second)
	how := map[bool]string{true: " over HTTPS", false: ", NTLM-encrypted"}[https]
	if https && strings.TrimSpace(q.HostKey) != "" {
		// The certificate is pinned to the confirmed fingerprint (B3): the
		// TLS connection is made here and accepts only that certificate,
		// the WinRM library then speaks NTLM-sealed HTTP over it.
		dial := pinnedTLSDial(strings.TrimSpace(q.HostKey))
		params.TransportDecorator = func() winrm.Transporter { return winrm.NewClientNTLMWithDial(dial) }
		ep = winrm.NewEndpoint(q.Host, q.Port, false, false, nil, nil, nil, 60*time.Second)
		how = " over HTTPS, certificate " + strings.TrimSpace(q.HostKey) + " pinned"
	}
	d.logf("connecting to %s:%d as %s (WinRM%s)", q.Host, q.Port, q.User, how)
	c, err := winrm.NewClientWithParameters(ep, q.User, q.Password, &params)
	if err != nil {
		return "", err
	}
	out, errOut, code0, err := c.RunPSWithContextWithString(ctx, `$env:COMPUTERNAME; [Environment]::OSVersion.Version.Major; [Environment]::Is64BitOperatingSystem; $c = Join-Path $env:ProgramData 'backupzit\agent.json'; if (Test-Path $c) { (Get-Content $c -Raw | ConvertFrom-Json).agent_uuid }`, "")
	if err != nil {
		return "", fmt.Errorf("connect: %w (WinRM must be enabled: winrm quickconfig)", err)
	}
	if code0 != 0 {
		return "", fmt.Errorf("detect the system: %s", strings.TrimSpace(errOut))
	}
	f := strings.Fields(out)
	if len(f) < 3 {
		return "", fmt.Errorf("unexpected answer: %q", out)
	}
	hostname, major, is64 := f[0], f[1], f[2]
	d.logf("machine %s, Windows version %s", hostname, major)
	uuid := ""
	if len(f) > 3 {
		uuid = f[3]
	}
	known := s.knownAgent(ctx, d, uuid)
	reenroll := ""
	if uuid != "" && !known {
		// The MSI keeps an existing enrollment; replace it explicitly.
		reenroll = `$svc = Get-CimInstance Win32_Service -Filter "Name='backupzit-agent'"
$exe = $svc.PathName.Split('"')[1]
if (-not $exe) { $exe = ($svc.PathName -split ' ')[0] }
& $exe enroll --force --code '` + code + `' | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'enrollment failed' }
Restart-Service backupzit-agent
'agent enrolled'
`
	}
	if is64 != "True" {
		return "", errors.New("the agent needs 64-bit Windows")
	}
	format := "msi"
	if n, _ := strconv.Atoi(major); n < 10 {
		format = "legacy-msi" // Windows 7, Server 2008 R2, 2012 R2
	}
	path, sum, err := s.agentPackage(format)
	if err != nil {
		return "", err
	}
	tok := s.deployState().offerFile(path)
	name := filepath.Base(path)
	url := strings.TrimRight(consoleURL, "/") + "/api/deploy/" + tok + "/" + name
	ps := `$ErrorActionPreference = 'Stop'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
# The package is checked by its SHA-256 below, so the console's own certificate is accepted here.
[Net.ServicePointManager]::ServerCertificateValidationCallback = { $true }
$f = Join-Path $env:TEMP '` + name + `'
(New-Object Net.WebClient).DownloadFile('` + url + `', $f)
if ((Get-FileHash $f -Algorithm SHA256).Hash -ne '` + strings.ToUpper(sum) + `') { Remove-Item $f; throw 'the downloaded package is damaged (checksum mismatch)' }
'package verified'
$log = Join-Path $env:TEMP 'backupzit-agent-install.log'
$p = Start-Process msiexec.exe -ArgumentList @('/i', ('"' + $f + '"'), '/qn', '/norestart', 'ENROLLCODE="` + code + `"', '/l*v', ('"' + $log + '"')) -Wait -PassThru
Remove-Item $f
if ($p.ExitCode -ne 0 -and $p.ExitCode -ne 3010) { throw ('msiexec exit code ' + $p.ExitCode + ', see ' + $log) }
'package installed'
` + reenroll
	d.logf("installing %s (SHA-256 %s…)", name, sum[:16])
	out, errOut, rc, err := c.RunPSWithContextWithString(ctx, ps, "")
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			d.logf("  %s", l)
		}
	}
	if err != nil {
		return "", err
	}
	if rc != 0 {
		msg := strings.TrimSpace(errOut)
		if i := strings.Index(msg, "\n"); i > 0 && strings.HasPrefix(msg, "#< CLIXML") {
			msg = msg[i+1:]
		}
		return "", fmt.Errorf("installation failed: %s", cleanCLIXML(msg))
	}
	return hostname, nil
}

// cleanCLIXML extracts the error text from PowerShell's CLIXML stderr.
func cleanCLIXML(s string) string {
	var parts []string
	for {
		i := strings.Index(s, `<S S="Error">`)
		if i < 0 {
			break
		}
		s = s[i+len(`<S S="Error">`):]
		j := strings.Index(s, "</S>")
		if j < 0 {
			break
		}
		parts = append(parts, strings.ReplaceAll(s[:j], "_x000D__x000A_", ""))
		s = s[j:]
	}
	if len(parts) == 0 {
		if len(s) > 500 {
			s = s[:500]
		}
		return s
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

// ---- pages

func (s *Server) handleDeployStart(w http.ResponseWriter, r *http.Request, user string) {
	q := deployRequest{Host: r.FormValue("host"), OS: r.FormValue("os"), User: r.FormValue("user"), Password: r.FormValue("password"),
		Key: r.FormValue("key"), KeyPassphrase: r.FormValue("key_passphrase"), SudoPassword: r.FormValue("sudo_password"),
		HostKey: strings.TrimSpace(r.FormValue("host_key")), Port: atoiDefault(r.FormValue("port")), By: user}
	if err := q.validate(); err != nil {
		redirectErr(w, r, "/agents#install", err)
		return
	}
	// Without a fingerprint in the form, show the machine's and continue
	// only once it is confirmed (B3). WinRM over HTTP has none to show.
	if q.HostKey == "" && (q.OS == "linux" || q.Port == 5986) {
		addr := net.JoinHostPort(q.Host, strconv.Itoa(q.Port))
		p := &pendingConnect{By: user, Deploy: &q}
		hint := "On the machine: ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub"
		if q.OS == "windows" {
			fp, err := fetchTLSFingerprint(r.Context(), addr)
			if err != nil {
				redirectErr(w, r, "/agents#install", fmt.Errorf("cannot read the WinRM certificate of %s: %v", addr, err))
				return
			}
			p.Prints = []identityPrint{{Label: "WinRM certificate (SHA-256)", Value: fp}}
			hint = "On the machine (PowerShell): Get-ChildItem WSMan:\\localhost\\Listener lists the HTTPS listener and its CertificateThumbprint; the console shows the SHA-256 of the certificate."
		} else {
			fp, err := fetchSSHHostKey(r.Context(), addr)
			if err != nil {
				redirectErr(w, r, "/agents#install", fmt.Errorf("cannot read the SSH host key of %s: %v", addr, err))
				return
			}
			p.Prints = []identityPrint{{Label: "SSH host key", Value: fp}}
		}
		s.renderConfirmIdentity(w, r, user, p, "/agents/install/confirm", "/agents#install", hint)
		return
	}
	s.runDeploy(w, r, q)
}

// handleDeployConfirm continues an installation once the user confirmed
// the machine's fingerprint (B3).
func (s *Server) handleDeployConfirm(w http.ResponseWriter, r *http.Request, user string) {
	p, err := s.pendings().take(r.FormValue("pending"), user)
	if err != nil || p.Deploy == nil || len(p.Prints) == 0 {
		redirectErr(w, r, "/agents#install", errors.New("the confirmation expired; enter the details again"))
		return
	}
	q := *p.Deploy
	q.HostKey = p.Prints[0].Value
	s.runDeploy(w, r, q)
}

// runDeploy starts an installation and shows its page.
func (s *Server) runDeploy(w http.ResponseWriter, r *http.Request, q deployRequest) {
	pub := s.publicURL(r)
	d, err := s.startDeploy(q, pub)
	if err != nil {
		redirectErr(w, r, "/agents#install", err)
		return
	}
	s.audit(r, "agent.install", "%s (%s) as %s", q.Host, q.OS, q.User)
	http.Redirect(w, r, "/agents/install/"+d.ID, http.StatusSeeOther)
}

func (s *Server) handleDeployPage(w http.ResponseWriter, r *http.Request, user string) {
	dp := s.deployState()
	dp.mu.Lock()
	d, ok := dp.runs[r.PathValue("id")]
	dp.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.render(w, r, "deploy", pageData{Title: "Installing agent on " + d.Host, Nav: "agents", User: user, Data: d.view()})
}
