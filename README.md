# rbxrt — from-scratch Android-Roblox runtime for Linux

A native Android-Roblox client runtime for Linux. No Wine, no Android
emulator, no VM. Open-source and auditable, written in Go.

Goals:

1. **Engine runtime** — load and execute Roblox's native Android binary
   (`libroblox.so`) on Linux (this milestone, in progress).
2. **Launcher & settings** — multi-instance, first-run APK download or
   user-import, per-instance settings.
3. **Plugins, Flatpak, AppImage** — packaging and extension points.

## Security model

Before anything from an APK is executed it must be **cryptographically proven
to be signed by Roblox**. The engine refuses v1 (JAR) scheme-only signatures and
verifies whole-file integrity with the v2/v3 APK Signature Scheme, then checks
the signing certificate's SHA-256 against a built-in list of Roblox's signing
certificates.

Everything here is based on public formats (APK signing block, AXML, ELF) and
public facts. The byte layout of the v2/v3 signer structures was verified
against AOSP's `apksig` sources, and the content-digest algorithm was
verified byte-for-byte against `ApkSigningBlockUtils.java`. The reference
open-source project Cordial (GPL-3.0) was consulted only for public facts
(certificate digests, APKPure mirror endpoint); no code was copied.

### Trusted certificates

Built-in Roblox signing certificate SHA-256:

```
44932ea35a17a267372d71b54d1a0cb3da0dca5113e94406ae2fe18090ba1477
```

Cross-checked against genuine `base.apk` (Roblox 2.734.0.917, x86_64) via the
Cordial trust file. Override for testing/auditing:

```
RBXRT_TRUSTED_CERTIFICATES=/path/to/list.json rbxrt verify roblox.apk
# list.json: ["<sha256-hex>", ...]
```

## Milestone status

- [x] `internal/apk` — APK set reading (base+split), binary-manifest (AXML)
      parser, v2/v3 signature verification, content-digest integrity, Roblox
      certificate pinning. Fixtures generated with the real Android SDK tools
      (`aapt2`, `zipalign`, `apksigner`) and verifiable with `apksigner`.
- [ ] `internal/engine` — ELF import/export analysis of `libroblox.so`.
- [ ] `internal/loader` — map, relocate, resolve shims, call entry.
- [ ] `internal/fetch` — APKPure mirror download with digest verification.
- [ ] CLI (`verify`/`inspect`/`run`/`doctor`).

## Development

```
go test ./...            # all tests
./scripts/fixtures.sh    # regenerate internal/apk/testdata with Android SDK tools
```

JNI/ART framework calls that we cannot honour are designed to fail loudly
rather than fake success; native code paths that cannot work on bare Linux are
shimmed (or refused) until implemented.

> **Legal note:** Roblox Corporation prohibits third-party clients. Requesting,
> downloading, or running Roblox through this runtime may breach Roblox's Terms
> of Service and result in account termination. Use only on accounts you are
> willing to lose.