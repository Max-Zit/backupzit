// Package launchd builds macOS launchd job definitions (property lists) and,
// on macOS, controls jobs of the system domain with launchctl.
//
// The property list is generated here instead of by kardianos/service: that
// library labels the job with the service name ("backupzit-agent", shared
// with Linux and Windows), only knows a few keys (no ThrottleInterval,
// ExitTimeOut, Nice, LaunchOnlyOnce) and controls jobs with the legacy
// "launchctl load/unload/list", which cannot see the system domain from a
// user's shell. The generation is pure, so it is tested on every OS.
package launchd

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Job is a launchd job definition (see launchd.plist(5)). Zero values are
// left out of the property list, so launchd applies its own defaults.
type Job struct {
	Label            string
	ProgramArguments []string
	UserName         string
	GroupName        string
	RunAtLoad        bool
	KeepAlive        bool
	// LaunchOnlyOnce runs the job once per boot: a one-shot job is not
	// started again if launchd itself restarts.
	LaunchOnlyOnce bool
	// MaterializeDatalessFiles, when set, overrides the system's policy for
	// reading "dataless" files (iCloud Drive with Optimize Mac Storage):
	// false makes such a read fail at once instead of asking the file
	// provider to download the file.
	MaterializeDatalessFiles *bool
	// AbandonProcessGroup keeps processes the job started when its main
	// process exits (launchd kills the process group otherwise).
	AbandonProcessGroup bool
	// ThrottleInterval is the minimum time between two starts, in seconds
	// (launchd's default is 10).
	ThrottleInterval int
	// ExitTimeOut is how long launchd waits after SIGTERM before SIGKILL, in
	// seconds (launchd's default is 20).
	ExitTimeOut       int
	Nice              int
	WorkingDirectory  string
	StandardOutPath   string
	StandardErrorPath string
	Environment       map[string]string
}

// Plist renders the job as an XML property list.
func (j Job) Plist() ([]byte, error) {
	if j.Label == "" || strings.ContainsAny(j.Label, "/ \t\n") {
		return nil, fmt.Errorf("invalid launchd label %q", j.Label)
	}
	if len(j.ProgramArguments) == 0 || !strings.HasPrefix(j.ProgramArguments[0], "/") {
		// launchd does not search PATH for a daemon: the program must be an
		// absolute path, or the job never starts.
		return nil, fmt.Errorf("launchd job %s: the program must be an absolute path", j.Label)
	}
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	// Keys in alphabetical order, as plutil writes them: the file reads the
	// same as one converted by Apple's tools, and the output is stable.
	if j.AbandonProcessGroup {
		writeBool(&b, "AbandonProcessGroup", true)
	}
	if len(j.Environment) > 0 {
		writeEnv(&b, j.Environment)
	}
	if j.ExitTimeOut > 0 {
		writeInt(&b, "ExitTimeOut", j.ExitTimeOut)
	}
	if j.GroupName != "" {
		writeString(&b, "GroupName", j.GroupName)
	}
	writeBool(&b, "KeepAlive", j.KeepAlive)
	writeString(&b, "Label", j.Label)
	if j.LaunchOnlyOnce {
		writeBool(&b, "LaunchOnlyOnce", true)
	}
	if j.MaterializeDatalessFiles != nil {
		writeBool(&b, "MaterializeDatalessFiles", *j.MaterializeDatalessFiles)
	}
	if j.Nice != 0 {
		writeInt(&b, "Nice", j.Nice)
	}
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, a := range j.ProgramArguments {
		b.WriteString("\t\t<string>" + escape(a) + "</string>\n")
	}
	b.WriteString("\t</array>\n")
	writeBool(&b, "RunAtLoad", j.RunAtLoad)
	if j.StandardErrorPath != "" {
		writeString(&b, "StandardErrorPath", j.StandardErrorPath)
	}
	if j.StandardOutPath != "" {
		writeString(&b, "StandardOutPath", j.StandardOutPath)
	}
	if j.ThrottleInterval > 0 {
		writeInt(&b, "ThrottleInterval", j.ThrottleInterval)
	}
	if j.UserName != "" {
		writeString(&b, "UserName", j.UserName)
	}
	if j.WorkingDirectory != "" {
		writeString(&b, "WorkingDirectory", j.WorkingDirectory)
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes(), nil
}

func writeString(b *bytes.Buffer, key, v string) {
	b.WriteString("\t<key>" + escape(key) + "</key>\n\t<string>" + escape(v) + "</string>\n")
}

func writeBool(b *bytes.Buffer, key string, v bool) {
	b.WriteString("\t<key>" + escape(key) + "</key>\n\t<" + strconv.FormatBool(v) + "/>\n")
}

func writeInt(b *bytes.Buffer, key string, v int) {
	b.WriteString("\t<key>" + escape(key) + "</key>\n\t<integer>" + strconv.Itoa(v) + "</integer>\n")
}

func writeEnv(b *bytes.Buffer, env map[string]string) {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
	for _, k := range keys {
		b.WriteString("\t\t<key>" + escape(k) + "</key>\n\t\t<string>" + escape(env[k]) + "</string>\n")
	}
	b.WriteString("\t</dict>\n")
}

// escape makes text safe inside an XML element: a path or a shell script
// with "&" or "<" must not end the element or break the file. Only "&",
// "<" and ">" become entities; newlines, tabs and quotes are written as
// they are, as Apple's plutil writes them. xml.EscapeText was not kept: it
// writes a newline as the hexadecimal reference "&#xA;", which launchd's
// property list parser does not decode, so a multi-line "/bin/sh -c"
// script reached the shell as one line where "&" backgrounded the first
// command and "#xA;..." made the rest a comment: updates and
// uninstalls never ran. Characters XML does not allow are replaced, as
// xml.EscapeText did, so the file stays readable.
func escape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '&':
			b.WriteString("&amp;")
		case r == '<':
			b.WriteString("&lt;")
		case r == '>':
			b.WriteString("&gt;")
		case r == '\t' || r == '\n' || r == '\r' || (r >= 0x20 && r <= 0xD7FF) || (r >= 0xE000 && r <= 0xFFFD) || (r >= 0x10000 && r <= 0x10FFFF):
			b.WriteRune(r)
		default:
			b.WriteRune('\uFFFD')
		}
	}
	return b.String()
}

// State is what launchctl reports for a job.
type State int

const (
	// NotLoaded: launchd does not know the job (not bootstrapped, or
	// booted out by "stop").
	NotLoaded State = iota
	// Stopped: the job is loaded but its process is not running (between
	// two restarts, or a one-shot job that finished).
	Stopped
	// Running: the job's process runs.
	Running
)

// ParsePrint reads the output of "launchctl print <domain>/<label>": the
// job runs when it has a pid or its state is "running". The format of that
// output is not an API, so anything unexpected is read as "loaded, not
// running" rather than failing.
func ParsePrint(out string) State {
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), " = ")
		if !ok {
			continue
		}
		switch {
		case k == "state" && v == "running":
			return Running
		case k == "pid":
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				return Running
			}
		}
	}
	return Stopped
}
