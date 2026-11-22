package launchd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func sampleJob() Job {
	return Job{
		Label:             "com.example.agent",
		ProgramArguments:  []string{"/Library/Example/bin/agent", "service", "run", "--config", "/Library/Application Support/Ex & <Co>/agent.json"},
		UserName:          "root",
		GroupName:         "wheel",
		RunAtLoad:         true,
		KeepAlive:         true,
		ThrottleInterval:  30,
		ExitTimeOut:       90,
		Nice:              10,
		StandardOutPath:   "/Library/Logs/Example/launchd.log",
		StandardErrorPath: "/Library/Logs/Example/launchd.log",
		Environment:       map[string]string{"B": "2", "A": "1"},
	}
}

func TestPlistContent(t *testing.T) {
	b, err := sampleJob().Plist()
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		"<key>Label</key>\n\t<string>com.example.agent</string>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<key>KeepAlive</key>\n\t<true/>",
		"<key>UserName</key>\n\t<string>root</string>",
		"<key>ThrottleInterval</key>\n\t<integer>30</integer>",
		"<key>ExitTimeOut</key>\n\t<integer>90</integer>",
		"<key>Nice</key>\n\t<integer>10</integer>",
		"<string>/Library/Application Support/Ex &amp; &lt;Co&gt;/agent.json</string>",
		"\t\t<key>A</key>\n\t\t<string>1</string>\n\t\t<key>B</key>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("plist lacks %q:\n%s", want, s)
		}
	}
	for _, absent := range []string{"LaunchOnlyOnce", "AbandonProcessGroup", "WorkingDirectory"} {
		if strings.Contains(s, absent) {
			t.Errorf("plist has unset key %s", absent)
		}
	}
	// Keys must be in order (stable output, as plutil writes it).
	if strings.Index(s, "<key>KeepAlive</key>") > strings.Index(s, "<key>Label</key>") ||
		strings.Index(s, "<key>ProgramArguments</key>") > strings.Index(s, "<key>RunAtLoad</key>") {
		t.Error("keys out of order")
	}
}

func TestPlistRefusesBadJobs(t *testing.T) {
	for name, j := range map[string]Job{
		"no label":      {ProgramArguments: []string{"/bin/true"}},
		"label slash":   {Label: "a/b", ProgramArguments: []string{"/bin/true"}},
		"no program":    {Label: "a"},
		"relative path": {Label: "a", ProgramArguments: []string{"true"}},
	} {
		if _, err := j.Plist(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// On macOS the generated file must be a property list Apple's own parser
// accepts, with the values it was given.
func TestPlistLint(t *testing.T) {
	plutil, err := exec.LookPath("plutil")
	if err != nil {
		t.Skip("plutil not available (not macOS)")
	}
	b, err := sampleJob().Plist()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "job.plist")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(plutil, "-lint", p).CombinedOutput(); err != nil {
		t.Fatalf("plutil -lint: %v: %s", err, out)
	}
	out, err := exec.Command(plutil, "-extract", "ProgramArguments.4", "raw", p).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "/Library/Application Support/Ex & <Co>/agent.json" {
		t.Errorf("argument read back as %q", got)
	}
}

func TestParsePrint(t *testing.T) {
	running := "system/com.example.agent = {\n\tactive count = 1\n\tstate = running\n\tpid = 423\n\tendpoints = {\n\t\tstate = active\n\t}\n}\n"
	if s := ParsePrint(running); s != Running {
		t.Errorf("running job read as %v", s)
	}
	waiting := "system/com.example.agent = {\n\tstate = not running\n\tlast exit code = 1\n\tsockets = {\n\t\tstate = active\n\t}\n}\n"
	if s := ParsePrint(waiting); s != Stopped {
		t.Errorf("stopped job read as %v", s)
	}
	if s := ParsePrint("unexpected output"); s != Stopped {
		t.Errorf("unknown output read as %v", s)
	}
}

// Text is written as plutil writes it: only &, < and > as entities, never
// a numeric reference, which launchd's parser does not decode (&#xA; for a
// newline cut every multi-line script after its first line, issue #5).
func TestEscapeLikePlutil(t *testing.T) {
	in := "printf '%s' \"$x\" >a 2>&1\necho <b>\ttab & done\x00"
	want := "printf '%s' \"$x\" &gt;a 2&gt;&amp;1\necho &lt;b&gt;\ttab &amp; done�"
	if got := escape(in); got != want {
		t.Fatalf("escape:\n%q\nwant\n%q", got, want)
	}
	plist, err := Job{Label: "com.backupzit.agent.update.1", ProgramArguments: []string{"/bin/sh", "-c", "echo one >f\necho two >>f"}, LaunchOnlyOnce: true, UserName: "root"}.Plist()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plist), "&#") {
		t.Fatalf("numeric character reference in the plist:\n%s", plist)
	}
	if !strings.Contains(string(plist), "echo one &gt;f\necho two &gt;&gt;f") {
		t.Fatalf("script not on its lines:\n%s", plist)
	}
}
