package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/imaging"
	"github.com/max-zit/backupzit/internal/recovery"
	"github.com/max-zit/backupzit/internal/repo"
)

// withLang switches the texts to language l for the rest of the test.
func withLang(t *testing.T, l string) {
	t.Helper()
	old := lang
	lang = l
	t.Cleanup(func() { lang = old })
}

func TestReasonTextIsTranslated(t *testing.T) {
	lim := imaging.PartitionLimits{Reason: "EFI System partitions keep their size", Why: imaging.ReasonFixedSize}
	withLang(t, "de")
	if got := reasonText(lim, "EFI System"); strings.Contains(got, "keep their size") || !strings.Contains(got, "EFI System") {
		t.Errorf("de: %q", got)
	}
	withLang(t, "en")
	if got := reasonText(lim, "EFI System"); !strings.Contains(got, "keep their size") {
		t.Errorf("en: %q", got)
	}
	withLang(t, "fr")
	if got := reasonText(lim, "EFI System"); strings.Contains(got, "keep their size") || !strings.Contains(got, "EFI System") {
		t.Errorf("fr: %q", got)
	}
	// A read error has no translation: its text is shown.
	withLang(t, "de")
	if got := reasonText(imaging.PartitionLimits{Reason: "read block 7: EOF", Why: imaging.ReasonError}, ""); !strings.Contains(got, "read block 7: EOF") {
		t.Errorf("error: %q", got)
	}
}

func TestHumanBytesUsesTheUnitsOfTheSizeFields(t *testing.T) {
	withLang(t, "de")
	for _, c := range []struct {
		b    uint64
		want string
	}{
		{512, "512 B"},
		{272629760, "260,0 MiB"},
		{42658168832, "39,7 GiB"},
		{40 << 30, "40,0 GiB"},
		{2 << 40, "2,0 TiB"},
	} {
		if got := humanBytes(c.b); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.b, got, c.want)
		}
	}
	// The decimal separator follows the language; the unit stays GiB, as
	// in the size fields.
	for l, want := range map[string]string{"en": "39.7 GiB", "ja": "39.7 GiB", "zh": "39.7 GiB", "fr": "39,7 GiB", "ru": "39,7 GiB", "sr": "39,7 GiB"} {
		withLang(t, l)
		if got := humanBytes(42658168832); got != want {
			t.Errorf("%s: humanBytes = %q, want %q", l, got, want)
		}
	}
}

func TestBarSpansKeepSmallPartitionsVisible(t *testing.T) {
	// The test disk: EFI 260 MiB, MSR 16 MiB, Windows 39.7 GiB on 40 GiB.
	parts := []span{{1 << 20, 272629760}, {273678336, 16777216}, {290455552, 42658168832}}
	got := barSpans(parts, 40<<30, 800, 60)
	if len(got) != 3 {
		t.Fatalf("got %v", got)
	}
	for i, g := range got {
		if g.w < 60 {
			t.Errorf("bar %d is %d px wide, want at least 60", i, g.w)
		}
		if i > 0 && g.x < got[i-1].x+got[i-1].w {
			t.Errorf("bar %d overlaps bar %d: %v", i, i-1, got)
		}
	}
	if end := got[2].x + got[2].w; end > 800 {
		t.Errorf("bars end at %d, wider than 800", end)
	}
	if got[2].w < 600 {
		t.Errorf("Windows should keep most of the width: %v", got)
	}
	// Free space at the end stays free.
	half := []span{{0, 20 << 30}}
	if g := barSpans(half, 40<<30, 800, 60); g[0].x != 0 || g[0].w != 400 {
		t.Errorf("half the disk: %v", g)
	}
}

func TestChooseBackupDir(t *testing.T) {
	cd, target, stick, other := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	for _, r := range []string{cd, target, stick} {
		if err := os.Mkdir(filepath.Join(r, "backupzit"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	readOnly := filepath.Join(cd, "backupzit")
	writable := func(dir string) bool { return dir != readOnly }
	avoid := map[string]bool{target: true}

	// The CD is read-only and the target is the disk being repaired: the stick wins.
	if got := chooseBackupDir([]string{cd, target, stick, other}, avoid, writable); got != filepath.Join(stick, "backupzit") {
		t.Errorf("got %q", got)
	}
	// Nothing suitable: the temp folder.
	if got := chooseBackupDir([]string{cd, target, other}, avoid, writable); got != os.TempDir() {
		t.Errorf("fallback: got %q", got)
	}
}

func TestLogTextInGerman(t *testing.T) {
	withLang(t, "de")
	for in, want := range map[string]string{
		`boot data backed up to D:\backupzit\boot-backup-disk0-1`:                `Bootdaten gesichert nach D:\backupzit\boot-backup-disk0-1`,
		"disk controller drivers set to start at boot: storahci stornvme pciide": "Treiber für 3 Disk-Controller beim Start aktiviert",
		"partition 1 is now active":                                              "Partition 1 ist jetzt aktiv",
		"MBR boot code written":                                                  "Windows-Bootcode in den MBR geschrieben",
		"boot files for UEFI":                                                    "Bootdateien für UEFI",
		"firmware UEFI (Secure Boot on), boot files for UEFI":                    "Firmware UEFI (Secure Boot an), Bootdateien für UEFI",
		"firmware unknown":                                                       "Firmware unbekannt",
		`Windows found on E:`:                                                    `Windows gefunden auf E:`,
		`new hardware: Windows found on E:`:                                      `Neue Hardware: Windows gefunden auf E:`,
		"wrote 3 partitions, 42947575808 bytes in 9m46s":                         "3 Partitionen geschrieben, 40,0 GiB in 9m46s",
		"verifying the snapshot":                                                 "Backup wird geprüft",
		"snapshot verified":                                                      "Backup geprüft, keine Fehler",
		"warning: rebuild the boot files: x":                                     "Hinweis: rebuild the boot files: x",
		"something new":                                                          "something new",
	} {
		if got := logText(in); got != want {
			t.Errorf("logText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLogTextInOtherLanguages(t *testing.T) {
	// English keeps the lines, only the counts and sizes read better.
	withLang(t, "en")
	for in, want := range map[string]string{
		"disk controller drivers set to start at boot: a b":   "disk controller drivers enabled at start: 2",
		"wrote 3 partitions, 42947575808 bytes in 9m46s":      "wrote 3 partitions, 40.0 GiB in 9m46s",
		"firmware UEFI (Secure Boot on), boot files for UEFI": "firmware UEFI (Secure Boot on), boot files for UEFI",
		`new hardware: Windows found on E:`:                   `new hardware: Windows found on E:`,
		"something new":                                       "something new",
	} {
		if got := logText(in); got != want {
			t.Errorf("en: logText(%q) = %q, want %q", in, got, want)
		}
	}
	// The other languages take the translations; arguments stay as they are.
	withLang(t, "sr")
	for in, want := range map[string]string{
		"partition 1 is now active": "particija 1 je sada aktivna",
		`Windows found on E:`:       `Windows pronađen na E:`,
		"something new":             "something new",
	} {
		if got := logText(in); got != want {
			t.Errorf("sr: logText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFirmwareText(t *testing.T) {
	for l, want := range map[string]string{"en": "UEFI (Secure Boot on)", "de": "UEFI (Secure Boot an)", "sr": "UEFI (Secure Boot uključen)"} {
		withLang(t, l)
		if got := firmwareText("UEFI (Secure Boot on)"); got != want {
			t.Errorf("%s: %q, want %q", l, got, want)
		}
	}
}

func TestProblemText(t *testing.T) {
	withLang(t, "de")
	if got := problemText("0xC0000490"); !strings.Contains(got, "kein passender Treiber") || !strings.Contains(got, "0xC0000490") {
		t.Errorf("missing driver: %q", got)
	}
	if got := problemText("CM_PROB_FAILED_INSTALL (28)"); !strings.Contains(got, "kein passender Treiber") {
		t.Errorf("code 28: %q", got)
	}
	if got := problemText("0x0000000A"); got != "0x0000000A" {
		t.Errorf("other: %q", got)
	}
}

func TestNormLang(t *testing.T) {
	for in, want := range map[string]string{
		"de-DE": "de", "DE": "de", "en-GB": "en", "sr-Latn-RS": "sr", "zh-CN": "zh", "zh_TW": "zh",
		"pt-BR": "pt", "ja-jp": "ja", " tr-TR ": "tr",
		// No texts in these: English is chosen by detectLang.
		"hr-HR": "", "ko-KR": "", "": "", "x": "",
	} {
		if got := normLang(in); got != want {
			t.Errorf("normLang(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDetectLang(t *testing.T) {
	sys := func(l string) func() string { return func() string { return l } }
	for _, c := range []struct {
		name   string
		args   []string
		env    string
		system string
		want   string
	}{
		{"the command line wins", []string{"--open=restore", "--lang=fr"}, "de", "ja-JP", "fr"},
		{"then the language the medium was built with", nil, "sr-latn-rs", "en-US", "sr"},
		{"then the display language", nil, "", "pl-PL", "pl"},
		{"an unknown language on the command line is passed over", []string{"--lang=xx"}, "", "it-IT", "it"},
		{"an unknown build language is passed over", nil, "hr-hr", "cs-CZ", "cs"},
		{"English when nothing fits", nil, "", "ko-KR", "en"},
		{"English when the display language cannot be read", nil, "", "", "en"},
	} {
		if got := detectLang(c.args, c.env, sys(c.system)); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestUntranslatedTextStaysEnglish(t *testing.T) {
	withLang(t, "fr")
	if got := t2("nicht übersetzt", "not translated yet"); got != "not translated yet" {
		t.Errorf("got %q", got)
	}
}

// t2 is the program's t for tests, where t is the *testing.T.
func t2(de, en string) string { return t(de, en) }

// TestLanguagesAreTheConsoles keeps the program in the languages of the
// console: a language added there must be added here too.
func TestLanguagesAreTheConsoles(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "internal", "server", "i18n", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("console catalogs: %v %v", files, err)
	}
	var console []string
	for _, f := range files {
		console = append(console, strings.TrimSuffix(filepath.Base(f), ".json"))
	}
	have := slices.Clone(languages)
	slices.Sort(console)
	slices.Sort(have)
	if !slices.Equal(console, have) {
		t.Errorf("console languages %v, recovery program %v", console, have)
	}
}

// TestEveryTextIsTranslated checks the texts against the translations:
// every language has every text, with the format verbs of the English
// one, the same spaces and line breaks at the ends, and no entries for
// texts the program no longer has. German is checked against English too.
func TestEveryTextIsTranslated(t *testing.T) {
	texts := programTexts(t)
	if len(texts) < 150 {
		t.Fatalf("only %d texts found; is the parser looking at the right files?", len(texts))
	}
	used := map[string]bool{}
	for _, p := range texts {
		used[p.en] = true
		if msg := compareText(p.en, p.de); msg != "" {
			t.Errorf("%s: German %s: %q", p.pos, msg, p.de)
		}
	}
	for l := range translations {
		if !slices.Contains(languages, l) || l == "de" || l == "en" {
			t.Errorf("translations for %q: not a language of the program, or written in the code", l)
		}
	}
	for _, l := range languages {
		if l == "de" || l == "en" {
			continue
		}
		m := translations[l]
		var missing []string
		for en := range used {
			s, ok := m[en]
			switch {
			case !ok || s == "":
				missing = append(missing, strconv.Quote(en))
			default:
				if msg := compareText(en, s); msg != "" {
					t.Errorf("%s: %s for %q: %q", l, msg, en, s)
				}
			}
		}
		slices.Sort(missing)
		for _, en := range missing {
			t.Errorf("%s: no translation for %s", l, en)
		}
		for en := range m {
			if !used[en] {
				t.Errorf("%s: translation of %q, a text the program no longer has", l, en)
			}
		}
	}
}

// compareText says what is wrong with a translation s of the English
// text en, or "" when it fits: the same format verbs for the same
// arguments, the same spaces and line breaks at both ends, the same number
// of lines, and a format that fmt can fill without complaint.
func compareText(en, s string) string {
	if a, b := verbs(en), verbs(s); !slices.Equal(a, b) {
		return fmt.Sprintf("format verbs %v, English has %v", b, a)
	}
	const ws = " \r\n"
	if lead := func(x string) string { return x[:len(x)-len(strings.TrimLeft(x, ws))] }; lead(en) != lead(s) {
		return "other spaces or line breaks at the start"
	}
	if trail := func(x string) string { return x[len(strings.TrimRight(x, ws)):] }; trail(en) != trail(s) {
		return "other spaces or line breaks at the end"
	}
	if strings.Count(en, "\n") != strings.Count(s, "\n") {
		return "another number of lines"
	}
	if vs := verbs(en); len(vs) > 0 {
		args := make([]any, len(vs))
		for _, v := range vs {
			n, _ := strconv.Atoi(v[:strings.IndexByte(v, ':')])
			if v[len(v)-1] == 'd' {
				args[n-1] = 1
			} else {
				args[n-1] = "x"
			}
		}
		if out := fmt.Sprintf(s, args...); strings.Contains(out, "%!") {
			return "a format fmt cannot fill: " + out
		}
	}
	return ""
}

// verbs lists the format verbs of s as "argument:verb", sorted, so a
// translation that names its arguments (%[2]s) where the sentence needs
// another order compares equal to the English.
func verbs(s string) []string {
	const flags = "+-# 0123456789."
	var out []string
	arg := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			continue
		}
		i++
		if i < len(s) && s[i] == '%' {
			continue
		}
		for i < len(s) && strings.IndexByte(flags, s[i]) >= 0 {
			i++
		}
		if i < len(s) && s[i] == '[' {
			end := strings.IndexByte(s[i:], ']')
			if end < 0 {
				return append(out, "bad argument index")
			}
			n, err := strconv.Atoi(s[i+1 : i+end])
			if err != nil || n < 1 {
				return append(out, "bad argument index")
			}
			arg = n - 1
			i += end + 1
			for i < len(s) && strings.IndexByte(flags, s[i]) >= 0 {
				i++
			}
		}
		if i >= len(s) {
			return append(out, "% at the end")
		}
		out = append(out, fmt.Sprintf("%d:%c", arg+1, s[i]))
		arg++
	}
	slices.Sort(out)
	return out
}

// textPair is a text of the program as written in the code: German and
// English, and where it is.
type textPair struct{ de, en, pos string }

// programTexts returns every text of the program: the literals passed to
// t and tf, and the entries of [][2]string tables (German, English) that
// are passed to t by index, in all source files of every platform.
func programTexts(tb testing.TB) []textPair {
	tb.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		tb.Fatal(err)
	}
	fset := token.NewFileSet()
	var out []textPair
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			tb.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				// t and tf themselves pass their arguments on.
				return n.Name.Name != "t" && n.Name.Name != "tf"
			case *ast.CompositeLit:
				if types.ExprString(n.Type) != "[][2]string" {
					return true
				}
				for _, e := range n.Elts {
					el, ok := e.(*ast.CompositeLit)
					if !ok || len(el.Elts) != 2 {
						tb.Errorf("%s: a text table entry must be {German, English}", fset.Position(e.Pos()))
						continue
					}
					de, ok1 := stringValue(el.Elts[0])
					en, ok2 := stringValue(el.Elts[1])
					if !ok1 || !ok2 {
						tb.Errorf("%s: a text table entry must be two string literals", fset.Position(e.Pos()))
						continue
					}
					out = append(out, textPair{de, en, fset.Position(e.Pos()).String()})
				}
			case *ast.CallExpr:
				id, ok := n.Fun.(*ast.Ident)
				if !ok || (id.Name != "t" && id.Name != "tf") || len(n.Args) < 2 {
					return true
				}
				de, ok1 := stringValue(n.Args[0])
				en, ok2 := stringValue(n.Args[1])
				if ok1 && ok2 {
					out = append(out, textPair{de, en, fset.Position(n.Pos()).String()})
					return true
				}
				// A lookup in a text table (collected above) is fine; any
				// other expression could not be checked.
				_, idx1 := n.Args[0].(*ast.IndexExpr)
				_, idx2 := n.Args[1].(*ast.IndexExpr)
				if !idx1 || !idx2 {
					tb.Errorf("%s: %s needs string literals (or a [][2]string table) so its translations can be checked", fset.Position(n.Pos()), id.Name)
				}
			}
			return true
		})
	}
	return out
}

// stringValue is the value of a string literal or of a sum of them.
func stringValue(e ast.Expr) (string, bool) {
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(e.Value)
		return s, err == nil
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		a, ok1 := stringValue(e.X)
		b, ok2 := stringValue(e.Y)
		return a + b, ok1 && ok2
	case *ast.ParenExpr:
		return stringValue(e.X)
	}
	return "", false
}

// #61 finding 1: a partition that can grow but not shrink says why its
// minimum is its size, in the language of the program; the explanation of
// the file system check stays as it is.
func TestLimitsInfoNamesTheReason(t *testing.T) {
	p := &repo.PartitionImage{Number: 3, Length: 58 << 30}
	held := imaging.PartitionLimits{Resizable: true, MinSize: 40 << 30, UsedBytes: 13 << 30, Why: imaging.ReasonNoShrink,
		Reason: imaging.NoShrinkPrefix + "the file system in the backup was not shut down cleanly"}
	for l, want := range map[string]string{
		"en": "used 13.0 GiB, at least 40.0 GiB – cannot be shrunk: the file system in the backup was not shut down cleanly",
		"de": "belegt 13,0 GiB, mindestens 40,0 GiB – nicht verkleinerbar: the file system in the backup was not shut down cleanly",
		"sr": "zauzeto 13,0 GiB, najmanje 40,0 GiB – ne može da se smanji: the file system in the backup was not shut down cleanly",
	} {
		withLang(t, l)
		if got := limitsInfo(held, p); got != want {
			t.Errorf("%s: %q, want %q", l, got, want)
		}
	}
	withLang(t, "de")
	if got := limitsInfo(imaging.PartitionLimits{Resizable: true, MinSize: 20 << 30, UsedBytes: 13 << 30}, p); got != "belegt 13,0 GiB, mindestens 20,0 GiB" {
		t.Errorf("without a reason: %q", got)
	}
}

// The "too small" errors of the layout page are in the language of the
// program and list the partitions held up by a reason.
func TestLayoutErrText(t *testing.T) {
	why := imaging.PartitionLimits{Resizable: true, Why: imaging.ReasonNoShrink, Reason: imaging.NoShrinkPrefix + "dirty"}
	small := &imaging.TooSmallError{DiskSize: 25 << 30, Needed: 40 << 30, Held: []imaging.HeldPartition{{Partition: 3, Limits: why}}}
	withLang(t, "de")
	if got, want := layoutErrText(fmt.Errorf("wrapped: %w", small)), "Die Zieldisk ist zu klein: 25,0 GiB, die Partitionen brauchen mindestens 40,0 GiB.\r\nPartition 3: nicht verkleinerbar: dirty"; got != want {
		t.Errorf("too small: %q, want %q", got, want)
	}
	minErr := &imaging.MinSizeError{Partition: 3, Min: 40 << 30, Length: 30 << 30, Limits: why}
	if got, want := layoutErrText(minErr), "Partition 3 braucht mindestens 40,0 GiB, das Layout gibt ihr 30,0 GiB.\r\nPartition 3: nicht verkleinerbar: dirty"; got != want {
		t.Errorf("needs at least: %q, want %q", got, want)
	}
	// Sizes that round alike are given in bytes, so the message does not
	// say "40,0 GiB, at least 40,0 GiB".
	withLang(t, "en")
	if got := layoutErrText(&imaging.TooSmallError{DiskSize: 40 << 30, Needed: 40<<30 + 1<<20}); !strings.Contains(got, "42949672960 B") || !strings.Contains(got, "42950721536 B") {
		t.Errorf("close sizes: %q", got)
	}
	if got := layoutErrText(errors.New("an MBR disk holds at most 4 partitions")); got != "an MBR disk holds at most 4 partitions" {
		t.Errorf("other errors stay as they are: %q", got)
	}
}

// #61: the warnings of a restore that the program knows are shown in its
// language (the line about shrunk partitions was English in the German
// program), in the final message and in the log; the English stays the
// text of the restore functions.
func TestWarningText(t *testing.T) {
	offline := fmt.Sprintf(imaging.SameIDOfflineWarning, 3, 2)
	withLang(t, "en")
	for _, s := range []string{recovery.WarnShrunk, offline, "rebuild the boot files: x"} {
		if got := warningText(s); got != s {
			t.Errorf("en: %q became %q", s, got)
		}
	}
	for _, l := range languages {
		if l == "en" {
			continue
		}
		withLang(t, l)
		if got := warningText(recovery.WarnShrunk); got == recovery.WarnShrunk || !strings.Contains(got, "chkdsk") {
			t.Errorf("%s: shrunk: %q", l, got)
		}
		if got := warningText(offline); got == offline || !strings.Contains(got, "3") || !strings.Contains(got, "2") {
			t.Errorf("%s: offline: %q", l, got)
		}
		if got := warningText("partition 3: not extended, because of something else"); got != "partition 3: not extended, because of something else" {
			t.Errorf("%s: a text that only starts the same: %q", l, got)
		}
	}
	withLang(t, "de")
	if got := logText("warning: " + recovery.WarnShrunk); !strings.HasPrefix(got, "Hinweis: Partitionen wurden verkleinert") {
		t.Errorf("log line: %q", got)
	}
}

// #61 finding 2: the warning before a restore next to the backed up disk
// names that disk and the option and the button it refers to, as the
// program calls them in each language.
func TestSourceDiskText(t *testing.T) {
	label := func(s string) string {
		if i := strings.IndexAny(s, "(（"); i > 0 {
			s = s[:i]
		}
		return strings.TrimSpace(s)
	}
	for _, l := range languages {
		withLang(t, l)
		got := sourceDiskText(7)
		keep := label(t2("Zieldisk danach offline lassen (wenn die alte Disk noch im Rechner steckt)", "Leave the target disk offline (when the old disk is still in the computer)"))
		repair := t2("Boot reparieren", "Repair boot")
		if !strings.Contains(got, "7") || !strings.Contains(got, keep) || !strings.Contains(got, repair) {
			t.Errorf("%s: %q does not name disk 7, %q and %q", l, got, keep, repair)
		}
	}
}

// #61: a disk that was never initialized is not listed as MBR.
func TestDiskStyleText(t *testing.T) {
	withLang(t, "de")
	if got := diskStyleText(imaging.StyleRaw); got != "nicht initialisiert" {
		t.Errorf("raw: %q", got)
	}
	if got := diskStyleText(imaging.StyleGPT); got != "GPT" {
		t.Errorf("gpt: %q", got)
	}
	withLang(t, "fr")
	if got := diskStyleText(imaging.StyleRaw); got != "non initialisé" {
		t.Errorf("fr raw: %q", got)
	}
}

// #61: the percentage keeps "%" with the number; a plain space let the
// label wrap it away and show "100".
func TestPercentText(t *testing.T) {
	for _, c := range []struct {
		done, total uint64
		want        string
	}{
		{1, 1, "100\u00a0%"},
		{0, 10, "0\u00a0%"},
		{5, 10, "50\u00a0%"},
		{11, 10, "100\u00a0%"},
		{0, 0, "100\u00a0%"},
	} {
		if got := percentText(c.done, c.total); got != c.want || strings.Contains(got, " ") {
			t.Errorf("percentText(%d, %d) = %q, want %q", c.done, c.total, got, c.want)
		}
	}
}
