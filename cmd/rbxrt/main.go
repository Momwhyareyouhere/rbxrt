// Command rbxrt is the rbxrt CLI. See ../README.md.
//
// Subcommands (the interesting ones are implemented; the rest come with their
// milestones):
//
//	rbxrt verify  <apk>            whole-file signature + Roblox cert pin check
//	rbxrt manifest <apk>           print the binary AndroidManifest.xml
//	rbxrt inspect <apk>            signing certificate details
//	rbxrt engine <libroblox.so>    ELF import/export analysis
//	rbxrt load <libroblox.so>      map + relocate + resolve + run constructors;
//	                               [--jni] calls JNI_OnLoad, [--entry] probes
//	                               the GameActivity app entry point, [--launch]
//	                               plays the full lifecycle over real graphics
//	rbxrt run <instance-dir>       launch a Roblox instance     (planned)
//	rbxrt doctor                   environment diagnostics      (planned)
package main

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
)

func main() {
	// The engine asks "am I on the thread that set me up?" with pthread_self
	// and, if the answer is no, hands the work to that thread and blocks until
	// it runs. Go may otherwise move this goroutine between OS threads at a
	// blocking call, which would make a later boot call look like a foreign
	// thread and deadlock the engine's main-thread queue. Pinning the main
	// goroutine to the process's first OS thread keeps the engine's notion of
	// the main thread and this goroutine's thread the same.
	runtime.LockOSThread()
	// Guest threads block in the kernel (futex, poll) through raw syscalls that
	// cannot use Go's entersyscall, because they run on a guest stack. Such a
	// thread keeps its P for as long as it is blocked. With the default
	// GOMAXPROCS (the CPU count) a handful of waiting engine threads therefore
	// consume every P, and the goroutine that would wake them - or the boot
	// watchdog, or a timer - never runs again. Size the P pool for the engine's
	// thread count instead; idle Ps cost almost nothing. This must happen
	// before any guest code runs: changing GOMAXPROCS stops the world, which
	// can never complete once a P is parked inside a raw futex wait.
	runtime.GOMAXPROCS(guestMaxProcs())
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "manifest":
		err = cmdManifest(os.Args[2:])
	case "inspect":
		err = cmdInspect(os.Args[2:])
	case "engine":
		err = cmdEngine(os.Args[2:])
	case "load":
		err = cmdLoad(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "rbxrt: "+err.Error())
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  rbxrt verify <apk>            verify signature + trusted-Roblox-certificate pin
  rbxrt manifest <apk>          print the binary manifest fields
  rbxrt inspect <apk>           print signing certificate details
  rbxrt engine <lib.so> [import NAME...]   ELF import/export/relocation analysis
  rbxrt load <lib.so> [--init] [--trace] [--jni] [--entry] [--launch] [--gfx]  map + relocate + resolve like bionic, run constructors, call JNI_OnLoad, probe the app entry, or play the GameActivity lifecycle over real X11/EGL/GLES
  rbxrt run <instance-dir>      launch a Roblox instance
  rbxrt help                    this text
`)
}

// guestMaxProcs is the GOMAXPROCS the runtime runs with; see main. The engine
// starts on the order of a hundred threads, so 256 leaves headroom.
// RBXRT_GOMAXPROCS overrides it.
func guestMaxProcs() int {
	if s := os.Getenv("RBXRT_GOMAXPROCS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	return 256
}
