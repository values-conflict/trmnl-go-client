# Running trmnl-go as a systemd Service on a Dedicated Console (framebuffer mode)

Goal: a Raspberry Pi (or any Linux box) that boots straight into the TRMNL
display on a dedicated virtual console, with no getty/login shell on that
VT, logs in journald, and restarts on crash.

Target state:
- **tty7** (any unused VT works): runs `trmnl-go -output=framebuffer`,
  owns `/dev/fb0` visually, input read directly from the TTY (see
  tianon-framebuffer-input.md).
- **tty1**: untouched getty — the escape hatch / admin console.
- `journalctl -u trmnl-framebuffer -f` shows all program output.

## 1. Install the binary

```sh
install -m 0755 trmnl-go-linux-arm64 /usr/local/bin/trmnl-framebuffer
```

The headless build (`-tags headless`) is the right artifact here: no GL/
X11 runtime deps at all. Verify it runs once manually *from the console
you intend to dedicate*:

```sh
# from a login on tty7 (Ctrl+Alt+F7, log in):
trmnl-framebuffer -output=framebuffer -model TRMNL -epaper -verbose
# Ctrl+C to stop (restores the cursor)
```

## 2. The unit file

`/etc/systemd/system/trmnl-framebuffer.service`:

```ini
[Unit]
Description=TRMNL virtual display (framebuffer, console on tty7)
# Start once networking is up so the first fetch doesn't fail;
# don't hard-block boot on it.
Wants=network-online.target
After=network.target network-online.target systemd-vconsole-setup.service

[Service]
Type=simple
# TTYPath makes /dev/tty7 our CONTROLLING terminal. This is what makes
# /dev/tty work inside the program: cursor control and raw keyboard
# input rely on the process owning the TTY. (VT-active detection does
# NOT need it: it reads /sys/class/tty/tty0/active, which is
# world-readable.)
TTYPath=/dev/tty7
StandardInput=tty
StandardOutput=journal
StandardError=journal
SYSLOG_IDENTIFIER=trmnl-framebuffer

# Runs as root: the framebuffer device (root:video, 0660) and the TTY
# (root:tty, 0620) are the two permission gates. See "Non-root variant"
# below for the hardened alternative.
# -take-console: at every start (incl. boot and crash-restarts) the
# kernel switches the active console to tty7, X11-style. Without it the
# display would appear only when someone manually Ctrl+Alt+F7's.
ExecStart=/usr/local/bin/trmnl-framebuffer -output=framebuffer -fb=/dev/fb0 -model TRMNL -epaper -verbose -take-console

# SIGTERM on stop/exit -> our graceful path: refreshLoop exits, Close()
# clears the screen to black (if our console is still on display),
# unmaps the fb, and restores the console cursor.
KillSignal=SIGTERM
TimeoutStopSec=10

# Crash-loop protection: restart, but back off if it's thrashing.
Restart=on-failure
RestartSec=2
StartLimitIntervalSec=300
StartLimitBurst=10

# Kiosk hardening (optional, see notes):
# NoNewPrivileges=true
# ProtectSystem=full          # read-only /usr,/boot,/etc -- fine, we write nothing there
# PrivateTmp=true
# LockPersonality=true

[Install]
# getty.target is the same target the getty units use: we take tty7's
# place in the boot sequence.
WantedBy=getty.target
```

## 3. Replace getty on tty7 (and only tty7)

First check whether you need this at all: `ps -t tty7`. On a default
Debian install the getty-generator only covers **tty1–tty6**, so if the
gettys you see are on tty1–6 there is *nothing to disable* — tty7 is
already free and `-take-console` (below) is all that's needed. The
steps below only apply if something actually owns tty7.

If it is owned, the console gettys come from
`systemd/getty-generator` (up to 6 VTs), or an enabled
`systemd-getty@ttyX.service` override. Check which one owns tty7:

```sh
systemctl list-units 'systemd-getty@*' | cat
ls -l /etc/systemd/system/getty.target.wants/
```

Disable it for tty7 only — a masked override beats the generator:

```sh
mkdir -p /etc/systemd/system/systemd-getty@tty7.service.d
cat > /etc/systemd/system/systemd-getty@tty7.service.d/override.conf <<'EOF'
[Unit]
ConditionPathExists=!/etc/disable-tty7-getty
EOF
```

…or, more bluntly, just:

```sh
systemctl mask systemd-getty@tty7
```

(If a *generated* getty@tty7 exists, `mask` may report "cannot mask a
generated unit"; the drop-in `ConditionPathExists=!` trick above, or
editing `/etc/securetty` + `getty-static.conf` as appropriate, is the
portable route. Verify at boot with `ps -t tty7`.)

**Why this matters:** two readers on one TTY fight. A getty would print
its `login:` prompt over our image within seconds of boot (and after any
VT switch-back), and its cursor would blink. With tty7's getty gone, the
only process talking to that console is us.

## 4. Enable + verify

```sh
systemctl daemon-reload
systemctl enable trmnl-framebuffer
systemctl start trmnl-framebuffer

systemctl status trmnl-framebuffer
journalctl -u trmnl-framebuffer -f          # program output, live
journalctl -u trmnl-framebuffer --since -1h # history

# Confirm the split:
ps -t tty1    # getty (admin console, untouched)
ps -t tty7    # trmnl-framebuffer
```

Expected verbose lines at startup (on tty7):
`Using console /dev/tty7 (active), cursor hidden`,
`/dev/fb0: 1920x1080 @ 16bpp, visual 2, stride 3840`,
then image updates as the TRMNL API pushes them.

## 5. Logs → journald

`StandardOutput=journal` / `StandardError=journal` are already the
defaults for system services, but they're explicit here because we use
`-verbose`: every `fmt.Printf` (stdout) and `log.Printf` (stderr) lands
in the journal tagged with `SYSLOG_IDENTIFIER=trmnl-framebuffer`.

Notes:
- Verbosity at service scale: the per-image decode lines are fine, but
  drop `-verbose` once settled (the config file can carry `verbose`
  instead), or the journal fills with one line per refresh.
- No journal socket needed: stderr/stdout-to-journal is exactly what
  journald expects. If structured logs are ever wanted, Go can write to
  the native journal via the `systemd.journal` stdio protocol
  (PRIORITY/OBJECT_SYSLOG_IDENTIFIER fields per line) — overkill for now.
- The existing API log flusher (`log-flush-interval`) is unaffected.

## 6. Non-root variant (optional hardening)

A dedicated user with just the two device groups:

```sh
useradd --system --no-create-home --shell /usr/sbin/nologin trmnl
usermod -aG video,tty,trmnl trmnl
```

Unit changes: `User=trmnl`, `SupplementaryGroups=video tty`, plus a udev
rule granting the TTY (the `tty` group usually suffices for `/dev/tty7`,
which is `root:tty 0620`):

```sh
# /etc/udev/rules.d/99-trmnl.rules
KERNEL=="fb0", GROUP="video", MODE="0660"
```

Caveat: keep `NoNewPrivileges=true` OUT of the root variant discussion —
it's safe with User=trmnl; just make sure nothing in the command path
needs setuid. On a dedicated kiosk, plain-root is defensible; the
non-root variant is the better hygiene and costs ~10 lines.

## 7. What this changes in the design

- **Console takeover (`-take-console`)**: at startup the service
  issues `VT_ACTIVATE(7)` + `VT_WAITACTIVE` (the same mechanism X11
  uses at login), so boot lands on the display instead of a getty
  prompt — no getty replacement needed for the *switch* itself. The
  cost: *every* start — including a crash-restart loop — yanks the
  console from wherever you are to tty7. That's the desired kiosk
  behavior; drop the flag if you'd rather switch manually.
- **The "no controlling TTY" case disappears.** `TTYPath=` guarantees
  `/dev/tty` works: `acquireConsoleTTY()` will always find `/dev/tty7`,
  `watchVT()` is meaningful, and the signal-based fallback (SIGHUP/
  USR1/USR2) remains for when someone is *not* on tty7 (it fires
  regardless of VT).
- **Input (implemented, per tianon-framebuffer-input.md):** the program
  reads its controlling TTY in near-raw mode — Ctrl+R / Ctrl+T, debounced,
  ISIG kept so Ctrl+C still exits. It works by construction here (we own
  the VT), and also over ssh (`/dev/pts/N`), where the shortcuts arrive
  from the ssh session rather than a device keyboard. No evdev, no
  `input` group.
- **VT switch-back flash:** with no getty on tty7, switching back
  shows a blank/last-state screen (not a login prompt), and `watchVT()`
  repaints within ~0.5s.
- **Exit semantics:** `systemctl stop` sends SIGTERM → clean shutdown
  (cursor restored, fb unmapped). `systemctl restart` gets you a fresh
  start. If the unit is stopped while *you're* on tty7, the VT shows
  the stale kernel-console contents and has no shell — switch to tty1
  and `systemctl start trmnl-framebuffer`, or `systemctl mask` + reboot.
- **Boot ordering:** `network-online.target` is a Wants (soft) dep on
  purpose — the display should come up even if the network is down; the
  app already renders error screens and retries.

## 8. Operations cheatsheet

```sh
journalctl -u trmnl-framebuffer -f                 # follow logs
systemctl restart trmnl-framebuffer                # restart
kill -s USR1 $(systemctl show -p MainPID --value trmnl-framebuffer)   # manual refresh
kill -s USR2 $(systemctl show -p MainPID --value trmnl-framebuffer)   # rotate
ps -t tty7                                         # who owns the VT
# escape from the kiosk: Ctrl+Alt+F1 (tty1, getty, login)
# back to the display: Ctrl+Alt+F7 (image repaints within ~0.5s)
```
