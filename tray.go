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
	"embed"
	"errors"
	"fmt"
	"image/color"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

// --- the notification icons ------------------------------------------------

// trayNotifySVGs is the notification artwork: one flat SVG per notifyIcon,
// each built on the app icon's blue ring and navy disc so a notification is
// recognisably ours. They are SVGs and not rendered PNGs because a notification
// server scales the file it is handed, and a raster at one size looks soft at
// every other one.
//
//go:embed assets/notify/*.svg
var trayNotifySVGs embed.FS

// trayNotifyIconNames is every icon that has a file, in a fixed order so the
// error a caller sees does not depend on map iteration.
var trayNotifyIconNames = []notifyIcon{
	iconMicMuted, iconSpeakerMuted, iconUnmuted, iconJoin, iconLeave,
}

// trayNotifyIconFiles unpacks the artwork into $XDG_CACHE_HOME/ts6tray/notify/
// and returns notifyIcon -> path. Like trayIconFile it rewrites a file only
// when the bytes differ, so a restart does not churn the cache directory and a
// server watching those paths sees nothing move.
func trayNotifyIconFiles() (map[notifyIcon]string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(base, "ts6tray", "notify")
	out := make(map[notifyIcon]string, len(trayNotifyIconNames))
	made := false
	for _, ic := range trayNotifyIconNames {
		data, rerr := trayNotifySVGs.ReadFile("assets/notify/" + string(ic) + ".svg")
		if rerr != nil {
			return out, rerr
		}
		path := filepath.Join(dir, string(ic)+".svg")
		if old, oerr := os.ReadFile(path); oerr == nil && bytes.Equal(old, data) {
			out[ic] = path
			continue
		}
		if !made {
			if merr := os.MkdirAll(dir, 0o700); merr != nil {
				return out, merr
			}
			made = true
		}
		if werr := os.WriteFile(path, data, 0o644); werr != nil {
			return out, werr
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
	// trayWhite is the glyph colour.
	trayWhite = color.RGBA{0xff, 0xff, 0xff, 0xff}
	// trayRed is the mute slash, #e5322d — the exact red the mic-muted and
	// speaker-muted notification SVGs strike their glyphs through with, so the
	// tray and the notifications say "muted" in the same colour.
	trayRed = color.RGBA{0xe5, 0x32, 0x2d, 0xff}
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
// white microphone or speaker with a single red slash across it, separated
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
	// same dark rim as the glyph, for the same reason, and the same red as the
	// muted notification icons.
	cv.erase(traySegment(0.15, 0.13, 0.85, 0.87, 0.115, 0.048))
	cv.fill(traySegment(0.16, 0.14, 0.84, 0.86, 0.115, 0.018), trayOutline)
	cv.fill(traySegment(0.16, 0.14, 0.84, 0.86, 0.115, 0), trayRed)
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

// trayMenuEvent is one entry of EventGroup's a(isvu).
type trayMenuEvent struct {
	ID        int32
	EventID   string
	Data      dbus.Variant
	Timestamp uint32
}

// trayRow is one menu row before it becomes dbusmenu properties. The menu is
// flat — every row hangs off the menu bar — so the rows stay a comparable
// slice and trayRowsEqual can compare them with ==.
type trayRow struct {
	id        int32
	label     string
	separator bool
	enabled   bool
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

	// mute and snapshot default to the TSClient's methods; tests replace them
	// so nothing talks to a real TeamSpeak.
	mute     func(target, mode string) (bool, error)
	snapshot func() ([]Conn, Icon, bool)

	// notifyFn, when set, takes the place of the D-Bus Notify call. Tests use
	// it to capture what would have been sent, replaces_id and icon path
	// included, and to make a send fail.
	notifyFn func(replaces uint32, title, body, icon string) (uint32, error)

	// now is the clock, so a test can age lastEventAt past the replace window
	// without sleeping. nil means time.Now.
	now func() time.Time

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
	iconPath        string                // cached app icon, "" when it could not be written
	notifyIcons     map[notifyIcon]string // unpacked notification SVGs, by icon
	lastEventID     uint32                // id of the last event notification, for replaces_id
	lastEventAt     time.Time             // when the notification on screen was first shown, for the replace window
	click           string                // left-click target: "mic" or "speaker"
	notif           map[string]bool       // notification switches, by config key
	timeout         string                // notify.timeout: how long a notification stays up

	props *prop.Properties
}

func (t *tray) muteFunc() func(string, string) (bool, error) {
	if t.mute != nil {
		return t.mute
	}
	return t.ts.SetMute
}

func (t *tray) nowFn() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

func (t *tray) snapshotFunc() func() ([]Conn, Icon, bool) {
	if t.snapshot != nil {
		return t.snapshot
	}
	if t.ts == nil {
		return func() ([]Conn, Icon, bool) { return nil, IconNone, false }
	}
	return t.ts.Snapshot
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

// quietNow reports whether notifications should be held back because our own
// speakers are muted. "Any live connection" rather than the active one: the
// active connection is the one holding the *microphone*, which says nothing
// about what we can hear, and a user who muted their speakers anywhere has
// stepped away from all of it.
func (t *tray) quietNow() bool {
	if !t.notifyOptOn(trayOptQuietWhenDeaf) {
		return false
	}
	conns, _, up := t.snapshotFunc()()
	if !up {
		return false
	}
	for _, c := range conns {
		if c.OutputMuted {
			return true
		}
	}
	return false
}

// noticeIgnoresQuiet reports whether a kind is shown even while "Silence while
// my speakers are muted" is holding everything else back: the lost connection,
// and our own actions.
func noticeIgnoresQuiet(k noticeKind) bool {
	return k == noticeConnLost || k == noticeSelf
}

// reloadConfig re-reads the config file and republishes the menu, so the
// checkmarks follow a change made by `ts6tray settings` in another process.
// It is what the IPC "reload" request calls.
func (t *tray) reloadConfig() error {
	path := trayConfigPath()
	t.cfgMu.Lock()
	click, notif, timeout := trayReadClick(path), trayReadNotify(path), trayReadTimeout(path)
	t.cfgMu.Unlock()

	t.mu.Lock()
	t.click, t.notif, t.timeout = click, notif, timeout
	t.mu.Unlock()

	// Switching batching off must not strand whatever is already queued.
	if !notif[trayOptBatch] && t.batch != nil {
		t.batch.flush()
	}
	// Rebuild and republish the menu. No setting appears in it any more, so
	// this changes nothing today and emits nothing — refresh only signals when
	// the rows really moved — but it keeps the menu the config's dependent
	// rather than something that has to be remembered separately.
	t.refresh()
	return nil
}

// trayReload is the handle the IPC server holds on a tray that does not exist
// yet: main.go starts ServeIPC before RunTray, and RunTray fills it in once the
// tray is up. Until then — and forever, when there is no session bus — Reload
// says so rather than pretending it worked.
type trayReload struct{ fn atomic.Pointer[func() error] }

func (r *trayReload) set(fn func() error) { r.fn.Store(&fn) }

func (r *trayReload) Reload() error {
	if r == nil {
		return errNoTray
	}
	if fn := r.fn.Load(); fn != nil {
		return (*fn)()
	}
	return errNoTray
}

// errNoTray is what a reload reports when the daemon is running without a tray
// icon. The settings themselves are already on disk; only the menu is missing.
var errNoTray = errors.New("the daemon is running without a tray icon; the settings are saved and take effect on its next start")

// onNotice shows one notice from the TSClient, if its kind is switched on.
// With "Group bursts" on it goes into the batcher instead and is shown, with
// whatever else arrives in the next half second, as a single notification.
func (t *tray) onNotice(n notice) {
	if !t.notifyEnabled(n.kind) {
		return
	}
	// "Silence while my speakers are muted": dropped outright rather than
	// queued, so unmuting does not then flush a pile of stale news. Two kinds
	// still get through. Losing the connection, because the whole point of that
	// notice is that nothing else will be arriving; and anything about
	// ourselves, because muting the speakers silences *other people's* news, not
	// the confirmation of what we just did — which is exactly the moment we
	// mute them.
	if !noticeIgnoresQuiet(n.kind) && t.quietNow() {
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
	rows := trayRowsFor(t.conns)
	changed := !trayRowsEqual(t.rows, rows)
	if changed {
		t.rows = rows
		t.revision++
	}
	rev, exported := t.revision, t.exported
	t.mu.Unlock()
	if changed && exported {
		t.emitMenuChange(rev)
	}
}

// emitMenuChange tells the host the menu moved. Everything this menu can change
// is structural — a server row appearing or its label changing, a toggle
// enabling — and a host learns about that only by fetching the layout again, so
// LayoutUpdated with the bumped revision is the whole story.
func (t *tray) emitMenuChange(rev uint32) {
	t.emit(trayMenuPath, trayMenuIface+".LayoutUpdated", rev, trayIDRoot)
}

// RunTray runs the StatusNotifierItem until ctx is done. It never returns an
// error for a missing or restarting tray host; only a broken session bus or a
// failed export is fatal.
// rl, when not nil, is handed the tray's config reload so the IPC server —
// which was already listening before this function ran — can call it.
func RunTray(ctx context.Context, c *TSClient, quit func(), rl *trayReload) error {
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
		ctx:     ctx,
		conn:    conn,
		ts:      c,
		quit:    quit,
		name:    fmt.Sprintf("org.kde.StatusNotifierItem-%d-1", os.Getpid()),
		icon:    IconNone,
		click:   trayReadClick(trayConfigPath()),
		notif:   trayReadNotify(trayConfigPath()),
		timeout: trayReadTimeout(trayConfigPath()),
	}
	if path, ierr := trayIconFile(); ierr != nil {
		log.Printf("tray: writing the app icon: %v", ierr)
	} else {
		t.iconPath = path
	}
	// A notification wants a picture of what happened — the mentioned user's
	// resulting mute state, or an arrival or a departure — so the artwork is
	// unpacked once here. A failure is not fatal: every notification simply
	// keeps the app icon.
	icons, ierr := trayNotifyIconFiles()
	if ierr != nil {
		log.Printf("tray: writing the notification icons: %v", ierr)
	}
	t.notifyIcons = icons // whatever got written before the error still counts
	t.batch = &noticeBatcher{
		window: noticeBatchWindow,
		cap:    noticeBatchCap,
		send:   t.notifyEvent,
	}
	if quit == nil {
		t.quit = func() {}
	}
	if rl != nil {
		rl.set(t.reloadConfig)
	}

	// Watch for the tray host coming and going, so a shell restart re-registers,
	// and for our own notifications being closed, so we stop replacing them.
	sigs := make(chan *dbus.Signal, 8)
	conn.Signal(sigs)
	defer conn.RemoveSignal(sigs)
	if err := conn.AddMatchSignal(
		dbus.WithMatchSender("org.freedesktop.DBus"),
		dbus.WithMatchObjectPath("/org/freedesktop/DBus"),
		dbus.WithMatchInterface("org.freedesktop.DBus"),
		dbus.WithMatchMember("NameOwnerChanged"),
		dbus.WithMatchArg(0, trayWatcherName),
	); err != nil {
		log.Printf("tray: cannot watch %s: %v", trayWatcherName, err)
	}
	if err := conn.AddMatchSignal(
		dbus.WithMatchInterface("org.freedesktop.Notifications"),
		dbus.WithMatchMember("NotificationClosed"),
	); err != nil {
		log.Printf("tray: cannot watch NotificationClosed: %v", err)
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
		case sig := <-sigs:
			if sig == nil {
				continue
			}
			if sig.Name == "org.freedesktop.Notifications.NotificationClosed" && len(sig.Body) > 0 {
				if id, ok := sig.Body[0].(uint32); ok {
					t.onNotificationClosed(id)
				}
				continue
			}
			if sig.Name != "org.freedesktop.DBus.NameOwnerChanged" || len(sig.Body) < 3 {
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
	rows := trayRowsFor(conns)
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
		t.emitMenuChange(rev)
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

// trayRowsFor builds the whole menu: the per-server rows, the two toggles and
// Quit. Everything else the user can change lives in `ts6tray settings`.
func trayRowsFor(conns []Conn) []trayRow {
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
	return append(rows,
		trayRow{id: trayIDSep1, separator: true},
		trayRow{id: trayIDMic, label: "Toggle microphone mute", enabled: len(conns) > 0},
		trayRow{id: trayIDSpeaker, label: "Toggle speaker mute", enabled: len(conns) > 0},
		trayRow{id: trayIDSep2, separator: true},
		trayRow{id: trayIDQuit, label: "Quit", enabled: true},
	)
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
// toggles is the user's choice, from `ts6tray settings`.
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
func (t *tray) notify(title, body string) { t.sendNotify(0, title, body, t.iconFor(iconApp)) }

// notifyTimeout is the configured display time, as Notify's expire_timeout in
// milliseconds.
func (t *tray) notifyTimeout() int32 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return trayTimeoutMillis(t.timeout)
}

// iconFor resolves a notice's icon to a file path: the unpacked notification
// SVG when the notice names one and it was written, otherwise our own app icon.
// "" means neither exists, and sendNotify falls back to a theme name.
func (t *tray) iconFor(ic notifyIcon) string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if p := t.notifyIcons[ic]; p != "" {
		return p
	}
	return t.iconPath
}

// trayAppName is the app_name every Notify call carries. The user wants the
// notifications to read as TeamSpeak's, not as some helper's: ts6tray is the
// only thing sending them and saying so twice helps nobody.
const trayAppName = "TeamSpeak"

// sendNotify performs the Notify call and returns the id the server assigned.
// replaces is the id of the notification to take the place of, or 0 for a new
// one. Replacing is part of the freedesktop spec, so every server honours it.
// icon is the path the server should draw: a notification SVG when the notice
// has one, our app icon for everything else.
func (t *tray) sendNotify(replaces uint32, title, body, icon string) (uint32, error) {
	if t.notifyFn != nil {
		return t.notifyFn(replaces, title, body, icon)
	}
	if t.conn == nil {
		log.Printf("tray: notify (no bus): %s: %s", title, body)
		return 0, nil
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
		trayAppName, replaces, icon,
		title, body,
		[]string{}, hints, t.notifyTimeout())
	if call.Err != nil {
		log.Printf("tray: notify: %v (message: %s: %s)", call.Err, title, body)
		return 0, call.Err
	}
	var id uint32
	if err := call.Store(&id); err != nil {
		log.Printf("tray: notify: reading the id back: %v", err)
	}
	return id, nil
}

// trayDefaultExpiry is how long a notification is assumed to stay on screen
// when we let the server decide (expire_timeout -1). It is only used to size
// the replace window below; 5 s is what the common servers use.
const trayDefaultExpiry = 5 * time.Second

// trayReplaceWindow is how long after a notification was first shown it is
// still safe to replace it. 0 means "no limit".
//
// Replacing a notification the server has already taken off screen is the bug
// this exists for: Quickshell/DMS applies such a Notify in place, silently
// editing an invisible row in its notification centre, and emits no
// NotificationClosed at expiry to tell us. So once the notification can no
// longer be on screen we stop claiming to replace it and send a fresh one.
// A replace does not extend the server's expiry, so the window runs from when
// the notification was first shown, not from the last send.
// "never" is the exception: that notification really does stay up.
func trayReplaceWindow(timeout string) time.Duration {
	switch timeout {
	case "never":
		return 0
	case "default":
		return trayDefaultExpiry
	}
	ms := trayTimeoutMillis(timeout)
	if ms <= 0 {
		return trayDefaultExpiry
	}
	return time.Duration(ms) * time.Millisecond
}

// onNotificationClosed forgets lastEventID when the server says that
// notification is gone, so the next event opens a fresh one instead of editing
// something nobody can see.
func (t *tray) onNotificationClosed(id uint32) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if id != 0 && id == t.lastEventID {
		t.lastEventID = 0
	}
}

// notifyEvent sends one event notification, or the batch that stands for
// several. With "Replace previous notification" on it takes the place of the
// last one — while that one can still be on screen — so only the newest
// ts6tray notification is ever up. ic is the notice's icon, or the batch's
// newest notice's.
func (t *tray) notifyEvent(title, body string, ic notifyIcon) {
	var replaces uint32
	t.mu.RLock()
	if t.lastEventID != 0 && t.notifyOptOnLocked(trayOptReplace) {
		if w := trayReplaceWindow(t.timeout); w == 0 || t.nowFn().Sub(t.lastEventAt) < w {
			replaces = t.lastEventID
		}
	}
	t.mu.RUnlock()

	id, err := t.sendNotify(replaces, title, body, t.iconFor(ic))
	if err != nil && replaces != 0 {
		// The id we tried to replace may be the reason it failed. Drop it and
		// try once more as a new notification, so the news still gets through.
		replaces = 0
		id, err = t.sendNotify(0, title, body, t.iconFor(ic))
	}
	if err != nil {
		id = 0
	}

	t.mu.Lock()
	t.lastEventID = id
	if replaces == 0 {
		// A fresh notification: the popup is on screen from now. A replace
		// rides on the one already up and does not restart its expiry, so the
		// window has to keep running from that first show.
		t.lastEventAt = t.nowFn()
	}
	t.mu.Unlock()
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

// trayNodeFor builds one node. The menu is flat, so no row has children and
// the dbusmenu recursionDepth never matters below the root.
func trayNodeFor(r trayRow, propertyNames []string) trayMenuNode {
	return trayMenuNode{ID: r.id, Props: trayFilter(r.props(), propertyNames), Children: []dbus.Variant{}}
}

// GetLayout implements com.canonical.dbusmenu.GetLayout.
func (t *tray) GetLayout(parentID, recursionDepth int32, propertyNames []string) (uint32, trayMenuNode, *dbus.Error) {
	rev, rows := t.layout()

	if parentID != trayIDRoot {
		for _, r := range rows {
			if r.id == parentID {
				return rev, trayNodeFor(r, propertyNames), nil
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
			root.Children = append(root.Children, dbus.MakeVariant(trayNodeFor(r, propertyNames)))
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
	case trayIDQuit:
		go t.quit()
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
