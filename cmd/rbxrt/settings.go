package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// The Android app fetches its ClientAppSettings (the fast flags for the
// "AndroidApp" application) from Roblox's public settings CDN and hands them
// to nativeInitClientSettings. The engine's behaviour depends on them: with
// the built-in defaults, for example, the Lua app hands login to a native Java
// screen instead of drawing its own (FIntLuaAppLoginMethod=1 on the CDN).
const clientSettingsURL = "https://clientsettingscdn.roblox.com/v2/settings/application/AndroidApp"

// clientSettingsJSON returns the payload for nativeInitClientSettings:
// {"applicationSettings":{...}}. The flags come from the CDN, as on a device
// (cached in the data directory, which is also the fallback when the fetch
// fails); RBXRT_OFFLINE=1 skips the fetch and uses the cache, or the engine's
// built-in defaults when there is none. The live flags matter: with the
// built-in defaults the app hands login to a native Java screen this runtime
// does not have, while the live ones (FIntLuaAppLoginMethod=1) let the Lua app
// draw its own start screen. runtimeFlagOverrides is applied on top, then
// RBXRT_CLIENT_SETTINGS (individual flags, e.g. {"FLogGraphics":"6"});
// RBXRT_CLIENT_SETTINGS_FILE supplies a whole settings document instead.
func clientSettingsJSON() string {
	flags := map[string]any{}
	cache := filepath.Join(rbxrtDataDir(), "ClientAppSettings.json")
	body, source := []byte(nil), ""
	fetch := os.Getenv("RBXRT_OFFLINE") == ""
	if f := os.Getenv("RBXRT_CLIENT_SETTINGS_FILE"); f != "" {
		// A saved settings document ({"applicationSettings":{...}}), used
		// as-is instead of the CDN: for pinning or bisecting a flag set.
		b, err := os.ReadFile(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "rbxrt: RBXRT_CLIENT_SETTINGS_FILE: %v\n", err)
		} else {
			body, source = b, f
		}
		fetch = false
	}
	if fetch {
		if b, err := fetchClientSettings(); err == nil {
			body, source = b, "CDN"
			if err := os.WriteFile(cache, b, 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "rbxrt: client settings: cache: %v\n", err)
			}
		} else {
			fmt.Fprintf(os.Stderr, "rbxrt: client settings: %v\n", err)
		}
	}
	if body == nil {
		if b, err := os.ReadFile(cache); err == nil {
			body, source = b, "cache"
		}
	}
	if body != nil {
		var doc struct {
			ApplicationSettings map[string]any `json:"applicationSettings"`
		}
		if err := json.Unmarshal(body, &doc); err == nil && doc.ApplicationSettings != nil {
			flags = doc.ApplicationSettings
		} else {
			fmt.Fprintf(os.Stderr, "rbxrt: client settings from %s are not valid JSON; using none\n", source)
			source = ""
		}
	}
	for k, v := range runtimeFlagOverrides {
		flags[k] = v
	}
	if o := os.Getenv("RBXRT_CLIENT_SETTINGS"); o != "" {
		var overrides map[string]any
		if err := json.Unmarshal([]byte(o), &overrides); err != nil {
			fmt.Fprintf(os.Stderr, "rbxrt: RBXRT_CLIENT_SETTINGS is not a JSON object: %v\n", err)
		}
		for k, v := range overrides {
			flags[k] = v
		}
	}
	if source == "" {
		source = "none"
	}
	fmt.Printf("boot: client settings: %d flags (%s)\n", len(flags), source)
	out, _ := json.Marshal(map[string]any{"applicationSettings": flags})
	return string(out)
}

func fetchClientSettings() ([]byte, error) {
	c := &http.Client{Timeout: 20 * time.Second}
	resp, err := c.Get(clientSettingsURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", clientSettingsURL, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

// runtimeFlagOverrides turns off features that depend on Android components
// this runtime does not provide. Each entry says why; remove it once the
// component exists.
var runtimeFlagOverrides = map[string]any{
	// The new video-capture service sits on Android's MediaCodec and
	// AImageReader. Binding the workspace to the renderer waits on it, so the
	// render job - and the whole task-scheduler frame - never completes.
	"FFlagVideoCaptureUseNewService6": "False",
}
