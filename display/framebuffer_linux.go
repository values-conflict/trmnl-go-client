//go:build linux

// Direct framebuffer display backend for Linux.
//
// Bypasses X11/Wayland/window managers entirely: it memory-maps a Linux
// framebuffer device (/dev/fbN) and writes pixels straight to it, making
// this suitable for headless or kiosk use (e.g. a Raspberry Pi running as
// the console).
//
// Input handling (keyboard shortcuts) is NOT implemented here. In this
// backend the display is driven out-of-band via signals:
//
//	SIGHUP / SIGUSR1 -> manual refresh (same as Ctrl+R in window mode)
//	SIGUSR2          -> cycle rotation (same as Ctrl+T in window mode)
//
// e.g. kill -s USR1 $(pgrep trmnl-go)
//
// See tianon-framebuffer-input.md for the planned evdev-based input layer.
//
// Caveats:
//   - Writing to /dev/fb0 overwrites whatever the kernel console was
//     showing. This backend does NOT restore the console on exit; switch
//     VTs (Ctrl+Alt+F1..F6) or reboot to get a normal console back.
//   - Requires read+write access to the framebuffer device (root, or a
//     udev rule / membership in the "video" group).
//   - Only 16-bit and 32-bit framebuffer depths are supported (the depths
//     used by virtually all modern hardware, including Raspberry Pi).

package display

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	xdraw "golang.org/x/image/draw"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
	"golang.org/x/sys/unix"

	"github.com/semaja2/trmnl-go/config"
)

const (
	// statusStripHeight is the height of the status bar reserved along the
	// bottom of the framebuffer (mirrors the status label in window mode).
	// basicfont.Face7x13 is 13px tall, leaving a few px of padding.
	statusStripHeight = 18
)

// fbBitfield mirrors struct fb_bitfield from <linux/fb.h>
type fbBitfield struct {
	Offset   uint32
	Length   uint32
	MSBRight uint32
}

// fbVarScreenInfo mirrors struct fb_var_screeninfo from <linux/fb.h>
// (160 bytes, arch-independent).
type fbVarScreenInfo struct {
	XRes        uint32
	YRes        uint32
	XResVirtual uint32
	YResVirtual uint32
	XOffset     uint32
	YOffset     uint32
	Bpp         uint32
	Grayscale   uint32
	Red         fbBitfield
	Green       fbBitfield
	Blue        fbBitfield
	Trans       fbBitfield
	Nonstd      uint32
	Activate    uint32
	Height      uint32
	Width       uint32
	AccelFlags  uint32
	PixClock    uint32
	LeftMargin  uint32
	RightMargin uint32
	UpperMargin uint32
	LowerMargin uint32
	HSyncLen    uint32
	VSyncLen    uint32
	Sync        uint32
	VMode       uint32
	Rotate      uint32
	Colorspace  uint32
	Reserved    [4]uint32
}

// fbFixScreenInfo mirrors struct fb_fix_screeninfo from <linux/fb.h>
// (80 bytes on 64-bit, 66 bytes on 32-bit; Go's uintptr field and the
// implicit alignment padding before MmioStart reproduce the C layout).
type fbFixScreenInfo struct {
	ID           [16]byte
	SmemStart    uintptr
	SmemLen      uint32
	Type         uint32
	TypeAux      uint32
	Visual       uint32
	XPanStep     uint16
	YPanStep     uint16
	YWrapStep    uint16
	LineLength   uint32
	MmioStart    uintptr
	MmioLen      uint32
	Accel        uint32
	Capabilities uint16
	Reserved     [2]uint16
}

const (
	// FBIOGET_VSCREENINFO / FBIOGET_FSCREENINFO, from <linux/fb.h>.
	//
	// Note: the fbdev ioctls predate the _IOR/_IOWR convention and are
	// plain 16-bit command numbers -- the kernel's fb_ioctl dispatch
	// matches on the literal value. Do NOT pack the struct size into the
	// high bits (0xA04600 gets rejected with ENOTTY).
	fbiogetVScreenInfo = 0x4600
	fbiogetFScreenInfo = 0x4602
)

func init() {
	// These structs are passed to kernel ioctls; a layout mismatch would
	// corrupt memory, so fail loudly at startup rather than guessing.
	if s := unsafe.Sizeof(fbVarScreenInfo{}); s != 160 {
		panic(fmt.Sprintf("fbVarScreenInfo size mismatch: expected 160 bytes, got %d", s))
	}
	if s := unsafe.Sizeof(fbFixScreenInfo{}); s != 66 && s != 80 {
		panic(fmt.Sprintf("fbFixScreenInfo size mismatch: expected 66 (32-bit) or 80 (64-bit) bytes, got %d", s))
	}
}

// FramebufferWindow implements the DisplayWindow interface by writing
// directly to a Linux framebuffer device.
type FramebufferWindow struct {
	config  *config.Config
	verbose bool

	vinfo  fbVarScreenInfo
	fbFD   int
	mmap   []byte
	stride int // bytes per row, from fb_fix_screeninfo.line_length

	mu        sync.Mutex
	open      bool
	closed    chan struct{}
	closeOnce sync.Once

	lastImage image.Image // last decoded+transformed image, kept for status redraws
	status    string

	refreshCallback func()
	rotateCallback  func()
	// onClosed is accepted for interface compatibility but is never
	// invoked: in framebuffer mode there is no window-close event, and the
	// application (app.go) closes its own stopCh before calling Close().
	onClosed func()
}

// NewFramebufferWindow opens and memory-maps the configured framebuffer
// device. It does not start any event loop; call Show() to block.
func NewFramebufferWindow(cfg *config.Config, verbose bool) (*FramebufferWindow, error) {
	device := cfg.FramebufferDevice
	if device == "" {
		device = config.DefaultFramebufferDevice
	}

	fd, err := os.OpenFile(device, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open framebuffer device %s: %w (hint: needs root or 'video' group access)", device, err)
	}

	var vinfo fbVarScreenInfo
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, fd.Fd(), uintptr(fbiogetVScreenInfo), uintptr(unsafe.Pointer(&vinfo))); errno != 0 {
		_ = fd.Close()
		return nil, fmt.Errorf("failed to query framebuffer %s info (ioctl 0x%X): %w", device, uintptr(fbiogetVScreenInfo), errno)
	}

	var finfo fbFixScreenInfo
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, fd.Fd(), uintptr(fbiogetFScreenInfo), uintptr(unsafe.Pointer(&finfo))); errno != 0 {
		_ = fd.Close()
		return nil, fmt.Errorf("failed to query framebuffer %s fix info (ioctl 0x%X): %w", device, uintptr(fbiogetFScreenInfo), errno)
	}

	// Some drivers report line_length as 0; fall back to width * bytes-per-pixel.
	stride := int(finfo.LineLength)
	if stride <= 0 {
		stride = int(vinfo.XRes) * int(vinfo.Bpp) / 8
	}
	size := stride * int(vinfo.YRes)

	mmap, err := unix.Mmap(int(fd.Fd()), 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		_ = fd.Close()
		return nil, fmt.Errorf("failed to map framebuffer: %w", err)
	}

	if verbose {
		fmt.Printf("[FrameBuffer] %s: %dx%d @ %dbpp, visual %d, stride %d\n",
			device, vinfo.XRes, vinfo.YRes, vinfo.Bpp, finfo.Visual, stride)
	}

	return &FramebufferWindow{
		config:  cfg,
		verbose: verbose,
		vinfo:   vinfo,
		fbFD:    int(fd.Fd()),
		mmap:    mmap,
		stride:  stride,
		open:    true,
		closed:  make(chan struct{}),
	}, nil
}

// Show blocks until Close() is called. It also installs the signal handlers
// used to drive refresh/rotate in framebuffer mode (see package comment),
// and hides the console cursor when this process owns the console's TTY.
func (w *FramebufferWindow) Show() {
	w.acquireConsoleTTY()
	go w.watchVT()
	go w.watchInput()

	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGUSR1, syscall.SIGUSR2)

	if w.verbose {
		fmt.Println("[FrameBuffer] Signalling: SIGHUP/SIGUSR1 = refresh, SIGUSR2 = rotate")
	}

	go func() {
		for sig := range sigCh {
			// On a virtual console we only act on manual triggers while we're
			// actually the visible console: a refresh/rotate triggered from
			// elsewhere (ssh, systemd) would blit over whatever VT the user is
			// looking at. watchVT redraws when they switch back to us.
			if isVT && !consoleIsActive(consoleTTY) {
				if w.verbose {
					fmt.Printf("[FrameBuffer] %v received while not the active console; ignoring (will redraw on return)\n", sig)
				}
				continue
			}
			switch sig {
			case syscall.SIGHUP, syscall.SIGUSR1:
				if w.verbose {
					fmt.Printf("[FrameBuffer] Received %v, triggering manual refresh\n", sig)
				}
				if w.refreshCallback != nil {
					w.refreshCallback()
				}
			case syscall.SIGUSR2:
				if w.verbose {
					fmt.Println("[FrameBuffer] Received SIGUSR2, triggering rotate")
				}
				if w.rotateCallback != nil {
					w.rotateCallback()
				}
			}
		}
	}()

	<-w.closed
}

// UpdateImage decodes, transforms, and displays the given image.
func (w *FramebufferWindow) UpdateImage(imageData []byte) error {
	if w.verbose {
		fmt.Printf("[FrameBuffer] Decoding image (%d bytes)\n", len(imageData))
	}

	// Apply the same transformations as window mode (rotation, dark mode, e-paper).
	transformedData, err := applyImageTransformations(imageData, w.config.Rotation, w.config.DarkMode, w.config.EPaperMode)
	if err != nil {
		return err
	}

	img, _, err := image.Decode(bytes.NewReader(transformedData))
	if err != nil {
		return fmt.Errorf("failed to decode transformed image: %w", err)
	}

	w.mu.Lock()
	w.lastImage = img
	w.mu.Unlock()

	if err := w.redraw(); err != nil {
		return err
	}

	if w.verbose {
		effects := []string{}
		if w.config.Rotation != 0 {
			effects = append(effects, fmt.Sprintf("rotation: %d°", w.config.Rotation))
		}
		if w.config.DarkMode {
			effects = append(effects, "dark mode")
		}
		if w.config.EPaperMode {
			effects = append(effects, "e-paper")
		}
		if len(effects) > 0 {
			fmt.Printf("[FrameBuffer] Applied effects: %v\n", effects)
		}
		fmt.Printf("[FrameBuffer] Image updated: %dx%d\n", img.Bounds().Dx(), img.Bounds().Dy())
	}
	return nil
}

// UpdateStatus updates the status text drawn in the strip along the bottom
// of the framebuffer.
func (w *FramebufferWindow) UpdateStatus(status string) {
	w.mu.Lock()
	w.status = status
	w.mu.Unlock()

	// Best effort: ignore errors (e.g. if we're already closed).
	_ = w.redraw()
}

// SetOnClosed is accepted for interface compatibility; see onClosed field.
func (w *FramebufferWindow) SetOnClosed(callback func()) {
	w.onClosed = callback
}

// SetOnRefresh sets the callback for manual refresh (SIGHUP / SIGUSR1).
func (w *FramebufferWindow) SetOnRefresh(callback func()) {
	w.refreshCallback = callback
}

// SetOnRotate sets the callback for manual rotate (SIGUSR2).
func (w *FramebufferWindow) SetOnRotate(callback func()) {
	w.rotateCallback = callback
}

// Close clears the screen (if we're the one on it), unmaps the
// framebuffer, and unblocks Show().
func (w *FramebufferWindow) Close() {
	w.closeOnce.Do(func() {
		// If our VT is still the one being displayed and we did NOT
		// enter the alternate screen (its leave-sequence is what
		// normally restores the user's console), clear the fb to black
		// so a stopped service doesn't leave a frozen, stale dashboard
		// on screen. If the user has switched to a different VT, the fb
		// shows that VT's kernel-drawn console and we must not touch it.
		// All-zero is black in every truecolor format.
		if isVT && consoleIsActive(consoleTTY) && !onAltScreen {
			clear(w.mmap)
			if w.verbose {
				fmt.Println("[FrameBuffer] Cleared screen on shutdown")
			}
		}

		close(w.closed)

		w.mu.Lock()
		w.open = false
		w.mu.Unlock()

		if err := unix.Munmap(w.mmap); err != nil && w.verbose {
			fmt.Printf("[FrameBuffer] Warning: munmap failed: %v\n", err)
		}
		if err := unix.Close(w.fbFD); err != nil && w.verbose {
			fmt.Printf("[FrameBuffer] Warning: close failed: %v\n", err)
		}
		w.releaseConsoleTTY()
	})
}

// The kernel VT layer blinks the console cursor by periodically inverting a
// character cell, which (on DRM-fb consoles) repaints over whatever we blit
// to /dev/fb0. The console's vt100 emulation honors the standard DEC
// cursor-visibility escapes (the same ones setterm -cursor emits), which
// stops the artifact -- but only sequences written to the console's own TTY
// affect that console.
var (
	vtNumber      int // our VT number when consoleTTY is a virtual console (else -1)
	cursorHideSeq = []byte{0x1b, '[', '?', '2', '5', 'l'}
	cursorShowSeq = []byte{0x1b, '[', '?', '2', '5', 'h'}
	consoleTTY    *os.File      // the controlling TTY, if we have one (nil otherwise)
	origTermios   *unix.Termios // saved so we can restore it on exit
	isVT          bool          // whether consoleTTY is a virtual console (/dev/ttyN)
	cursorHidden  bool          // whether we hid the cursor on consoleTTY
	onAltScreen   bool          // whether we're in the console's alternate screen
)

// controllingTTYDevice decodes field 7 ("tty_nr") of /proc/self/stat
// into the major/minor of our controlling terminal, using the kernel's
// tty_nr_to_dev() encoding: major = (nr & 0xff00) >> 8, minor =
// (nr & 0xff) | ((nr >> 12) & 0xff000). ok is false when there is no
// controlling terminal or the field cannot be read.
func controllingTTYDevice() (major, minor uint32, ok bool) {
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0, 0, false
	}
	// Field 2 (comm) is wrapped in parens and may itself contain spaces
	// or parens: drop everything up to and including the last ')'.
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, 0, false
	}
	fields := strings.Fields(s[i+1:])
	// After comm: state(0) ppid(1) pgrp(2) session(3) tty_nr(4)
	if len(fields) < 5 {
		return 0, 0, false
	}
	nr, err := strconv.ParseUint(fields[4], 10, 32)
	if err != nil || nr == 0 {
		return 0, 0, false
	}
	ttyNr := uint32(nr)
	return (ttyNr & 0xff00) >> 8, (ttyNr & 0xff) | ((ttyNr >> 12) & 0xff000), true
}

const (
	altScreenOn  = "\x1b[?1049h" // enter alternate screen (saves current screen)
	altScreenOff = "\x1b[?1049l" // leave alternate screen (restores saved screen)
)

const (
	keyCtrlR = 0x12 // console/terminal byte for Ctrl+R (manual refresh)
	keyCtrlT = 0x14 // console/terminal byte for Ctrl+T (rotate)

	// Trigger debounce: held keys auto-repeat in the console's byte
	// stream (there are no press/release events), so a trigger is
	// ignored if one fired within this window.
	triggerDebounce = 2 * time.Second
)

// vtActivate / vtWaitActive, from <linux/vt.h>: plain 16-bit command
// numbers. VT_ACTIVATE makes the given VT number the active console;
// VT_WAITACTIVE blocks until the switch has completed. (Same mechanism
// X11 uses when it takes over a console at login.)
const (
	vtActivate   = 0x5606
	vtWaitActive = 0x5607
	vtGetState   = 0x5603 // get global vt state info
)

// vtStat mirrors struct vt_stat from <linux/vt.h>. v_active is the
// currently displayed VT number. (The kernel notes VT_GETSTATE is only
// reliable for VTs below 16 via the v_state bitmask; v_active itself is
// fine for our purposes.)
type vtStat struct {
	vActive uint16
	vSignal uint16
	vState  uint16
}

// consoleIsActive reports whether our VT is the currently active console
// (the one being rendered to the framebuffer).
//
// Primary: VT_GETSTATE on our tty -- a pure read of fg_console with no
// permission requirement, which works for unprivileged services.
// Fallback: /sys/class/tty/tty0/active, which holds the name of the
// active virtual console (e.g. "tty7"); world-readable. (NOT
// /sys/class/tty/console/active -- that lists *registered* consoles and,
// with a serial console enabled, reads "tty0 ttyS1" regardless of the
// visible VT.)
func consoleIsActive(f *os.File) bool {
	if !isVT {
		return false
	}
	var st vtStat
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), vtGetState, uintptr(unsafe.Pointer(&st))); errno == 0 {
		return int(st.vActive) == vtNumber
	}
	if data, err := os.ReadFile("/sys/class/tty/tty0/active"); err == nil {
		return strings.TrimSpace(string(data)) == fmt.Sprintf("tty%d", vtNumber)
	}
	return false
}

// acquireConsoleTTY opens the process's controlling TTY if it is a
// virtual console, and hides the cursor (and its blink) if that console
// is currently active. It does nothing if the process has no controlling
// TTY (daemon, ssh session, systemd unit without a TTY) or the TTY is
// not a VT. If the console is not active at startup we keep the TTY
// anyway: watchVT re-hides the cursor and repaints when we become active
// later.
func (w *FramebufferWindow) acquireConsoleTTY() {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		// No controlling terminal (daemon, ssh without -t): no keyboard
		// input, no TTY management.
		if w.verbose {
			fmt.Printf("[FrameBuffer] No controlling TTY (%v): keyboard input disabled\n", err)
		}
		return
	}
	// /dev/tty is a magic device: neither its pathname, /proc/self/fd
	// links, nor even fstat() (which returns the 5:0 magic node) reveal
	// the real terminal. The kernel-authoritative route is field 7 of
	// /proc/self/stat ("tty_nr"), decoded with the kernel's
	// tty_nr_to_dev(): virtual consoles are major 4 (minor = VT number),
	// PTY slaves major 136 (minor = pts index).
	major, minor, ok := controllingTTYDevice()
	var name string
	vtNumber = -1
	switch {
	case ok && major == 4:
		isVT = true
		vtNumber = int(minor)
		name = fmt.Sprintf("/dev/tty%d", minor)
	case ok && major == 136:
		name = fmt.Sprintf("/dev/pts/%d", minor)
	case ok:
		name = fmt.Sprintf("device %d:%d", major, minor)
	default:
		name = "unknown"
	}
	// Input works on any controlling TTY, including a PTY (e.g. an ssh
	// session): Ctrl+R / Ctrl+T typed into that session reach us. Cursor
	// hiding, active-console detection and VT_ACTIVATE only apply to
	// real VTs.
	consoleTTY = f

	// Take the TTY over for input: near-raw mode (cfmakeraw minus ISIG,
	// so Ctrl+C still raises SIGINT and the usual shutdown path works).
	// Failing to save/restore termios strands the user's terminal, so
	// either both work or we don't touch the TTY at all -- say which.
	term, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[FrameBuffer] ERROR: could not read terminal settings (%v): leaving terminal untouched, keyboard input disabled\n", err)
		f.Close()
		consoleTTY = nil
		return
	}
	origTermios = term
	raw := *term
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP |
		unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	// NB: Oflag is left alone on purpose. Clearing OPOST (as cfmakeraw
	// does) disables ONLCR, so \n would no longer become \r\n and every
	// log line would drift one column right.
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.IEXTEN // keep ISIG
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(int(f.Fd()), unix.TCSETS, &raw); err != nil {
		fmt.Fprintf(os.Stderr, "[FrameBuffer] ERROR: could not set raw mode (%v): restoring terminal and disabling keyboard input\n", err)
		if err2 := unix.IoctlSetTermios(int(f.Fd()), unix.TCSETS, term); err2 != nil {
			fmt.Fprintf(os.Stderr, "[FrameBuffer] ERROR: and could not even restore it: %v -- run 'reset' in that terminal\n", err2)
		}
		consoleTTY = nil
		origTermios = nil
		f.Close()
		return
	}

	if !isVT {
		// We're drawing over whatever console is currently active (e.g.
		// a getty on tty1) and we CANNOT restore it on exit, so say so
		// unconditionally -- this is how a remote/ssh deployment looks.
		fmt.Fprintf(os.Stderr, "[FrameBuffer] Warning: not running on a virtual console (TTY is %s): the image covers the active console and the screen is NOT restored on exit. For proper behavior, run on the console itself (see systemd-framebuffer.md).\n", name)
		return
	}

	w.setAltScreen(true)

	if consoleIsActive(f) {
		w.setCursor(true)
		if w.verbose {
			fmt.Printf("[FrameBuffer] Using console %s (active), cursor hidden\n", name)
		}
		return
	}

	if !w.config.TakeConsole {
		if w.verbose {
			fmt.Printf("[FrameBuffer] Using console %s (not the active console); will repaint when it becomes active\n", name)
		}
		return
	}

	// Take over the display the way X does: activate our VT, then wait
	// for the switch to settle so we don't blit into a mid-switch fb.
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), vtActivate, uintptr(vtNumber)); errno != 0 {
		if w.verbose {
			fmt.Printf("[FrameBuffer] VT_ACTIVATE(%d) for %s failed: %v; will repaint when it becomes active\n", vtNumber, name, errno)
		}
		return
	}
	// arg is the VT number; the kernel rejects 0 with ENXIO. May still
	// fail with EPERM for unprivileged units that don't own the tty --
	// harmless: VT_ACTIVATE already switched, and watchVT picks up the
	// settled console within its 500ms poll.
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), vtWaitActive, uintptr(vtNumber)); errno != 0 && w.verbose {
		fmt.Printf("[FrameBuffer] VT_WAITACTIVE(%d) failed: %v\n", vtNumber, errno)
	}
	w.setCursor(true)
	if w.verbose {
		fmt.Printf("[FrameBuffer] Activated console %s (take-console), cursor hidden\n", name)
	}
}

// setAltScreen enters or leaves the console's alternate screen, the
// same mechanism full-screen TUIs (vim, htop) use. On a real VT the
// kernel saves the user's current screen buffer when we enter and
// restores+redraws it when we leave, so exiting our program puts the
// user's console back exactly as it was -- no manual `reset` needed.
// Only valid on VTs (a PTY would just receive the literal escape).
func (w *FramebufferWindow) setAltScreen(on bool) {
	if consoleTTY == nil || !isVT || on == onAltScreen {
		return
	}
	seq := altScreenOff
	if on {
		seq = altScreenOn
	}
	if _, err := consoleTTY.Write([]byte(seq)); err != nil {
		if w.verbose {
			action := "leave"
			if on {
				action = "enter"
			}
			fmt.Printf("[FrameBuffer] Could not %s alternate screen: %v\n", action, err)
		}
		return
	}
	onAltScreen = on
	if w.verbose {
		if on {
			fmt.Println("[FrameBuffer] Entered alternate screen (user console saved)")
		} else {
			fmt.Println("[FrameBuffer] Left alternate screen (user console restored)")
		}
	}
}

// setCursor hides or shows the console cursor on our TTY (idempotent).
func (w *FramebufferWindow) setCursor(hidden bool) {
	if consoleTTY == nil || hidden == cursorHidden {
		return
	}
	seq := cursorShowSeq
	action := "show"
	if hidden {
		seq, action = cursorHideSeq, "hide"
	}
	if _, err := consoleTTY.Write(seq); err != nil {
		if w.verbose {
			fmt.Printf("[FrameBuffer] Could not %s console cursor: %v\n", action, err)
		}
		return
	}
	cursorHidden = hidden
}

// watchVT polls the active console (/sys/class/tty/tty0/active) so we
// notice when the console switches back to
// our VT (the kernel repaints the fb with that VT's old contents, which
// wipes our image). On the away->active transition we re-hide the cursor
// and repaint. One ioctl per tick; the cost is negligible.
func (w *FramebufferWindow) watchVT() {
	if consoleTTY == nil || !isVT {
		return
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	active := consoleIsActive(consoleTTY)
	for {
		select {
		case <-w.closed:
			return
		case <-ticker.C:
			nowActive := consoleIsActive(consoleTTY)
			if nowActive && !active {
				if w.verbose {
					fmt.Println("[FrameBuffer] Back on our console, redrawing")
				}
				w.setCursor(true)
				_ = w.redraw()
			}
			active = nowActive
		}
	}
}

// watchInput reads the console TTY and maps key bytes to actions:
// Ctrl+R (0x12) -> refresh, Ctrl+T (0x14) -> rotate. Everything else is
// ignored (including ESC-prefixed Alt/function-key sequences). It exits
// when the TTY is closed (Close) or becomes unreadable.
func (w *FramebufferWindow) watchInput() {
	if consoleTTY == nil {
		return
	}
	if w.verbose {
		fmt.Println("[FrameBuffer] Keyboard input: Ctrl+R refresh, Ctrl+T rotate")
	}
	buf := make([]byte, 1)
	var lastRefresh, lastRotate time.Time
	for {
		select {
		case <-w.closed:
			return
		default:
		}
		n, err := consoleTTY.Read(buf)
		if n == 0 {
			if err != nil {
				return // TTY closed or gone
			}
			continue
		}
		now := time.Now()
		switch buf[0] {
		case keyCtrlR:
			if now.Sub(lastRefresh) < triggerDebounce {
				continue // key repeat
			}
			lastRefresh = now
			if w.verbose {
				fmt.Println("[FrameBuffer] Ctrl+R: manual refresh")
			}
			if w.refreshCallback != nil {
				w.refreshCallback()
			}
		case keyCtrlT:
			if now.Sub(lastRotate) < triggerDebounce {
				continue // key repeat
			}
			lastRotate = now
			if w.verbose {
				fmt.Println("[FrameBuffer] Ctrl+T: rotate")
			}
			if w.rotateCallback != nil {
				w.rotateCallback()
			}
		}
	}
}

// releaseConsoleTTY restores the TTY's original termios FIRST (a failed
// restore strands the user with a raw, echo-less terminal, so it gets a
// loud, unconditional warning), then leaves the alternate screen,
// restores the cursor, and closes our TTY.
func (w *FramebufferWindow) releaseConsoleTTY() {
	if consoleTTY == nil {
		return
	}
	if origTermios != nil {
		if err := unix.IoctlSetTermios(int(consoleTTY.Fd()), unix.TCSETS, origTermios); err != nil {
			fmt.Fprintf(os.Stderr, "[FrameBuffer] ERROR: could not restore terminal settings: %v -- run 'reset' or 'stty sane' in that terminal\n", err)
		}
		origTermios = nil
	} else if w.verbose {
		fmt.Println("[FrameBuffer] No saved termios to restore (termios was never read)")
	}
	w.setAltScreen(false)
	w.setCursor(false)
	consoleTTY.Close()
	consoleTTY = nil
	isVT = false
	vtNumber = -1
	cursorHidden = false
}

// GetApp returns the (nonexistent) backend app instance.
func (w *FramebufferWindow) GetApp() interface{} { return nil }

// SetMenuItemsEnabled is a no-op for the framebuffer backend.
func (w *FramebufferWindow) SetMenuItemsEnabled(bool) {}

// redraw composites the current image (aspect-fit, letterboxed) plus the
// status strip and writes the result to the framebuffer.
func (w *FramebufferWindow) redraw() error {
	w.mu.Lock()
	if !w.open {
		w.mu.Unlock()
		return fmt.Errorf("framebuffer is closed")
	}
	src := w.lastImage
	status := w.status
	w.mu.Unlock()

	fw := int(w.vinfo.XRes)
	fh := int(w.vinfo.YRes)

	composite := image.NewRGBA(image.Rect(0, 0, fw, fh))

	// Letterbox background.
	draw.Draw(composite, composite.Bounds(), &image.Uniform{color.RGBA{0, 0, 0, 255}}, image.Point{}, draw.Src)

	// Aspect-fit scale the image and center it.
	if src != nil {
		sb := src.Bounds()
		sx := float64(fw) / float64(sb.Dx())
		sy := float64(fh) / float64(sb.Dy())
		scale := math.Min(sx, sy)
		dw := int(math.Round(float64(sb.Dx()) * scale))
		dh := int(math.Round(float64(sb.Dy()) * scale))
		if dw < 1 {
			dw = 1
		}
		if dh < 1 {
			dh = 1
		}
		ox := (fw - dw) / 2
		oy := (fh - dh) / 2
		dst := image.Rect(ox, oy, ox+dw, oy+dh)
		xdraw.ApproxBiLinear.Scale(composite, dst, src, sb, xdraw.Src, nil)
	}

	// Status strip along the bottom.
	if status != "" {
		stripTop := fh - statusStripHeight
		bg := color.RGBA{255, 255, 255, 255}
		fg := color.RGBA{0, 0, 0, 255}
		if w.config.DarkMode {
			bg, fg = color.RGBA{0, 0, 0, 255}, color.RGBA{255, 255, 255, 255}
		}
		draw.Draw(composite, image.Rect(0, stripTop, fw, fh), &image.Uniform{bg}, image.Point{}, draw.Src)
		drawStatusText(composite, fw, stripTop, status, fg)
	}

	return w.blit(composite)
}

// drawStatusText draws centered text inside the status strip.
// font.Drawer.Dot is the text BASELINE (glyphs extend upward from it),
// so anchor it a couple of pixels above the bottom of the strip.
func drawStatusText(img *image.RGBA, width, stripTop int, text string, col color.Color) {
	face := basicfont.Face7x13
	textWidth := font.MeasureString(face, text).Ceil()
	x := (width - textWidth) / 2
	if x < 0 {
		x = 2
	}
	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(col),
		Face: face,
		Dot:  fixed.P(x, stripTop+statusStripHeight-2),
	}
	d.DrawString(text)
}

// packPixel converts 8-bit RGB channels into the framebuffer's packed
// truecolor pixel format described by the given shifts/masks/offsets.
// (Unit-tested in framebuffer_linux_test.go.)
func packPixel(r, g, b uint8, shiftRed, shiftGreen, shiftBlue uint32,
	maskRed, maskGreen, maskBlue uint32, offRed, offGreen, offBlue uint32) uint32 {
	// Widen BEFORE shifting: on a 5-bit channel, 255>>3 must be 31,
	// which is impossible if the shift happens in 8-bit arithmetic.
	r32, g32, b32 := uint32(r), uint32(g), uint32(b)
	return (r32>>shiftRed&maskRed)<<offRed |
		(g32>>shiftGreen&maskGreen)<<offGreen |
		(b32>>shiftBlue&maskBlue)<<offBlue
}

// blit writes the composed RGBA image to the framebuffer, honoring the
// device's pixel format (16- or 32-bit truecolor) and stride.
func (w *FramebufferWindow) blit(img *image.RGBA) error {
	if w.vinfo.Bpp != 16 && w.vinfo.Bpp != 32 {
		return fmt.Errorf("unsupported framebuffer depth: %d bpp (only 16 and 32 are supported)", w.vinfo.Bpp)
	}

	shiftRed := 8 - w.vinfo.Red.Length
	shiftGreen := 8 - w.vinfo.Green.Length
	shiftBlue := 8 - w.vinfo.Blue.Length
	maskRed := uint32((1 << w.vinfo.Red.Length) - 1)
	maskGreen := uint32((1 << w.vinfo.Green.Length) - 1)
	maskBlue := uint32((1 << w.vinfo.Blue.Length) - 1)

	srcBounds := img.Bounds()
	bytesPerPix := int(w.vinfo.Bpp) / 8

	for y := 0; y < srcBounds.Dy(); y++ {
		dstRow := w.mmap[y*w.stride:]
		srcRow := img.Pix[y*srcBounds.Dx()*4:]
		for x := 0; x < srcBounds.Dx(); x++ {
			r := uint32(srcRow[x*4])
			g := uint32(srcRow[x*4+1])
			b := uint32(srcRow[x*4+2])
			pix := packPixel(uint8(r), uint8(g), uint8(b), shiftRed, shiftGreen, shiftBlue,
				maskRed, maskGreen, maskBlue,
				w.vinfo.Red.Offset, w.vinfo.Green.Offset, w.vinfo.Blue.Offset)

			base := x * bytesPerPix
			if w.vinfo.Bpp == 16 {
				dstRow[base] = byte(pix)
				dstRow[base+1] = byte(pix >> 8)
			} else {
				dstRow[base] = byte(pix)
				dstRow[base+1] = byte(pix >> 8)
				dstRow[base+2] = byte(pix >> 16)
				dstRow[base+3] = byte(pix >> 24)
			}
		}
	}
	return nil
}
