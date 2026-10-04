// Command licenses writes THIRD_PARTY_LICENSES.txt: the license texts of
// every Go module linked into the BackupZit programs (Windows and Linux
// builds), which the permissive licenses require to ship with binaries.
// It fails on licenses that are not known to be compatible with AGPLv3.
//
//	go run ./tools/licenses > THIRD_PARTY_LICENSES.txt
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// adapted lists source files derived from other projects. Their header
// comment holds the original copyright and license.
var adapted = []struct{ file, project, license string }{
	{"internal/vss/vss_windows.go", "restic (https://github.com/restic/restic)", "BSD 2-Clause License"},
}

type module struct {
	Path, Version, Dir string
}

func main() {
	mods := map[string]module{}
	for _, goos := range []string{"windows", "linux"} {
		cmd := exec.Command("go", "list", "-deps", "-json=Module", "./cmd/...")
		cmd.Env = append(os.Environ(), "GOOS="+goos)
		out, err := cmd.Output()
		if err != nil {
			fail("go list: %v", err)
		}
		dec := json.NewDecoder(bytes.NewReader(out))
		for dec.More() {
			var p struct{ Module *module }
			if err := dec.Decode(&p); err != nil {
				fail("decode: %v", err)
			}
			if m := p.Module; m != nil && m.Path != "github.com/max-zit/backupzit" {
				mods[m.Path] = *m
			}
		}
	}
	var paths []string
	for p := range mods {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var b strings.Builder
	b.WriteString("BackupZit includes the following third-party software.\n")
	b.WriteString("BackupZit itself is licensed under the GNU Affero General Public License v3 (see LICENSE).\n\n")
	var bad []string
	var b2 strings.Builder
	for _, p := range paths {
		m := mods[p]
		text, name := licenseText(m.Dir)
		kind := classify(text)
		if kind == "" {
			bad = append(bad, p+" ("+name+")")
		}
		fmt.Fprintf(&b, "%s\n%s %s — %s\n%s\n\n%s\n\n", strings.Repeat("=", 78), m.Path, m.Version, kind, strings.Repeat("=", 78), strings.TrimSpace(text))
	}
	// Code adapted from other projects keeps its license header in the
	// source file; its notice ships here too.
	for _, f := range adapted {
		b, err := os.ReadFile(f.file)
		if err != nil {
			fail("%v", err)
		}
		var lines []string
		for _, l := range strings.Split(string(b), "\n") {
			if !strings.HasPrefix(l, "//") {
				break
			}
			lines = append(lines, strings.TrimPrefix(strings.TrimPrefix(l, "//"), " "))
		}
		fmt.Fprintf(&b2, "%s\n%s (in %s) — %s\n%s\n\n%s\n\n", strings.Repeat("=", 78), f.project, f.file, f.license, strings.Repeat("=", 78), strings.TrimSpace(strings.Join(lines, "\n")))
	}
	b.WriteString(b2.String())
	if len(bad) > 0 {
		fail("licenses not known to be AGPLv3-compatible, check by hand: %s", strings.Join(bad, ", "))
	}
	os.Stdout.WriteString(b.String())
}

func licenseText(dir string) (string, string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		n := strings.ToLower(e.Name())
		if strings.HasPrefix(n, "license") || strings.HasPrefix(n, "licence") || strings.HasPrefix(n, "copying") || strings.HasPrefix(n, "unlicense") {
			if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil {
				return string(b), e.Name()
			}
		}
	}
	return "", "no license file"
}

// classify names licenses compatible with distribution under AGPLv3.
func classify(t string) string {
	t = strings.ToLower(strings.Join(strings.Fields(t), " "))
	switch {
	case strings.Contains(t, "business source license"), strings.Contains(t, "server side public license"), strings.Contains(t, "commons clause"):
		return ""
	case strings.Contains(t, "apache license"):
		return "Apache License 2.0"
	case strings.Contains(t, "mozilla public license"):
		return "Mozilla Public License 2.0"
	case strings.Contains(t, "permission is hereby granted, free of charge"):
		return "MIT License"
	case strings.Contains(t, "redistribution and use in source and binary forms"):
		return "BSD License"
	case strings.Contains(t, "permission to use, copy, modify, and/or distribute"), strings.Contains(t, "isc license"):
		return "ISC License"
	case strings.Contains(t, "this software is provided 'as-is'") && strings.Contains(t, "altered source versions must be plainly marked"):
		return "zlib License"
	case strings.Contains(t, "put into the public domain"), strings.Contains(t, "this is free and unencumbered software"):
		return "Public domain"
	}
	return ""
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "licenses: "+format+"\n", a...)
	os.Exit(1)
}
