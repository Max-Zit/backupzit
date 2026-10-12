package server

import (
	"bytes"
	"embed"
	"fmt"
	"net/http"
	"strings"
	"unicode"

	"github.com/go-pdf/fpdf"
)

// PDF reports: the report of a period as a printable A4 document for a
// manager or a client — key figures, a daily chart, every job and the
// problems. Text is written with DejaVu Sans (Latin, Cyrillic, Greek);
// Chinese and Japanese characters with Droid Sans Fallback, so names in
// any script print in every console language.

//go:embed fonts/DejaVuSans.ttf fonts/DejaVuSans-Bold.ttf fonts/DroidSansFallbackFull.ttf
var pdfFonts embed.FS

// Colors of the PDF (RGB).
var (
	pdfAccent = [3]int{79, 70, 229}
	pdfText   = [3]int{15, 23, 42}
	pdfMuted  = [3]int{100, 116, 139}
	pdfLine   = [3]int{226, 232, 240}
	pdfZebra  = [3]int{248, 250, 252}
	pdfOK     = [3]int{22, 163, 74}
	pdfWarn   = [3]int{217, 119, 6}
	pdfBad    = [3]int{220, 38, 38}
)

// pdfWriter writes text in runs of one font: CJK characters in the CJK
// font, everything else in DejaVu.
type pdfWriter struct {
	pdf   *fpdf.Fpdf
	style string
	size  float64
}

func isCJK(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) ||
		(r >= 0x3000 && r <= 0x30ff) || (r >= 0x31f0 && r <= 0x31ff) || (r >= 0xff00 && r <= 0xffef)
}

type pdfRun struct {
	text string
	cjk  bool
}

func pdfRuns(s string) []pdfRun {
	var out []pdfRun
	for _, r := range s {
		c := isCJK(r)
		if n := len(out); n > 0 && out[n-1].cjk == c {
			out[n-1].text += string(r)
			continue
		}
		out = append(out, pdfRun{string(r), c})
	}
	return out
}

func (w *pdfWriter) font(style string, size float64) {
	w.style, w.size = style, size
	w.pdf.SetFont("DejaVu", style, size)
}

func (w *pdfWriter) use(cjk bool) {
	if cjk {
		w.pdf.SetFont("Droid", "", w.size)
	} else {
		w.pdf.SetFont("DejaVu", w.style, w.size)
	}
}

// width is the width of s in the current size.
func (w *pdfWriter) width(s string) float64 {
	var total float64
	for _, r := range pdfRuns(s) {
		w.use(r.cjk)
		total += w.pdf.GetStringWidth(r.text)
	}
	w.use(false)
	return total
}

// fit shortens s to max millimetres.
func (w *pdfWriter) fit(s string, max float64) string {
	if w.width(s) <= max {
		return s
	}
	r := []rune(s)
	for len(r) > 1 && w.width(string(r)+"…") > max {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

// cell writes s in a cell of width cw (align "L", "R" or "C"); ln as in
// fpdf: 0 right of the cell, 1 next line, 2 below.
func (w *pdfWriter) cell(cw, h float64, s, align string, fill bool, ln int) {
	p := w.pdf
	x0, y0 := p.GetXY()
	if fill {
		p.Rect(x0, y0, cw, h, "F")
	}
	const pad = 1.0
	tw := w.width(s)
	x := x0 + pad
	switch align {
	case "R":
		x = x0 + cw - pad - tw
	case "C":
		x = x0 + (cw-tw)/2
	}
	m := p.GetCellMargin()
	p.SetCellMargin(0)
	for _, r := range pdfRuns(s) {
		w.use(r.cjk)
		rw := p.GetStringWidth(r.text)
		p.SetXY(x, y0)
		p.CellFormat(rw, h, r.text, "", 0, "L", false, 0, "")
		x += rw
	}
	p.SetCellMargin(m)
	w.use(false)
	switch ln {
	case 1:
		lm, _, _, _ := p.GetMargins()
		p.SetXY(lm, y0+h)
	case 2:
		p.SetXY(x0, y0+h)
	default:
		p.SetXY(x0+cw, y0)
	}
}

// PDFFileName is the name of the PDF of the report.
func (rep *Report) PDFFileName() string {
	return strings.TrimSuffix(rep.FileName(), ".csv") + ".pdf"
}

// titleIn is the period of the report as a heading, e.g. "Last 30 days
// (2026-09-13 – 2026-10-12)".
func (rep *Report) titleIn(l *language) string {
	for _, p := range periodPresets {
		if p.Key == rep.Period.Key && p.Key != "custom" {
			return l.T(p.Name) + " (" + rep.periodText() + ")"
		}
	}
	return rep.periodText()
}

// PDF renders the report in a console language; consoleURL goes into the
// footer.
func (rep *Report) PDF(l *language, consoleURL string) ([]byte, error) {
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(15, 15, 15)
	pdf.SetAutoPageBreak(true, 18)
	for _, f := range []struct{ family, style, file string }{
		{"DejaVu", "", "fonts/DejaVuSans.ttf"}, {"DejaVu", "B", "fonts/DejaVuSans-Bold.ttf"}, {"Droid", "", "fonts/DroidSansFallbackFull.ttf"},
	} {
		b, err := pdfFonts.ReadFile(f.file)
		if err != nil {
			return nil, err
		}
		pdf.AddUTF8FontFromBytes(f.family, f.style, b)
	}
	w := &pdfWriter{pdf: pdf}
	pdf.SetTitle(l.T("Backup report")+" "+rep.periodText(), true)
	pdf.SetCreator("BackupZit", true)
	color := func(c [3]int) { pdf.SetTextColor(c[0], c[1], c[2]) }
	fill := func(c [3]int) { pdf.SetFillColor(c[0], c[1], c[2]) }
	draw := func(c [3]int) { pdf.SetDrawColor(c[0], c[1], c[2]) }
	pdf.AliasNbPages("{nb}")
	pdf.SetFooterFunc(func() {
		pdf.SetXY(15, -12)
		w.font("", 8)
		color(pdfMuted)
		left := "BackupZit"
		if consoleURL != "" {
			left += " · " + consoleURL
		}
		w.cell(120, 5, left, "L", false, 0)
		w.cell(60, 5, l.T("Page %d of %s", pdf.PageNo(), "{nb}"), "R", false, 0)
	})
	pdf.AddPage()

	// Header band.
	fill(pdfAccent)
	pdf.Rect(0, 0, 210, 24, "F")
	pdf.SetXY(15, 7)
	pdf.SetTextColor(255, 255, 255)
	w.font("B", 16)
	w.cell(90, 10, "BackupZit", "L", false, 0)
	w.font("", 12)
	w.cell(90, 10, l.T("Backup report"), "R", false, 0)
	pdf.SetXY(15, 30)
	color(pdfText)
	w.font("B", 14)
	w.cell(180, 7, w.fit(rep.titleIn(l), 179), "L", false, 1)
	w.font("", 9)
	color(pdfMuted)
	w.cell(180, 5, rep.ScopeIn(l)+" · "+l.T("created %s", rep.Generated.Format("2006-01-02 15:04")), "L", false, 1)
	pdf.Ln(4)

	// Key figures.
	rate := "—"
	if rep.Backups > 0 {
		rate = fmt.Sprintf("%.1f %%", rep.SuccessRate)
	}
	tests := "—"
	if rep.Tests > 0 {
		tests = fmt.Sprintf("%d / %d", rep.Tests-rep.TestsFailed, rep.Tests)
	}
	pick := func(cond bool, a, b [3]int) [3]int {
		if cond {
			return a
		}
		return b
	}
	kpis := []struct {
		value, label string
		c            [3]int
	}{
		{fmt.Sprint(rep.Backups), l.T("backups"), pdfText},
		{rate, l.T("success rate"), pick(rep.Failed == 0, pdfOK, pdfWarn)},
		{fmt.Sprint(rep.Failed), l.T("failed"), pick(rep.Failed > 0, pdfBad, pdfText)},
		{fmt.Sprint(rep.Warning), l.T("with warnings"), pick(rep.Warning > 0, pdfWarn, pdfText)},
		{humanBytes(rep.BytesRead), l.T("data read"), pdfText},
		{humanBytes(rep.BytesStored), l.T("uploaded"), pdfText},
		{fmt.Sprint(rep.Restores), l.T("restores"), pdfText},
		{tests, l.T("restore tests passed"), pick(rep.TestsFailed > 0, pdfBad, pdfText)},
	}
	y0 := pdf.GetY()
	for i, k := range kpis {
		x := 15 + float64(i%4)*45.5
		y := y0 + float64(i/4)*21
		draw(pdfLine)
		fill(pdfZebra)
		pdf.RoundedRect(x, y, 43, 18, 2, "1234", "FD")
		pdf.SetXY(x+2, y+2.5)
		w.font("B", 13)
		color(k.c)
		w.cell(39, 7, k.value, "L", false, 2)
		w.font("", 8)
		color(pdfMuted)
		w.cell(39, 5, w.fit(k.label, 37), "L", false, 0)
	}
	pdf.SetXY(15, y0+44)

	// Daily chart.
	if n := len(rep.Days); n > 0 && n <= 92 && rep.MaxDay > 0 {
		w.font("B", 11)
		color(pdfText)
		w.cell(180, 7, l.T("Backups per day"), "L", false, 1)
		top, h := pdf.GetY()+2, 34.0
		draw(pdfLine)
		pdf.Line(15, top+h, 195, top+h)
		bw := 180.0 / float64(n)
		step := (n + 9) / 10
		for i, d := range rep.Days {
			x := 15 + float64(i)*bw + bw*0.15
			y := top + h
			for _, seg := range []struct {
				v int
				c [3]int
			}{{d.Success, pdfOK}, {d.Warning, pdfWarn}, {d.Failed, pdfBad}} {
				if seg.v == 0 {
					continue
				}
				sh := h * float64(seg.v) / float64(rep.MaxDay)
				fill(seg.c)
				pdf.Rect(x, y-sh, bw*0.7, sh, "F")
				y -= sh
			}
			if i%step == 0 {
				w.font("", 6.5)
				color(pdfMuted)
				pdf.SetXY(15+float64(i)*bw-4, top+h+1)
				w.cell(bw+8, 4, d.Date.Format("02.01."), "C", false, 0)
			}
		}
		pdf.SetXY(15, top+h+6)
		w.font("", 8)
		for _, lg := range []struct {
			label string
			c     [3]int
		}{{l.T("successful"), pdfOK}, {l.T("with warnings"), pdfWarn}, {l.T("failed"), pdfBad}} {
			fill(lg.c)
			pdf.Rect(pdf.GetX(), pdf.GetY()+1.2, 3, 3, "F")
			pdf.SetX(pdf.GetX() + 4)
			color(pdfMuted)
			w.cell(w.width(lg.label)+6, 5, lg.label, "L", false, 0)
		}
		pdf.Ln(9)
	}

	// Jobs.
	w.font("B", 11)
	color(pdfText)
	w.cell(180, 7, l.T("Jobs"), "L", false, 1)
	cols := []struct {
		title string
		w     float64
		align string
	}{{l.T("Job"), 52, "L"}, {l.T("Agent"), 30, "L"}, {l.T("Runs"), 14, "R"}, {l.T("OK"), 12, "R"}, {l.T("Warn."), 14, "R"},
		{l.T("Failed"), 14, "R"}, {l.T("Rate"), 14, "R"}, {l.T("Last good backup"), 30, "R"}}
	header := func() {
		w.font("B", 8)
		fill(pdfLine)
		color(pdfText)
		for _, c := range cols {
			w.cell(c.w, 6, w.fit(c.title, c.w-2), c.align, true, 0)
		}
		pdf.Ln(-1)
		pdf.SetX(15)
	}
	header()
	w.font("", 8)
	for i, j := range rep.Jobs {
		if pdf.GetY() > 270 {
			pdf.AddPage()
			header()
			w.font("", 8)
		}
		fill(pdfZebra)
		zebra := i%2 == 1
		mark := pdfOK
		switch {
		case j.Missed || j.LastStatus == "failed":
			mark = pdfBad
		case j.Warning > 0:
			mark = pdfWarn
		}
		y := pdf.GetY()
		last := l.T("never")
		if j.LastSuccess != nil {
			last = j.LastSuccess.Format("2006-01-02 15:04")
		}
		jr := "—"
		if j.Runs > 0 {
			jr = fmt.Sprintf("%.0f %%", j.SuccessRate)
		}
		cells := []string{"    " + j.Name, j.Hostname, fmt.Sprint(j.Runs), fmt.Sprint(j.Success), fmt.Sprint(j.Warning), fmt.Sprint(j.Failed), jr, last}
		for k, c := range cols {
			color(pdfText)
			if k == 5 && j.Failed > 0 {
				color(pdfBad)
			}
			w.cell(c.w, 6, w.fit(cells[k], c.w-2), c.align, zebra, 0)
		}
		pdf.Ln(-1)
		pdf.SetX(15)
		fill(mark)
		pdf.Circle(17.6, y+3, 1.1, "F")
	}
	if len(rep.Jobs) == 0 {
		color(pdfMuted)
		w.cell(180, 6, l.T("No backups in this period."), "L", false, 1)
	}
	pdf.Ln(5)

	// Problems.
	if len(rep.Problems) > 0 {
		if pdf.GetY() > 240 {
			pdf.AddPage()
		}
		w.font("B", 11)
		color(pdfText)
		w.cell(180, 7, l.T("Problems"), "L", false, 1)
		w.font("", 8)
		const maxProblems = 40
		for i, r := range rep.Problems {
			if i == maxProblems {
				color(pdfMuted)
				w.cell(180, 5, l.T("... and %d more (see the attached CSV)", len(rep.Problems)-maxProblems), "L", false, 1)
				break
			}
			when := r.QueuedAt
			if r.FinishedAt != nil {
				when = *r.FinishedAt
			}
			st, msg := l.T(r.Status), r.Message
			if r.Anomaly != "" {
				st, msg = l.T("suspicious"), r.Anomaly
			}
			who := r.Hostname
			if r.JobName != nil {
				who = *r.JobName + " · " + r.Hostname
			}
			color(pdfMuted)
			w.cell(27, 5, when.Format("2006-01-02 15:04"), "L", false, 0)
			color(pdfText)
			w.cell(50, 5, w.fit(who, 48), "L", false, 0)
			color(pick(r.Status == "failed" || r.Anomaly != "", pdfBad, pdfWarn))
			w.cell(22, 5, w.fit(st, 20), "L", false, 0)
			color(pdfMuted)
			w.cell(81, 5, w.fit(msg, 79), "L", false, 1)
		}
	}
	var b bytes.Buffer
	if err := pdf.Output(&b); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// reportAttachments are the files attached to a report email: the PDF and
// the CSV of every run.
func reportAttachments(rep *Report, l *language, consoleURL string) []attachment {
	atts := []attachment{}
	if b, err := rep.PDF(l, consoleURL); err == nil {
		atts = append(atts, attachment{Name: rep.PDFFileName(), ContentType: "application/pdf", Data: b})
	}
	return append(atts, attachment{Name: rep.FileName(), ContentType: "text/csv; charset=utf-8", Data: rep.CSV()})
}

// handleReportPDF serves GET /reports/pdf.
func (s *Server) handleReportPDF(w http.ResponseWriter, r *http.Request, _ string) {
	rep, err := s.reportFromRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	b, err := rep.PDF(requestLanguage(r), s.consoleURL())
	if err != nil {
		s.serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `attachment; filename="`+rep.PDFFileName()+`"`)
	w.Write(b)
}
