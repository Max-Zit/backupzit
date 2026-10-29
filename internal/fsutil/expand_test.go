package fsutil

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func mkdirs(t *testing.T, root string, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func setProfiles(t *testing.T, os_, root string, homes ...string) {
	t.Helper()
	g, u, h := goos, usersRoot, homeDirs
	goos, usersRoot, homeDirs = os_, func() string { return root }, homes
	t.Cleanup(func() { goos, usersRoot, homeDirs = g, u, h })
}

func rel(t *testing.T, root string, paths []string) []string {
	t.Helper()
	var out []string
	for _, p := range paths {
		r, err := filepath.Rel(root, p)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, filepath.ToSlash(r))
	}
	return out
}

func TestExpandUsersWindows(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "ana/Desktop", "ana/Documents", "marko/Documents", "marko/OneDrive - MaxZit/Desktop",
		"Public/Desktop", "Default/Desktop", ".hidden/Desktop")
	setProfiles(t, "windows", root)

	got, unmatched := ExpandSources([]string{"{users}/Desktop", `{USERS}\Documents`, "{users}/Downloads"})
	want := []string{"ana/Desktop", "marko/OneDrive - MaxZit/Desktop", "ana/Documents", "marko/Documents"}
	if r := rel(t, root, got); !slices.Equal(r, want) {
		t.Fatalf("paths = %v, want %v", r, want)
	}
	if !slices.Equal(unmatched, []string{"{users}/Downloads"}) {
		t.Fatalf("unmatched = %v", unmatched)
	}
	got, _ = ExpandSources([]string{"{users}"})
	if r := rel(t, root, got); !slices.Equal(r, []string{"ana", "marko"}) {
		t.Fatalf("whole profiles = %v", r)
	}
}

func TestExpandWildcards(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "ana/Desktop", "Public/Desktop", "data/p1/docs", "data/p2/docs", "data/p3/other")
	setProfiles(t, "linux", root)

	plain := filepath.Join(root, "data")
	got, unmatched := ExpandSources([]string{
		filepath.Join(root, "*", "Desktop"),      // system profile Public left out
		filepath.Join(root, "data", "*", "docs"), // any wildcard folder
		plain, plain,                             // plain paths kept once
		filepath.Join(root, "nothing", "*"), // matches nothing
	})
	want := []string{"ana/Desktop", "data/p1/docs", "data/p2/docs", "data"}
	if r := rel(t, root, got); !slices.Equal(r, want) {
		t.Fatalf("paths = %v, want %v", r, want)
	}
	if len(unmatched) != 1 {
		t.Fatalf("unmatched = %v", unmatched)
	}
}

func TestExpandUsersLinuxRoot(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	admin := filepath.Join(root, "root")
	mkdirs(t, root, "home/ana/Documents", "root/Documents")
	setProfiles(t, "linux", home, admin, filepath.Join(root, "missing"))
	got, _ := ExpandSources([]string{"{users}/Documents"})
	if r := rel(t, root, got); !slices.Equal(r, []string{"home/ana/Documents", "root/Documents"}) {
		t.Fatalf("paths = %v", r)
	}
}

func TestIsPattern(t *testing.T) {
	for p, want := range map[string]bool{
		"{users}": true, "{users}/Desktop": true, `{Users}\Documents`: true, "C:\\Users\\*\\Desktop": true,
		"/srv/data": false, `D:\Projects [2024]`: false, "{usersx}/a": false, "/srv/{users}": false,
	} {
		if IsPattern(p) != want {
			t.Errorf("IsPattern(%q) = %v", p, !want)
		}
	}
}

// On Windows a wildcard ignores upper and lower case, like the file system.
func TestExpandWildcardsFoldCase(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "Ana/Desktop", "Marko/DESKTOP", "Data/Proj1/Docs")
	setProfiles(t, "windows", root)
	got, _ := ExpandSources([]string{filepath.Join(root, "*", "desktop"), filepath.Join(root, "data", "PROJ*", "docs")})
	want := []string{"Ana/Desktop", "Marko/DESKTOP", "Data/Proj1/Docs"}
	if r := rel(t, root, got); !slices.Equal(r, want) {
		t.Fatalf("paths = %v, want %v", r, want)
	}
}
