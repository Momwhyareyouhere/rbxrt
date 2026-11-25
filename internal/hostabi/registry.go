package hostabi

import "sync"

// hostFuncs maps a bionic symbol name to the Go implementation that backs it.
// Names that are absent here get a trap thunk (see Lookup): calling them aborts
// loudly with the symbol name, which is the runtime's standing policy for
// unimplemented behaviour. Aliases and fortified variants map to the same
// implementation.
var hostFuncs = map[string]any{
	// memory / string
	"memcpy":      goMemcpy,
	"memmove":     goMemmove,
	"memset":      goMemset,
	"memcmp":      goMemcmp,
	"memchr":      goMemchr,
	"memrchr":     goMemrchr,
	"strlen":      goStrlen,
	"strnlen":     goStrnlen,
	"strcpy":      goStrcpy,
	"strncpy":     goStrncpy,
	"strcat":      goStrcat,
	"strncat":     goStrncat,
	"strcmp":      goStrcmp,
	"strncmp":     goStrncmp,
	"strcasecmp":  goStrcasecmp,
	"strncasecmp": goStrncasecmp,
	"strchr":      goStrchr,
	"strrchr":     goStrrchr,
	"strstr":      goStrstr,
	"strcspn":     goStrcspn,
	"strspn":      goStrspn,
	"strpbrk":     goStrpbrk,

	// fortified variants
	"__memcpy_chk":   goMemcpyChk,
	"__memmove_chk":  goMemmoveChk,
	"__memset_chk":   goMemsetChk,
	"__strcpy_chk":   goStrncpyChk, // placeholder replaced below
	"__strncpy_chk":  goStrncpyChk,
	"__strncpy_chk2": goStrncpyChk,
	"__strcat_chk":   goStrcatChk,
	"__strchr_chk":   goStrchrChk,
	"__strlen_chk":   goStrlenChk,
	"__read_chk":     goReadChk,
	"__write_chk":    goWriteChk,
	"__FD_CLR_chk":   goFdClrChk,
	"__FD_ISSET_chk": goFdIssetChk,
	"__FD_SET_chk":   goFdSetChk,

	// wide characters
	"wcslen":  goWcslen,
	"wmemchr": goWmemchr,
	"wmemcmp": goWmemcmp,

	// numeric conversion
	"atoi":     goAtoi,
	"atol":     goAtol,
	"atoll":    goAtol,
	"strtol":   goStrtol,
	"strtoll":  goStrtol,
	"strtoul":  goStrtoul,
	"strtoull": goStrtoul,

	// math (double)
	"sin": goSin, "cos": goCos, "tan": goTan,
	"asin": goAsin, "acos": goAcos, "atan": goAtan, "atan2": goAtan2,
	"sinh": goSinh, "cosh": goCosh, "tanh": goTanh,
	"exp": goExp, "exp2": goExp2, "expm1": goExpm1,
	"log": goLog, "log2": goLog2, "log10": goLog10, "log1p": goLog1p,
	"cbrt": goCbrt, "pow": goPow, "hypot": goHypot,
	"floor": goFloor, "ceil": goCeil, "round": goRound,
	"fmod": goFmod, "remainder": goRemainder,
	"copysign": goCopysign, "fma": goFma,
	"erf": goErf, "erfc": goErfc,

	// math (single)
	"sinf": goSinF, "cosf": goCosF, "tanf": goTanF,
	"expf": goExpF, "logf": goLogF, "log10f": goLog10F,
	"powf": goPowF, "fmodf": goFmodF, "floorf": goFloorF,
	"ceilf": goCeilF, "roundf": goRoundF, "hypotf": goHypotF,

	// process / exit
	"exit": goExit, "_exit": goExit, "_Exit": goExit,
	"abort": goAbort, "__stack_chk_fail": goStackChkFail,
	"android_set_abort_message": goSetAbortMessage,
	"getpid":                    goGetpid, "getppid": goGetppid, "gettid": goGettid,
	"getuid": goGetuid, "geteuid": goGeteuid,
	"getpagesize": goGetpagesize, "sysconf": goSysconf,
	"getauxval":   goGetauxval,
	"getnameinfo": goGetnameinfo,
	"inet_pton":   goInetPton,

	// environment
	"getenv": goGetenv, "setenv": goSetenv, "unsetenv": goUnsetenv,

	// time
	"time": goTime, "gettimeofday": goGettimeofday,
	"clock_gettime": goClockGettime, "nanosleep": goNanosleep,

	// memory mapping
	"mmap": goMmap, "munmap": goMunmap, "mprotect": goMprotect,
	"madvise": goMadvise,

	// file descriptors
	"open": goOpen, "__open_2": goOpen, "close": goClose,
	"read": goRead, "write": goWrite, "pread": goPread, "pread64": goPread,
	"pwrite": goPwrite, "lseek": goLseek, "access": goAccess,
	"fcntl": goFcntl, "getcwd": goGetcwd,
	"mkdir": goMkdir, "rmdir": goRmdir, "unlink": goUnlink,

	// android logging
	"__android_log_write":     goAndroidLogWrite,
	"__android_log_print":     goAndroidLogPrint,
	"__android_log_buf_write": goAndroidLogBufWrite,

	// stdio (formatting)
	"vsnprintf":       goVsnprintf,
	"__vsnprintf_chk": goVsnprintfChk,
	"vsprintf":        goVsprintf,
	"vfprintf":        goVfprintf,
	"vprintf":         goVprintf,
	"__vsprintf_chk":  goVsprintfChk,
	"vsscanf":         goVsscanf,

	// stdio (FILE streams)
	"fopen": goFopen, "fopen64": goFopen,
	"fdopen": goFdopen, "fclose": goFclose, "fflush": goFflush,
	"fread": goFread, "__fread_chk": goFreadChk,
	"fwrite": goFwrite, "__fwrite_chk": goFwriteChk,
	"fseek": goFseek, "fseeko": goFseek, "fseeko64": goFseek,
	"ftell": goFtell, "ftello": goFtell, "ftello64": goFtell,
	"fgetc": goFgetc, "getc": goFgetc,
	"fputc": goFputc, "putc": goFputc,
	"ungetc":   goUngetc,
	"fgets":    goFgets,
	"fputs":    goFputs,
	"puts":     goPuts,
	"fputwc":   goFputwc,
	"getwc":    goGetwc,
	"ungetwc":  goUngetwc,
	"feof":     goFeof,
	"ferror":   goFerror,
	"clearerr": goClearerr,
	"fileno":   goFileno,
	"setvbuf":  goSetvbuf,

	// dynamic linker
	"dlopen": goDlopen, "dlsym": goDlsym, "dlclose": goDlclose,
	"dlerror": goDlerror, "dladdr": goDladdr, "dl_iterate_phdr": goDlIteratePhdr,

	// pthread (single-threaded semantics)
	"pthread_mutex_init":          goPthreadMutexInit,
	"pthread_mutex_lock":          goPthreadMutexLock,
	"pthread_mutex_trylock":       goPthreadMutexTrylock,
	"pthread_mutex_unlock":        goPthreadMutexUnlock,
	"pthread_mutex_destroy":       goPthreadMutexDestroy,
	"pthread_mutexattr_init":      goPthreadMutexattrInit,
	"pthread_mutexattr_destroy":   goPthreadMutexattrDestroy,
	"pthread_mutexattr_settype":   goPthreadMutexattrSettype,
	"pthread_self":                goPthreadSelf,
	"pthread_equal":               goPthreadEqual,
	"pthread_once":                goPthreadOnce,
	"pthread_key_create":          goPthreadKeyCreate,
	"pthread_key_delete":          goPthreadKeyDelete,
	"pthread_getspecific":         goPthreadGetspecific,
	"pthread_setspecific":         goPthreadSetspecific,
	"pthread_setname_np":          goPthreadSetnameNp,
	"pthread_sigmask":             goPthreadSigmask,
	"pthread_attr_init":           goPthreadAttrInit,
	"pthread_attr_destroy":        goPthreadAttrDestroy,
	"pthread_attr_setdetachstate": goPthreadAttrSetdetachstate,
	"pthread_attr_setstacksize":   goPthreadAttrSetstacksize,
	"pthread_create":              goPthreadCreate,

	// semaphores
	"sem_init":    goSemInit,
	"sem_wait":    goSemWait,
	"sem_post":    goSemPost,
	"sem_destroy": goSemDestroy,

	// stat family
	"stat":      goStat,
	"lstat":     goLstat,
	"fstat":     goFstat,
	"readlink":  goReadlink,
	"ftruncate": goFtruncate,
	"fchmod":    goFchmod,
	"fchown":    goFchown,
	"mremap":    goMremap,
	"remove":    goRemove,
	"rename":    goRename,

	// errno / strerror
	"__errno":  goErrnoAddr,
	"strerror": goStrerror,
	"syscall":  goSyscall,

	// atexit
	"__cxa_atexit":      goCxaAtexit,
	"__cxa_finalize":    goCxaFinalize,
	"__register_atfork": goRegisterAtfork,
	"pthread_atfork":    goPthreadAtfork,

	// sched / threads
	"sched_yield":              goSchedYield,
	"pthread_join":             goPthreadJoin,
	"pthread_detach":           goPthreadDetach,
	"pthread_cond_init":        goCondInit,
	"pthread_cond_destroy":     goCondDestroy,
	"pthread_cond_signal":      goCondSignal,
	"pthread_cond_broadcast":   goCondBroadcast,
	"pthread_cond_wait":        goCondWait,
	"pthread_cond_timedwait":   goCondTimedwait,
	"pthread_condattr_init":    goCondattrInit,
	"pthread_condattr_destroy": goCondattrDestroy,

	// math decomposers
	"ldexp": goLdexp,
	"modf":  goModf,
	"frexp": goFrexp,

	// assert / log
	"__android_log_assert": goAndroidLogAssert,

	// locale
	"newlocale":  goNewlocale,
	"freelocale": goFreelocale,
	"duplocale":  goDuplocale,
	"uselocale":  goUselocale,
	"setlocale":  goSetlocale,
	"localeconv": goLocaleconv,
	"strcoll_l":  goStrcollL,
	"strxfrm_l":  goStrxfrmL,
	"wcscoll_l":  goWcscollL,
	"wcsxfrm_l":  goWcsxfrmL,
	"strtoll_l":  goStrtollL,
	"strtoull_l": goStrtoullL,

	// character classification (narrow)
	"isspace":  goIsspace,
	"isalpha":  goIsalpha,
	"isdigit":  goIsdigit,
	"isalnum":  goIsalnum,
	"isupper":  goIsupper,
	"islower":  goIslower,
	"isxdigit": goIsxdigit,
	"isprint":  goIsprint,
	"isgraph":  goIsgraph,
	"iscntrl":  goIscntrl,
	"ispunct":  goIspunct,
	"isblank":  goIsblank,
	"tolower":  goTolower,
	"toupper":  goToupper,

	// character classification (wide) and *_l variants
	"iswalpha":   goIswalpha,
	"iswdigit":   goIswdigit,
	"iswspace":   goIswspace,
	"iswprint":   goIswprint,
	"iswblank":   goIswblank,
	"iswcntrl":   goIswcntrl,
	"iswupper":   goIswupper,
	"iswlower":   goIswlower,
	"iswpunct":   goIswpunct,
	"iswxdigit":  goIswxdigit,
	"towupper":   goTowupper,
	"towlower":   goTowlower,
	"iswalpha_l": goIswalphaL, "iswdigit_l": goIswdigitL,
	"iswspace_l": goIswspaceL, "iswprint_l": goIswprintL,
	"iswblank_l": goIswblankL, "iswcntrl_l": goIswcntrlL,
	"iswupper_l": goIswupperL, "iswlower_l": goIswlowerL,
	"iswpunct_l": goIswpunctL, "iswxdigit_l": goIswxdigitL,
	"towupper_l": goTowupperL, "towlower_l": goTowlowerL,

	// multibyte <-> wide (UTF-8)
	"mbrtowc":                goMbrtowc,
	"mbtowc":                 goMbtowc,
	"mbrlen":                 goMbrlen,
	"btowc":                  goBtowc,
	"wctob":                  goWctob,
	"__ctype_get_mb_cur_max": goCtypeGetMbCurMax,
	"wcrtomb":                goWcrtomb,
	"mbsrtowcs":              goMbsrtowcs,
	"wcsrtombs":              goWcsrtombs,
	"mbsnrtowcs":             goMbsnrtowcs,
	"wcsnrtombs":             goWcsnrtombs,

	// additional math
	"acosf": goAcosF, "asinf": goAsinF, "atanf": goAtanF, "atan2f": goAtan2F,
	"cbrtf": goCbrtF, "coshf": goCoshF, "sinhf": goSinhF, "tanhf": goTanhF,
	"erff": goErfF, "erfcf": goErfcF, "exp2f": goExp2F, "log2f": goLog2F,
	"nextafterf": goNextafterF, "remainderf": goRemainderF, "remquof": goRemquoF,
	"modff": goModfF, "frexpf": goFrexpF, "ldexpf": goLdexpf, "finitef": goFinitef,
	"ilogb": goIlogb, "llroundf": goLlroundf, "lroundf": goLroundf,
	"llround": goLlround, "lround": goLround, "sincos": goSincos, "sincosf": goSincosF,
	"strtod": goStrtod, "strtod_l": goStrtodL, "strtof": goStrtof, "strtof_l": goStrtofL,
	"atof": goAtof, "nan": goNan,

	// random numbers, sorting, searching
	"srand": goSrand, "rand": goRand,
	"qsort": goQsort, "bsearch": goBsearch,
	"abs": goAbs, "labs": goLabs, "llabs": goLabs2,

	// time decomposition
	"gmtime": goGmtime, "gmtime_r": goGmtimeR,
	"localtime": goLocaltime, "localtime_r": goLocaltimeR,
	"mktime": goMktime, "difftime": goDifftime,
	"tzset": goTzset, "clock": goClock,
	"strftime": goStrftime, "strftime_l": goStrftimeL,
	"getaddrinfo": goGetaddrinfo,
	"freeaddrinfo": goFreeaddrinfo,
	"gai_strerror": goGaiStrerror,
	"inet_ntop": goInetNtop,
}


func init() {
	// __strcpy_chk is a real bionic function; fix the placeholder alias.
	hostFuncs["__strcpy_chk"] = any(goStrcpyChk)
}

var (
	symMu    sync.Mutex
	symCache map[string]uintptr
	// rawSyms maps a guest import name directly to a host address. Unlike
	// hostFuncs (Go implementations reached through thunks), a raw symbol is
	// bound straight into the guest's GOT, so the engine calls the host function
	// with the native ABI. The graphics layer uses this for GPU and window-system
	// entry points, whose argument lists (floating point, more than six integers)
	// the integer-only thunk marshaller cannot carry.
	rawSyms = map[string]uintptr{}
)

// rawResolver is an optional last-chance oracle consulted before a name is
// bound to a trap thunk. The graphics layer installs one that dlsym()s unknown
// names from the host GPU libraries, so every gl*/egl* import the engine has is
// bound to the real Mesa implementation without enumerating them here.
var rawResolver func(name string) (uintptr, bool)

// RegisterRawSymbol binds a guest import to a host address. Call it before the
// loader applies relocations; an already-cached resolution is dropped.
func RegisterRawSymbol(name string, addr uintptr) {
	symMu.Lock()
	rawSyms[name] = addr
	delete(symCache, name)
	symMu.Unlock()
}

// Lookup returns the address the link-time resolver should bind name to.
// implemented reports whether the address is a real implementation; when it is
// false the address is a trap thunk that aborts if called.
func Lookup(name string) (addr uintptr, implemented bool) {
	symMu.Lock()
	defer symMu.Unlock()
	if symCache == nil {
		symCache = map[string]uintptr{}
	}
	if a, ok := symCache[name]; ok {
		return a, a != 0
	}
	if id, ok := variadicID[name]; ok {
		a := mkVariadicThunk(id)
		symCache[name] = a
		arenaMakeExec()
		return a, true
	}
	if fn, ok := hostFuncs[name]; ok {
		a := mkThunk(funcAddr(fn))
		symCache[name] = a
		arenaMakeExec()
		return a, true
	}
	if a, ok := rawSyms[name]; ok {
		symCache[name] = a
		return a, true
	}
	if rawResolver != nil {
		if a, ok := rawResolver(name); ok {
			rawSyms[name] = a
			symCache[name] = a
			return a, true
		}
	}
	if d, ok := dataThunks()[name]; ok {
		symCache[name] = d
		return d, true
	}
	if a, ok := rawCodeSymbol(name); ok {
		symCache[name] = a
		return a, true
	}
	a := trapThunk(name)
	symCache[name] = a
	arenaMakeExec()
	return a, false
}

var (
	dataOnce sync.Once
	dataMap  map[string]uintptr
)

func dataThunks() map[string]uintptr {
	dataOnce.Do(func() { dataMap = DataSymbols() })
	return dataMap
}

// IsData reports whether name is a data object provided by the host.
func IsData(name string) bool {
	_, ok := dataThunks()[name]
	return ok
}

// DataSymbol returns the address of a host data object, if the host provides it.
func DataSymbol(name string) (uintptr, bool) {
	a, ok := dataThunks()[name]
	return a, ok
}

// Symbols returns a snapshot of every symbol the host can fully provide.
func Symbols() map[string]uintptr {
	m := map[string]uintptr{}
	for name := range hostFuncs {
		a, ok := Lookup(name)
		if ok {
			m[name] = a
		}
	}
	for name, a := range dataThunks() {
		m[name] = a
	}
	return m
}

// TrapAddr returns (creating if necessary) a failing stub for name.
func TrapAddr(name string) uintptr {
	a, ok := Lookup(name)
	if ok {
		return a
	}
	return a
}
