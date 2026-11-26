package main

import (
	"fmt"
	"os"

	"rbxrt/internal/hostabi"
)

// buildProfile describes the engine build being run. Roblox builds are
// identified by the exact version string the engine embeds
// (engine.Binary.EngineVersion); the version is also what the runtime reports
// to Roblox as the app version.
//
// rbxrt applies no patches to the engine image. Earlier builds (2.734.0.917)
// needed a stop-gap that forced the engine's per-thread telemetry check before
// its task scheduler had run; current builds bring the scheduler up on their
// own and run unmodified.
type buildProfile struct {
	version string
}

// testedBuilds lists the builds rbxrt has been run against end to end.
var testedBuilds = map[string]bool{
	"2.748.0.1527": true,
}

// activeProfile is selected once the image is opened. It is never nil.
var activeProfile = &buildProfile{}

func selectProfile(version string) {
	activeProfile = &buildProfile{version: version}
	// A few diagnostics read fixed addresses that are only valid in 2.734.
	hostabi.SetLegacyDiagOffsets(version == "2.734.0.917")
	if testedBuilds[version] {
		fmt.Printf("engine:   %s (tested build)\n", version)
		return
	}
	if version == "" {
		version = "unknown"
	}
	fmt.Fprintf(os.Stderr, "rbxrt: note: engine build %s has not been tested with rbxrt\n", version)
}
