package server

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/m365"
	"github.com/max-zit/backupzit/internal/update"
)

// Microsoft 365 jobs back up Exchange Online mailboxes and OneDrive
// through Microsoft Graph, with an app registration of the tenant that
// has the application permissions User.Read.All, Mail.Read and
// Files.Read.All. The selected agent downloads the data; it needs only
// internet access. Paths are the accounts (none: all accounts with a
// mailbox or OneDrive). Backups are file backups of .eml messages and
// OneDrive files, in their own repository per tenant.

const ctxM365Secret = "jobs.m365_secret"

// minM365Version is the first agent version that backs up Microsoft 365.
const minM365Version = "0.34.0"

var (
	guidRE   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	domainRE = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?)+$`)
	upnRE    = regexp.MustCompile(`^[^\s@/\\]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`)
)

// checkM365Job validates the tenant, the app and the accounts of a job.
func (s *Store) checkM365Job(j *Job) error {
	o := &j.Options
	o.M365Tenant = strings.ToLower(strings.TrimSpace(o.M365Tenant))
	o.M365Client = strings.ToLower(strings.TrimSpace(o.M365Client))
	if !guidRE.MatchString(o.M365Tenant) && !domainRE.MatchString(o.M365Tenant) {
		return errors.New("enter the directory (tenant) ID of the app registration, e.g. 0244a840-1973-4553-901c-ead0f5823b22, or the tenant's domain (contoso.onmicrosoft.com)")
	}
	if !guidRE.MatchString(o.M365Client) {
		return errors.New("enter the application (client) ID of the app registration, e.g. 6be64ab9-0b80-44b4-a37e-5dd8fb5644db")
	}
	if o.M365Secret == "" {
		return errors.New("enter the client secret (its Value) of the app registration")
	}
	if o.M365NoMail && o.M365NoOneDrive && !o.M365Calendar && !o.M365SharePoint {
		return errors.New("choose what to back up: mail, OneDrive, calendars and contacts or SharePoint")
	}
	sites := []string{}
	if o.M365SharePoint {
		for _, raw := range o.M365Sites {
			raw = strings.TrimRight(strings.TrimSpace(raw), "/")
			u, err := url.Parse(raw)
			if err != nil || u.Scheme != "https" || !validAddress(u.Host) || u.RawQuery != "" || u.Fragment != "" {
				return fmt.Errorf("%q is not a SharePoint site address (https://contoso.sharepoint.com/sites/sales)", raw)
			}
			if !slices.Contains(sites, raw) {
				sites = append(sites, raw)
			}
		}
	}
	o.M365Sites = sites
	if len(o.M365Sites) == 0 {
		o.M365Sites = nil
	}
	users := []string{}
	for _, p := range j.Paths {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" || slices.Contains(users, p) {
			continue
		}
		if !upnRE.MatchString(p) {
			return fmt.Errorf("%q is not an account address (user@contoso.com)", p)
		}
		users = append(users, p)
	}
	if o.M365NoMail && o.M365NoOneDrive && !o.M365Calendar {
		users = []string{} // SharePoint only: no accounts
	}
	j.Paths, j.Excludes, j.ImageDisk, j.ImagePartitions = users, nil, nil, nil
	if !strings.HasPrefix(o.M365Secret, secretPrefix) {
		o.M365Secret = s.seal(ctxM365Secret, o.M365Secret)
	}
	return nil
}

// clearM365 removes Microsoft 365 settings from jobs of other kinds.
func (o *JobOptions) clearM365() {
	o.M365Tenant, o.M365Client, o.M365Secret, o.M365NoMail, o.M365NoOneDrive = "", "", "", false, false
	o.M365Calendar, o.M365SharePoint, o.M365Sites = false, false, nil
}

// m365RepoDir is the repository of a tenant's jobs below a target: it does
// not depend on the agent that downloads, which can be replaced.
func m365RepoDir(tenant string) string {
	return "m365-" + strings.NewReplacer(".", "-", "/", "-", `\`, "-").Replace(strings.ToLower(tenant))
}

// jobRepoDir is where a job's backups are stored below its target.
func (s *Store) jobRepoDir(ctx context.Context, j Job, a Agent) (string, error) {
	switch {
	case j.Kind == JobM365:
		return m365RepoDir(j.Options.M365Tenant), nil
	case j.VMwareHostID != nil:
		// A VMware host has its own repository, independent of its proxy.
		h, err := s.GetVMwareHost(ctx, *j.VMwareHostID)
		if err != nil {
			return "", err
		}
		return h.RepoDir, nil
	}
	return a.RepoDir, nil
}

// m365Secret is the job's client secret in clear text.
func (s *Store) m365Secret(o JobOptions) (string, error) {
	if s.box == nil {
		return o.M365Secret, nil
	}
	return s.box.open(ctxM365Secret, o.M365Secret)
}

// addM365 attaches the tenant of a Microsoft 365 job to its backups.
func (s *Server) addM365(ctx context.Context, run *Run, ar *api.Run) error {
	if run.JobID == nil {
		return nil
	}
	j, err := s.store.GetJob(ctx, *run.JobID)
	if err != nil || j.Kind != JobM365 {
		return nil
	}
	if a, err := s.store.GetAgent(ctx, run.AgentID); err == nil {
		if min := m365MinVersion(j.Options); agentOlder(a, min) {
			return fmt.Errorf("the agent on %s (%s) cannot back up %s yet; update it to %s or newer (Agents page)", a.Hostname, a.Version, m365Parts(j.Options), min)
		}
	}
	secret, err := s.store.m365Secret(j.Options)
	if err != nil {
		return err
	}
	o := j.Options
	ar.M365 = &api.M365Source{Tenant: o.M365Tenant, Client: o.M365Client, Secret: secret, Mail: !o.M365NoMail, OneDrive: !o.M365NoOneDrive,
		Calendar: o.M365Calendar, SharePoint: o.M365SharePoint, Sites: o.M365Sites}
	return nil
}

// minM365PIMVersion is the first agent version that backs up calendars,
// contacts and SharePoint.
const minM365PIMVersion = "0.35.0"

// m365MinVersion is the oldest agent that can run a job with these options.
func m365MinVersion(o JobOptions) string {
	if o.M365Calendar || o.M365SharePoint {
		return minM365PIMVersion
	}
	return minM365Version
}

func agentOlder(a Agent, min string) bool {
	return strings.Count(a.Version, ".") == 2 && update.Newer(min, a.Version)
}

// m365Parts names what a job backs up, in English, e.g. "Microsoft 365
// calendars and SharePoint" (for messages).
func m365Parts(o JobOptions) string {
	var p []string
	if o.M365Calendar {
		p = append(p, "calendars and contacts")
	}
	if o.M365SharePoint {
		p = append(p, "SharePoint")
	}
	if len(p) == 0 {
		return "Microsoft 365"
	}
	return "Microsoft 365 " + strings.Join(p, " and ")
}

// m365TestTimeout bounds the check of an app registration.
var m365TestTimeout = 20 * time.Second

// m365Precheck checks the app registration of a Microsoft 365 job before
// it is saved (see checkM365Access); other jobs pass. note is a remark for
// the page.
func (s *Server) m365Precheck(ctx context.Context, l *language, j Job) (note string, err error) {
	if j.Kind != JobM365 {
		return "", nil
	}
	if err := s.store.checkM365Job(&j); err != nil {
		return "", err
	}
	return s.checkM365Access(ctx, l, j)
}

// checkM365Access signs in as the app and checks its permissions and the
// named accounts, so mistakes show when the job is saved and not at the
// first backup. A console without internet access cannot check; then the
// note says so and err is nil.
func (s *Server) checkM365Access(ctx context.Context, l *language, j Job) (note string, err error) {
	secret, err := s.store.m365Secret(j.Options)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, m365TestTimeout)
	defer cancel()
	c := m365.NewClient(m365.Credentials{Tenant: j.Options.M365Tenant, Client: j.Options.M365Client, Secret: secret})
	roles, err := c.Roles(ctx)
	if err != nil {
		var ge *m365.Error
		if errors.As(err, &ge) {
			if h := m365Hint(ge.Message); h != "" {
				return "", fmt.Errorf("%v — %s", err, l.T(h))
			}
			return "", err
		}
		// No answer from Microsoft: the agent may still reach it.
		return l.T("The console could not reach Microsoft 365 to check the app (%v); the first backup shows whether it works.", err), nil
	}
	o := j.Options
	var need []string
	if !o.M365NoMail || !o.M365NoOneDrive || o.M365Calendar {
		need = append(need, "User.Read.All")
	}
	if !o.M365NoMail {
		need = append(need, "Mail.Read")
	}
	if !o.M365NoOneDrive {
		need = append(need, "Files.Read.All")
	}
	if o.M365Calendar {
		need = append(need, "Calendars.Read", "Contacts.Read")
	}
	if o.M365SharePoint {
		need = append(need, "Sites.Read.All")
	}
	var missing []string
	for _, n := range need {
		if !slices.Contains(roles, n) && !slices.Contains(roles, strings.Replace(n, ".Read", ".ReadWrite", 1)) {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return "", errors.New(l.T("The app is missing the application permissions %s. In the Microsoft Entra admin center open App registrations → the app → API permissions, add them as Microsoft Graph application permissions and click \"Grant admin consent\".", strings.Join(missing, ", ")))
	}
	for _, u := range j.Paths {
		if _, err := c.User(ctx, u); err != nil {
			if m365.IsNotFound(err) {
				return "", errors.New(l.T("The account %s does not exist in this tenant.", u))
			}
			return "", fmt.Errorf("%s: %w", u, err)
		}
	}
	for _, raw := range o.M365Sites {
		if _, err := c.SiteByURL(ctx, raw); err != nil {
			if m365.IsNotFound(err) {
				return "", errors.New(l.T("The SharePoint site %s does not exist in this tenant.", raw))
			}
			return "", fmt.Errorf("%s: %w", raw, err)
		}
	}
	return l.T("Microsoft 365 checked: the app signs in and has the permissions %s.", strings.Join(need, ", ")), nil
}

// m365Hint explains the usual sign-in errors (AADSTS codes).
func m365Hint(msg string) string {
	switch {
	case strings.Contains(msg, "AADSTS7000215"), strings.Contains(msg, "AADSTS7000222"):
		return "the client secret is wrong or expired; copy the secret's Value (not its Secret ID) from Certificates & secrets"
	case strings.Contains(msg, "AADSTS700016"):
		return "there is no app with this application (client) ID in the tenant"
	case strings.Contains(msg, "AADSTS90002"), strings.Contains(msg, "AADSTS900023"):
		return "the directory (tenant) ID is wrong"
	}
	return ""
}

// joinNote appends a translated remark to a translated message.
func joinNote(l *language, msg, note string) string {
	if note == "" {
		return l.T(msg)
	}
	return l.T(msg) + " " + note
}

// M365Items lists what a Microsoft 365 job backs up (texts to translate).
func (o JobOptions) M365Items() []string {
	var out []string
	if !o.M365NoMail {
		out = append(out, "Mail")
	}
	if !o.M365NoOneDrive {
		out = append(out, "OneDrive")
	}
	if o.M365Calendar {
		out = append(out, "Calendars and contacts")
	}
	if o.M365SharePoint {
		out = append(out, "SharePoint")
	}
	return out
}

// isM365Run reports whether a backup (or a copy of one) holds Microsoft
// 365 data, which is restored into a folder or downloaded, not to an
// original location.
func (s *Store) isM365Run(ctx context.Context, run Run) bool {
	if run.JobID == nil {
		return false
	}
	j, err := s.GetJob(ctx, *run.JobID)
	if err != nil {
		return false
	}
	if j.Kind == JobCopy && j.SourceJobID != nil {
		if j, err = s.GetJob(ctx, *j.SourceJobID); err != nil {
			return false
		}
	}
	return j.Kind == JobM365
}

// M365Accounts reports whether a Microsoft 365 job backs up accounts
// (mail, OneDrive, calendars or contacts), not only SharePoint.
func (o JobOptions) M365Accounts() bool {
	return !o.M365NoMail || !o.M365NoOneDrive || o.M365Calendar
}
