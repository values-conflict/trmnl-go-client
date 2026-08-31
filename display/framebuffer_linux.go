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
	"path/filepath"
	"regexp"
	"strconv"
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

	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGUSR1, syscall.SIGUSR2)

	if w.verbose {
		fmt.Println("[FrameBuffer] Signalling: SIGHUP/SIGUSR1 = refresh, SIGUSR2 = rotate")
	}

	go func() {
		for sig := range sigCh {
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

// Close unmaps the framebuffer and unblocks Show().
func (w *FramebufferWindow) Close() {
	w.closeOnce.Do(func() {
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
	ttyRe         = regexp.MustCompile(`^/dev/tty([0-9]+)$`)
	cursorHideSeq = []byte{0x1b, '[', '?', '2', '5', 'l'}
	cursorShowSeq = []byte{0x1b, '[', '?', '2', '5', 'h'}
	consoleTTY    *os.File // the console TTY, if we own it (nil otherwise)
	cursorHidden  bool     // whether we hid the cursor on consoleTTY
)

// tiocgcons is TIOCGCONS, _IO('T', 0x54) from the kernel's uapi
// <linux/tty.h>: 0 if the terminal is the currently active console.
const tiocgcons = 0x5454

// vtActivate / vtWaitActive, from <linux/vt.h>: plain 16-bit command
// numbers. VT_ACTIVATE makes the given VT number the active console;
// VT_WAITACTIVE blocks until the switch has completed. (Same mechanism
// X11 uses when it takes over a console at login.)
const (
	vtActivate   = 0x5606
	vtWaitActive = 0x5607
)

// consoleIsActive reports whether the given TTY is the currently active
// console (the one being rendered to the framebuffer), via TIOCGCONS.
// NB: TIOCGCONS also fails with ENOTTY if the kernel doesn't know the
// command, so a wrong constant degrades to "treated as inactive" rather
// than anything worse.
func consoleIsActive(f *os.File) bool {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), tiocgcons, 0)
	return errno == 0
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
		return // no controlling terminal; nothing to do
	}
	name, err := filepath.EvalSymlinks("/proc/self/fd/" + fmt.Sprint(f.Fd()))
	if err != nil || !ttyRe.MatchString(name) {
		f.Close()
		return // controlling terminal isn't a virtual console
	}

	consoleTTY = f
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
	vtNo, err := strconv.Atoi(ttyRe.FindStringSubmatch(name)[1])
	if err != nil {
		return // unreachable: the regex guarantees digits
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), vtActivate, uintptr(vtNo)); errno != 0 {
		if w.verbose {
			fmt.Printf("[FrameBuffer] VT_ACTIVATE(%d) for %s failed: %v; will repaint when it becomes active\n", vtNo, name, errno)
		}
		return
	}
	// arg is ignored; 0 is conventional.
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), vtWaitActive, 0); errno != 0 && w.verbose {
		fmt.Printf("[FrameBuffer] VT_WAITACTIVE failed: %v\n", errno)
	}
	w.setCursor(true)
	if w.verbose {
		fmt.Printf("[FrameBuffer] Activated console %s (take-console), cursor hidden\n", name)
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

// watchVT polls TIOCGCONS so we notice when the console switches back to
// our VT (the kernel repaints the fb with that VT's old contents, which
// wipes our image). On the away->active transition we re-hide the cursor
// and repaint. One ioctl per tick; the cost is negligible.
func (w *FramebufferWindow) watchVT() {
	if consoleTTY == nil {
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

// releaseConsoleTTY restores the cursor and closes our TTY when we leave.
func (w *FramebufferWindow) releaseConsoleTTY() {
	if consoleTTY == nil {
		return
	}
	w.setCursor(false)
	consoleTTY.Close()
	consoleTTY = nil
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
