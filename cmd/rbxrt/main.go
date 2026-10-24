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
