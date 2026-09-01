# TTY Input — Implementation Notes

Status: **implemented** (see `display/framebuffer_linux.go`:
`watchInput`, the termios block in `acquireConsoleTTY`, and the restore in
`releaseConsoleTTY`). This document replaces the earlier evdev plan, which
was **dropped in favor of reading the controlling TTY directly**.

## What was built

In framebuffer mode, when the process has a controlling TTY, the program:

1. Puts the TTY in **near-raw mode** (cfmakeraw *minus ISIG*) and saves
   the original `termios`, restoring it on exit.
2. Reads key bytes in `watchInput()` (a goroutine started from `Show()`,
   exiting on `w.closed` / read error):
   - `0x12` (Ctrl+R) → refresh callback, debounced
   - `0x14` (Ctrl+T) → rotate callback, debounced
   - everything else ignored (Alt/function keys arrive as ESC-prefixed
     sequences; unknown bytes are simply skipped)
3. Ctrl+C still raises SIGINT (ISIG kept) → the app's normal graceful
   shutdown.

## Design decisions (and why)

- **TTY, not evdev.** When the program owns the console (the systemd
  deployment in systemd-framebuffer.md), the TTY byte stream is simpler
  (no protocol parsing, no `/dev/input` permissions, no `input` group),
  and — unlike evdev — input is naturally scoped to our VT: we only get
  keys while our console is active, which is exactly when the image is
  visible. evdev would have delivered keyboard events from *every* VT
  regardless of which one was on screen.
- **Unilateral TTY input, PTY included.** The input reader starts for
  *any* controlling TTY, not just `/dev/ttyN`. Over SSH the controlling
  TTY is `/dev/pts/N`; Ctrl+R typed in that session reaches the program
  as `0x12` and triggers a refresh. VT-specific behaviors (cursor hiding,
  active-console repaint, `VT_ACTIVATE`/`-take-console`) remain gated
  on real VTs via the `isVT` check.
- **Active-console detection: `VT_GETSTATE`.** The classic ioctl
  (0x5603) returns `struct vt_stat` whose `v_active` is the currently
  displayed VT; the kernel implements it as a pure read of
  `fg_console` with *no permission check*, so unprivileged services can
  use it. Fallback: `/sys/class/tty/tty0/active`, which holds the name
  of the active VT (e.g. `tty7`); also world-readable. The tempting
  lookalike, `/sys/class/tty/console/active`, lists *registered*
  consoles instead -- with a serial console enabled (the Debian Pi
  default) it reads `tty0 ttyS1` forever, regardless of the visible
  VT. (Post-mortem note: an earlier revision of this code used a made-up
  `TIOCGCONS` constant, 0x5454, which is actually `TIOCSERGWILD`, a
  serial ioctl -- no such TIOCGCONS exists. It silently failed with
  ENOTTY and masked the real problem for several test cycles.)
- **TTY identified by device number, not path.** `/dev/tty` is a magic
  device: the path (and `/proc/self/fd/N`) is always literally
  `/dev/tty`, so name-matching can never work. Instead we `fstat` the
  open fd and read the resolved `st_rdev`: virtual consoles are major 4
  (minor = VT number), PTY slaves use the dynamic major 136 (matched
  back to `/dev/pts/N` by rdev for logging). `ttyDeviceName()` does
  this; it also supplies the VT number for `VT_ACTIVATE` directly.
- **Debounce instead of key-state tracking.** The console byte stream has
  no press/release distinction — a held key auto-repeats bytes. A trigger
  within `triggerDebounce` (2s) of the previous one is dropped.
- **ISIG kept on purpose.** Full raw mode would eat Ctrl+C and break the
  graceful-shutdown path.
- **Shutdown is sequential on the main goroutine (app.go).** The signal
  handler only closes `stopCh`; main then waits for `doneCh` (refresh
  loop fully stopped, no in-flight blit) and *then* calls
  `window.Close()`. This order matters for two real bugs it prevents:
  (1) running `Close()` from the signal goroutine raced process exit --
  `main` could return while `Close` was still mid-way through releasing
  the framebuffer, killing it before the console TTY's termios was
  restored (the terminal was left raw; the restore syscalls were
  correct, the process just died first); and (2) `munmap` racing an
  in-flight `blit` (SIGSEGV). `Close()` after `doneCh` has both
  properties: nothing can blit, and the process cannot exit until the
  TTY is released.
- **Oflag left alone on purpose.** Clearing `OPOST` (as cfmakeraw does)
  disables `ONLCR`, so `\n` stops becoming `\r\n` and log output drifts
  one column right per line. We only ever write plain logs + cursor
  escapes, so output processing stays enabled. (Live fix if you ever
  see it: `stty onlcr` on that console.)

## Known caveats

- While the program runs, the controlling TTY is raw: no line editing /
  echo in that terminal (expected; it's the kiosk's input). Restored on
  clean exit.
- On a VT, the program enters the console's **alternate screen**
  (`\x1b[?1049h`, the vim/htop mechanism) at startup: the kernel saves
  the user's screen buffer and restores+redraws it on clean exit, so no
  manual `reset` is needed afterwards. `SIGKILL` (or a crash) skips all
  of this: the TTY stays raw and the saved screen is only restored by
  `reset` (or a VT switch away and back).
- Reading the TTY should not interfere with kernel VT switching
  (Ctrl+Alt+F* is handled by the VT layer before bytes reach a reader),
  but **this must be verified on hardware** (below).
- If stdin is redirected *and* there is no controlling TTY (daemon with
  `StandardInput=null`, no `TTYPath=`), there is no input at all — the
  signal fallback (SIGHUP/SIGUSR1 refresh, SIGUSR2 rotate) remains.

## Verification checklist (hardware, Pi)

1. `systemd-framebuffer.md` setup with `TTYPath=/dev/tty7` +
   `-take-console`. Boot → lands on the display.
2. On the kiosk console: Ctrl+R fetches a new screen (verbose:
   `Ctrl+R: manual refresh`); Ctrl+T cycles rotation.
3. Hold Ctrl+R for 5s → exactly one refresh (debounce).
4. **Escape works:** Ctrl+Alt+F1 still switches to the admin console.
   If it doesn't, that is a stop-the-world bug (kiosk trap).
5. Switch back (Ctrl+Alt+F7) → image repaints within ~0.5s (`watchVT`).
6. SSH: `ssh pi 'trmnl-framebuffer -output=framebuffer ...'` run from a
   *different* VT session → Ctrl+R typed over ssh triggers refresh.
   Expected warnings: "not running on a virtual console" at startup
   (the image covers the active console; the screen is NOT restored on
   exit), and on clean exit the TTY must be back to normal (echo on).
   If you ever need `reset` after a clean exit, that's a bug: rerun with
   `-verbose` and check for "could not restore terminal settings".
7. Ctrl+C exits cleanly; afterwards the console is usable (termios
   restored) and the cursor is back.

## Why evdev was dropped (for the record)

The original plan (evdev reader on `/dev/input/eventN`, ~250 lines,
keycode 44/20/29/97, `EVIOCGBIT`/`EVIOCGKEY` discovery, no `EVIOCGRAB`)
was sound but unnecessary once the deployment model settled on "the
program owns its console": the TTY path covers the same two shortcuts
with ~100 lines, zero new permissions, and correct VT scoping. If a
future deployment needs input while *not* owning a VT (e.g. fb takeover
from another session on a multi-seat box), revisit evdev — the protocol
details from that plan remain in git history / earlier versions of this
file.
