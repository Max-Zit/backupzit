package fsutil

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
)

// UsersToken at the start of a source stands for the profile folder of
// every user on the computer: {users}/Documents backs up the Documents
// folder of each of them, on any operating system.
const UsersToken = "{users}"

// IsPattern reports whether a source is expanded on the agent: it starts
// with {users} or contains a wildcard (* or ?). Brackets alone are no
// pattern: folder names such as "Projects [2024]" are common.
func IsPattern(p string) bool {
	return hasUsersToken(p) || strings.ContainsAny(p, "*?")
}

func hasUsersToken(p string) bool {
	return len(p) >= len(UsersToken) && strings.EqualFold(p[:len(UsersToken)], UsersToken) &&
		(len(p) == len(UsersToken) || p[len(UsersToken)] == '/' || p[len(UsersToken)] == '\\')
}

// Where the profiles are; variables for the tests.
var (
	goos      = runtime.GOOS
	usersRoot = defaultUsersRoot
	homeDirs  = []string{"/root"} // Linux: besides /home/*
)

func defaultUsersRoot() string {
	switch goos {
	case "windows":
		if p := os.Getenv("PUBLIC"); p != "" { // C:\Users\Public
			return filepath.Dir(p)
		}
		if d := os.Getenv("SystemDrive"); d != "" {
			return d + `\Users`
		}
		return `C:\Users`
	case "darwin":
		return "/Users"
	}
	return "/home"
}

// System profiles that are no person's files.
var systemProfiles = []string{"all users", "default", "default user", "defaultuser0", "public", "wdagutilityaccount", "shared", "guest"}

// userProfiles lists the profile folders of the users of this computer.
func userProfiles() []string {
	root := usersRoot()
	entries, err := os.ReadDir(root)
	var out []string
	if err == nil {
		for _, e := range entries {
			if isUserProfile(root, e.Name()) {
				out = append(out, filepath.Join(root, e.Name()))
			}
		}
	}
	if goos == "linux" {
		for _, h := range homeDirs {
			if fi, err := os.Stat(h); err == nil && fi.IsDir() {
				out = append(out, h)
			}
		}
	}
	return out
}

func isUserProfile(root, name string) bool {
	if strings.HasPrefix(name, ".") || slices.Contains(systemProfiles, strings.ToLower(name)) {
		return false
	}
	// Junctions such as "Default User" and "All Users" are links.
	fi, err := os.Lstat(filepath.Join(root, name))
	return err == nil && fi.IsDir()
}

// inSystemProfile reports whether a wildcard match lies in a system
// profile (C:\Users\Public\Desktop for C:\Users\*\Desktop).
func inSystemProfile(p string) bool {
	root := usersRoot()
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return false
	}
	first := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
	return !isUserProfile(root, first)
}

// ExpandSources replaces {users} and wildcards in the sources of a backup
// with the folders and files that exist now. Plain paths are kept as they
// are. unmatched lists the patterns that matched nothing, which is no error:
// a computer may have no such folder, or a job can hold the paths of
// several operating systems.
func ExpandSources(sources []string) (paths, unmatched []string) {
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	for _, src := range sources {
		if !IsPattern(src) {
			add(src)
			continue
		}
		if _, err := os.Lstat(src); err == nil && !hasUsersToken(src) {
			add(src) // a name that really contains * or ?
			continue
		}
		var found []string
		if hasUsersToken(src) {
			found = expandUsers(src)
		} else {
			found = glob(src)
		}
		if len(found) == 0 {
			unmatched = append(unmatched, src)
		}
		for _, p := range found {
			add(p)
		}
	}
	return paths, unmatched
}

// expandUsers resolves {users}/sub for each profile. On Windows a folder
// that OneDrive moved (Desktop, Documents, Pictures) is found there too.
func expandUsers(src string) []string {
	sub := strings.Trim(src[len(UsersToken):], `/\`)
	var out []string
	for _, prof := range userProfiles() {
		if sub == "" {
			out = append(out, prof)
			continue
		}
		pattern := filepath.Join(prof, filepath.FromSlash(strings.ReplaceAll(sub, `\`, "/")))
		out = append(out, glob(pattern)...)
		if goos == "windows" {
			out = append(out, glob(filepath.Join(prof, "OneDrive*", filepath.FromSlash(strings.ReplaceAll(sub, `\`, "/"))))...)
		}
	}
	return out
}

// glob returns the existing paths matching a pattern, without those in
// system profiles. A pattern without wildcards matches itself if it exists.
func glob(pattern string) []string {
	var matches []string
	if strings.ContainsAny(pattern, "*?") {
		if goos == "windows" {
			matches = globFold(pattern)
		} else {
			matches, _ = filepath.Glob(pattern)
		}
	} else if _, err := os.Lstat(pattern); err == nil {
		matches = []string{pattern}
	}
	out := matches[:0]
	for _, m := range matches {
		if !inSystemProfile(m) {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

// globFold is filepath.Glob without regard to upper and lower case, as
// Windows treats names: C:\users\*\desktop finds C:\Users\Ana\Desktop.
// Matches carry the names as they are on disk.
func globFold(pattern string) []string {
	vol := filepath.VolumeName(pattern)
	parts := strings.FieldsFunc(pattern[len(vol):], func(r rune) bool { return r == '\\' || r == '/' })
	cur := []string{vol + string(filepath.Separator)}
	if !filepath.IsAbs(pattern) {
		cur = []string{"."}
	}
	for _, part := range parts {
		var next []string
		for _, dir := range cur {
			entries, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			pat := strings.ToLower(part)
			for _, e := range entries {
				if ok, err := filepath.Match(pat, strings.ToLower(e.Name())); err == nil && ok {
					next = append(next, filepath.Join(dir, e.Name()))
				}
			}
		}
		cur = next
	}
	return cur
}
