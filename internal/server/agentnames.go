package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"unicode"
)

// Display names and tags of agents (issue #34). Several customers often use
// the same host names, so the console can show a name of its own. Rules:
//   - only console users with the agents permission set them, never the
//     agent (whose reported host name is untrusted and still updated);
//   - the reported host name stays visible next to the display name;
//   - renaming never touches repo_dir, so repositories and backups stay;
//   - tags are labels for finding agents only: no permissions, no
//     separation between customers, no automatic job membership.

const (
	maxDisplayName = 64
	maxTags        = 10
	maxTagLen      = 32
)

// agentNameSQL is the name shown for the agent joined as "a".
const agentNameSQL = `COALESCE(NULLIF(a.display_name,''), a.hostname)`

// Name is the name shown in the console: the display name, else the host
// name.
func (a Agent) Name() string {
	if a.DisplayName != "" {
		return a.DisplayName
	}
	return a.Hostname
}

// Label is the name with the reported host name when they differ, for
// choosing an agent in a list ("Customer A file server (SRV01)").
func (a Agent) Label() string {
	if a.DisplayName != "" && !strings.EqualFold(a.DisplayName, a.Hostname) {
		return a.DisplayName + " (" + a.Hostname + ")"
	}
	return a.Hostname
}

// HasTag reports whether the agent carries tag (case-insensitive).
func (a Agent) HasTag(tag string) bool {
	return slices.ContainsFunc(a.Tags, func(t string) bool { return strings.EqualFold(t, tag) })
}

// cleanLabel trims a name or tag and refuses control and format
// characters (line breaks, bidi overrides) and over-long values.
func cleanLabel(s string, max int, errControl, errLong error) (string, error) {
	s = strings.TrimSpace(s)
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", errControl
		}
	}
	if len([]rune(s)) > max {
		return "", errLong
	}
	return s, nil
}

var (
	errNameControl = errors.New("the name must not contain line breaks or other control characters")
	errNameLong    = fmt.Errorf("the name can be at most %d characters long", maxDisplayName)
	errTagControl  = errors.New("a tag must not contain line breaks or other control characters")
	errTagLong     = fmt.Errorf("a tag can be at most %d characters long", maxTagLen)
	errTooManyTags = fmt.Errorf("an agent can have at most %d tags", maxTags)
)

// parseAgentLabels validates a display name and a comma-separated tag list.
func parseAgentLabels(name, tags string) (string, []string, error) {
	name, err := cleanLabel(name, maxDisplayName, errNameControl, errNameLong)
	if err != nil {
		return "", nil, err
	}
	var out []string
	for _, t := range strings.Split(tags, ",") {
		t, err := cleanLabel(t, maxTagLen, errTagControl, errTagLong)
		if err != nil {
			return "", nil, err
		}
		if t == "" || slices.ContainsFunc(out, func(o string) bool { return strings.EqualFold(o, t) }) {
			continue
		}
		out = append(out, t)
	}
	if len(out) > maxTags {
		return "", nil, errTooManyTags
	}
	return name, out, nil
}

// SetAgentLabels stores the display name and tags of an agent.
func (s *Store) SetAgentLabels(ctx context.Context, id int64, name string, tags []string) error {
	if tags == nil {
		tags = []string{}
	}
	tag, err := s.db.Exec(ctx, `UPDATE agents SET display_name=$2, tags=$3 WHERE id=$1 AND removed_at IS NULL`, id, name, tags)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// agentTags lists the tags in use, sorted, for the filter of the Agents page.
func agentTags(agents []Agent) []string {
	var out []string
	for _, a := range agents {
		for _, t := range a.Tags {
			if !slices.ContainsFunc(out, func(o string) bool { return strings.EqualFold(o, t) }) {
				out = append(out, t)
			}
		}
	}
	slices.SortFunc(out, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	return out
}

func (s *Server) handleAgentPage(w http.ResponseWriter, r *http.Request, user string) {
	id, err := pathID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	a, err := s.store.GetAgent(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		s.serverError(w, err)
		return
	}
	all, err := s.store.ListAgents(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	// Other agents with the same host name: the reason to give this one a
	// name of its own.
	var same []Agent
	for _, o := range all {
		if o.ID != a.ID && strings.EqualFold(o.Hostname, a.Hostname) {
			same = append(same, o)
		}
	}
	s.render(w, r, "agent", pageData{Title: a.Name(), Nav: "agents", User: user, Data: map[string]any{
		"Agent": a, "SameHost": same, "Tags": agentTags(all), "MaxName": maxDisplayName, "MaxTags": maxTags, "MaxTagLen": maxTagLen,
	}})
}

func (s *Server) handleAgentLabels(w http.ResponseWriter, r *http.Request, _ string) {
	id, err := pathID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	back := fmt.Sprintf("/agents/%d", id)
	a, err := s.store.GetAgent(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		s.serverError(w, err)
		return
	}
	name, tags, err := parseAgentLabels(r.FormValue("display_name"), r.FormValue("tags"))
	if err != nil {
		redirectErr(w, r, back, err)
		return
	}
	if strings.EqualFold(name, a.Hostname) {
		name = "" // the same as the host name: nothing to show
	}
	if err := s.store.SetAgentLabels(r.Context(), id, name, tags); err != nil {
		redirectErr(w, r, back, err)
		return
	}
	if name != a.DisplayName {
		s.audit(r, "agent.rename", "agent %d (%s): name %q → %q", a.ID, a.Hostname, a.DisplayName, name)
	}
	if strings.Join(tags, ",") != strings.Join(a.Tags, ",") {
		s.audit(r, "agent.tags", "agent %d (%s): tags %q → %q", a.ID, a.Hostname, strings.Join(a.Tags, ", "), strings.Join(tags, ", "))
	}
	redirectMsg(w, r, back, "Saved.")
}
