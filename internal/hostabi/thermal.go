package hostabi

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unsafe"
)

// The NDK thermal API (libandroid, API 30+), which the engine looks up with
// dlsym to scale its work back when the device runs hot. It is answered from
// the host's real sensors: every Linux thermal zone that has a critical trip
// point (/sys/class/thermal). Headroom is the hottest zone's temperature as a
// fraction of its critical temperature, so 1.0 means "at the throttling
// limit", which is how Android defines it.

// AThermalStatus values.
const (
	thermalStatusNone     = 0
	thermalStatusLight    = 1
	thermalStatusModerate = 2
	thermalStatusSevere   = 3
	thermalStatusCritical = 4
)

var thermalManager [8]byte // AThermal_acquireManager needs a non-null handle

func init() {
	reg := func(name string, fn any) { hostFuncs[name] = fn }
	reg("AThermal_acquireManager", goAThermalAcquireManager)
	reg("AThermal_releaseManager", goAThermalReleaseManager)
	reg("AThermal_getCurrentThermalStatus", goAThermalGetCurrentThermalStatus)
	reg("AThermal_getThermalHeadroom", goAThermalGetThermalHeadroom)
	reg("AThermal_getThermalHeadroomThresholds", goAThermalGetHeadroomThresholds)
}

// thermalHeadroom returns the hottest zone's temp/critical, and false when
// the host exposes no zone with a critical trip point.
func thermalHeadroom() (float32, bool) {
	zones, _ := filepath.Glob("/sys/class/thermal/thermal_zone*")
	best, found := float32(0), false
	for _, z := range zones {
		temp, ok := readMilli(filepath.Join(z, "temp"))
		if !ok {
			continue
		}
		trips, _ := filepath.Glob(filepath.Join(z, "trip_point_*_type"))
		for _, t := range trips {
			b, err := os.ReadFile(t)
			if err != nil || strings.TrimSpace(string(b)) != "critical" {
				continue
			}
			crit, ok := readMilli(strings.TrimSuffix(t, "_type") + "_temp")
			if !ok || crit <= 0 {
				continue
			}
			if h := float32(temp) / float32(crit); !found || h > best {
				best, found = h, true
			}
		}
	}
	return best, found
}

func readMilli(path string) (int64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return v, err == nil
}

//go:noinline
func goAThermalAcquireManager() uintptr { return uintptr(unsafe.Pointer(&thermalManager[0])) }

//go:noinline
func goAThermalReleaseManager(m uintptr) {}

//go:noinline
func goAThermalGetCurrentThermalStatus(m uintptr) int {
	h, ok := thermalHeadroom()
	switch {
	case !ok || h < 0.85:
		return thermalStatusNone
	case h < 0.95:
		return thermalStatusLight
	case h < 1.0:
		return thermalStatusModerate
	case h < 1.05:
		return thermalStatusSevere
	}
	return thermalStatusCritical
}

// getThermalHeadroom(manager, forecastSeconds) -> float; NaN when unknown, as
// on Android. The forecast is the current value: the host has no model.
//
//go:noinline
func goAThermalGetThermalHeadroom(m, forecastSeconds uintptr) float32 {
	if h, ok := thermalHeadroom(); ok {
		return h
	}
	return float32(math.NaN())
}

// Device-specific headroom thresholds are not known: ENOSYS, as an Android
// device without them reports.
//
//go:noinline
func goAThermalGetHeadroomThresholds(m, out, size uintptr) int {
	return 38 // ENOSYS
}
