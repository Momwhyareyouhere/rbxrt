// Package hostabi provides the bionic-compatible "host libc" that the runtime
// exposes to loaded Roblox native libraries.
//
// Guest code (compiled for Android x86-64 by the NDK) calls into these
// functions using the SysV ABI. Go functions use the ABIInternal register ABI,
// so every host function is reached through a tiny assembly trampoline
// (abi_amd64.s) that converts the first six integer registers and then jumps
// into the shared ·goShim, which performs the actual call with correct stack
// alignment. Float-only functions (math) need no register movement on amd64:
// both ABIs pass the first floating-point argument in X0/XMM0 and return in
// X0/XMM0, so they use ·goShimF.
//
// The symbol table is exposed via Symbols(); the linker's resolver registers it
// and supplies every symbol the engine classifies as host (plus data objects).
package hostabi
