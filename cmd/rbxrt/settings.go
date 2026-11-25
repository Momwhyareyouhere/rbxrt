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
// {"applicationSettings":{...}}. With RBXRT_FETCH_SETTINGS=1 the flags come
// from the CDN (cached in the data directory, which is also the fallback when
// the fetch fails); otherwise none are passed and the engine uses its
// built-in defaults. Fetching is opt-in for now: under the live flags the
// engine takes its newer startup path (startup render throttle released by
// performance control, deferred DataModel join), which this runtime does not
// complete yet, so no frames are presented. RBXRT_CLIENT_SETTINGS overrides
// individual flags, e.g. {"FLogGraphics":"6"} to enable an FLog channel.
func clientSettingsJSON() string {
	flags := map[string]any{}
	cache := filepath.Join(rbxrtDataDir(), "ClientAppSettings.json")
	body, source := []byte(nil), ""
	fetch := os.Getenv("RBXRT_FETCH_SETTINGS") != ""
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
	if body == nil && fetch {
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
