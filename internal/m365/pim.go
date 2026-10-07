package m365

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/repo"
)

// Calendars and contacts are stored in the standard formats that Outlook
// and other programs open: one iCalendar file (.ics) per event (a series
// once, with its recurrence rule) and one vCard (.vcf) per contact.

// Calendar is a calendar of a mailbox.
type Calendar struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type dateTimeTZ struct {
	DateTime string `json:"dateTime"`
	TimeZone string `json:"timeZone"`
}

type emailAddress struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

// Event is an event or the master of a recurring series.
type Event struct {
	ID           string                   `json:"id"`
	ICalUID      string                   `json:"iCalUId"`
	Subject      string                   `json:"subject"`
	Body         struct{ Content string } `json:"body"`
	Start        dateTimeTZ               `json:"start"`
	End          dateTimeTZ               `json:"end"`
	IsAllDay     bool                     `json:"isAllDay"`
	ShowAs       string                   `json:"showAs"`
	Sensitivity  string                   `json:"sensitivity"`
	IsCancelled  bool                     `json:"isCancelled"`
	Created      time.Time                `json:"createdDateTime"`
	LastModified time.Time                `json:"lastModifiedDateTime"`
	Categories   []string                 `json:"categories"`
	Location     struct {
		DisplayName string `json:"displayName"`
	} `json:"location"`
	Organizer struct {
		EmailAddress emailAddress `json:"emailAddress"`
	} `json:"organizer"`
	Attendees []struct {
		Type         string       `json:"type"`
		EmailAddress emailAddress `json:"emailAddress"`
		Status       struct {
			Response string `json:"response"`
		} `json:"status"`
	} `json:"attendees"`
	IsReminderOn bool `json:"isReminderOn"`
	ReminderMins int  `json:"reminderMinutesBeforeStart"`
	Recurrence   *struct {
		Pattern struct {
			Type           string   `json:"type"`
			Interval       int      `json:"interval"`
			Month          int      `json:"month"`
			DayOfMonth     int      `json:"dayOfMonth"`
			DaysOfWeek     []string `json:"daysOfWeek"`
			FirstDayOfWeek string   `json:"firstDayOfWeek"`
			Index          string   `json:"index"`
		} `json:"pattern"`
		Range struct {
			Type                string `json:"type"`
			StartDate           string `json:"startDate"`
			EndDate             string `json:"endDate"`
			NumberOfOccurrences int    `json:"numberOfOccurrences"`
		} `json:"range"`
	} `json:"recurrence"`
}

const eventFields = "id,iCalUId,subject,body,start,end,isAllDay,showAs,sensitivity,isCancelled,createdDateTime,lastModifiedDateTime," +
	"categories,location,organizer,attendees,isReminderOn,reminderMinutesBeforeStart,recurrence"

func (c *Client) calendars(ctx context.Context, user string) ([]Calendar, error) {
	return listAll[Calendar](ctx, c, "/users/"+url.PathEscape(user)+"/calendars?$top=100&$select=id,name")
}

// events lists the single events and series masters of a calendar (not
// the occurrences of series).
func (c *Client) events(ctx context.Context, user, cal string) ([]Event, error) {
	return listAll[Event](withPrefer(ctx), c, "/users/"+url.PathEscape(user)+"/calendars/"+url.PathEscape(cal)+
		"/events?$top=250&$select="+eventFields)
}

// ContactFolder is a folder of contacts; the default folder has no entry.
type ContactFolder struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

type physicalAddress struct {
	Street          string `json:"street"`
	City            string `json:"city"`
	State           string `json:"state"`
	CountryOrRegion string `json:"countryOrRegion"`
	PostalCode      string `json:"postalCode"`
}

// Contact is a personal contact.
type Contact struct {
	ID               string          `json:"id"`
	DisplayName      string          `json:"displayName"`
	GivenName        string          `json:"givenName"`
	MiddleName       string          `json:"middleName"`
	Surname          string          `json:"surname"`
	Title            string          `json:"title"`
	NickName         string          `json:"nickName"`
	CompanyName      string          `json:"companyName"`
	Department       string          `json:"department"`
	JobTitle         string          `json:"jobTitle"`
	EmailAddresses   []emailAddress  `json:"emailAddresses"`
	BusinessPhones   []string        `json:"businessPhones"`
	HomePhones       []string        `json:"homePhones"`
	MobilePhone      string          `json:"mobilePhone"`
	BusinessAddress  physicalAddress `json:"businessAddress"`
	HomeAddress      physicalAddress `json:"homeAddress"`
	OtherAddress     physicalAddress `json:"otherAddress"`
	Birthday         *time.Time      `json:"birthday"`
	PersonalNotes    string          `json:"personalNotes"`
	BusinessHomePage string          `json:"businessHomePage"`
	Categories       []string        `json:"categories"`
	LastModified     time.Time       `json:"lastModifiedDateTime"`
}

func (c *Client) contactFolders(ctx context.Context, user, parent string) ([]ContactFolder, error) {
	p := "/users/" + url.PathEscape(user) + "/contactFolders"
	if parent != "" {
		p += "/" + url.PathEscape(parent) + "/childFolders"
	}
	return listAll[ContactFolder](ctx, c, p+"?$top=100&$select=id,displayName")
}

// contacts lists the contacts of a folder ("" = the default folder).
func (c *Client) contacts(ctx context.Context, user, folder string) ([]Contact, error) {
	p := "/users/" + url.PathEscape(user) + "/contacts"
	if folder != "" {
		p = "/users/" + url.PathEscape(user) + "/contactFolders/" + url.PathEscape(folder) + "/contacts"
	}
	return listAll[Contact](ctx, c, p+"?$top=500")
}

// icsText escapes a value of an iCalendar or vCard property.
func icsText(s string) string {
	return strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\r\n", `\n`, "\n", `\n`, "\r", `\n`).Replace(s)
}

// foldLines writes content lines folded at 75 octets (RFC 5545 3.1).
func foldLines(lines []string) []byte {
	var b strings.Builder
	for _, l := range lines {
		for len(l) > 75 {
			cut := 75
			for cut > 0 && !utf8Start(l[cut]) {
				cut--
			}
			b.WriteString(l[:cut] + "\r\n ")
			l = l[cut:]
		}
		b.WriteString(l + "\r\n")
	}
	return []byte(b.String())
}

func utf8Start(c byte) bool { return c&0xC0 != 0x80 }

// graphTime reads a Graph dateTime in UTC ("2026-10-07T09:00:00.0000000").
func graphTime(d dateTimeTZ) time.Time {
	t, err := time.Parse("2006-01-02T15:04:05.9999999", d.DateTime)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

var icsDays = map[string]string{"sunday": "SU", "monday": "MO", "tuesday": "TU", "wednesday": "WE", "thursday": "TH", "friday": "FR", "saturday": "SA"}

// rrule converts a Graph recurrence to an iCalendar rule.
func (e *Event) rrule() string {
	r := e.Recurrence
	if r == nil {
		return ""
	}
	p := r.Pattern
	var parts []string
	days := func() string {
		var d []string
		for _, x := range p.DaysOfWeek {
			if v := icsDays[strings.ToLower(x)]; v != "" {
				d = append(d, v)
			}
		}
		return strings.Join(d, ",")
	}
	pos := map[string]string{"first": "1", "second": "2", "third": "3", "fourth": "4", "last": "-1"}[p.Index]
	switch p.Type {
	case "daily":
		parts = append(parts, "FREQ=DAILY")
	case "weekly":
		parts = append(parts, "FREQ=WEEKLY", "BYDAY="+days())
		if wk := icsDays[strings.ToLower(p.FirstDayOfWeek)]; wk != "" {
			parts = append(parts, "WKST="+wk)
		}
	case "absoluteMonthly":
		parts = append(parts, "FREQ=MONTHLY", fmt.Sprintf("BYMONTHDAY=%d", p.DayOfMonth))
	case "relativeMonthly":
		parts = append(parts, "FREQ=MONTHLY", "BYDAY="+days(), "BYSETPOS="+pos)
	case "absoluteYearly":
		parts = append(parts, "FREQ=YEARLY", fmt.Sprintf("BYMONTH=%d", p.Month), fmt.Sprintf("BYMONTHDAY=%d", p.DayOfMonth))
	case "relativeYearly":
		parts = append(parts, "FREQ=YEARLY", fmt.Sprintf("BYMONTH=%d", p.Month), "BYDAY="+days(), "BYSETPOS="+pos)
	default:
		return ""
	}
	if p.Interval > 1 {
		parts = append(parts, fmt.Sprintf("INTERVAL=%d", p.Interval))
	}
	switch r.Range.Type {
	case "endDate":
		if t, err := time.Parse("2006-01-02", r.Range.EndDate); err == nil {
			parts = append(parts, "UNTIL="+t.Add(24*time.Hour-time.Second).Format("20060102T150405Z"))
		}
	case "numbered":
		parts = append(parts, fmt.Sprintf("COUNT=%d", r.Range.NumberOfOccurrences))
	}
	return "RRULE:" + strings.Join(parts, ";")
}

// ICS is the event as an iCalendar file.
func (e *Event) ICS() []byte {
	l := []string{"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//MaxZit//BackupZit//EN", "BEGIN:VEVENT"}
	uid := e.ICalUID
	if uid == "" {
		uid = e.ID
	}
	l = append(l, "UID:"+uid, "DTSTAMP:"+e.LastModified.UTC().Format("20060102T150405Z"))
	start, end := graphTime(e.Start), graphTime(e.End)
	if e.IsAllDay {
		l = append(l, "DTSTART;VALUE=DATE:"+start.Format("20060102"), "DTEND;VALUE=DATE:"+end.Format("20060102"))
	} else {
		l = append(l, "DTSTART:"+start.Format("20060102T150405Z"), "DTEND:"+end.Format("20060102T150405Z"))
	}
	if rr := e.rrule(); rr != "" {
		l = append(l, rr)
	}
	l = append(l, "SUMMARY:"+icsText(e.Subject))
	if s := strings.TrimSpace(e.Location.DisplayName); s != "" {
		l = append(l, "LOCATION:"+icsText(s))
	}
	if s := strings.TrimSpace(e.Body.Content); s != "" {
		l = append(l, "DESCRIPTION:"+icsText(s))
	}
	if o := e.Organizer.EmailAddress; o.Address != "" {
		l = append(l, fmt.Sprintf("ORGANIZER;CN=%s:mailto:%s", icsParam(o.Name), o.Address))
	}
	for _, a := range e.Attendees {
		if a.EmailAddress.Address == "" {
			continue
		}
		role := "REQ-PARTICIPANT"
		if a.Type == "optional" {
			role = "OPT-PARTICIPANT"
		}
		stat := map[string]string{"accepted": "ACCEPTED", "declined": "DECLINED", "tentativelyAccepted": "TENTATIVE"}[a.Status.Response]
		if stat == "" {
			stat = "NEEDS-ACTION"
		}
		l = append(l, fmt.Sprintf("ATTENDEE;CN=%s;ROLE=%s;PARTSTAT=%s:mailto:%s", icsParam(a.EmailAddress.Name), role, stat, a.EmailAddress.Address))
	}
	if len(e.Categories) > 0 {
		cats := make([]string, len(e.Categories))
		for i, c := range e.Categories {
			cats[i] = icsText(c)
		}
		l = append(l, "CATEGORIES:"+strings.Join(cats, ","))
	}
	switch e.Sensitivity {
	case "private", "confidential":
		l = append(l, "CLASS:"+strings.ToUpper(e.Sensitivity))
	}
	if e.ShowAs == "free" {
		l = append(l, "TRANSP:TRANSPARENT")
	}
	if e.IsCancelled {
		l = append(l, "STATUS:CANCELLED")
	}
	if !e.Created.IsZero() {
		l = append(l, "CREATED:"+e.Created.UTC().Format("20060102T150405Z"))
	}
	l = append(l, "LAST-MODIFIED:"+e.LastModified.UTC().Format("20060102T150405Z"))
	if e.IsReminderOn {
		l = append(l, "BEGIN:VALARM", "ACTION:DISPLAY", "DESCRIPTION:Reminder", fmt.Sprintf("TRIGGER:-PT%dM", e.ReminderMins), "END:VALARM")
	}
	l = append(l, "END:VEVENT", "END:VCALENDAR")
	return foldLines(l)
}

// icsParam quotes a parameter value.
func icsParam(s string) string {
	s = strings.NewReplacer(`"`, "'", "\r", " ", "\n", " ").Replace(s)
	if strings.ContainsAny(s, ";:,") {
		return `"` + s + `"`
	}
	return s
}

// VCard is the contact as a vCard 3.0 file.
func (k *Contact) VCard() []byte {
	l := []string{"BEGIN:VCARD", "VERSION:3.0"}
	fn := strings.TrimSpace(k.DisplayName)
	if fn == "" {
		fn = strings.TrimSpace(k.GivenName + " " + k.Surname)
	}
	l = append(l, "FN:"+icsText(fn),
		fmt.Sprintf("N:%s;%s;%s;%s;", icsText(k.Surname), icsText(k.GivenName), icsText(k.MiddleName), icsText(k.Title)))
	if k.NickName != "" {
		l = append(l, "NICKNAME:"+icsText(k.NickName))
	}
	if k.CompanyName != "" || k.Department != "" {
		l = append(l, fmt.Sprintf("ORG:%s;%s", icsText(k.CompanyName), icsText(k.Department)))
	}
	if k.JobTitle != "" {
		l = append(l, "TITLE:"+icsText(k.JobTitle))
	}
	for i, e := range k.EmailAddresses {
		if e.Address == "" {
			continue
		}
		pref := ""
		if i == 0 {
			pref = ",PREF"
		}
		l = append(l, "EMAIL;TYPE=INTERNET"+pref+":"+e.Address)
	}
	for _, p := range k.BusinessPhones {
		l = append(l, "TEL;TYPE=WORK,VOICE:"+icsText(p))
	}
	for _, p := range k.HomePhones {
		l = append(l, "TEL;TYPE=HOME,VOICE:"+icsText(p))
	}
	if k.MobilePhone != "" {
		l = append(l, "TEL;TYPE=CELL:"+icsText(k.MobilePhone))
	}
	for _, x := range []struct {
		typ string
		a   physicalAddress
	}{{"WORK", k.BusinessAddress}, {"HOME", k.HomeAddress}, {"OTHER", k.OtherAddress}} {
		typ, a := x.typ, x.a
		if a == (physicalAddress{}) {
			continue
		}
		l = append(l, fmt.Sprintf("ADR;TYPE=%s:;;%s;%s;%s;%s;%s", typ, icsText(a.Street), icsText(a.City), icsText(a.State), icsText(a.PostalCode), icsText(a.CountryOrRegion)))
	}
	if k.Birthday != nil && !k.Birthday.IsZero() {
		l = append(l, "BDAY:"+k.Birthday.UTC().Format("2006-01-02"))
	}
	if k.BusinessHomePage != "" {
		l = append(l, "URL:"+k.BusinessHomePage)
	}
	if k.PersonalNotes != "" {
		l = append(l, "NOTE:"+icsText(k.PersonalNotes))
	}
	if len(k.Categories) > 0 {
		cats := make([]string, len(k.Categories))
		for i, c := range k.Categories {
			cats[i] = icsText(c)
		}
		l = append(l, "CATEGORIES:"+strings.Join(cats, ","))
	}
	l = append(l, "REV:"+k.LastModified.UTC().Format("20060102T150405Z"), "END:VCARD")
	return foldLines(l)
}

// calendars stores each calendar of an account as a folder of .ics files.
func (b *backup) calendars(ctx context.Context, u User, ptree *repo.Tree) (repo.Node, bool, error) {
	at := u.UserPrincipalName + "/" + CalendarDir
	cals, err := b.c.calendars(ctx, u.ID)
	if err != nil {
		if IsNotFound(err) {
			return repo.Node{}, false, nil
		}
		if isDenied(err) {
			return repo.Node{}, false, fmt.Errorf("read the calendars of %s: %w (grant the application permission Calendars.Read with admin consent)", u.UserPrincipalName, err)
		}
		b.addError(at, err)
		return repo.Node{}, false, nil
	}
	sort.Slice(cals, func(i, j int) bool { return cals[i].Name < cals[j].Name })
	t := &repo.Tree{}
	used := map[string]bool{}
	for _, cal := range cals {
		name := uniqueName(used, cleanName(cal.Name, 120))
		evs, err := b.c.events(ctx, u.ID, cal.ID)
		if err != nil {
			if ctx.Err() != nil {
				return repo.Node{}, false, ctx.Err()
			}
			b.addError(at+"/"+name, err)
			continue
		}
		items := make([]item, len(evs))
		fused := map[string]bool{}
		for i := range evs {
			e := &evs[i]
			items[i] = item{name: uniqueName(fused, eventName(e)), mtime: e.LastModified,
				open: func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(e.ICS())), nil }}
		}
		nodes, err := b.files(ctx, at+"/"+name, items, b.subtree(ctx, ptree, name))
		if err != nil {
			return repo.Node{}, false, err
		}
		n, err := b.dirNode(ctx, name, &repo.Tree{Nodes: nodes})
		if err != nil {
			return repo.Node{}, false, err
		}
		t.Nodes = append(t.Nodes, n)
	}
	n, err := b.dirNode(ctx, CalendarDir, t)
	return n, true, err
}

// contactBook stores the contacts of an account as .vcf files: the
// default folder directly below Contacts, other folders as subfolders.
func (b *backup) contactBook(ctx context.Context, u User, ptree *repo.Tree) (repo.Node, bool, error) {
	at := u.UserPrincipalName + "/" + ContactsDir
	t, err := b.contactFolder(ctx, u, at, "", ptree)
	if err != nil {
		if IsNotFound(err) {
			return repo.Node{}, false, nil
		}
		if isDenied(err) {
			return repo.Node{}, false, fmt.Errorf("read the contacts of %s: %w (grant the application permission Contacts.Read with admin consent)", u.UserPrincipalName, err)
		}
		if ctx.Err() != nil {
			return repo.Node{}, false, ctx.Err()
		}
		b.addError(at, err)
		return repo.Node{}, false, nil
	}
	n, err := b.dirNode(ctx, ContactsDir, t)
	return n, true, err
}

// contactFolder stores one folder of contacts and its subfolders. Errors
// of the account's default folder are returned; those of subfolders are
// recorded.
func (b *backup) contactFolder(ctx context.Context, u User, at, id string, ptree *repo.Tree) (*repo.Tree, error) {
	ks, err := b.c.contacts(ctx, u.ID, id)
	if err != nil {
		return nil, err
	}
	folders, err := b.c.contactFolders(ctx, u.ID, id)
	if err != nil {
		if id == "" {
			return nil, err
		}
		b.addError(at, err)
	}
	sort.Slice(folders, func(i, j int) bool { return folders[i].DisplayName < folders[j].DisplayName })
	t := &repo.Tree{}
	used := map[string]bool{}
	for _, f := range folders {
		name := uniqueName(used, cleanName(f.DisplayName, 120))
		sub, err := b.contactFolder(ctx, u, at+"/"+name, f.ID, b.subtree(ctx, ptree, name))
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			b.addError(at+"/"+name, err)
			continue
		}
		n, err := b.dirNode(ctx, name, sub)
		if err != nil {
			return nil, err
		}
		t.Nodes = append(t.Nodes, n)
	}
	items := make([]item, len(ks))
	for i := range ks {
		k := &ks[i]
		items[i] = item{name: uniqueName(used, contactName(k)), mtime: k.LastModified,
			open: func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(k.VCard())), nil }}
	}
	nodes, err := b.files(ctx, at, items, ptree)
	if err != nil {
		return nil, err
	}
	t.Nodes = append(t.Nodes, nodes...)
	return t, nil
}

// eventName names an event file by its start, subject and ID.
func eventName(e *Event) string {
	subj := cleanName(e.Subject, 80)
	if strings.TrimSpace(e.Subject) == "" {
		subj = "(no subject)"
	}
	start := graphTime(e.Start)
	ts := start.Format("2006-01-02 1504")
	if e.IsAllDay {
		ts = start.Format("2006-01-02")
	}
	return fmt.Sprintf("%s %s [%s].ics", ts, subj, idHash(e.ID))
}

// contactName names a contact file by its name and ID.
func contactName(k *Contact) string {
	n := strings.TrimSpace(k.DisplayName)
	if n == "" && len(k.EmailAddresses) > 0 {
		n = k.EmailAddresses[0].Address
	}
	if n == "" {
		n = "(no name)"
	}
	return fmt.Sprintf("%s [%s].vcf", cleanName(n, 80), idHash(k.ID))
}
