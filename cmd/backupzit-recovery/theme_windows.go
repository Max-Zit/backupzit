//go:build windows

package main

// The look of the recovery GUI: colors, fonts, the window frame (header,
// body, footer) and the controls that are drawn here instead of taken from
// Windows. Windows PE has no theme service, so native buttons look like
// Windows 2000 there; buttons, tiles, the wizard's step list and the status
// chips are drawn on a CustomWidget. Edit fields and lists stay native.

import (
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/lxn/walk"
	dcl "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
)

var (
	colBg        = walk.RGB(0xF3, 0xF4, 0xF6)
	colCard      = walk.RGB(0xFF, 0xFF, 0xFF)
	colHeader    = walk.RGB(0x1F, 0x29, 0x37)
	colHeaderSub = walk.RGB(0xCB, 0xD5, 0xE1)
	colAccent    = walk.RGB(0x25, 0x63, 0xEB)
	colAccentHi  = walk.RGB(0x1D, 0x4E, 0xD8)
	colAccentBg  = walk.RGB(0xDB, 0xEA, 0xFE)
	colText      = walk.RGB(0x11, 0x18, 0x27)
	colMuted     = walk.RGB(0x6B, 0x72, 0x80)
	colBorder    = walk.RGB(0xE5, 0xE7, 0xEB)
	colBorderHi  = walk.RGB(0x9C, 0xA3, 0xAF)
	colHover     = walk.RGB(0xF9, 0xFA, 0xFB)
	colDisabled  = walk.RGB(0xD1, 0xD5, 0xDB)
	colDanger    = walk.RGB(0xB9, 0x1C, 0x1C)
	colDangerHi  = walk.RGB(0x99, 0x1B, 0x1B)
	colWarn      = walk.RGB(0xB4, 0x53, 0x09)
	colOK        = walk.RGB(0x15, 0x80, 0x3D)
	colWhite     = walk.RGB(0xFF, 0xFF, 0xFF)
)

const uiFamily = "Segoe UI"

// baseFont is the font of every window; Windows PE has Segoe UI, and Windows
// substitutes another font where it is missing.
var baseFont = dcl.Font{Family: uiFamily, PointSize: 10}

// solid is a brush of one color.
func solid(c walk.Color) dcl.Brush { return dcl.SolidColorBrush{Color: c} }

var fontCache = map[[2]int]*walk.Font{}

// uiFont returns Segoe UI in the size; the fonts live as long as the program.
func uiFont(size int, bold bool) *walk.Font {
	k := [2]int{size, 0}
	style := walk.FontStyle(0)
	if bold {
		k[1], style = 1, walk.FontBold
	}
	if f := fontCache[k]; f != nil {
		return f
	}
	f, err := walk.NewFont(uiFamily, size, style)
	if err != nil {
		return nil
	}
	fontCache[k] = f
	return f
}

// --- Frame -------------------------------------------------------------------

// header is the dark bar at the top of a window.
func header(title, sub string, assignTitle, assignSub **walk.Label) dcl.Widget {
	return dcl.Composite{
		Background: solid(colHeader),
		Layout:     dcl.HBox{Margins: dcl.Margins{Left: 24, Top: 16, Right: 24, Bottom: 16}},
		Children: []dcl.Widget{
			dcl.Composite{Layout: dcl.VBox{MarginsZero: true, Spacing: 2}, Children: []dcl.Widget{
				dcl.Label{AssignTo: assignTitle, Text: title, Font: dcl.Font{Family: uiFamily, PointSize: 16, Bold: true}, TextColor: colWhite},
				dcl.Label{AssignTo: assignSub, Text: sub, Visible: sub != "" || assignSub != nil, TextColor: colHeaderSub},
			}},
			dcl.HSpacer{},
		},
	}
}

// fill is the stretch factor of a window's content area. The drawn buttons
// are greedy layout items, so the footer competes for spare height; this
// leaves it a negligible share.
const stretchFill = 1000

// footer is the white bar with the buttons at the bottom of a window.
func footer(children ...dcl.Widget) dcl.Widget {
	return dcl.Composite{
		Background: solid(colCard),
		Layout:     dcl.VBox{MarginsZero: true, SpacingZero: true},
		Children: []dcl.Widget{
			hline(),
			dcl.Composite{Layout: dcl.HBox{Margins: dcl.Margins{Left: 16, Top: 12, Right: 16, Bottom: 12}, Spacing: 8}, Children: children},
		},
	}
}

// hline is a one pixel line across, in the border color.
func hline() dcl.Widget {
	return dcl.Composite{Background: solid(colBorder), MinSize: dcl.Size{Height: 1}, MaxSize: dcl.Size{Height: 1}, Layout: dcl.HBox{MarginsZero: true}}
}

// vline is a one pixel line from top to bottom, in the border color.
func vline() dcl.Widget {
	return dcl.Composite{Background: solid(colBorder), MinSize: dcl.Size{Width: 1}, MaxSize: dcl.Size{Width: 1}, Layout: dcl.VBox{MarginsZero: true}}
}

// card is a white area with padding.
func card(children ...dcl.Widget) dcl.Widget {
	return dcl.Composite{Background: solid(colCard), Layout: dcl.VBox{Margins: dcl.Margins{Left: 20, Top: 18, Right: 20, Bottom: 18}, Spacing: 8}, Children: children}
}

// body puts a card on the gray window background.
func body(children ...dcl.Widget) dcl.Widget {
	return dcl.Composite{StretchFactor: stretchFill, Layout: dcl.VBox{Margins: dcl.Margins{Left: 20, Top: 20, Right: 20, Bottom: 20}}, Children: []dcl.Widget{card(children...)}}
}

// row lays widgets out left to right, left aligned.
func row(children ...dcl.Widget) dcl.Widget {
	return dcl.Composite{Layout: dcl.HBox{MarginsZero: true, Spacing: 8}, Children: append(children, dcl.HSpacer{})}
}

// section is the heading of a part of a dialog.
func section(text string) dcl.Widget {
	return dcl.Label{Text: text, Font: dcl.Font{Family: uiFamily, PointSize: 11, Bold: true}, TextColor: colText}
}

// muted is a grey note.
func muted(text string) dcl.Widget {
	return dcl.Label{Text: text, TextColor: colMuted}
}

// dialogFrame is the layout of a dialog: header, a card with the content
// and a footer with the buttons.
func dialogFrame(title, sub string, content, buttons []dcl.Widget) []dcl.Widget {
	return []dcl.Widget{header(title, sub, nil, nil), body(content...), footer(buttons...)}
}

// --- Drawing helpers ---------------------------------------------------------

// px converts 96-dpi pixels to the pixels of the window.
func px(w walk.Window, v int) int { return v * w.DPI() / 96 }

// fill paints a rectangle, rounded when radius > 0.
func fill(c *walk.Canvas, col walk.Color, r walk.Rectangle, radius int) {
	b, err := walk.NewSolidColorBrush(col)
	if err != nil {
		return
	}
	defer b.Dispose()
	if radius <= 0 {
		_ = c.FillRectanglePixels(b, r)
		return
	}
	_ = c.FillRoundedRectanglePixels(b, r, walk.Size{Width: radius, Height: radius})
}

// fillEllipse paints a filled circle or ellipse.
func fillEllipse(c *walk.Canvas, col walk.Color, r walk.Rectangle) {
	if b, err := walk.NewSolidColorBrush(col); err == nil {
		_ = c.FillEllipsePixels(b, r)
		b.Dispose()
	}
}

// inset shrinks r by d on every side.
func inset(r walk.Rectangle, d int) walk.Rectangle {
	return walk.Rectangle{X: r.X + d, Y: r.Y + d, Width: r.Width - 2*d, Height: r.Height - 2*d}
}

// bgOf is the color behind w: the first solid background of w or a parent.
func bgOf(w walk.Window) walk.Color {
	for w != nil {
		if b, ok := w.Background().(*walk.SolidColorBrush); ok {
			return b.Color()
		}
		wd, ok := w.(walk.Widget)
		if !ok {
			break
		}
		p := wd.Parent()
		if p == nil {
			break
		}
		w = p
	}
	return colBg
}

// textWidth is the width of a single line of text in Segoe UI at 96 dpi.
// (walk's MeasureText returns the whole given width for a single line.)
func textWidth(text string, size int, bold bool) int {
	lf := win.LOGFONT{LfHeight: -int32(size * 96 / 72), LfWeight: win.FW_NORMAL} // #nosec G115 -- a font size in points
	if bold {
		lf.LfWeight = win.FW_BOLD
	}
	copy(lf.LfFaceName[:], syscall.StringToUTF16(uiFamily))
	font := win.CreateFontIndirect(&lf)
	if font == 0 {
		return len(text) * size
	}
	defer win.DeleteObject(win.HGDIOBJ(font))
	dc := win.CreateCompatibleDC(0)
	defer win.DeleteDC(dc)
	old := win.SelectObject(dc, win.HGDIOBJ(font))
	defer win.SelectObject(dc, old)
	u := syscall.StringToUTF16(text)
	var sz win.SIZE
	if len(u) <= 1 || !win.GetTextExtentPoint32(dc, &u[0], int32(len(u)-1), &sz) { // #nosec G115 -- a label, far below 2^31 characters
		return 0
	}
	return int(sz.CX)
}

// --- Hover -------------------------------------------------------------------

// Walk reports mouse moves but not when the mouse leaves a widget, and a
// dialog takes Enter for its default button. trackHover subclasses the
// widget's window for both, and paints the flat buttons (ownPaint).
var (
	hoverOld  = map[win.HWND]uintptr{}
	hoverFn   = map[win.HWND]func(bool){}
	hoverIn   = map[win.HWND]bool{}
	hoverProc = syscall.NewCallback(hoverWndProc)

	procRtlMoveMemory = syscall.NewLazyDLL("kernel32.dll").NewProc("RtlMoveMemory")
)

// trackHover subclasses the window of w and calls changed when the
// mouse comes in or goes out.
func trackHover(w walk.Window, changed func(hover bool)) {
	h := w.Handle()
	hoverFn[h] = changed
	hoverOld[h] = win.SetWindowLongPtr(h, win.GWLP_WNDPROC, hoverProc)
}

// hoverWndProc is the window procedure of the subclassed widgets.
func hoverWndProc(hwnd win.HWND, msg uint32, wp, lp uintptr) uintptr {
	old := hoverOld[hwnd]
	if paint := ownPaint[hwnd]; paint != nil {
		switch msg {
		case win.WM_PAINT:
			paintOwn(hwnd, paint)
			return 0
		case win.WM_ERASEBKGND:
			return 1
		case win.WM_LBUTTONDOWN, win.WM_LBUTTONUP, win.WM_LBUTTONDBLCLK, win.WM_KEYDOWN, win.WM_KEYUP,
			win.WM_SETFOCUS, win.WM_KILLFOCUS, win.WM_ENABLE, win.WM_SETTEXT, win.WM_CAPTURECHANGED,
			win.BM_SETSTATE, win.BM_SETSTYLE, win.BM_SETCHECK, win.WM_UPDATEUISTATE:
			// A native button draws some states at once, not through
			// WM_PAINT; paint over it right after.
			r := win.CallWindowProc(old, hwnd, msg, wp, lp)
			win.InvalidateRect(hwnd, nil, false)
			win.UpdateWindow(hwnd)
			return r
		}
	}
	switch msg {
	case win.WM_MOUSEMOVE:
		if !hoverIn[hwnd] {
			hoverIn[hwnd] = true
			tme := win.TRACKMOUSEEVENT{DwFlags: win.TME_LEAVE, HwndTrack: hwnd}
			tme.CbSize = uint32(unsafe.Sizeof(tme))
			win.TrackMouseEvent(&tme)
			hoverFn[hwnd](true)
		}
	case win.WM_MOUSELEAVE:
		if hoverIn[hwnd] {
			hoverIn[hwnd] = false
			hoverFn[hwnd](false)
		}
	case win.WM_GETDLGCODE:
		if lp != 0 && ownPaint[hwnd] == nil {
			// lp points to the message; copied rather than converted,
			// which go vet rejects for a uintptr from Windows.
			var m win.MSG
			_, _, _ = procRtlMoveMemory.Call(uintptr(unsafe.Pointer(&m)), lp, unsafe.Sizeof(m)) // #nosec G103 -- pointer passed to a Windows API call
			if m.Message == win.WM_KEYDOWN && m.WParam == win.VK_RETURN {
				return win.DLGC_WANTMESSAGE
			}
		}
	case win.WM_NCDESTROY:
		r := win.CallWindowProc(old, hwnd, msg, wp, lp)
		delete(hoverOld, hwnd)
		delete(hoverFn, hwnd)
		delete(hoverIn, hwnd)
		delete(ownPaint, hwnd)
		return r
	}
	return win.CallWindowProc(old, hwnd, msg, wp, lp)
}

// clickable gives a drawn widget a hand cursor, hover, a focus ring and
// click by mouse, Enter or Space.
type clickable struct {
	cw      *walk.CustomWidget
	hover   bool
	down    bool
	onClick func()
}

// attach sets up the cursor, focus repaint and hover of the widget.
func (k *clickable) attach() {
	k.cw.SetCursor(walk.CursorHand())
	k.cw.FocusedChanged().Attach(func() { _ = k.cw.Invalidate() })
	trackHover(k.cw, func(h bool) {
		k.hover = h
		if !h {
			k.down = false
		}
		_ = k.cw.Invalidate()
	})
}

// mouseDown presses the widget and takes the focus.
func (k *clickable) mouseDown(_, _ int, b walk.MouseButton) {
	if b == walk.LeftButton {
		k.down = true
		_ = k.cw.SetFocus()
		_ = k.cw.Invalidate()
	}
}

// mouseUp clicks when the mouse is released over the pressed widget.
func (k *clickable) mouseUp(_, _ int, b walk.MouseButton) {
	if b != walk.LeftButton || !k.down {
		return
	}
	k.down = false
	_ = k.cw.Invalidate()
	if k.hover {
		k.click()
	}
}

// keyDown clicks on Enter and Space.
func (k *clickable) keyDown(key walk.Key) {
	if key == walk.KeyReturn || key == walk.KeySpace {
		k.click()
	}
}

// click runs the action unless the widget is disabled.
func (k *clickable) click() {
	if k.onClick != nil && k.cw.Enabled() {
		k.onClick()
	}
}

// declare is the CustomWidget of a clickable: a tab stop, buffered
// painting and the mouse and key handlers.
func (k *clickable) declare(paint walk.PaintFunc, minSize, maxSize dcl.Size) dcl.CustomWidget {
	return dcl.CustomWidget{
		AssignTo:            &k.cw,
		Style:               win.WS_TABSTOP,
		PaintPixels:         paint,
		PaintMode:           dcl.PaintBuffered,
		InvalidatesOnResize: true,
		MinSize:             minSize,
		MaxSize:             maxSize,
		OnMouseDown:         k.mouseDown,
		OnMouseUp:           k.mouseUp,
		OnKeyDown:           k.keyDown,
	}
}

// --- Buttons -----------------------------------------------------------------

type btnKind int

const (
	btnSecondary btnKind = iota
	btnPrimary
	btnDanger
)

// FlatButton is a native push button that paints itself flat: primary
// (blue), danger (red) or secondary (white with a border). Being a real
// button it keeps focus, Tab, Enter and Space, and can be a dialog's
// DefaultButton or CancelButton (Button).
type FlatButton struct {
	AssignTo  **flatButton
	Button    **walk.PushButton
	Text      string
	Kind      btnKind
	OnClicked func()
}

type flatButton struct {
	pb    *walk.PushButton
	kind  btnKind
	hover bool
}

// Create creates the button (walk declarative Widget).
func (fb FlatButton) Create(b *dcl.Builder) error {
	f := &flatButton{kind: fb.Kind}
	sz := buttonSize(fb.Text)
	if err := (dcl.PushButton{AssignTo: &f.pb, Text: fb.Text, OnClicked: fb.OnClicked, MinSize: sz, MaxSize: sz}).Create(b); err != nil {
		return err
	}
	f.pb.SetCursor(walk.CursorHand())
	h := f.pb.Handle()
	ownPaint[h] = f.paint
	trackHover(f.pb, func(v bool) {
		f.hover = v
		_ = f.pb.Invalidate()
	})
	if fb.AssignTo != nil {
		*fb.AssignTo = f
	}
	if fb.Button != nil {
		*fb.Button = f.pb
	}
	return nil
}

// buttonSize fits a button to its text, at least 96 pixels wide.
func buttonSize(text string) dcl.Size {
	return dcl.Size{Width: max(textWidth(text, 10, true)+36, 96), Height: 38}
}

// SetText changes the text and fits the button to it.
func (f *flatButton) SetText(s string) {
	_ = f.pb.SetText(s)
	sz := walk.Size(buttonSize(s))
	_ = f.pb.SetMinMaxSize(sz, sz)
	f.pb.RequestLayout()
	_ = f.pb.Invalidate()
}

// SetKind changes the color (primary, danger, secondary).
func (f *flatButton) SetKind(k btnKind) {
	f.kind = k
	_ = f.pb.Invalidate()
}

// SetEnabled enables or greys out the button.
func (f *flatButton) SetEnabled(v bool) { f.pb.SetEnabled(v) }

// SetVisible shows or hides the button.
func (f *flatButton) SetVisible(v bool) { f.pb.SetVisible(v) }

// paint draws the button in its state: focus ring, hover, pressed,
// disabled.
func (f *flatButton) paint(g *gdi) {
	h := f.pb.Handle()
	r := g.bounds
	g.fill(bgOf(f.pb), r, 0)
	radius := g.px(10)
	if win.GetFocus() == h {
		g.fill(colAccentBg, r, radius+g.px(2))
	}
	r = inset(r, g.px(2))
	enabled := win.IsWindowEnabled(h)
	down := win.SendMessage(h, win.BM_GETSTATE, 0, 0)&win.BST_PUSHED != 0
	var face, text, border walk.Color
	switch f.kind {
	case btnPrimary, btnDanger:
		face, text = colAccent, colWhite
		if f.hover {
			face = colAccentHi
		}
		if f.kind == btnDanger {
			face = colDanger
			if f.hover {
				face = colDangerHi
			}
		}
		if !enabled {
			face = colDisabled
		}
		border = face
	default:
		face, text, border = colCard, colText, colBorderHi
		if f.hover {
			face = colHover
		}
		if !enabled {
			text, border = colDisabled, colBorder
		}
	}
	if down {
		r = inset(r, g.px(1))
	}
	g.fill(border, r, radius)
	g.fill(face, inset(r, g.px(1)), radius)
	g.text(f.pb.Text(), 10, true, text, r, win.DT_CENTER|win.DT_VCENTER|win.DT_SINGLELINE)
}

// gdi paints with plain GDI: walk offers its Canvas only to its own widgets.
type gdi struct {
	hdc    win.HDC
	bounds walk.Rectangle
	dpi    int
}

// px converts 96-dpi pixels to device pixels.
func (g *gdi) px(v int) int { return v * g.dpi / 96 }

// fill paints a rectangle without a border, rounded when radius > 0.
func (g *gdi) fill(col walk.Color, r walk.Rectangle, radius int) {
	br := win.CreateBrushIndirect(&win.LOGBRUSH{LbStyle: win.BS_SOLID, LbColor: win.COLORREF(col)})
	if br == 0 {
		return
	}
	defer win.DeleteObject(win.HGDIOBJ(br))
	oldBrush := win.SelectObject(g.hdc, win.HGDIOBJ(br))
	oldPen := win.SelectObject(g.hdc, win.GetStockObject(win.NULL_PEN))
	// With no pen the shape is one pixel smaller on the right and bottom.
	win.RoundRect(g.hdc, int32(r.X), int32(r.Y), int32(r.X+r.Width+1), int32(r.Y+r.Height+1), int32(radius), int32(radius)) // #nosec G115 -- pixels of a window
	win.SelectObject(g.hdc, oldPen)
	win.SelectObject(g.hdc, oldBrush)
}

// text draws s in Segoe UI with the DrawText format.
func (g *gdi) text(s string, size int, bold bool, col walk.Color, r walk.Rectangle, format uint32) {
	font := hfont(size, bold, g.dpi)
	old := win.SelectObject(g.hdc, win.HGDIOBJ(font))
	defer win.SelectObject(g.hdc, old)
	win.SetBkMode(g.hdc, win.TRANSPARENT)
	win.SetTextColor(g.hdc, win.COLORREF(col))
	rc := win.RECT{Left: int32(r.X), Top: int32(r.Y), Right: int32(r.X + r.Width), Bottom: int32(r.Y + r.Height)} // #nosec G115 -- pixels of a window
	u := syscall.StringToUTF16(s)
	win.DrawTextEx(g.hdc, &u[0], int32(len(u)-1), &rc, format, nil) // #nosec G115 -- a label, far below 2^31 characters
}

var hfontCache = map[[3]int]win.HFONT{}

// hfont is Segoe UI for plain GDI; the fonts live as long as the program.
func hfont(size int, bold bool, dpi int) win.HFONT {
	k := [3]int{size, 0, dpi}
	lf := win.LOGFONT{LfHeight: -int32(size * dpi / 72), LfWeight: win.FW_NORMAL, LfQuality: win.CLEARTYPE_QUALITY} // #nosec G115 -- a font size in points
	if bold {
		k[1], lf.LfWeight = 1, win.FW_BOLD
	}
	if f, ok := hfontCache[k]; ok {
		return f
	}
	copy(lf.LfFaceName[:], syscall.StringToUTF16(uiFamily))
	f := win.CreateFontIndirect(&lf)
	hfontCache[k] = f
	return f
}

// ownPaint holds the paint functions of subclassed native controls.
var ownPaint = map[win.HWND]func(*gdi){}

// paintOwn paints a subclassed control double buffered.
func paintOwn(hwnd win.HWND, paint func(*gdi)) {
	var ps win.PAINTSTRUCT
	hdc := win.BeginPaint(hwnd, &ps)
	if hdc == 0 {
		return
	}
	defer win.EndPaint(hwnd, &ps)
	var rc win.RECT
	win.GetClientRect(hwnd, &rc)
	w, h := rc.Right-rc.Left, rc.Bottom-rc.Top
	if w <= 0 || h <= 0 {
		return
	}
	mem := win.CreateCompatibleDC(hdc)
	defer win.DeleteDC(mem)
	bmp := win.CreateCompatibleBitmap(hdc, w, h)
	defer win.DeleteObject(win.HGDIOBJ(bmp))
	old := win.SelectObject(mem, win.HGDIOBJ(bmp))
	defer win.SelectObject(mem, old)
	dpi := int(win.GetDpiForWindow(hwnd))
	if dpi == 0 {
		dpi = 96
	}
	paint(&gdi{hdc: mem, bounds: walk.Rectangle{Width: int(w), Height: int(h)}, dpi: dpi})
	win.BitBlt(hdc, 0, 0, w, h, mem, 0, 0, win.SRCCOPY)
}

// --- Tiles (main window) -----------------------------------------------------

type iconKind int

const (
	iconDisk iconKind = iota
	iconPower
	iconChip
	iconScreen
)

// Tile is a large clickable card with an icon, a title and a description.
type Tile struct {
	Title, Text string
	Icon        iconKind
	OnClicked   func()
}

type tile struct {
	clickable
	title, text string
	icon        iconKind
}

// Create creates the tile (walk declarative Widget).
func (td Tile) Create(b *dcl.Builder) error {
	tl := &tile{title: td.Title, text: td.Text, icon: td.Icon}
	tl.onClick = td.OnClicked
	if err := tl.declare(tl.paint, dcl.Size{Width: 300, Height: 112}, dcl.Size{Height: 150}).Create(b); err != nil {
		return err
	}
	tl.attach()
	return nil
}

// paint draws the card: border (accent on hover and focus), icon,
// title and description.
func (tl *tile) paint(c *walk.Canvas, _ walk.Rectangle) error {
	w := tl.cw
	r := w.ClientBoundsPixels()
	fill(c, bgOf(w), r, 0)
	radius := px(w, 14)
	border, face := colBorder, colCard
	if tl.hover || w.Focused() {
		border = colAccent
	}
	fill(c, border, r, radius)
	bw := px(w, 1)
	if tl.hover || w.Focused() {
		bw = px(w, 2)
	}
	fill(c, face, inset(r, bw), radius)

	pad := px(w, 20)
	ic := walk.Rectangle{X: r.X + pad, Y: r.Y + pad, Width: px(w, 48), Height: px(w, 48)}
	fillEllipse(c, colAccentBg, ic)
	drawIcon(c, w, tl.icon, ic)

	tx := ic.X + ic.Width + px(w, 16)
	tw := r.X + r.Width - tx - pad
	_ = c.DrawTextPixels(tl.title, uiFont(13, true), colText, walk.Rectangle{X: tx, Y: r.Y + pad - px(w, 2), Width: tw, Height: px(w, 28)}, walk.TextLeft|walk.TextTop|walk.TextSingleLine|walk.TextEndEllipsis)
	_ = c.DrawTextPixels(tl.text, uiFont(10, false), colMuted, walk.Rectangle{X: tx, Y: r.Y + pad + px(w, 30), Width: tw, Height: r.Height - pad*2 - px(w, 26)}, walk.TextLeft|walk.TextTop|walk.TextWordbreak|walk.TextEndEllipsis)
	return nil
}

// drawIcon draws a simple line icon into the circle r.
func drawIcon(c *walk.Canvas, w walk.Window, k iconKind, r walk.Rectangle) {
	brush, err := walk.NewSolidColorBrush(colAccent)
	if err != nil {
		return
	}
	defer brush.Dispose()
	pen, err := walk.NewGeometricPen(walk.PenSolid|walk.PenCapRound|walk.PenJoinRound, px(w, 2), brush)
	if err != nil {
		return
	}
	defer pen.Dispose()
	s := r.Width
	at := func(fx, fy float64) walk.Point {
		return walk.Point{X: r.X + int(fx*float64(s)), Y: r.Y + int(fy*float64(s))}
	}
	box := func(x0, y0, x1, y1 float64) walk.Rectangle {
		a, b := at(x0, y0), at(x1, y1)
		return walk.Rectangle{X: a.X, Y: a.Y, Width: b.X - a.X, Height: b.Y - a.Y}
	}
	line := func(x0, y0, x1, y1 float64) { _ = c.DrawLinePixels(pen, at(x0, y0), at(x1, y1)) }
	round := walk.Size{Width: px(w, 6), Height: px(w, 6)}
	switch k {
	case iconDisk: // a drive with an arrow into it
		_ = c.DrawRoundedRectanglePixels(pen, box(0.24, 0.48, 0.76, 0.72), round)
		fillEllipse(c, colAccent, box(0.63, 0.57, 0.69, 0.63))
		line(0.5, 0.24, 0.5, 0.40)
		line(0.42, 0.33, 0.5, 0.41)
		line(0.58, 0.33, 0.5, 0.41)
	case iconPower:
		_ = c.DrawEllipsePixels(pen, box(0.28, 0.30, 0.72, 0.74))
		fill(c, colAccentBg, box(0.42, 0.24, 0.58, 0.40), 0)
		line(0.5, 0.24, 0.5, 0.50)
	case iconChip:
		_ = c.DrawRoundedRectanglePixels(pen, box(0.33, 0.33, 0.67, 0.67), round)
		for _, f := range []float64{0.43, 0.57} {
			line(f, 0.24, f, 0.33)
			line(f, 0.67, f, 0.76)
			line(0.24, f, 0.33, f)
			line(0.67, f, 0.76, f)
		}
	case iconScreen:
		_ = c.DrawRoundedRectanglePixels(pen, box(0.24, 0.28, 0.76, 0.62), round)
		line(0.5, 0.62, 0.5, 0.72)
		line(0.38, 0.73, 0.62, 0.73)
	}
}

// --- Status chips (main window) ----------------------------------------------

type chip struct {
	text string
	dot  walk.Color
}

// StatusChips shows short facts as pills with a colored dot.
type StatusChips struct {
	AssignTo **statusChips
}

type statusChips struct {
	cw    *walk.CustomWidget
	chips []chip
}

// Create creates the status line (walk declarative Widget).
func (sd StatusChips) Create(b *dcl.Builder) error {
	s := &statusChips{}
	if err := (dcl.CustomWidget{
		AssignTo:            &s.cw,
		PaintPixels:         s.paint,
		PaintMode:           dcl.PaintBuffered,
		InvalidatesOnResize: true,
		MinSize:             dcl.Size{Height: 32},
		MaxSize:             dcl.Size{Height: 32},
	}).Create(b); err != nil {
		return err
	}
	if sd.AssignTo != nil {
		*sd.AssignTo = s
	}
	return nil
}

// Set shows the chips.
func (s *statusChips) Set(chips []chip) {
	s.chips = chips
	_ = s.cw.Invalidate()
}

// Text is the chips as one line (for tests).
func (s *statusChips) Text() string {
	var parts []string
	for _, c := range s.chips {
		parts = append(parts, c.text)
	}
	return strings.Join(parts, " · ")
}

// paint draws the chips side by side as pills with a colored dot.
func (s *statusChips) paint(c *walk.Canvas, _ walk.Rectangle) error {
	w := s.cw
	r := w.ClientBoundsPixels()
	fill(c, bgOf(w), r, 0)
	font := uiFont(9, false)
	x := r.X
	h := px(w, 28)
	y := r.Y + (r.Height-h)/2
	for _, ch := range s.chips {
		tw := px(w, textWidth(ch.text, 9, false))
		pw := tw + px(w, 36)
		if x+pw > r.X+r.Width {
			break
		}
		pill := walk.Rectangle{X: x, Y: y, Width: pw, Height: h}
		fill(c, colBorder, pill, h)
		fill(c, colCard, inset(pill, px(w, 1)), h)
		d := px(w, 8)
		fillEllipse(c, ch.dot, walk.Rectangle{X: x + px(w, 12), Y: y + (h-d)/2, Width: d, Height: d})
		_ = c.DrawTextPixels(ch.text, font, colText, walk.Rectangle{X: x + px(w, 26), Y: y, Width: tw + px(w, 4), Height: h}, walk.TextLeft|walk.TextVCenter|walk.TextSingleLine)
		x += pw + px(w, 8)
	}
	return nil
}

// --- Step list (wizard) ------------------------------------------------------

// StepRail lists the wizard's steps: done ones with a check mark, the
// current one in blue.
type StepRail struct {
	AssignTo **stepRail
	Steps    []string
}

type stepRail struct {
	cw      *walk.CustomWidget
	steps   []string
	current int
}

// Create creates the step rail (walk declarative Widget).
func (sd StepRail) Create(b *dcl.Builder) error {
	s := &stepRail{steps: sd.Steps}
	if err := (dcl.CustomWidget{
		AssignTo:            &s.cw,
		Background:          solid(colCard),
		PaintPixels:         s.paint,
		PaintMode:           dcl.PaintBuffered,
		InvalidatesOnResize: true,
		MinSize:             dcl.Size{Width: 230},
	}).Create(b); err != nil {
		return err
	}
	if sd.AssignTo != nil {
		*sd.AssignTo = s
	}
	return nil
}

// SetCurrent marks step i as the current one.
func (s *stepRail) SetCurrent(i int) {
	s.current = i
	_ = s.cw.Invalidate()
}

// paint draws the steps: done with a check, current in the accent
// color, the rest grey.
func (s *stepRail) paint(c *walk.Canvas, _ walk.Rectangle) error {
	w := s.cw
	r := w.ClientBoundsPixels()
	fill(c, colCard, r, 0)
	d := px(w, 28)
	step := px(w, 56)
	x := r.X + px(w, 20)
	y0 := r.Y + px(w, 24)
	white, err := walk.NewSolidColorBrush(colWhite)
	if err != nil {
		return err
	}
	defer white.Dispose()
	check, err := walk.NewGeometricPen(walk.PenSolid|walk.PenCapRound|walk.PenJoinRound, px(w, 2), white)
	if err != nil {
		return err
	}
	defer check.Dispose()
	for i, name := range s.steps {
		y := y0 + i*step
		if i < len(s.steps)-1 {
			lc := colBorder
			if i < s.current {
				lc = colAccent
			}
			fill(c, lc, walk.Rectangle{X: x + d/2 - px(w, 1), Y: y + d + px(w, 4), Width: px(w, 2), Height: step - d - px(w, 8)}, 0)
		}
		circle := walk.Rectangle{X: x, Y: y, Width: d, Height: d}
		num := walk.Rectangle{X: x, Y: y, Width: d, Height: d}
		textCol, bold := colMuted, false
		switch {
		case i < s.current:
			fillEllipse(c, colAccent, circle)
			p := func(fx, fy float64) walk.Point {
				return walk.Point{X: x + int(fx*float64(d)), Y: y + int(fy*float64(d))}
			}
			_ = c.DrawPolylinePixels(check, []walk.Point{p(0.3, 0.52), p(0.45, 0.66), p(0.71, 0.38)})
			textCol = colText
		case i == s.current:
			fillEllipse(c, colAccent, circle)
			_ = c.DrawTextPixels(strconv.Itoa(i+1), uiFont(10, true), colWhite, num, walk.TextCenter|walk.TextVCenter|walk.TextSingleLine)
			textCol, bold = colAccent, true
		default:
			fillEllipse(c, colBorder, circle)
			fillEllipse(c, colCard, inset(circle, px(w, 2)))
			_ = c.DrawTextPixels(strconv.Itoa(i+1), uiFont(10, true), colMuted, num, walk.TextCenter|walk.TextVCenter|walk.TextSingleLine)
		}
		tx := x + d + px(w, 12)
		_ = c.DrawTextPixels(name, uiFont(10, bold), textCol, walk.Rectangle{X: tx, Y: y - px(w, 6), Width: r.X + r.Width - tx - px(w, 8), Height: d + px(w, 12)}, walk.TextLeft|walk.TextVCenter|walk.TextWordbreak|walk.TextEndEllipsis)
	}
	return nil
}
