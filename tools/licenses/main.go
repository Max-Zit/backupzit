// Command licenses writes THIRD_PARTY_LICENSES.txt: the license texts of
// every Go module linked into the BackupZit programs (Windows, Linux and
// macOS builds), with the NOTICE files of Apache-licensed modules and the
// licenses inside modules (copied code, embedded files, notices at the top
// of single source files), which the permissive licenses require to ship
// with binaries, and the license of the WiX parts in the Windows installer.
// It fails on licenses that are not known to be compatible with AGPLv3.
//
//	go run ./tools/licenses > THIRD_PARTY_LICENSES.txt
//
// On Windows run it in Git Bash: PowerShell's ">" changes the encoding. The
// output has LF line ends everywhere, so CI can compare it byte for byte
// with the committed file.
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

// embedded lists other files built into the programs or the installers
// with their license; about, when set, comes before the license text.
var embedded = []struct{ name, license, file, about string }{
	{"DejaVu Sans fonts (internal/server/fonts, used in PDF reports)", "Bitstream Vera / DejaVu font license", "internal/server/fonts/LICENSE-DejaVu.txt", ""},
	{"Droid Sans Fallback font (internal/server/fonts, used in PDF reports)", "Apache License 2.0", "internal/server/fonts/LICENSE-DroidSansFallback.txt", ""},
	{"Feather icons (https://feathericons.com; the menu and button icons of the console and of the tray window are drawn after them)", "MIT License", "tools/licenses/feather.txt", ""},
	{"WiX Toolset 5.0.2 (https://wixtoolset.org), only in the Windows agent installer (MSI)", "Microsoft Reciprocal License (MS-RL)", "tools/licenses/ms-rl.txt", wixAbout},
}

// wixAbout names the WiX parts the agent MSI carries (what
// packaging/windows/agent.wxs uses of the UI and Util extensions that
// build-msi.ps1 builds it with, as found in the MSI's Binary, Dialog and
// CustomAction tables). The MS-RL asks for the license text and the source
// code of these files.
const wixAbout = `The Windows agent installer (backupzit-agent-*.msi) is built with the WiX
Toolset 5.0.2 and carries these unmodified WiX parts:
- from WixToolset.UI.wixext (WixUI): the standard dialogs of the installer
  (preparing, progress, files in use, disk space, error, cancel and the
  closing pages), their texts, and the information, warning, new folder and
  up folder icons (the banner and the side picture are BackupZit's own);
- from WixToolset.Util.wixext: the custom action library Wix4UtilCA
  (utilca.dll), which closes the tray program and sets the recovery options
  of the agent service during the installation.
The BackupZit programs contain no WiX code, and the other packages (Linux,
macOS, Docker image, appliance and recovery images) carry no WiX parts.

Copyright (c) .NET Foundation and contributors. All rights reserved.
Source code: https://github.com/wixtoolset/wix (tag v5.0.2; the dialogs are
in src/ext/UI, the custom actions in src/ext/Util).`

// inFile lists license notices written at the top of single source files of
// linked modules instead of in a license file, so the module's license does
// not cover them. The notice is copied from the file each time this runs.
// sameAs names a linked module whose license file holds the file's notice
// word for word (checked here); the notice is then not repeated.
var inFile = []struct{ mod, file, license, sameAs string }{
	// SCRAM sign-in of the PostgreSQL driver, which embedded-postgres links
	// into the console (Linux and Windows); lib/pq itself is MIT.
	{"github.com/lib/pq", "scram/scram.go", "BSD 2-Clause License", ""},
	// Adapted from go-ole, as its first comment says (the Windows tray app);
	// the MIT notice in it is go-ole's license, listed with that module.
	{"github.com/jchv/go-webview2", "pkg/edge/guid.go", "MIT License", "github.com/go-ole/go-ole"},
}

type module struct {
	Path, Version, Dir string
	// Replace is set for a module replaced by a patched copy in the tree.
	Replace *struct{ Path string }
}

// nested is a license file inside a module, below its root: a package that
// carries code of another project (go-mssqldb's internal copies), or files
// a package embeds into the programs (go-webview2's WebView2Loader.dll).
type nested struct {
	mod       module
	rel, file string
}

func main() {
	mods := map[string]module{}
	inner := map[string]nested{} // by file path
	linked := map[string]bool{}  // Go files of modules built into the programs
	for _, t := range []struct{ goos, goarch, cgo string }{{"windows", "amd64", ""}, {"linux", "amd64", ""}, {"darwin", "arm64", "1"}} {
		cmd := exec.Command("go", "list", "-deps", "-json=Dir,Module,GoFiles,EmbedFiles", "./cmd/...")
		cmd.Env = append(os.Environ(), "GOOS="+t.goos, "GOARCH="+t.goarch)
		if t.cgo != "" {
			// The macOS menu bar app is cgo code: list its packages too.
			cmd.Env = append(cmd.Env, "CGO_ENABLED="+t.cgo)
		}
		out, err := cmd.Output()
		if err != nil {
			fail("go list (%s): %v", t.goos, err)
		}
		dec := json.NewDecoder(bytes.NewReader(out))
		for dec.More() {
			var p struct {
				Dir        string
				Module     *module
				GoFiles    []string
				EmbedFiles []string
			}
			if err := dec.Decode(&p); err != nil {
				fail("decode: %v", err)
			}
			m := p.Module
			if m == nil || m.Path == "github.com/max-zit/backupzit" || m.Dir == "" {
				continue
			}
			mods[m.Path] = *m
			for _, f := range p.GoFiles {
				linked[filepath.Join(p.Dir, f)] = true
			}
			// License files between the package and the module root, and
			// next to the files it embeds.
			dirs := parentsBelow(p.Dir, m.Dir)
			for _, e := range p.EmbedFiles {
				dirs = append(dirs, parentsBelow(filepath.Join(p.Dir, filepath.Dir(e)), m.Dir)...)
			}
			for _, d := range dirs {
				if _, name := licenseText(d); name != "no license file" {
					f := filepath.Join(d, name)
					rel, _ := filepath.Rel(m.Dir, d)
					inner[f] = nested{*m, filepath.ToSlash(rel), f}
				}
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
		label := kind
		if m.Replace != nil && strings.HasPrefix(m.Replace.Path, "./") {
			label += " (patched copy in " + strings.TrimPrefix(m.Replace.Path, "./") + ", changes listed in its NOTICE.BackupZit)"
		}
		fmt.Fprintf(&b, "%s\n%s %s — %s\n%s\n\n%s\n\n", strings.Repeat("=", 78), m.Path, m.Version, label, strings.Repeat("=", 78), strings.TrimSpace(text))
		// Apache License 2.0, 4(d): its NOTICE file ships too.
		if n := noticeText(m.Dir); n != "" {
			fmt.Fprintf(&b, "NOTICE of %s:\n\n%s\n\n", m.Path, strings.TrimSpace(n))
		}
	}
	var files []string
	for f := range inner {
		files = append(files, f)
	}
	sort.Slice(files, func(i, j int) bool {
		a, c := inner[files[i]], inner[files[j]]
		return a.mod.Path+"/"+a.rel < c.mod.Path+"/"+c.rel
	})
	for _, f := range files {
		n := inner[f]
		text, err := os.ReadFile(f) // #nosec G304 -- license file in the module cache
		if err != nil {
			fail("%v", err)
		}
		kind := classify(string(text))
		if kind == "" {
			bad = append(bad, n.mod.Path+"/"+n.rel)
		}
		fmt.Fprintf(&b, "%s\n%s %s, %s — %s\n%s\n\n%s\n\n", strings.Repeat("=", 78), n.mod.Path, n.mod.Version, n.rel, kind, strings.Repeat("=", 78), strings.TrimSpace(string(text)))
		if t := noticeText(filepath.Dir(f)); t != "" {
			fmt.Fprintf(&b, "NOTICE of %s/%s:\n\n%s\n\n", n.mod.Path, n.rel, strings.TrimSpace(t))
		}
	}
	// Notices at the top of single files of modules.
	for _, f := range inFile {
		m, ok := mods[f.mod]
		path := filepath.Join(m.Dir, filepath.FromSlash(f.file))
		if !ok || !linked[path] {
			fail("%s/%s (inFile) is no longer built into the programs: check it and update the list", f.mod, f.file)
		}
		src, err := os.ReadFile(path) // #nosec G304 -- file in the module cache
		if err != nil {
			fail("%v", err)
		}
		var text string
		if f.sameAs != "" {
			o := mods[f.sameAs]
			lic, _ := licenseText(o.Dir)
			if !sameNotice(string(src), lic) {
				fail("%s/%s (inFile): its notice is not the license of %s any more: check it by hand", f.mod, f.file, f.sameAs)
			}
			text = fmt.Sprintf("The notice in this file is the license of %s %s (above), word for word.", f.sameAs, o.Version)
		} else {
			text = headerComment(string(src))
			if classify(text) == "" || !strings.Contains(strings.ToLower(text), "copyright") {
				bad = append(bad, f.mod+"/"+f.file)
			}
		}
		fmt.Fprintf(&b, "%s\n%s %s, %s — %s\n%s\n\n%s\n\n", strings.Repeat("=", 78), m.Path, m.Version, f.file, f.license, strings.Repeat("=", 78), text)
	}
	// Code adapted from other projects keeps its license header in the
	// source file; its notice ships here too.
	for _, f := range adapted {
		src, err := os.ReadFile(f.file)
		if err != nil {
			fail("%v", err)
		}
		fmt.Fprintf(&b2, "%s\n%s (in %s) — %s\n%s\n\n%s\n\n", strings.Repeat("=", 78), f.project, f.file, f.license, strings.Repeat("=", 78), headerComment(string(src)))
	}
	for _, f := range embedded {
		text, err := os.ReadFile(f.file)
		if err != nil {
			fail("%v", err)
		}
		fmt.Fprintf(&b2, "%s\n%s — %s\n%s\n\n", strings.Repeat("=", 78), f.name, f.license, strings.Repeat("=", 78))
		if f.about != "" {
			fmt.Fprintf(&b2, "%s\n\n", strings.TrimSpace(f.about))
		}
		fmt.Fprintf(&b2, "%s\n\n", strings.TrimSpace(string(text)))
	}
	b.WriteString(b2.String())
	if len(bad) > 0 {
		fail("licenses not known to be AGPLv3-compatible, check by hand: %s", strings.Join(bad, ", "))
	}
	// Some license files have CRLF line ends (go-nfs-client); git stores
	// the committed copy with LF, so LF here keeps the two the same.
	_, _ = os.Stdout.WriteString(strings.ReplaceAll(b.String(), "\r\n", "\n"))
}

// headerComment returns the "//" comment lines at the top of a Go file
// without the comment marks: the notice of a file that carries one.
func headerComment(src string) string {
	var lines []string
	for _, l := range strings.Split(src, "\n") {
		l = strings.TrimSuffix(l, "\r")
		if !strings.HasPrefix(l, "//") {
			break
		}
		lines = append(lines, strings.TrimPrefix(strings.TrimPrefix(l, "//"), " "))
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// sameNotice reports whether a source file contains the license text, apart
// from line breaks and indentation.
func sameNotice(src, license string) bool {
	squash := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	l := squash(license)
	return l != "" && strings.Contains(squash(src), l)
}

func licenseText(dir string) (string, string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		n := strings.ToLower(e.Name())
		if strings.HasPrefix(n, "license") || strings.HasPrefix(n, "licence") || strings.HasPrefix(n, "copying") || strings.HasPrefix(n, "unlicense") {
			if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil { // #nosec G304 -- path from the command line of a development tool
				return string(b), e.Name()
			}
		}
	}
	return "", "no license file"
}

// noticeText returns the NOTICE file of a folder, if there is one.
func noticeText(dir string) string {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if n := strings.ToLower(e.Name()); n == "notice" || strings.HasPrefix(n, "notice.") {
			if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil { // #nosec G304 -- file in the module cache
				return string(b)
			}
		}
	}
	return ""
}

// parentsBelow lists dir and its parent folders up to, not including, root.
func parentsBelow(dir, root string) []string {
	var out []string
	for d := filepath.Clean(dir); d != filepath.Clean(root) && strings.HasPrefix(d, filepath.Clean(root)+string(filepath.Separator)); d = filepath.Dir(d) {
		out = append(out, d)
	}
	return out
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
	case strings.Contains(t, "permission to use, copy, modify, and/or distribute"), strings.Contains(t, "permission to use, copy, modify, and distribute this software for any purpose with or without fee"), strings.Contains(t, "isc license"):
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
