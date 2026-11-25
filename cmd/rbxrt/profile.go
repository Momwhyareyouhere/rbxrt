package main

import (
	"fmt"
	"os"

	"rbxrt/internal/hostabi"
)

// buildProfile holds every guest-image offset the runtime patches. Offsets are
// specific to one engine build: the same function moves (or disappears) between
// Roblox releases, and writing a stale offset corrupts whatever now lives
// there. Profiles are therefore keyed by the exact version string the engine
// embeds (engine.Binary.EngineVersion), and every patch also checks the bytes
// it replaces.
//
// A zero offset means "this build needs no such patch" (or it has not been
// located yet); the patch is skipped with a note.
type buildProfile struct {
	version string

	// telemetryPredicate is the shared "thread telemetry context present?"
	// helper the TelemetryBridge wrappers call (al = *(ctx2+0x28) != 0).
	telemetryPredicate uintptr
	// telemetrySites are the wrappers' `je <unregistered>` branches.
	telemetrySites []telemetrySite

	// telemetrySink is the generated telemetry sink entered after InitWithParams
	// whose exception machinery traps (int3) before the scheduler exists.
	telemetrySink uintptr
	// keyEventProbe, luaStartKeyProbe and startGameKeyProbe are `call rel32`
	// instructions into the synthetic Android key-event probe, at the StartApp,
	// StartLuaAppDM and StartGame call sites respectively. The host has no
	// Android input queue.
	keyEventProbe     uintptr
	luaStartKeyProbe  uintptr
	startGameKeyProbe uintptr
}

type telemetrySite struct {
	off uintptr
	tgt uintptr // the unregistered handler, recorded for diagnostics only
	n   int     // encoded length of the je: 2 (rel8) or 6 (rel32)
}

var buildProfiles = map[string]*buildProfile{
	// Roblox 2.734.0.917, x86_64: the build milestones 1-7 were reached on.
	"2.734.0.917": {
		version:            "2.734.0.917",
		telemetryPredicate: 0x2de3c50,
		telemetrySites: []telemetrySite{
			{0x6d40465, 0x6d404ec, 6},
			{0x6d405fb, 0x6d4066a, 2},
			{0x6d40767, 0x6d407e4, 2},
			{0x6d408db, 0x6d4092a, 2},
			{0x6d40a0c, 0x6d40a6a, 2},
			{0x6d40b4f, 0x6d40bd0, 6},
			{0x6d40cdf, 0x6d40d5c, 2},
			{0x6d40e53, 0x6d40ed2, 2},
		},
		telemetrySink:     0x6d42f0c,
		keyEventProbe:     0x26aa865,
		luaStartKeyProbe:  0x24f4fed,
		startGameKeyProbe: 0x2e50483,
	},
	// Roblox 2.748.0.1527, x86_64. Offsets are filled in as each site is
	// located in this build; see milestone.md.
	"2.748.0.1527": {
		version: "2.748.0.1527",
	},
}

// activeProfile is selected once the image is opened. It is never nil: an
// unknown build gets an empty profile, which applies no patches at all.
var activeProfile = &buildProfile{}

func selectProfile(version string) {
	if p, ok := buildProfiles[version]; ok {
		activeProfile = p
		hostabi.SetLegacyDiagOffsets(version == "2.734.0.917")
		fmt.Printf("engine:   %s (known build profile)\n", version)
		return
	}
	activeProfile = &buildProfile{version: version}
	if version == "" {
		version = "unknown"
	}
	fmt.Fprintf(os.Stderr, "rbxrt: WARNING: engine build %s has no profile; "+
		"no image patches will be applied\n", version)
}

// isCallRel32 accepts a `call rel32` (e8 xx xx xx xx).
func isCallRel32(cur []byte) error {
	if len(cur) != 5 || cur[0] != 0xe8 {
		return fmt.Errorf("not a call rel32")
	}
	return nil
}

// isJe accepts `je rel8` (74 xx) or `je rel32` (0f 84 xx xx xx xx).
func isJe(cur []byte) error {
	switch {
	case len(cur) == 2 && cur[0] == 0x74:
		return nil
	case len(cur) == 6 && cur[0] == 0x0f && cur[1] == 0x84:
		return nil
	}
	return fmt.Errorf("not a je")
}

// nopCall NOPs out the `call rel32` at off, if the profile has one.
func nopCall(bias, off uintptr, what string) {
	if off == 0 {
		fmt.Fprintf(os.Stderr, "rbxrt: %s: not located in build %s, skipped\n", what, activeProfile.version)
		return
	}
	nops := []byte{0x90, 0x90, 0x90, 0x90, 0x90}
	if err := hostabi.PatchGuestChecked(bias+off, nops, isCallRel32); err != nil {
		fmt.Fprintf(os.Stderr, "rbxrt: %s skip: %v\n", what, err)
	}
}

// stubReturn0 makes the function at off return 0 immediately, if the profile
// has one.
func stubReturn0(bias, off uintptr, what string) {
	if off == 0 {
		fmt.Fprintf(os.Stderr, "rbxrt: %s: not located in build %s, skipped\n", what, activeProfile.version)
		return
	}
	if err := hostabi.PatchGuest(bias+off, []byte{0x31, 0xc0, 0xc3}); err != nil {
		fmt.Fprintf(os.Stderr, "rbxrt: %s skip: %v\n", what, err)
	}
}
