# rbxrt

Run the Android version of Roblox natively on Linux. No Wine, no Android
emulator, no virtual machine. Open source and written in Go.

> **Warning:** Roblox does not allow third-party clients. Using rbxrt may break
> Roblox's Terms of Service and can get your account banned. Only use it with an
> account you are willing to lose.

## What works

- The Roblox app: home page, friends, menus, search
- Signing in with **Quick Sign-in**, and staying signed in after a restart
- Mouse, keyboard and scroll wheel, with your own keyboard layout (German,
  French, …, including AltGr and dead keys)
- Native Wayland window (with title bar) or X11
- GPU rendering through Mesa (on Vulkan via zink)

Not yet:

- Signing in with a password (Roblox's captcha needs a web view rbxrt doesn't
  have; use Quick Sign-in instead)
- Joining games has not been tested, and so neither has game audio
- Text you type into a box only appears after you click out of it
- Microphone, camera and hardware video decoding

## Requirements

- Linux on x86_64 (64-bit Intel/AMD)
- A Wayland or X11 desktop
- Mesa graphics drivers with OpenGL ES 3 (most Linux systems)
- Go 1.27 or newer, to build
- About 500 MB of free disk space per instance
- Optional: PulseAudio or PipeWire (sound), libxkbcommon (keyboard layouts)

## Getting started

```sh
go build -o rbxrt ./cmd/rbxrt
./rbxrt doctor                 # checks graphics, display and network
./rbxrt run ~/rbxrt-instance   # downloads Roblox on first run, then starts it
```

The first `run` downloads the latest Roblox Android app (about 230 MB) and
checks that it is really signed by Roblox before using it. Close the window or
press Ctrl-C to quit.

To update Roblox later:

```sh
./rbxrt run ~/rbxrt-instance --update
```

### Signing in

1. Click **Sign In**, then **Quick Sign-in**.
2. Enter the code shown (or scan the QR code) in the Roblox app on your phone
   or on roblox.com.
3. You are signed in, and stay signed in the next time you start rbxrt.

Your login is saved in `~/rbxrt-instance/data/session.json`. That file is
private to your user, but anyone who gets it can use your account: don't share
it or upload it. Signing out in Roblox, or deleting the file, removes it.

## Settings

Settings are environment variables, for example
`RBXRT_WINDOW_WIDTH=1600 ./rbxrt run ~/rbxrt-instance`.

| Variable | What it does |
| --- | --- |
| `RBXRT_WINDOW_WIDTH`, `RBXRT_WINDOW_HEIGHT` | Window size in pixels (default 1280×720) |
| `RBXRT_DISPLAY=wayland` / `x11` | Force a window system (default: Wayland if available) |
| `RBXRT_GFX_BACKEND=mesa` | Use Mesa's own GL driver instead of zink/Vulkan |
| `RBXRT_NO_SESSION=1` | Don't save or restore your login |

## Folders

An *instance* is one Roblox installation. You can have several.

```
~/rbxrt-instance/
  base.apk                  the Roblox app (checked before every use)
  lib/x86_64/libroblox.so   the Roblox engine, unpacked from base.apk
  data/                     app data, caches and your saved login
  data/appData/logs/        Roblox's own log files
```

## Troubleshooting

- **Nothing shows up / graphics errors:** run `./rbxrt doctor` and check the
  lines marked `FAIL`.
- **Stuck at a captcha or "challenge" after entering a password:** use Quick
  Sign-in.
- **Something else:** Roblox's log is in `~/rbxrt-instance/data/appData/logs/`.
  More debug switches are listed in [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md).

## Security

rbxrt only runs an app that is cryptographically proven to be signed by
Roblox: it checks the APK's v2/v3 signature over the whole file and pins
Roblox's signing certificate
(`44932ea35a17a267372d71b54d1a0cb3da0dca5113e94406ae2fe18090ba1477`).
Everything is built from public file formats and public facts; no Roblox code
is included in this repository.

## Status

- [x] Verify and load the Android engine (`libroblox.so`)
- [x] Android runtime: Java bridge (JNI), threads, files, network
- [x] Graphics: Wayland and X11 windows, Mesa GLES
- [x] Input: mouse, keyboard, text boxes, keyboard layouts
- [x] Android platform libraries: audio (AAudio on PulseAudio/PipeWire), media,
      thermal, networking
- [x] Engine task scheduler and Java method calls, with no patches to the engine
- [x] Sign in and stay signed in
- [ ] Password sign-in (needs a web view for Roblox's captcha)
- [ ] Joining games, game audio
- [ ] Launcher, multiple instances, Flatpak/AppImage packages

Progress notes are in [milestone.md](milestone.md); how it all works is in
[docs/DEVELOPMENT.md](docs/DEVELOPMENT.md).

## License

rbxrt is free software under the [GNU General Public License v3.0](LICENSE).
Roblox and the Roblox app are property of Roblox Corporation and are not part
of this project.
