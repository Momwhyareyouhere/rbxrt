package hostabi

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"unsafe"
)

// This file implements the Android NDK configuration API
// (android/configuration.h). On Android the framework hands native code a
// fully-populated AConfiguration describing the device (locale, screen size,
// density, input capabilities). GameActivity reads it to size its surface and
// pick resources, so the runtime must present a coherent device configuration.
//
// We are the platform, so the values are derived from the host where that is
// meaningful -- the locale comes from LANG/LC_ALL and the logical screen size
// from RBXRT_SCREEN_WIDTH_DP / RBXRT_SCREEN_HEIGHT_DP -- rather than invented.
// A headless desktop has no touchscreen or sensor hardware, so those report the
// "not present" values the framework would report on such a device.
//
// AConfiguration is an opaque type: callers only ever hold pointers returned by
// AConfiguration_new, so the runtime represents a configuration as a small
// integer handle, exactly as it does for Java objects.

// ACONFIGURATION_* constants (android/configuration.h).
const (
	aConfigScreenSizeAny    = 0
	aConfigScreenSizeSmall  = 1
	aConfigScreenSizeNormal = 2
	aConfigScreenSizeLarge  = 3
	aConfigScreenSizeXLarge = 4

	aConfigNavHiddenAny = 0
	aConfigNavHiddenNo  = 1
	aConfigNavHiddenYes = 2

	aConfigOrientationAny       = 0
	aConfigOrientationPortrait  = 1
	aConfigOrientationLandscape = 2
)

var debugConfig = os.Getenv("RBXRT_DEBUG_CONFIG") != ""

// aConfiguration mirrors the subset of the device configuration the engine asks
// about. It is a handle value, not guest memory.
type aConfiguration struct {
	language   [2]byte
	country    [2]byte
	navHidden  int
	screenSize int
	widthDp    int
	heightDp   int
}

var (
	configMu      sync.Mutex
	configs               = map[uintptr]*aConfiguration{}
	configNext    uintptr = 0x5000000
	configOnce    sync.Once
	configDefault aConfiguration
)

// hostConfiguration derives the device configuration from the host environment.
func hostConfiguration() aConfiguration {
	c := aConfiguration{
		navHidden:  aConfigNavHiddenYes, // a desktop window has no on-screen nav bar
		screenSize: aConfigScreenSizeNormal,
		widthDp:    960,
		heightDp:   540,
	}
	// Locale: LANG/LC_ALL, e.g. "en_US.UTF-8" or "en_GB".
	loc := os.Getenv("LC_ALL")
	if loc == "" {
		loc = os.Getenv("LANG")
	}
	lang, country := parseLocale(loc)
	copy(c.language[:], lang)
	copy(c.country[:], country)
	if v := os.Getenv("RBXRT_SCREEN_WIDTH_DP"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.widthDp = n
		}
	}
	if v := os.Getenv("RBXRT_SCREEN_HEIGHT_DP"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.heightDp = n
		}
	}
	return c
}

// parseLocale turns "en_US.UTF-8" into ("en", "US"), defaulting to en/US.
func parseLocale(loc string) (string, string) {
	loc = strings.TrimSpace(loc)
	if i := strings.IndexByte(loc, '.'); i >= 0 {
		loc = loc[:i]
	}
	if i := strings.IndexByte(loc, '@'); i >= 0 {
		loc = loc[:i]
	}
	lang, country := "en", "US"
	if loc == "" || loc == "C" || loc == "POSIX" {
		return lang, country
	}
	if i := strings.IndexByte(loc, '_'); i >= 0 {
		if loc[:i] != "" {
			lang = loc[:i]
		}
		if loc[i+1:] != "" {
			country = loc[i+1:]
		}
	} else {
		lang = loc
	}
	if len(lang) > 2 {
		lang = lang[:2]
	}
	if len(country) > 2 {
		country = country[:2]
	}
	return lang, country
}

func defaultConfiguration() *aConfiguration {
	configOnce.Do(func() { configDefault = hostConfiguration() })
	c := configDefault
	return &c
}

func configAt(h uintptr) *aConfiguration {
	configMu.Lock()
	defer configMu.Unlock()
	return configs[h]
}

// --- NDK entry points ----------------------------------------------------

//go:noinline
func goAConfigurationNew() uintptr {
	configMu.Lock()
	h := configNext
	configNext += 16
	c := *defaultConfiguration()
	configs[h] = &c
	configMu.Unlock()
	if debugConfig {
		fmt.Fprintf(os.Stderr, "rbxrt: AConfiguration_new -> %#x lang=%q country=%q %dx%d dp\n",
			h, c.language, c.country, c.widthDp, c.heightDp)
	}
	return h
}

//go:noinline
func goAConfigurationDelete(h uintptr) {
	configMu.Lock()
	delete(configs, h)
	configMu.Unlock()
}

// goAConfigurationFromAssetManager populates out from the AssetManager's
// resources. We have no Android resource system, so out receives the host's
// device configuration.
//
//go:noinline
func goAConfigurationFromAssetManager(out, mgr uintptr) {
	configMu.Lock()
	if c, ok := configs[out]; ok {
		*c = *defaultConfiguration()
	} else {
		c := *defaultConfiguration()
		configs[out] = &c
	}
	configMu.Unlock()
	if debugConfig {
		fmt.Fprintf(os.Stderr, "rbxrt: AConfiguration_fromAssetManager(out=%#x mgr=%#x)\n", out, mgr)
	}
}

//go:noinline
func goAConfigurationGetLanguage(h, out uintptr) uintptr {
	c := configAt(h)
	if c == nil {
		return 0
	}
	if out != 0 {
		*(*byte)(unsafe.Pointer(out)) = c.language[0]
		*(*byte)(unsafe.Pointer(out + 1)) = c.language[1]
	}
	return 2
}

//go:noinline
func goAConfigurationGetCountry(h, out uintptr) uintptr {
	c := configAt(h)
	if c == nil {
		return 0
	}
	if out != 0 {
		*(*byte)(unsafe.Pointer(out)) = c.country[0]
		*(*byte)(unsafe.Pointer(out + 1)) = c.country[1]
	}
	return 2
}

//go:noinline
func goAConfigurationGetNavHidden(h uintptr) int {
	if c := configAt(h); c != nil {
		return c.navHidden
	}
	return aConfigNavHiddenAny
}

//go:noinline
func goAConfigurationGetScreenSize(h uintptr) int {
	if c := configAt(h); c != nil {
		return c.screenSize
	}
	return aConfigScreenSizeAny
}

//go:noinline
func goAConfigurationGetScreenWidthDp(h uintptr) int {
	if c := configAt(h); c != nil {
		return c.widthDp
	}
	return 0
}

//go:noinline
func goAConfigurationGetScreenHeightDp(h uintptr) int {
	if c := configAt(h); c != nil {
		return c.heightDp
	}
	return 0
}

func init() {
	hostFuncs["AConfiguration_new"] = goAConfigurationNew
	hostFuncs["AConfiguration_delete"] = goAConfigurationDelete
	hostFuncs["AConfiguration_fromAssetManager"] = goAConfigurationFromAssetManager
	hostFuncs["AConfiguration_getLanguage"] = goAConfigurationGetLanguage
	hostFuncs["AConfiguration_getCountry"] = goAConfigurationGetCountry
	hostFuncs["AConfiguration_getNavHidden"] = goAConfigurationGetNavHidden
	hostFuncs["AConfiguration_getScreenSize"] = goAConfigurationGetScreenSize
	hostFuncs["AConfiguration_getScreenWidthDp"] = goAConfigurationGetScreenWidthDp
	hostFuncs["AConfiguration_getScreenHeightDp"] = goAConfigurationGetScreenHeightDp
}
