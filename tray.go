package main

// tray.go is the system tray icon: a StatusNotifierItem spoken directly over
// D-Bus (D7, D10), with its menu over com.canonical.dbusmenu. There is no
// XEmbed fallback and no desktop-specific code; GNOME (AppIndicator) and
// Hyprland+DankMaterialShell are merely test targets of the same protocol.
//
// While TeamSpeak is unreachable the item is not merely hidden but gone: the
// objects are unexported and the bus name released, so every watcher drops us
// (D4). When TS6 returns we export and register again.
//
// Icon artwork is generated in Go at init (see trayPixmapsFor): we ship ARGB32
// pixmaps at 22/32/48 px and leave IconName empty, because a host that sees a
// name prefers it and draws the theme's icon instead of ours (see export()).
//
// Everything unexported in this file is either a method or prefixed "tray",
// because ts.go, ipc.go and install.go share this package.

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"
)

const (
	trayItemPath    = dbus.ObjectPath("/StatusNotifierItem")
	trayMenuPath    = dbus.ObjectPath("/MenuBar")
	trayItemIface   = "org.kde.StatusNotifierItem"
	trayMenuIface   = "com.canonical.dbusmenu"
	trayWatcherName = "org.kde.StatusNotifierWatcher"
	trayWatcherPath = dbus.ObjectPath("/StatusNotifierWatcher")

	// trayNotifyEvery is the floor between two "key not bound" notifications.
	// It applies to that warning only; the bind helper always notifies.
	trayNotifyEvery = 30 * time.Second

	// trayBindDelay is how long the bind helper waits before pressing the
	// button, so the user can switch to TeamSpeak and start the assignment.
	trayBindDelay = 10 * time.Second
)

// --- the app icon file -----------------------------------------------------

// trayIconSVG is our own icon, used both for the notifications we send and for
// the Icon= line of the autostart entry. A notification server wants a path or
// a theme name, not bytes, so the embedded file is unpacked into the cache dir
// once and referenced from there.
//
//go:embed assets/ts6tray.svg
var trayIconSVG []byte

// trayIconFile writes the embedded icon to $XDG_CACHE_HOME/ts6tray/ts6tray.svg
// and returns its absolute path. It rewrites the file only when the content
// differs, so a running daemon does not touch it on every start. The daemon and
// the installer both call it; whichever runs first creates the file.
func trayIconFile() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "ts6tray")
	path := filepath.Join(dir, "ts6tray.svg")
	if old, rerr := os.ReadFile(path); rerr == nil && bytes.Equal(old, trayIconSVG) {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, trayIconSVG, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// --- the per-state notification icons --------------------------------------

// The three states a mute notification can report, and the file each one is
// cached under. They are PNGs and not the app SVG because a notification server
// is only required to handle a path; PNG is the one raster format every one of
// them reads, and rendering our own artwork keeps the notification and the tray
// telling the same story with the same picture.
var trayStateIconNames = map[Icon]string{
	IconMicMuted:     "state-mic-muted.png",
	IconSpeakerMuted: "state-speaker-muted.png",
	IconQuiet:        "state-quiet.png",
}

// trayStateIconSize is the edge of the rendered PNG. A notification server
// scales down far better than up, and 64 px is the largest size any of them
// asks for in practice.
const trayStateIconSize = 64

// trayStatePNG renders one state with the tray's own painter and encodes it.
//
// trayCanvas.pixmap hands back SNI's ARGB32: big-endian, so byte order A, R, G,
// B, with straight (un-premultiplied) alpha. image.NRGBA is R, G, B, A, also
// straight — so this is purely a reshuffle of the four bytes, no alpha maths.
// Getting it wrong is silent: the blue ring comes out orange.
func trayStatePNG(ic Icon) ([]byte, error) {
	p := trayDraw(trayStateIconSize*trayOversample, ic).pixmap(trayStateIconSize)
	img := image.NewNRGBA(image.Rect(0, 0, int(p.Width), int(p.Height)))
	for i := 0; i+3 < len(p.Data); i += 4 {
		a, r, g, b := p.Data[i], p.Data[i+1], p.Data[i+2], p.Data[i+3]
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = r, g, b, a
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// trayStateIconFiles renders the three state icons into
// $XDG_CACHE_HOME/ts6tray/ and returns Icon -> path. Like trayIconFile it
// rewrites a file only when the bytes differ, so a restart does not churn the
// cache directory and a server watching those paths sees nothing move.
func trayStateIconFiles() (map[Icon]string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(base, "ts6tray")
	out := make(map[Icon]string, len(trayStateIconNames))
	made := false
	// Sorted, so the error a caller sees does not depend on map order.
	icons := make([]Icon, 0, len(trayStateIconNames))
	for ic := range trayStateIconNames {
		icons = append(icons, ic)
	}
	sort.Slice(icons, func(i, j int) bool { return icons[i] < icons[j] })
	for _, ic := range icons {
		path := filepath.Join(dir, trayStateIconNames[ic])
		data, perr := trayStatePNG(ic)
		if perr != nil {
			return out, perr
		}
		if old, rerr := os.ReadFile(path); rerr == nil && bytes.Equal(old, data) {
			out[ic] = path
			continue
		}
		if !made {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return out, err
			}
			made = true
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return out, err
		}
		out[ic] = path
	}
	return out, nil
}

// --- pixmaps ---------------------------------------------------------------

// trayPixmap is one entry of the SNI IconPixmap array: width, height and
// width*height ARGB32 pixels, big-endian, not premultiplied. Signature (iiay).
type trayPixmap struct {
	Width  int32
	Height int32
	Data   []byte
}

// trayTooltip is the SNI ToolTip property: (sa(iiay)ss).
type trayTooltip struct {
	IconName    string
	IconPixmap  []trayPixmap
	Title       string
	Description string
}

// trayCanvas is a tiny premultiplied-alpha painter used to draw the icons.
// It works at trayOversample times the target size and box-filters down, which
// buys antialiasing for free without any drawing library.
type trayCanvas struct {
	n  int // side length in samples
	px []color.RGBA
}

const trayOversample = 4

func trayNewCanvas(n int) *trayCanvas {
	return &trayCanvas{n: n, px: make([]color.RGBA, n*n)}
}

// trayShape reports whether a point (in 0..1 icon space) is inside it.
type trayShape func(x, y float64) bool

func (cv *trayCanvas) each(s trayShape, f func(i int)) {
	for yi := 0; yi < cv.n; yi++ {
		y := (float64(yi) + 0.5) / float64(cv.n)
		for xi := 0; xi < cv.n; xi++ {
			x := (float64(xi) + 0.5) / float64(cv.n)
			if s(x, y) {
				f(yi*cv.n + xi)
			}
		}
	}
}

// fill paints col (opaque) over every sample inside s.
func (cv *trayCanvas) fill(s trayShape, col color.RGBA) {
	cv.each(s, func(i int) { cv.px[i] = col })
}

// erase punches s back to full transparency; this is how the slash gets its
// dark gap and how the "hollow" icon becomes an outline.
func (cv *trayCanvas) erase(s trayShape) {
	cv.each(s, func(i int) { cv.px[i] = color.RGBA{} })
}

// pixmap box-filters the canvas down to size px and encodes ARGB32 big-endian,
// un-premultiplied.
func (cv *trayCanvas) pixmap(size int) trayPixmap {
	step := cv.n / size
	out := make([]byte, size*size*4)
	area := float64(step * step)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var r, g, b, a float64
			for sy := 0; sy < step; sy++ {
				for sx := 0; sx < step; sx++ {
					p := cv.px[(y*step+sy)*cv.n+(x*step+sx)]
					r += float64(p.R)
					g += float64(p.G)
					b += float64(p.B)
					a += float64(p.A)
				}
			}
			r, g, b, a = r/area, g/area, b/area, a/area
			// Stored premultiplied; SNI wants straight ARGB32.
			if a > 0 {
				r = math.Min(255, r*255/a)
				g = math.Min(255, g*255/a)
				b = math.Min(255, b*255/a)
			}
			o := (y*size + x) * 4
			out[o+0] = byte(a + 0.5)
			out[o+1] = byte(r + 0.5)
			out[o+2] = byte(g + 0.5)
			out[o+3] = byte(b + 0.5)
		}
	}
	return trayPixmap{Width: int32(size), Height: int32(size), Data: out}
}

// --- shape primitives (icon space is the unit square) ----------------------

func trayRoundRect(x0, y0, x1, y1, r, grow float64) trayShape {
	x0, y0, x1, y1, r = x0-grow, y0-grow, x1+grow, y1+grow, r+grow
	if r < 0 {
		r = 0
	}
	cx0, cy0, cx1, cy1 := x0+r, y0+r, x1-r, y1-r
	return func(x, y float64) bool {
		if x < x0 || x > x1 || y < y0 || y > y1 {
			return false
		}
		px := math.Max(cx0-x, math.Max(0, x-cx1))
		py := math.Max(cy0-y, math.Max(0, y-cy1))
		return px*px+py*py <= r*r
	}
}

// trayArc is a ring segment: radii ri..ro around (cx,cy), limited to the
// samples for which keep reports true (used to take just the lower half).
func trayArc(cx, cy, ri, ro, grow float64, keep func(dx, dy float64) bool) trayShape {
	ri, ro = ri-grow, ro+grow
	if ri < 0 {
		ri = 0
	}
	return func(x, y float64) bool {
		dx, dy := x-cx, y-cy
		d2 := dx*dx + dy*dy
		return d2 >= ri*ri && d2 <= ro*ro && keep(dx, dy)
	}
}

// traySegment is a thick line from (x0,y0) to (x1,y1) with round caps.
func traySegment(x0, y0, x1, y1, w, grow float64) trayShape {
	r := w/2 + grow
	vx, vy := x1-x0, y1-y0
	l2 := vx*vx + vy*vy
	return func(x, y float64) bool {
		t := 0.0
		if l2 > 0 {
			t = ((x-x0)*vx + (y-y0)*vy) / l2
			t = math.Max(0, math.Min(1, t))
		}
		dx, dy := x-(x0+t*vx), y-(y0+t*vy)
		return dx*dx+dy*dy <= r*r
	}
}

// trayTrapezoid is the cone of the speaker glyph: x from xa to xb, half-height
// growing linearly from ha to hb around cy.
func trayTrapezoid(xa, xb, ha, hb, cy, grow float64) trayShape {
	return func(x, y float64) bool {
		if x < xa-grow || x > xb+grow {
			return false
		}
		t := (x - xa) / (xb - xa)
		t = math.Max(0, math.Min(1, t))
		h := ha + t*(hb-ha) + grow
		return math.Abs(y-cy) <= h
	}
}

func trayUnion(ss ...trayShape) trayShape {
	return func(x, y float64) bool {
		for _, s := range ss {
			if s(x, y) {
				return true
			}
		}
		return false
	}
}

// --- glyphs ----------------------------------------------------------------

// trayMicGlyph is a microphone: capsule, cradle arc, stem and base.
func trayMicGlyph(grow float64) trayShape {
	return trayUnion(
		trayRoundRect(0.375, 0.10, 0.625, 0.56, 0.125, grow),
		trayArc(0.5, 0.46, 0.245, 0.315, grow, func(dx, dy float64) bool { return dy >= 0 }),
		trayRoundRect(0.465, 0.74, 0.535, 0.87, 0.01, grow),
		trayRoundRect(0.32, 0.845, 0.68, 0.905, 0.03, grow),
	)
}

// traySpeakerGlyph is a speaker: body, cone and two sound waves.
func traySpeakerGlyph(grow float64) trayShape {
	right := func(dx, dy float64) bool { return dx >= math.Abs(dy)*0.45 }
	return trayUnion(
		trayRoundRect(0.14, 0.375, 0.33, 0.625, 0.03, grow),
		trayTrapezoid(0.30, 0.52, 0.125, 0.30, 0.5, grow),
		trayArc(0.53, 0.5, 0.155, 0.215, grow, right),
		trayArc(0.53, 0.5, 0.28, 0.34, grow, right),
	)
}

// The dot geometry comes from the reference artwork: a 20 px circle whose ring
// is 2 px thick, i.e. a stroke of one fifth of the radius. trayDotGrow then
// widens the radius by one target pixel in every direction, which the eye reads
// as a slightly more present dot; the stroke keeps its thickness.
const (
	trayDotR      = 0.40
	trayDotStroke = 0.08
	trayDotGrow   = 1.0 // target pixels of extra radius
)

func trayAnywhere(dx, dy float64) bool { return true }

// trayDisc is a filled circle of radius r about the icon's centre.
func trayDisc(r, grow float64) trayShape { return trayArc(0.5, 0.5, 0, r, grow, trayAnywhere) }

// onePixel is one pixel of the finished icon, in the 0..1 icon space the
// shapes are written in. It is how a nudge stays a nudge at 22 px and at 48.
func (cv *trayCanvas) onePixel() float64 { return float64(trayOversample) / float64(cv.n) }

var (
	// trayBlue is the exact blue of the reference artwork's ring, #0353f4.
	trayBlue = color.RGBA{0x03, 0x53, 0xf4, 0xff}
	// trayBlueLit is the reference's lit interior, #0b59ce. Measuring the
	// artwork shows both dots share one ring and differ only in their fill:
	// quiet holds a near-transparent wash, talking holds this solid blue.
	trayBlueLit = color.RGBA{0x0b, 0x59, 0xce, 0xff}
	// trayGrey is the same dot, desaturated, for the disabled state.
	trayGrey = color.RGBA{0x8a, 0x90, 0x97, 0xff}
	// trayWhite is the glyph colour and the colour of the mute slash.
	trayWhite = color.RGBA{0xff, 0xff, 0xff, 0xff}
	// trayOutline is a subtle dark rim so a white glyph survives a light panel.
	trayOutline = color.RGBA{0x14, 0x16, 0x19, 0xd8}
)

// trayNoneAlpha is how much of the dot is left in the "no connection" state,
// which is the disabled dot turned down rather than a shape of its own.
const trayNoneAlpha = 0.45

// trayRingFill is how opaque the tint inside an unlit ring is; the reference
// shows the ring is not hollow but holds a faint wash of its own colour.
const trayRingFill = 0.19

// trayFade scales a colour towards full transparency. The canvas is
// premultiplied, so scaling every component keeps the hue and only moves alpha.
func trayFade(c color.RGBA, f float64) color.RGBA {
	s := func(v byte) byte { return byte(float64(v)*f + 0.5) }
	return color.RGBA{s(c.R), s(c.G), s(c.B), s(c.A)}
}

// trayDrawDot paints the indicator: one outline ring in ring, and inside it
// the fill that says whether it is lit. Every unmuted state is this same dot,
// so lighting up reads as a change of state, not a change of icon.
func trayDrawDot(cv *trayCanvas, ring, fill color.RGBA) {
	ro := trayDotR + trayDotGrow*cv.onePixel()
	ri := ro - trayDotStroke
	cv.fill(trayDisc(ri, 0), fill)
	cv.fill(trayArc(0.5, 0.5, ri, ro, 0, trayAnywhere), ring)
}

// trayDraw renders one state onto a fresh canvas.
//
// Every state that is not muted is the same outlined dot, lit blue while
// talking, washed out while quiet, grey while the mic is disabled and fainter
// still with no connection. Only the two muted states carry a pictogram — a
// white microphone or speaker with a single white slash across it, separated
// from the glyph by a transparent gap — because those are the only states that
// have to say *what* is muted.
func trayDraw(n int, ic Icon) *trayCanvas {
	cv := trayNewCanvas(n)

	switch ic {
	case IconTalking:
		trayDrawDot(cv, trayBlue, trayBlueLit)
		return cv
	case IconQuiet:
		trayDrawDot(cv, trayBlue, trayFade(trayBlue, trayRingFill))
		return cv
	case IconMicDisabled:
		trayDrawDot(cv, trayGrey, trayFade(trayGrey, trayRingFill))
		return cv
	case IconNone:
		trayDrawDot(cv, trayFade(trayGrey, trayNoneAlpha),
			trayFade(trayGrey, trayRingFill*trayNoneAlpha))
		return cv
	}

	glyph := trayMicGlyph
	if ic == IconSpeakerMuted {
		glyph = traySpeakerGlyph
	}

	// A dark rim keeps the white glyph readable on light panels too.
	cv.fill(glyph(0.030), trayOutline)
	cv.fill(glyph(0), trayWhite)

	// Punch a transparent gap first, then lay the slash inside it, so the
	// single line stays legible where it crosses the glyph. The slash gets the
	// same dark rim as the glyph, for the same reason.
	cv.erase(traySegment(0.15, 0.13, 0.85, 0.87, 0.115, 0.048))
	cv.fill(traySegment(0.16, 0.14, 0.84, 0.86, 0.115, 0.018), trayOutline)
	cv.fill(traySegment(0.16, 0.14, 0.84, 0.86, 0.115, 0), trayWhite)
	return cv
}

var traySizes = []int{22, 32, 48}

var trayPixmapCache = func() map[Icon][]trayPixmap {
	m := make(map[Icon][]trayPixmap, 6)
	for _, ic := range []Icon{IconNone, IconQuiet, IconTalking, IconMicMuted, IconSpeakerMuted, IconMicDisabled} {
		var ps []trayPixmap
		for _, s := range traySizes {
			ps = append(ps, trayDraw(s*trayOversample, ic).pixmap(s))
		}
		m[ic] = ps
	}
	return m
}()

// trayPixmapsFor returns the generated ARGB32 pixmaps for a state.
//
// It must hand out a fresh copy, never the cached slice. godbus/prop keeps a
// pointer to the value it was exported with and stores every later value
// *through* that pointer (prop.set → dbus.Store), which writes into the
// existing backing arrays rather than replacing them. Handing it the cache
// therefore makes the exported IconPixmap alias trayPixmapCache[ic], and the
// next SetMust overwrites that state's artwork with the new state's: the icon
// exported at export() time (quiet) turned into the talking icon after the
// first talk, and stayed that way until restart.
func trayPixmapsFor(ic Icon) []trayPixmap {
	src := trayPixmapCache[ic]
	out := make([]trayPixmap, len(src))
	for i, p := range src {
		out[i] = trayPixmap{Width: p.Width, Height: p.Height, Data: append([]byte(nil), p.Data...)}
	}
	return out
}

// --- menu model ------------------------------------------------------------

// dbusmenu item ids. Server rows occupy trayIDServer0 and up.
const (
	trayIDRoot    int32 = 0
	trayIDSep1    int32 = 10
	trayIDMic     int32 = 11
	trayIDSpeaker int32 = 12
	trayIDSep2    int32 = 13
	trayIDQuit    int32 = 14

	// The Settings submenu and its children.
	trayIDSettings     int32 = 20
	trayIDBindMic      int32 = 21
	trayIDBindSpeaker  int32 = 22
	trayIDSep3         int32 = 23
	trayIDClickMic     int32 = 24
	trayIDClickSpeaker int32 = 25

	// The Notifications submenu inside Settings, and the separator above it.
	// Its checkmark children start at trayIDNotify0, one per trayNotifyGroups
	// entry, in that order.
	//
	// Below those, a separator and the two option checkmarks, which are
	// switches of the same kind but about how notifications are delivered
	// rather than which events produce them.
	trayIDNotify     int32 = 26
	trayIDSep4       int32 = 27
	trayIDNotify0    int32 = 30
	trayIDSep5       int32 = 38
	trayIDNotifyOpt0 int32 = 39

	trayIDServer0 int32 = 100
)

// trayMenuNode is the dbusmenu layout struct: (ia{sv}av).
type trayMenuNode struct {
	ID       int32
	Props    map[string]dbus.Variant
	Children []dbus.Variant
}

// trayMenuProps is one entry of GetGroupProperties' a(ia{sv}).
type trayMenuProps struct {
	ID    int32
	Props map[string]dbus.Variant
}

// trayMenuRemovedProps is one entry of ItemsPropertiesUpdated's second
// argument, a(ias): the properties that went back to their default. We never
// remove one, but the signal's signature demands the array.
type trayMenuRemovedProps struct {
	ID    int32
	Props []string
}

// trayMenuEvent is one entry of EventGroup's a(isvu).
type trayMenuEvent struct {
	ID        int32
	EventID   string
	Data      dbus.Variant
	Timestamp uint32
}

// trayRow is one menu row before it becomes dbusmenu properties.
//
// The rows stay a flat, comparable slice — trayRowsEqual compares them with ==,
// which a []trayRow field would break — so nesting is expressed by parent: a
// row with parent == trayIDRoot hangs off the menu bar, any other parent names
// the submenu row it belongs to.
type trayRow struct {
	id        int32
	parent    int32 // trayIDRoot, or the id of the submenu row holding this one
	label     string
	separator bool
	enabled   bool
	submenu   bool // has children: "children-display" = "submenu"
	radio     bool // "toggle-type" = "radio"
	check     bool // "toggle-type" = "checkmark"
	checked   bool // the radio's or checkmark's "toggle-state"
}

func (r trayRow) props() map[string]dbus.Variant {
	p := map[string]dbus.Variant{}
	if r.separator {
		p["type"] = dbus.MakeVariant("separator")
		p["enabled"] = dbus.MakeVariant(false)
		return p
	}
	// In dbusmenu "_" marks the following character as the mnemonic, so a
	// server called my_server would render as "myserver". Double it to escape.
	p["label"] = dbus.MakeVariant(strings.ReplaceAll(r.label, "_", "__"))
	p["enabled"] = dbus.MakeVariant(r.enabled)
	p["visible"] = dbus.MakeVariant(true)
	if r.submenu {
		p["children-display"] = dbus.MakeVariant("submenu")
	}
	if r.radio || r.check {
		kind := "checkmark"
		if r.radio {
			kind = "radio"
		}
		p["toggle-type"] = dbus.MakeVariant(kind)
		state := int32(0)
		if r.checked {
			state = 1
		}
		p["toggle-state"] = dbus.MakeVariant(state)
	}
	return p
}

// --- the tray itself -------------------------------------------------------

// trayCallTimeout bounds a call to the tray host, which runs on the update
// loop: a hung host must not stall icon updates for the bus's ~25 s default.
const trayCallTimeout = 2 * time.Second

type tray struct {
	ctx  context.Context // for bounding outgoing calls; cancelled on shutdown
	conn *dbus.Conn
	ts   *TSClient
	quit func()
	name string // org.kde.StatusNotifierItem-<pid>-1

	// mute and press default to the TSClient's methods; tests replace them so
	// nothing talks to a real TeamSpeak.
	mute      func(target, mode string) (bool, error)
	press     func(button string) error
	bindDelay time.Duration // 0 means trayBindDelay

	// notifyFn, when set, takes the place of the D-Bus Notify call. Tests use
	// it to capture what would have been sent, replaces_id and icon path
	// included.
	notifyFn func(replaces uint32, title, body, icon string) uint32

	// emitFn, when set, takes the place of conn.Emit. Tests use it to capture
	// which menu signal a change produced, without a bus.
	emitFn func(path dbus.ObjectPath, name string, args ...any)

	// batch coalesces bursts of event notices; nil means send each one at once.
	batch *noticeBatcher

	// cfgMu serialises the read-modify-write of the config file. Two menu
	// clicks land in two goroutines, and without this the second one's rewrite
	// can be built from a copy of the file taken before the first one's landed,
	// silently undoing it.
	cfgMu sync.Mutex

	mu              sync.RWMutex
	icon            Icon
	conns           []Conn
	rows            []trayRow
	revision        uint32
	exported        bool
	lastNotify      time.Time
	warnedNoWatcher bool
	iconPath        string          // cached app icon, "" when it could not be written
	stateIcons      map[Icon]string // rendered per-state notification PNGs, by Icon
	lastEventID     uint32          // id of the last event notification, for replaces_id
	click           string          // left-click target: "mic" or "speaker"
	notif           map[string]bool // Settings -> Notifications switches, by group key
	binding         string          // "" or the target whose bind countdown is running

	props *prop.Properties
}

func (t *tray) muteFunc() func(string, string) (bool, error) {
	if t.mute != nil {
		return t.mute
	}
	return t.ts.SetMute
}

func (t *tray) pressFunc() func(string) error {
	if t.press != nil {
		return t.press
	}
	return t.ts.Press
}

// --- the click-target setting ----------------------------------------------

// trayConfigPath is $XDG_CONFIG_HOME/ts6tray/config, next to the API key.
func trayConfigPath() string { return filepath.Join(filepath.Dir(DefaultKeyPath()), "config") }

// The config file is a flat list of "key=value" lines:
//
//	click=mic|speaker
//	notify.<group>=on|off
//
// Anything else — blank lines, "#" comments, garbage, unknown keys — is ignored,
// and a missing key falls back to its default, so the older one-line
// "click=speaker" file still loads unchanged. Writes always rewrite the whole
// file, keeping the keys we do not own.

// trayReadConfig parses the config file into key/value pairs. A missing or
// unreadable file is an empty config, which means "all defaults".
func trayReadConfig(path string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

// trayWriteConfig rewrites the whole config file, creating the dir. Keys are
// sorted so the file is stable and diffable.
func trayWriteConfig(path string, kv map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(kv[k])
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

// trayReadClick reads the persisted left-click target. A missing, unreadable or
// unrecognised value means the default, "mic".
func trayReadClick(path string) string {
	if v := trayReadConfig(path)["click"]; v == "speaker" || v == "mic" {
		return v
	}
	return "mic"
}

// trayWriteClick persists the left-click target, leaving the notification
// switches in the file alone.
func trayWriteClick(path, click string) error {
	kv := trayReadConfig(path)
	kv["click"] = click
	return trayWriteConfig(path, kv)
}

// trayNotifyGroup is one on/off switch in Settings -> Notifications. One switch
// can cover more than one noticeKind: "joins or leaves" is a single choice for
// the user but two kinds on the wire.
type trayNotifyGroup struct {
	key   string // config key suffix: notify.<key>
	label string
	def   bool // default when the config says nothing
	kinds []noticeKind
}

// trayNotifyGroups is the menu order, the config keys and the defaults, all in
// one place. Messages, pokes, channel messages and other people's mute changes
// are off by default: TeamSpeak already pops up its own notification for the
// first three, and the fourth is constant chatter.
var trayNotifyGroups = []trayNotifyGroup{
	{"joinleave", "Someone joins or leaves my channel", true, []noticeKind{noticeJoin, noticeLeave}},
	{"moved", "Someone is moved in or out", true, []noticeKind{noticeMoved}},
	{"kicked", "Someone is kicked or times out", true, []noticeKind{noticeKicked}},
	{"mute", "Someone in my channel mutes or unmutes", false, []noticeKind{noticeMute}},
	{"privateMsg", "Private messages", false, []noticeKind{noticePrivateMsg}},
	{"poke", "Pokes", false, []noticeKind{noticePoke}},
	{"channelMsg", "Channel messages", false, []noticeKind{noticeChannelMsg}},
	{"connLost", "Connection lost", true, []noticeKind{noticeConnLost}},
}

// trayNotifyOptions are the two delivery switches at the bottom of the same
// submenu. They govern no noticeKind, so kinds is nil and notifyEnabled never
// sees them; notifyOptOn reads them instead.
const (
	trayOptBatch   = "batch"
	trayOptReplace = "replace"
)

var trayNotifyOptions = []trayNotifyGroup{
	{trayOptBatch, "Group bursts (0.5 s)", true, nil},
	{trayOptReplace, "Replace previous notification", true, nil},
}

// trayNotifySwitches is every switch in the submenu, kinds first: what the
// config file stores and what the defaults cover.
var trayNotifySwitches = append(append([]trayNotifyGroup{}, trayNotifyGroups...), trayNotifyOptions...)

// trayNotifyKindGroup maps a noticeKind back to the switch that governs it.
var trayNotifyKindGroup = func() map[noticeKind]string {
	m := map[noticeKind]string{}
	for _, g := range trayNotifyGroups {
		for _, k := range g.kinds {
			m[k] = g.key
		}
	}
	return m
}()

// trayNotifyDefaults is a fresh copy of the default switch positions.
func trayNotifyDefaults() map[string]bool {
	m := make(map[string]bool, len(trayNotifySwitches))
	for _, g := range trayNotifySwitches {
		m[g.key] = g.def
	}
	return m
}

// trayReadNotify reads the notification switches, defaulting anything missing
// or unparsable.
func trayReadNotify(path string) map[string]bool {
	m := trayNotifyDefaults()
	kv := trayReadConfig(path)
	for _, g := range trayNotifySwitches {
		switch kv["notify."+g.key] {
		case "on":
			m[g.key] = true
		case "off":
			m[g.key] = false
		}
	}
	return m
}

// trayWriteNotify persists the notification switches, leaving click= alone.
func trayWriteNotify(path string, m map[string]bool) error {
	kv := trayReadConfig(path)
	for _, g := range trayNotifySwitches {
		v := "off"
		if m[g.key] {
			v = "on"
		}
		kv["notify."+g.key] = v
	}
	return trayWriteConfig(path, kv)
}

// trayOther is the target the middle click gets: whichever one left-click has
// not taken.
func trayOther(target string) string {
	if target == "speaker" {
		return "mic"
	}
	return "speaker"
}

// clickTarget is the configured left-click target, never empty.
func (t *tray) clickTarget() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.click == "speaker" {
		return "speaker"
	}
	return "mic"
}

// setClick selects a left-click target, persists it and republishes the menu so
// the radio marks move.
func (t *tray) setClick(target string) {
	if target != "speaker" {
		target = "mic"
	}
	t.mu.Lock()
	changed := t.click != target
	t.click = target
	t.mu.Unlock()
	if !changed {
		return
	}
	t.cfgMu.Lock()
	err := trayWriteClick(trayConfigPath(), target)
	t.cfgMu.Unlock()
	if err != nil {
		log.Printf("tray: saving the click setting: %v", err)
	}
	t.refresh()
}

// toggleNotify flips one Settings -> Notifications switch, persists the whole
// set and republishes the menu so the checkmark moves.
func (t *tray) toggleNotify(key string) {
	// Held across the flip and the write, so two clicks in two goroutines
	// cannot each rewrite the file from a stale copy.
	t.cfgMu.Lock()
	t.mu.Lock()
	if t.notif == nil {
		t.notif = trayNotifyDefaults()
	}
	t.notif[key] = !t.notif[key]
	snap := make(map[string]bool, len(t.notif))
	for k, v := range t.notif {
		snap[k] = v
	}
	t.mu.Unlock()
	err := trayWriteNotify(trayConfigPath(), snap)
	t.cfgMu.Unlock()
	if err != nil {
		log.Printf("tray: saving the notification settings: %v", err)
	}
	// Switching batching off mid-burst must not strand whatever is queued.
	if key == trayOptBatch && !snap[trayOptBatch] && t.batch != nil {
		t.batch.flush()
	}
	t.refresh()
}

// notifyEnabled reports whether the user wants to see this kind of notice.
func (t *tray) notifyEnabled(k noticeKind) bool {
	key, ok := trayNotifyKindGroup[k]
	if !ok {
		return false
	}
	t.mu.RLock()
	v, set := t.notif[key]
	t.mu.RUnlock()
	if set {
		return v
	}
	return trayNotifyDefaults()[key]
}

// notifyOptOn reports one of the two delivery options. notifyEnabled cannot
// answer for them: they govern no noticeKind.
func (t *tray) notifyOptOn(key string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.notifyOptOnLocked(key)
}

// notifyOptOnLocked is notifyOptOn for a caller already holding t.mu.
func (t *tray) notifyOptOnLocked(key string) bool {
	if v, ok := t.notif[key]; ok {
		return v
	}
	return trayNotifyDefaults()[key]
}

// onNotice shows one notice from the TSClient, if its kind is switched on.
// With "Group bursts" on it goes into the batcher instead and is shown, with
// whatever else arrives in the next half second, as a single notification.
func (t *tray) onNotice(n notice) {
	if !t.notifyEnabled(n.kind) {
		return
	}
	if t.batch != nil && t.notifyOptOn(trayOptBatch) {
		t.batch.add(n)
		return
	}
	t.notifyEvent(n.title, n.body, n.icon)
}

// refresh rebuilds the rows from the current state and, if they moved, bumps
// the revision and tells the host. sync does the same for TeamSpeak's state;
// this is for the settings the user changes from inside the menu.
func (t *tray) refresh() {
	t.mu.Lock()
	rows := trayRowsFor(t.conns, t.click, t.binding, t.notif)
	old := t.rows
	changed := !trayRowsEqual(old, rows)
	if changed {
		t.rows = rows
		t.revision++
	}
	rev, exported := t.revision, t.exported
	t.mu.Unlock()
	if changed && exported {
		t.emitMenuChange(old, rows, rev)
	}
}

// emitMenuChange tells the host what moved, in the smallest terms that say it.
//
// A checkmark or a radio flipping changes nothing about the menu's shape, so it
// goes out as ItemsPropertiesUpdated carrying only the new toggle-state of the
// rows that flipped. LayoutUpdated is what a host answers by fetching the whole
// layout again, which for a click on a checkbox is a rebuild of every row to
// move one tick — and some hosts close the open menu over it.
//
// Anything structural — a server row appearing, a bind countdown relabelling
// and disabling its row, a row added or removed — still needs LayoutUpdated,
// because the host cannot learn about it from properties alone. The revision is
// bumped either way, so a host that re-reads the layout for its own reasons
// never sees a stale number.
func (t *tray) emitMenuChange(old, rows []trayRow, rev uint32) {
	if upd, ok := trayToggleOnlyDiff(old, rows); ok {
		t.emit(trayMenuPath, trayMenuIface+".ItemsPropertiesUpdated",
			upd, []trayMenuRemovedProps{})
		return
	}
	t.emit(trayMenuPath, trayMenuIface+".LayoutUpdated", rev, trayIDRoot)
}

// trayToggleOnlyDiff reports whether old and rows differ in nothing but the
// toggle-state of some rows, and if so returns just those rows' new state as
// ItemsPropertiesUpdated's a(ia{sv}).
//
// ok is false for an identical pair too: there is then nothing to send, and the
// caller only reaches this after establishing that something did change.
func trayToggleOnlyDiff(old, rows []trayRow) ([]trayMenuProps, bool) {
	if len(old) != len(rows) {
		return nil, false
	}
	var upd []trayMenuProps
	for i := range rows {
		if old[i] == rows[i] {
			continue
		}
		// Everything except checked has to match: compare the old row with its
		// checked flag set to the new one's, which leaves exactly that field out.
		was := old[i]
		was.checked = rows[i].checked
		if was != rows[i] {
			return nil, false
		}
		state := int32(0)
		if rows[i].checked {
			state = 1
		}
		upd = append(upd, trayMenuProps{
			ID:    rows[i].id,
			Props: map[string]dbus.Variant{"toggle-state": dbus.MakeVariant(state)},
		})
	}
	return upd, len(upd) > 0
}

// RunTray runs the StatusNotifierItem until ctx is done. It never returns an
// error for a missing or restarting tray host; only a broken session bus or a
// failed export is fatal.
func RunTray(ctx context.Context, c *TSClient, quit func()) error {
	// Nothing to show if we are already shutting down: a second daemon whose
	// IPC bind failed must not flash an icon on its way out.
	if ctx.Err() != nil {
		return nil
	}

	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return fmt.Errorf("tray: session bus: %w", err)
	}
	defer conn.Close()

	t := &tray{
		ctx:   ctx,
		conn:  conn,
		ts:    c,
		quit:  quit,
		name:  fmt.Sprintf("org.kde.StatusNotifierItem-%d-1", os.Getpid()),
		icon:  IconNone,
		click: trayReadClick(trayConfigPath()),
		notif: trayReadNotify(trayConfigPath()),
	}
	if path, ierr := trayIconFile(); ierr != nil {
		log.Printf("tray: writing the app icon: %v", ierr)
	} else {
		t.iconPath = path
	}
	// The mute notifications want the mentioned user's state as their picture,
	// so the three states are rendered once here. A failure is not fatal: every
	// notification simply keeps the app icon.
	icons, ierr := trayStateIconFiles()
	if ierr != nil {
		log.Printf("tray: writing the state icons: %v", ierr)
	}
	t.stateIcons = icons // whatever got written before the error still counts
	t.batch = &noticeBatcher{
		window: noticeBatchWindow,
		cap:    noticeBatchCap,
		send:   t.notifyEvent,
	}
	if quit == nil {
		t.quit = func() {}
	}

	// Watch for the tray host coming and going, so a shell restart re-registers.
	owners := make(chan *dbus.Signal, 8)
	conn.Signal(owners)
	defer conn.RemoveSignal(owners)
	if err := conn.AddMatchSignal(
		dbus.WithMatchSender("org.freedesktop.DBus"),
		dbus.WithMatchObjectPath("/org/freedesktop/DBus"),
		dbus.WithMatchInterface("org.freedesktop.DBus"),
		dbus.WithMatchMember("NameOwnerChanged"),
		dbus.WithMatchArg(0, trayWatcherName),
	); err != nil {
		log.Printf("tray: cannot watch %s: %v", trayWatcherName, err)
	}

	updates := c.Updates()
	notices := c.Notices()
	t.sync(true)

	for {
		select {
		case <-ctx.Done():
			// Show what the last half second collected rather than drop it.
			t.batch.flush()
			t.teardown()
			return nil
		case <-updates:
			t.sync(false)
		case n := <-notices:
			t.onNotice(n)
		case sig := <-owners:
			if sig == nil || sig.Name != "org.freedesktop.DBus.NameOwnerChanged" || len(sig.Body) < 3 {
				continue
			}
			newOwner, _ := sig.Body[2].(string)
			if newOwner == "" {
				continue // host went away; our objects stay put, it will be back
			}
			log.Printf("tray: %s reappeared, re-registering", trayWatcherName)
			t.mu.Lock()
			t.warnedNoWatcher = false
			t.mu.Unlock()
			t.register()
		}
	}
}

// sync pulls the current snapshot, brings the exported objects in line with it
// and emits the change signals. first suppresses nothing; it only exists so the
// initial pass does not log a spurious state transition.
func (t *tray) sync(first bool) {
	conns, icon, up := t.ts.Snapshot()

	if !up {
		t.teardown()
		return
	}

	tip := trayTooltipFor(conns)

	t.mu.Lock()
	rows := trayRowsFor(conns, t.click, t.binding, t.notif)
	oldRows := t.rows
	iconChanged := first || t.icon != icon
	menuChanged := first || !trayRowsEqual(oldRows, rows)
	t.icon, t.conns, t.rows = icon, conns, rows
	if menuChanged {
		t.revision++
	}
	rev := t.revision
	wasExported := t.exported
	t.mu.Unlock()

	if !wasExported {
		if err := t.export(); err != nil {
			log.Printf("tray: export failed: %v", err)
			return
		}
		t.emit(trayItemPath, trayItemIface+".NewStatus", "Active")
		t.register()
		return // export/register publishes the current values already
	}

	if t.props != nil {
		if iconChanged {
			t.props.SetMust(trayItemIface, "IconPixmap", trayPixmapsFor(icon))
		}
		t.props.SetMust(trayItemIface, "ToolTip", tip)
	}
	if iconChanged {
		t.emit(trayItemPath, trayItemIface+".NewIcon")
	}
	t.emit(trayItemPath, trayItemIface+".NewToolTip")
	if menuChanged {
		// first makes oldRows nil, so this is always a LayoutUpdated then.
		t.emitMenuChange(oldRows, rows, rev)
	}
}

func (t *tray) emit(path dbus.ObjectPath, name string, args ...any) {
	if t.emitFn != nil {
		t.emitFn(path, name, args...)
		return
	}
	if t.conn == nil {
		return
	}
	if err := t.conn.Emit(path, name, args...); err != nil {
		log.Printf("tray: emit %s: %v", name, err)
	}
}

// export claims the bus name and publishes both objects.
func (t *tray) export() error {
	t.mu.RLock()
	icon, conns := t.icon, t.conns
	t.mu.RUnlock()

	reply, err := t.conn.RequestName(t.name, dbus.NameFlagDoNotQueue)
	if err != nil {
		return fmt.Errorf("request name %s: %w", t.name, err)
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		return fmt.Errorf("request name %s: not primary owner (%v)", t.name, reply)
	}

	props, err := prop.Export(t.conn, trayItemPath, prop.Map{
		trayItemIface: {
			"Category":   {Value: "Hardware", Emit: prop.EmitConst},
			"Id":         {Value: "ts6tray", Emit: prop.EmitConst},
			"Title":      {Value: "TeamSpeak", Emit: prop.EmitConst},
			"Status":     {Value: "Active", Emit: prop.EmitTrue},
			"WindowId":   {Value: int32(0), Emit: prop.EmitConst},
			"ItemIsMenu": {Value: false, Emit: prop.EmitConst},
			"Menu":       {Value: trayMenuPath, Emit: prop.EmitConst},
			// IconName is empty on purpose, and never set. A host that sees a
			// non-empty name prefers it over IconPixmap and draws the theme's
			// icon instead of ours: Quickshell, and so DankMaterialShell,
			// resolves the item to "image://icon/<IconName>" whenever the name
			// is set and only falls back to the pixmap provider when it is
			// empty. IconThemePath stays empty for exactly the same reason.
			"IconName":          {Value: "", Emit: prop.EmitConst},
			"IconPixmap":        {Value: trayPixmapsFor(icon), Emit: prop.EmitTrue},
			"AttentionIconName": {Value: "", Emit: prop.EmitConst},
			"OverlayIconName":   {Value: "", Emit: prop.EmitConst},
			"IconThemePath":     {Value: "", Emit: prop.EmitConst},
			"ToolTip":           {Value: trayTooltipFor(conns), Emit: prop.EmitTrue},
		},
	})
	if err != nil {
		t.conn.ReleaseName(t.name)
		return fmt.Errorf("export item properties: %w", err)
	}
	t.props = props

	if _, err := prop.Export(t.conn, trayMenuPath, prop.Map{
		trayMenuIface: {
			"Version":       {Value: uint32(3), Emit: prop.EmitConst},
			"TextDirection": {Value: "ltr", Emit: prop.EmitConst},
			"Status":        {Value: "normal", Emit: prop.EmitTrue},
			"IconThemePath": {Value: []string{}, Emit: prop.EmitConst},
		},
	}); err != nil {
		t.conn.ReleaseName(t.name)
		return fmt.Errorf("export menu properties: %w", err)
	}

	if err := t.conn.Export(t, trayItemPath, trayItemIface); err != nil {
		t.conn.ReleaseName(t.name)
		return fmt.Errorf("export item: %w", err)
	}
	if err := t.conn.Export(t, trayMenuPath, trayMenuIface); err != nil {
		t.conn.ReleaseName(t.name)
		return fmt.Errorf("export menu: %w", err)
	}
	if err := t.conn.Export(introspect.NewIntrospectable(trayItemNode()), trayItemPath,
		"org.freedesktop.DBus.Introspectable"); err != nil {
		t.conn.ReleaseName(t.name)
		return fmt.Errorf("export item introspection: %w", err)
	}
	if err := t.conn.Export(introspect.NewIntrospectable(trayMenuNodeDesc()), trayMenuPath,
		"org.freedesktop.DBus.Introspectable"); err != nil {
		t.conn.ReleaseName(t.name)
		return fmt.Errorf("export menu introspection: %w", err)
	}

	t.mu.Lock()
	t.exported = true
	t.mu.Unlock()
	return nil
}

// register hands the bus name to the watcher. A missing watcher is logged once
// and otherwise ignored: the NameOwnerChanged match will catch it later.
func (t *tray) register() {
	t.mu.RLock()
	ok := t.exported
	warned := t.warnedNoWatcher
	t.mu.RUnlock()
	if !ok {
		return
	}
	base := t.ctx
	if base == nil {
		base = context.Background()
	}
	cctx, cancel := context.WithTimeout(base, trayCallTimeout)
	defer cancel()

	obj := t.conn.Object(trayWatcherName, trayWatcherPath)
	call := obj.CallWithContext(cctx, trayWatcherName+".RegisterStatusNotifierItem", 0, t.name)
	if call.Err != nil {
		if !warned {
			log.Printf("tray: no %s yet (%v); waiting for a tray host", trayWatcherName, call.Err)
			t.mu.Lock()
			t.warnedNoWatcher = true
			t.mu.Unlock()
		}
		return
	}
	log.Printf("tray: registered %s with %s", t.name, trayWatcherName)
}

// teardown unexports everything and drops the bus name, which is how D4's
// "hide the icon" is implemented: watchers see the name vanish and forget us.
func (t *tray) teardown() {
	t.mu.Lock()
	if !t.exported {
		t.mu.Unlock()
		return
	}
	t.exported = false
	t.props = nil
	t.mu.Unlock()

	for _, p := range []struct {
		path  dbus.ObjectPath
		iface string
	}{
		{trayItemPath, trayItemIface},
		{trayItemPath, "org.freedesktop.DBus.Properties"},
		{trayItemPath, "org.freedesktop.DBus.Introspectable"},
		{trayMenuPath, trayMenuIface},
		{trayMenuPath, "org.freedesktop.DBus.Properties"},
		{trayMenuPath, "org.freedesktop.DBus.Introspectable"},
	} {
		if err := t.conn.Export(nil, p.path, p.iface); err != nil {
			log.Printf("tray: unexport %s %s: %v", p.path, p.iface, err)
		}
	}
	if _, err := t.conn.ReleaseName(t.name); err != nil {
		log.Printf("tray: release %s: %v", t.name, err)
	}
	log.Printf("tray: TeamSpeak is down, tray icon withdrawn")
}

// --- tooltip and menu text -------------------------------------------------

func trayMicWord(c Conn) string {
	switch {
	case c.InputMuted:
		return "muted"
	case c.MicDisabled():
		return "disabled"
	}
	return "on"
}

func traySpeakerWord(c Conn) string {
	if c.OutputMuted {
		return "muted"
	}
	return "on"
}

// trayLine is the one-line summary of a connection used in both the tooltip
// and the menu's label rows.
//
// withTalking is true only for the tooltip. A menu label that changed on every
// talk start/stop would bump the dbusmenu revision and emit LayoutUpdated every
// few seconds, and some hosts close an open menu on that; the tooltip is not
// revision-gated, so "talking" lives there.
func trayLine(c Conn, withTalking bool) string {
	s := fmt.Sprintf("%s: mic %s, speaker %s", c.ServerName, trayMicWord(c), traySpeakerWord(c))
	if withTalking && c.Talking {
		s += ", talking"
	}
	return s
}

// trayActiveID is the id of the connection that holds the capture device, or 0.
func trayActiveID(conns []Conn) int {
	for _, c := range conns {
		if !c.MicDisabled() {
			return c.ID
		}
	}
	return 0
}

func trayEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

func trayTooltipFor(conns []Conn) trayTooltip {
	active := trayActiveID(conns)
	var b strings.Builder
	if len(conns) == 0 {
		b.WriteString("No server connected")
	}
	for i, c := range conns {
		if i > 0 {
			b.WriteString("\n")
		}
		if c.ID == active {
			b.WriteString("● ") // filled bullet marks the active server
		} else {
			b.WriteString("○ ")
		}
		b.WriteString(trayEscape(trayLine(c, true)))
	}
	// Freshly built on every call, and it must stay that way: prop stores the
	// ToolTip through the pointer it already holds, so a cached value here
	// would alias the exported property exactly as trayPixmapsFor once did.
	return trayTooltip{
		IconName:    "",
		IconPixmap:  []trayPixmap{},
		Title:       "TeamSpeak",
		Description: b.String(),
	}
}

// trayBindLabel is the label of a bind-helper row. While that target's
// countdown runs the row says so and is disabled, which is the whole feedback
// the user gets between the two notifications.
func trayBindLabel(what, binding, target string) (string, bool) {
	if binding == target {
		return "Pressing " + what + " key in 10 s…", false
	}
	return "Bind " + what + " key…", true
}

// trayRowsFor builds the whole menu: the root rows first, then the children of
// the Settings submenu. click is the left-click target and binding is the
// target of a running bind countdown ("" for none).
func trayRowsFor(conns []Conn, click, binding string, notif map[string]bool) []trayRow {
	active := trayActiveID(conns)
	var rows []trayRow
	if len(conns) == 0 {
		rows = append(rows, trayRow{id: trayIDServer0, label: "No server connected"})
	}
	for i, c := range conns {
		label := trayLine(c, false)
		if c.ID == active {
			label = "● " + label
		} else {
			label = "○ " + label
		}
		rows = append(rows, trayRow{id: trayIDServer0 + int32(i), label: label})
	}
	micLabel, micOn := trayBindLabel("microphone", binding, "mic")
	spkLabel, spkOn := trayBindLabel("speaker", binding, "speaker")
	rows = append(rows,
		trayRow{id: trayIDSep1, separator: true},
		trayRow{id: trayIDMic, label: "Toggle microphone mute", enabled: len(conns) > 0},
		trayRow{id: trayIDSpeaker, label: "Toggle speaker mute", enabled: len(conns) > 0},
		trayRow{id: trayIDSep2, separator: true},
		trayRow{id: trayIDSettings, label: "Settings", enabled: true, submenu: true},
		trayRow{id: trayIDQuit, label: "Quit", enabled: true},

		// Children of Settings. They live in the same flat slice; parent is
		// what puts them inside the submenu.
		trayRow{id: trayIDBindMic, parent: trayIDSettings, label: micLabel, enabled: micOn},
		trayRow{id: trayIDBindSpeaker, parent: trayIDSettings, label: spkLabel, enabled: spkOn},
		trayRow{id: trayIDSep3, parent: trayIDSettings, separator: true},
		trayRow{id: trayIDClickMic, parent: trayIDSettings, label: "Left-click toggles microphone",
			enabled: true, radio: true, checked: click != "speaker"},
		trayRow{id: trayIDClickSpeaker, parent: trayIDSettings, label: "Left-click toggles speaker",
			enabled: true, radio: true, checked: click == "speaker"},
		trayRow{id: trayIDSep4, parent: trayIDSettings, separator: true},
		trayRow{id: trayIDNotify, parent: trayIDSettings, label: "Notifications", enabled: true, submenu: true},
	)
	// Children of Notifications: one checkmark per switch, in the declared
	// order, so the id is the index and nothing has to be looked up by label.
	for i, g := range trayNotifyGroups {
		on := g.def
		if v, ok := notif[g.key]; ok {
			on = v
		}
		rows = append(rows, trayRow{id: trayIDNotify0 + int32(i), parent: trayIDNotify,
			label: g.label, enabled: true, check: true, checked: on})
	}
	// …then, below a separator, how the ones that are on get delivered.
	rows = append(rows, trayRow{id: trayIDSep5, parent: trayIDNotify, separator: true})
	for i, o := range trayNotifyOptions {
		on := o.def
		if v, ok := notif[o.key]; ok {
			on = v
		}
		rows = append(rows, trayRow{id: trayIDNotifyOpt0 + int32(i), parent: trayIDNotify,
			label: o.label, enabled: true, check: true, checked: on})
	}
	return rows
}

func trayRowsEqual(a, b []trayRow) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- org.kde.StatusNotifierItem methods ------------------------------------

// Activate is the left click (ItemIsMenu is false, so hosts send this). What it
// toggles is the user's choice, from the Settings submenu.
func (t *tray) Activate(x, y int32) *dbus.Error {
	go t.toggle(t.clickTarget())
	return nil
}

// SecondaryActivate is the middle click: whichever target left-click did not
// take, so both are always one click away.
func (t *tray) SecondaryActivate(x, y int32) *dbus.Error {
	go t.toggle(trayOther(t.clickTarget()))
	return nil
}

// ContextMenu exists only so hosts that insist on calling it get a reply; the
// actual menu is the dbusmenu object named by the Menu property.
func (t *tray) ContextMenu(x, y int32) *dbus.Error { return nil }

// Scroll is accepted and ignored.
func (t *tray) Scroll(delta int32, orientation string) *dbus.Error { return nil }

func (t *tray) toggle(target string) {
	if _, err := t.muteFunc()(target, "toggle"); err != nil {
		if errors.Is(err, ErrNotBound) {
			t.notifyNotBound(err)
			return
		}
		log.Printf("tray: %s toggle: %v", target, err)
	}
}

// notifyNotBound tells the user how to bind the key, at most once per 30 s.
func (t *tray) notifyNotBound(err error) {
	t.mu.Lock()
	if time.Since(t.lastNotify) < trayNotifyEvery {
		t.mu.Unlock()
		return
	}
	t.lastNotify = time.Now()
	t.mu.Unlock()

	t.notify("TeamSpeak hotkey not bound", err.Error())
}

// notify sends one standalone desktop notification: the bind helper and the
// "key not bound" warning. Those never replace anything, because they are
// answers to something the user just clicked. It is not rate-limited; only
// notifyNotBound is, because that one can fire on every stray click.
func (t *tray) notify(title, body string) { t.sendNotify(0, title, body, t.iconFor(IconNone)) }

// iconFor resolves a notice's icon to a file path: the rendered state PNG when
// the notice names one and it was written, otherwise our own app icon. "" means
// neither exists, and sendNotify falls back to a theme name.
func (t *tray) iconFor(ic Icon) string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if p := t.stateIcons[ic]; p != "" {
		return p
	}
	return t.iconPath
}

// sendNotify performs the Notify call and returns the id the server assigned.
// replaces is the id of the notification to take the place of, or 0 for a new
// one. Replacing is part of the freedesktop spec, so every server honours it.
// icon is the path the server should draw: a per-state PNG for a mute notice,
// our app SVG for everything else.
func (t *tray) sendNotify(replaces uint32, title, body, icon string) uint32 {
	if t.notifyFn != nil {
		return t.notifyFn(replaces, title, body, icon)
	}
	if t.conn == nil {
		log.Printf("tray: notify (no bus): %s: %s", title, body)
		return 0
	}
	hints := map[string]dbus.Variant{"urgency": dbus.MakeVariant(byte(1))}
	if icon == "" {
		// The icon file could not be written; a theme name still shows
		// something rather than a blank square.
		icon = "audio-input-microphone"
	} else {
		// Some servers read app_icon, others only the image-path hint.
		hints["image-path"] = dbus.MakeVariant(icon)
	}
	obj := t.conn.Object("org.freedesktop.Notifications", "/org/freedesktop/Notifications")
	call := obj.Call("org.freedesktop.Notifications.Notify", 0,
		"ts6tray", replaces, icon,
		title, body,
		[]string{}, hints, int32(15000))
	if call.Err != nil {
		log.Printf("tray: notify: %v (message: %s: %s)", call.Err, title, body)
		return 0
	}
	var id uint32
	if err := call.Store(&id); err != nil {
		log.Printf("tray: notify: reading the id back: %v", err)
	}
	return id
}

// notifyEvent sends one event notification, or the batch that stands for
// several. With "Replace previous notification" on it takes the place of the
// last one, so only the newest ts6tray notification is ever on screen.
// ic is the notice's state icon, or the batch's newest notice's.
func (t *tray) notifyEvent(title, body string, ic Icon) {
	var replaces uint32
	t.mu.RLock()
	if t.notifyOptOnLocked(trayOptReplace) {
		replaces = t.lastEventID
	}
	t.mu.RUnlock()

	id := t.sendNotify(replaces, title, body, t.iconFor(ic))

	t.mu.Lock()
	t.lastEventID = id
	t.mu.Unlock()
}

// --- the bind helper --------------------------------------------------------

// bindKey is the Settings submenu's "Bind … key…" item: tell the user to switch
// to TeamSpeak and open the hotkey assignment, wait, then press the button once
// so TeamSpeak records it as the key for that action.
//
// Only one countdown runs at a time; a second click while one is pending is
// dropped, because two presses would land in whichever assignment dialog is
// open and bind the wrong action.
func (t *tray) bindKey(target string) {
	button, what, said := ButtonMic, "microphone", "toggle microphone mute"
	if target == "speaker" {
		button, what, said = ButtonSpeaker, "speaker", "toggle speaker mute"
	}

	t.mu.Lock()
	if t.binding != "" {
		pending := t.binding
		t.mu.Unlock()
		log.Printf("tray: bind %s ignored, the %s countdown is still running", target, pending)
		return
	}
	t.binding = target
	delay := t.bindDelay
	t.mu.Unlock()
	if delay <= 0 {
		delay = trayBindDelay
	}
	defer func() {
		t.mu.Lock()
		t.binding = ""
		t.mu.Unlock()
		t.refresh()
	}()
	t.refresh()

	t.notify("ts6tray key binding", fmt.Sprintf(
		"Switch to TeamSpeak now and start the hotkey assignment for '%s' — ts6tray presses the key in %d seconds.",
		said, int(delay/time.Second)))

	done := t.ctx
	if done == nil {
		done = context.Background()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-done.Done():
		log.Printf("tray: bind %s cancelled, shutting down", target)
		return
	case <-timer.C:
	}

	if err := t.pressFunc()(button); err != nil {
		log.Printf("tray: bind %s: %v", target, err)
		t.notify("ts6tray key binding failed", err.Error())
		return
	}
	t.notify("ts6tray key binding", strings.ToUpper(what[:1])+what[1:]+
		" key pressed — TeamSpeak should have recorded it.")
}

// --- com.canonical.dbusmenu methods ----------------------------------------

func (t *tray) layout() (uint32, []trayRow) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.revision, t.rows
}

func (t *tray) rowByID(id int32) (trayRow, bool) {
	_, rows := t.layout()
	for _, r := range rows {
		if r.id == id {
			return r, true
		}
	}
	return trayRow{}, false
}

func trayFilter(p map[string]dbus.Variant, names []string) map[string]dbus.Variant {
	if len(names) == 0 {
		return p
	}
	out := make(map[string]dbus.Variant, len(names))
	for _, n := range names {
		if v, ok := p[n]; ok {
			out[n] = v
		}
	}
	return out
}

// trayNodeFor builds one node and, while depth allows, its subtree. depth is
// the dbusmenu recursionDepth: 0 stops here and a negative value never does.
func trayNodeFor(rows []trayRow, r trayRow, depth int32, propertyNames []string) trayMenuNode {
	n := trayMenuNode{ID: r.id, Props: trayFilter(r.props(), propertyNames), Children: []dbus.Variant{}}
	if r.submenu && depth != 0 {
		for _, c := range rows {
			if c.parent == r.id {
				n.Children = append(n.Children, dbus.MakeVariant(trayNodeFor(rows, c, depth-1, propertyNames)))
			}
		}
	}
	return n
}

// GetLayout implements com.canonical.dbusmenu.GetLayout.
func (t *tray) GetLayout(parentID, recursionDepth int32, propertyNames []string) (uint32, trayMenuNode, *dbus.Error) {
	rev, rows := t.layout()

	if parentID != trayIDRoot {
		for _, r := range rows {
			if r.id == parentID {
				return rev, trayNodeFor(rows, r, recursionDepth, propertyNames), nil
			}
		}
		return rev, trayMenuNode{}, dbus.NewError("com.canonical.dbusmenu.Error.InvalidId", []any{"no such item"})
	}

	root := trayMenuNode{
		ID: trayIDRoot,
		Props: trayFilter(map[string]dbus.Variant{
			"children-display": dbus.MakeVariant("submenu"),
		}, propertyNames),
		Children: []dbus.Variant{},
	}
	if recursionDepth != 0 {
		for _, r := range rows {
			if r.parent == trayIDRoot {
				root.Children = append(root.Children,
					dbus.MakeVariant(trayNodeFor(rows, r, recursionDepth-1, propertyNames)))
			}
		}
	}
	return rev, root, nil
}

// GetGroupProperties implements com.canonical.dbusmenu.GetGroupProperties. An
// empty id list means every item.
func (t *tray) GetGroupProperties(ids []int32, propertyNames []string) ([]trayMenuProps, *dbus.Error) {
	_, rows := t.layout()
	out := []trayMenuProps{}
	want := func(id int32) bool {
		if len(ids) == 0 {
			return true
		}
		for _, i := range ids {
			if i == id {
				return true
			}
		}
		return false
	}
	if want(trayIDRoot) {
		out = append(out, trayMenuProps{ID: trayIDRoot, Props: trayFilter(
			map[string]dbus.Variant{"children-display": dbus.MakeVariant("submenu")}, propertyNames)})
	}
	for _, r := range rows {
		if want(r.id) {
			out = append(out, trayMenuProps{ID: r.id, Props: trayFilter(r.props(), propertyNames)})
		}
	}
	return out, nil
}

// GetProperty implements com.canonical.dbusmenu.GetProperty.
func (t *tray) GetProperty(id int32, name string) (dbus.Variant, *dbus.Error) {
	var props map[string]dbus.Variant
	if id == trayIDRoot {
		props = map[string]dbus.Variant{"children-display": dbus.MakeVariant("submenu")}
	} else {
		r, ok := t.rowByID(id)
		if !ok {
			return dbus.Variant{}, dbus.NewError("com.canonical.dbusmenu.Error.InvalidId", []any{"no such item"})
		}
		props = r.props()
	}
	v, ok := props[name]
	if !ok {
		return dbus.Variant{}, dbus.NewError("com.canonical.dbusmenu.Error.InvalidProperty", []any{name})
	}
	return v, nil
}

// Event implements com.canonical.dbusmenu.Event.
func (t *tray) Event(id int32, eventID string, data dbus.Variant, timestamp uint32) *dbus.Error {
	if eventID != "clicked" {
		return nil
	}
	switch id {
	case trayIDMic:
		go t.toggle("mic")
	case trayIDSpeaker:
		go t.toggle("speaker")
	case trayIDBindMic:
		go t.bindKey("mic")
	case trayIDBindSpeaker:
		go t.bindKey("speaker")
	case trayIDClickMic:
		go t.setClick("mic")
	case trayIDClickSpeaker:
		go t.setClick("speaker")
	case trayIDQuit:
		go t.quit()
	default:
		if i := int(id - trayIDNotify0); id >= trayIDNotify0 && i < len(trayNotifyGroups) {
			go t.toggleNotify(trayNotifyGroups[i].key)
		}
		if i := int(id - trayIDNotifyOpt0); id >= trayIDNotifyOpt0 && i < len(trayNotifyOptions) {
			go t.toggleNotify(trayNotifyOptions[i].key)
		}
	}
	return nil
}

// EventGroup implements com.canonical.dbusmenu.EventGroup. It returns the ids
// it did not recognise, as the spec requires.
func (t *tray) EventGroup(events []trayMenuEvent) ([]int32, *dbus.Error) {
	bad := []int32{}
	for _, e := range events {
		if _, ok := t.rowByID(e.ID); !ok && e.ID != trayIDRoot {
			bad = append(bad, e.ID)
			continue
		}
		t.Event(e.ID, e.EventID, e.Data, e.Timestamp)
	}
	return bad, nil
}

// AboutToShow implements com.canonical.dbusmenu.AboutToShow. The layout is
// always current, so nothing needs refreshing before the menu pops up.
func (t *tray) AboutToShow(id int32) (bool, *dbus.Error) { return false, nil }

// AboutToShowGroup implements com.canonical.dbusmenu.AboutToShowGroup.
func (t *tray) AboutToShowGroup(ids []int32) ([]int32, []int32, *dbus.Error) {
	return []int32{}, []int32{}, nil
}

// --- introspection ---------------------------------------------------------

func trayItemNode() *introspect.Node {
	return &introspect.Node{
		Name: string(trayItemPath),
		Interfaces: []introspect.Interface{
			introspect.IntrospectData,
			prop.IntrospectData,
			{
				Name: trayItemIface,
				Methods: []introspect.Method{
					{Name: "Activate", Args: []introspect.Arg{{Name: "x", Type: "i", Direction: "in"}, {Name: "y", Type: "i", Direction: "in"}}},
					{Name: "SecondaryActivate", Args: []introspect.Arg{{Name: "x", Type: "i", Direction: "in"}, {Name: "y", Type: "i", Direction: "in"}}},
					{Name: "ContextMenu", Args: []introspect.Arg{{Name: "x", Type: "i", Direction: "in"}, {Name: "y", Type: "i", Direction: "in"}}},
					{Name: "Scroll", Args: []introspect.Arg{{Name: "delta", Type: "i", Direction: "in"}, {Name: "orientation", Type: "s", Direction: "in"}}},
				},
				Signals: []introspect.Signal{
					{Name: "NewIcon"},
					{Name: "NewAttentionIcon"},
					{Name: "NewOverlayIcon"},
					{Name: "NewToolTip"},
					{Name: "NewTitle"},
					{Name: "NewStatus", Args: []introspect.Arg{{Name: "status", Type: "s"}}},
				},
				Properties: []introspect.Property{
					{Name: "Category", Type: "s", Access: "read"},
					{Name: "Id", Type: "s", Access: "read"},
					{Name: "Title", Type: "s", Access: "read"},
					{Name: "Status", Type: "s", Access: "read"},
					{Name: "WindowId", Type: "i", Access: "read"},
					{Name: "ItemIsMenu", Type: "b", Access: "read"},
					{Name: "Menu", Type: "o", Access: "read"},
					{Name: "IconName", Type: "s", Access: "read"},
					{Name: "IconPixmap", Type: "a(iiay)", Access: "read"},
					{Name: "AttentionIconName", Type: "s", Access: "read"},
					{Name: "OverlayIconName", Type: "s", Access: "read"},
					{Name: "IconThemePath", Type: "s", Access: "read"},
					{Name: "ToolTip", Type: "(sa(iiay)ss)", Access: "read"},
				},
			},
		},
	}
}

func trayMenuNodeDesc() *introspect.Node {
	return &introspect.Node{
		Name: string(trayMenuPath),
		Interfaces: []introspect.Interface{
			introspect.IntrospectData,
			prop.IntrospectData,
			{
				Name: trayMenuIface,
				Methods: []introspect.Method{
					{Name: "GetLayout", Args: []introspect.Arg{
						{Name: "parentId", Type: "i", Direction: "in"},
						{Name: "recursionDepth", Type: "i", Direction: "in"},
						{Name: "propertyNames", Type: "as", Direction: "in"},
						{Name: "revision", Type: "u", Direction: "out"},
						{Name: "layout", Type: "(ia{sv}av)", Direction: "out"},
					}},
					{Name: "GetGroupProperties", Args: []introspect.Arg{
						{Name: "ids", Type: "ai", Direction: "in"},
						{Name: "propertyNames", Type: "as", Direction: "in"},
						{Name: "properties", Type: "a(ia{sv})", Direction: "out"},
					}},
					{Name: "GetProperty", Args: []introspect.Arg{
						{Name: "id", Type: "i", Direction: "in"},
						{Name: "name", Type: "s", Direction: "in"},
						{Name: "value", Type: "v", Direction: "out"},
					}},
					{Name: "Event", Args: []introspect.Arg{
						{Name: "id", Type: "i", Direction: "in"},
						{Name: "eventId", Type: "s", Direction: "in"},
						{Name: "data", Type: "v", Direction: "in"},
						{Name: "timestamp", Type: "u", Direction: "in"},
					}},
					{Name: "EventGroup", Args: []introspect.Arg{
						{Name: "events", Type: "a(isvu)", Direction: "in"},
						{Name: "idErrors", Type: "ai", Direction: "out"},
					}},
					{Name: "AboutToShow", Args: []introspect.Arg{
						{Name: "id", Type: "i", Direction: "in"},
						{Name: "needUpdate", Type: "b", Direction: "out"},
					}},
					{Name: "AboutToShowGroup", Args: []introspect.Arg{
						{Name: "ids", Type: "ai", Direction: "in"},
						{Name: "updatesNeeded", Type: "ai", Direction: "out"},
						{Name: "idErrors", Type: "ai", Direction: "out"},
					}},
				},
				Signals: []introspect.Signal{
					{Name: "ItemsPropertiesUpdated", Args: []introspect.Arg{
						{Name: "updatedProps", Type: "a(ia{sv})"},
						{Name: "removedProps", Type: "a(ias)"},
					}},
					{Name: "LayoutUpdated", Args: []introspect.Arg{
						{Name: "revision", Type: "u"},
						{Name: "parent", Type: "i"},
					}},
					{Name: "ItemActivationRequested", Args: []introspect.Arg{
						{Name: "id", Type: "i"},
						{Name: "timestamp", Type: "u"},
					}},
				},
				Properties: []introspect.Property{
					{Name: "Version", Type: "u", Access: "read"},
					{Name: "TextDirection", Type: "s", Access: "read"},
					{Name: "Status", Type: "s", Access: "read"},
					{Name: "IconThemePath", Type: "as", Access: "read"},
				},
			},
		},
	}
}
