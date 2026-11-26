package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"rbxrt/internal/apk"
	"rbxrt/internal/fetch"
	"rbxrt/internal/hostabi"
)

// cmdDoctor checks that this machine can run the engine, and optionally an
// instance directory. Each check prints OK, WARN or FAIL with what it found;
// the command fails if any check fails.
//
//	rbxrt doctor [instance-dir]
func cmdDoctor(args []string) error {
	if len(args) > 1 {
		return fmt.Errorf("usage: rbxrt doctor [instance-dir]")
	}
	d := &doctor{}

	// Platform: the engine is the x86_64 Android build, run natively.
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
		d.ok("platform", "linux/amd64")
	} else {
		d.fail("platform", fmt.Sprintf("%s/%s; rbxrt runs the x86_64 engine natively and needs linux/amd64", runtime.GOOS, runtime.GOARCH))
	}

	// Display and graphics: the same path the engine takes (X11 window, EGL
	// display, GLES 3 context). The window it opens is closed on exit.
	if os.Getenv("DISPLAY") == "" {
		d.fail("display", "DISPLAY is not set; rbxrt renders into an X11 window (Xwayland works)")
	} else {
		d.ok("display", "DISPLAY="+os.Getenv("DISPLAY"))
		runtime.LockOSThread()
		info, err := hostabi.ProbeGraphics()
		runtime.UnlockOSThread()
		if err != nil {
			d.fail("graphics", err.Error())
		} else {
			d.ok("graphics", fmt.Sprintf("EGL %s (%s), %s", info.EGLVersion, info.EGLVendor, info.GLVersion))
			switch {
			case strings.Contains(info.GLRenderer, "zink"):
				d.ok("renderer", info.GLRenderer+" (Mesa zink on Vulkan)")
			case strings.Contains(strings.ToLower(info.GLRenderer), "llvmpipe"), strings.Contains(strings.ToLower(info.GLRenderer), "softpipe"):
				d.warn("renderer", info.GLRenderer+": software rendering; expect it to be slow")
			default:
				d.ok("renderer", info.GLRenderer)
			}
			if !strings.Contains(info.GLVersion, "OpenGL ES 3") {
				d.fail("gles", "the engine needs OpenGL ES 3.x; got "+info.GLVersion)
			}
		}
	}

	// Network: client settings come from Roblox's CDN, the client from the
	// mirror. Only reachability is checked; nothing is downloaded.
	d.reach("network: settings", clientSettingsURL)
	d.reach("network: mirror", fetch.MirrorURL(fetch.Package))

	// Instance.
	if len(args) == 1 {
		d.instance(args[0])
	}

	if d.failed > 0 {
		return fmt.Errorf("%d check(s) failed", d.failed)
	}
	fmt.Println("doctor: all checks passed")
	return nil
}

type doctor struct{ failed int }

func (d *doctor) ok(what, msg string)   { fmt.Printf("  OK    %-18s %s\n", what, msg) }
func (d *doctor) warn(what, msg string) { fmt.Printf("  WARN  %-18s %s\n", what, msg) }
func (d *doctor) fail(what, msg string) {
	d.failed++
	fmt.Printf("  FAIL  %-18s %s\n", what, msg)
}

// reach checks that url answers, without downloading the body.
func (d *doctor) reach(what, url string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		d.fail(what, err.Error())
		return
	}
	req.Header.Set("User-Agent", "rbxrt")
	resp, err := http.DefaultClient.Do(req)
	if err == nil && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed) {
		// Some CDNs refuse HEAD where GET works; ask again with GET and stop
		// after the headers (closing the body abandons the transfer).
		resp.Body.Close()
		req.Method = http.MethodGet
		resp, err = http.DefaultClient.Do(req)
	}
	if err != nil {
		d.fail(what, err.Error())
		return
	}
	resp.Body.Close()
	host := req.URL.Host
	if resp.StatusCode >= 400 {
		d.warn(what, fmt.Sprintf("%s answered HTTP %d", host, resp.StatusCode))
		return
	}
	d.ok(what, host+" reachable")
}

func (d *doctor) instance(dir string) {
	apkPath := instanceAPK(dir)
	if _, err := os.Stat(apkPath); err != nil {
		d.warn("instance", apkPath+" not present yet; rbxrt run fetches it")
	} else if trusted, err := apk.TrustedCertificates(); err != nil {
		d.fail("instance", err.Error())
	} else if v, err := apk.Verify(apkPath, trusted); err != nil {
		d.fail("instance", fmt.Sprintf("%s: %v", apkPath, err))
	} else {
		version := ""
		if a, err := apk.OpenArchive(apkPath); err == nil {
			if data, err := a.Read("AndroidManifest.xml"); err == nil {
				if m, err := apk.ParseManifest(data); err == nil {
					version = " " + m.VersionName
				}
			}
			a.Close()
		}
		d.ok("instance", fmt.Sprintf("%s%s, signature v%d, Roblox certificate", apkPath, version, v.Scheme))
	}

	// Space for the extracted engine, assets and caches (~1 GB in practice).
	target := dir
	for {
		if _, err := os.Stat(target); err == nil || target == filepath.Dir(target) {
			break
		}
		target = filepath.Dir(target)
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(target, &st); err == nil {
		free := st.Bavail * uint64(st.Bsize)
		msg := fmt.Sprintf("%d GB free at %s", free>>30, target)
		if free < 2<<30 {
			d.warn("disk", msg+"; an instance needs about 1 GB")
		} else {
			d.ok("disk", msg)
		}
	}
}
