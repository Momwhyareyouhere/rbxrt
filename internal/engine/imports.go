package engine

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// providerRule matches one provider library.
type providerRule struct {
	lib      string // canonical library: "libc.so", "libandroid.so", …
	cl       ImportClass
	prefixes []string
	exact    map[string]struct{}
}

func (r providerRule) match(name string) bool {
	if _, ok := r.exact[name]; ok {
		return true
	}
	for _, p := range r.prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// providerRules is consulted in order; the first match wins. Versioned symbols
// (VersionLib) short-circuit this and classify directly by their library name.
var providerRules = []providerRule{
	// ---- libdl.so: dynamic loading (host provides dlopen/dlsym…) ----
	{lib: "libdl.so", cl: ClassHost, exact: set(
		"dlopen", "dlclose", "dlsym", "dladdr", "dlvsym", "dlerror", "dl_iterate_phdr",
	)},
	// ---- libm.so: math (host) ----
	{lib: "libm.so", cl: ClassHost, prefixes: []string{
		"nextafter", "remainder", "scalbln", "scalbn", "nearbyint", "lrint", "llrint",
		"fma", "fmax", "fmin", "fdim", "nan", "nanf", "nanl", "exp", "log", "pow",
		"sin", "cos", "tan", "asin", "acos", "atan", "sinh", "cosh", "tanh", "asinh",
		"acosh", "atanh", "atan2", "cbrt", "hypot", "erf", "erfc", "tgamma", "lgamma",
		"ceil", "floor", "round", "trunc", "fabs", "frexp", "ldexp", "modf", "sqrt",
		"cabs", "carg", "cimag", "creal", "conj", "proj", "dnrm2", "significand",
	}, exact: set(
		"acosf", "acoshf", "acoshl", "acosl", "asinf", "asinhf", "asinhl", "asinl",
		"atanf", "atanhf", "atanhl", "atanl", "atan2f", "atan2l", "cbrtf", "cbrtl",
		"ceilf", "ceill", "copysign", "copysignf", "copysignl", "cosf", "coshf",
		"coshl", "cosl", "erff", "erfl", "erfcf", "erfcl", "exp2", "exp2f", "exp2l",
		"expf", "expl", "expm1", "expm1f", "expm1l", "fabsf", "fabsl", "fdimf",
		"fdiml", "floorf", "floorl", "fmaf", "fmal", "fmaxf", "fmaxl", "fminf",
		"fminl", "fmod", "fmodf", "fmodl", "frexpf", "frexpl", "hypotf", "hypotl",
		"ilogb", "ilogbf", "ilogbl", "ldexpf", "ldexpl", "lgammaf", "lgammal",
		"llrintf", "llrintl", "llround", "llroundf", "llroundl", "log10", "log10f",
		"log10l", "log1p", "log1pf", "log1pl", "log2", "log2f", "log2l", "logbf",
		"logbl", "logf", "logl", "lrintf", "lrintl", "lround", "lroundf", "lroundl",
		"modff", "modfl", "nanl", "nearbyintf", "nearbyintl", "nextafterf",
		"nextafterl", "nexttoward", "nexttowardf", "nexttowardl", "powf", "powl",
		"remainderf", "remainderl", "remquo", "remquof", "remquol", "rint", "rintf",
		"rintl", "roundf", "roundl", "scalblnf", "scalblnl", "scalbnf", "scalbnl",
		"significandf", "significandl", "sinf", "sinhf", "sinhl", "sinl", "sqrtf",
		"sqrtl", "tanf", "tanhf", "tanhl", "tanl", "tgammaf", "tgammal", "truncf",
		"truncl", "j0", "j0f", "j0l", "j1", "j1f", "j1l", "jn", "jnf", "jnl",
		"y0", "y0f", "y0l", "y1", "y1f", "y1l", "yn", "ynf", "ynl", "drem", "dremf",
	)},
	// ---- liblog.so: Android logging (shim) ----
	{lib: "liblog.so", cl: ClassHost, prefixes: []string{"__android_log"},
		exact: set("__android_log_print", "__android_log_vprint", "__android_log_write",
			"__android_log_assert", "__android_log_buf_write", "__android_log_buf_print",
			"__android_log_is_loggable", "__android_log_set_minimum_priority",
			"__android_log_get_minimum_priority")},
	// ---- libandroid.so: NDK platform services (shim) ----
	{lib: "libandroid.so", cl: ClassAndroidLib, prefixes: []string{
		"AAsset", "AAssetManager", "AConfiguration", "ALooper", "ANativeActivity",
		"ANativeWindow", "ASurfaceTexture", "AImageReader", "AHardwareBuffer",
		"ASharedMemory", "AInputQueue", "AMotionEvent", "AKeyEvent", "AInputEvent",
		"AFontMatcher", "AFont", "ACameraMetadata", "ACameraManager", "ACameraDevice",
		"ACameraCaptureSession", "ACaptureRequest", "ACaptureSessionOutput",
		"ACaptureSessionOutputContainer", "AChoreographer", "ADisplay", "AKeyCharacterMap",
		"AMedia", "ANetwork", "AReadableByteChannel", "AStreamGeometry", "AStream",
		"ASurface", "AWindow",
	}, exact: set(
		"AAssetManager_fromJava", "AAssetManager_open", "AAssetManager_openDir",
		"AAssetManager_getDirectoryName", "AAsset_getLength", "AAsset_getRemainingLength",
		"AAsset_read", "AAsset_seek", "AAsset_close", "AAsset_getBuffer",
		"ALooper_prepare", "ALooper_prepareWithOpts", "ALooper_acquire", "ALooper_release",
		"ALooper_pollOnce", "ALooper_pollAll", "ALooper_wake", "ALooper_addFd",
		"ALooper_removeFd", "ALooper_getFd",
		"ANativeWindow_acquire", "ANativeWindow_release", "ANativeWindow_setBuffersGeometry",
		"ANativeWindow_lock", "ANativeWindow_unlockAndPost", "ANativeWindow_setBuffersTransform",
		"ANativeWindow_getWidth", "ANativeWindow_getHeight",
		"ANativeActivity_setWindowFlags", "ANativeActivity_setWindowFormat",
		"ANativeActivity_showSoftInput", "ANativeActivity_hideSoftInput",
		"ASurfaceTexture_acquire", "ASurfaceTexture_release", "ASurfaceTexture_attachToGLContext",
		"ASurfaceTexture_detachFromGLContext", "ASurfaceTexture_updateTexImage",
		"ASurfaceTexture_getTimestamp", "ASurfaceTexture_getTransformMatrix",
		"ASurfaceTexture_setDefaultBufferSize",
		"AHardwareBuffer_allocate", "AHardwareBuffer_acquire", "AHardwareBuffer_release",
		"AHardwareBuffer_lock", "AHardwareBuffer_unlock", "AHardwareBuffer_describe",
		"AHardwareBuffer_isSupported", "AHardwareBuffer_getNativeHandle",
		"AHardwareBuffer_createFromHardwareBuffer", "AHardwareBuffer_fromHardwareBuffer",
		"AHardwareBuffer_setRequiredUsageFlags",
		"ASharedMemory_create", "ASharedMemory_setProt", "ASharedMemory_getSize", "ASharedMemory_dupFromJava",
		"ATrace_beginSection", "ATrace_endSection", "ATrace_beginAsyncSection",
		"ATrace_endAsyncSection", "ATrace_isEnabled", "ATrace_setCounter",
	)},
	// ---- libGLESv2.so / libGLESv3.so ----
	{lib: "libGLESv2.so", cl: ClassAndroidLib, prefixes: []string{"gl"},
		exact: set("glActiveTexture", "glAttachShader", "glBindAttribLocation",
			"glBindBuffer", "glBindFramebuffer", "glBindRenderbuffer", "glBindTexture",
			"glBlendColor", "glBlendEquation", "glBlendEquationSeparate", "glBlendFunc",
			"glBlendFuncSeparate", "glBufferData", "glBufferSubData", "glCheckFramebufferStatus",
			"glClear", "glClearColor", "glClearDepthf", "glClearStencil", "glColorMask",
			"glCompileShader", "glCompressedTexImage2D", "glCompressedTexSubImage2D",
			"glCopyTexImage2D", "glCopyTexSubImage2D", "glCreateProgram", "glCreateShader",
			"glCullFace", "glDeleteBuffers", "glDeleteFramebuffers", "glDeleteProgram",
			"glDeleteRenderbuffers", "glDeleteShader", "glDeleteTextures", "glDepthFunc",
			"glDepthMask", "glDepthRangef", "glDetachShader", "glDisable", "glDisableVertexAttribArray",
			"glDrawArrays", "glDrawElements", "glEnable", "glEnableVertexAttribArray",
			"glFinish", "glFlush", "glFramebufferRenderbuffer", "glFramebufferTexture2D",
			"glFrontFace", "glGenBuffers", "glGenerateMipmap", "glGenFramebuffers",
			"glGenRenderbuffers", "glGenTextures", "glGetActiveAttrib", "glGetActiveUniform",
			"glGetAttachedShaders", "glGetAttribLocation", "glGetBooleanv", "glGetBufferParameteriv",
			"glGetError", "glGetFloatv", "glGetFramebufferAttachmentParameteriv", "glGetIntegerv",
			"glGetProgramInfoLog", "glGetProgramiv", "glGetRenderbufferParameteriv",
			"glGetShaderInfoLog", "glGetShaderiv", "glGetShaderPrecisionFormat",
			"glGetString", "glGetTexParameterfv", "glGetTexParameteriv", "glGetUniformfv",
			"glGetUniformiv", "glGetUniformLocation", "glGetVertexAttribfv", "glGetVertexAttribiv",
			"glGetVertexAttribPointerv", "glHint", "glIsBuffer", "glIsEnabled", "glIsFramebuffer",
			"glIsProgram", "glIsRenderbuffer", "glIsShader", "glIsTexture", "glLineWidth",
			"glLinkProgram", "glPixelStorei", "glPolygonOffset", "glReadPixels", "glReleaseShaderCompiler",
			"glRenderbufferStorage", "glSampleCoverage", "glScissor", "glShaderBinary",
			"glShaderSource", "glStencilFunc", "glStencilFuncSeparate", "glStencilMask",
			"glStencilMaskSeparate", "glStencilOp", "glStencilOpSeparate", "glTexImage2D",
			"glTexParameterf", "glTexParameterfv", "glTexParameteri", "glTexParameteriv",
			"glTexSubImage2D", "glUniform1f", "glUniform1fv", "glUniform1i", "glUniform1iv",
			"glUniform2f", "glUniform2fv", "glUniform2i", "glUniform2iv", "glUniform3f",
			"glUniform3fv", "glUniform3i", "glUniform3iv", "glUniform4f", "glUniform4fv",
			"glUniform4i", "glUniform4iv", "glUniformMatrix2fv", "glUniformMatrix3fv",
			"glUniformMatrix4fv", "glUseProgram", "glValidateProgram", "glVertexAttrib1f",
			"glVertexAttrib1fv", "glVertexAttrib2f", "glVertexAttrib2fv", "glVertexAttrib3f",
			"glVertexAttrib3fv", "glVertexAttrib4f", "glVertexAttrib4fv", "glVertexAttribPointer",
			"glViewport")},
	// ---- libEGL.so ----
	{lib: "libEGL.so", cl: ClassAndroidLib, prefixes: []string{"egl"},
		exact: set("eglGetDisplay", "eglInitialize", "eglTerminate", "eglGetError",
			"eglGetProcAddress", "eglGetDisplayGLES2EXT", "eglBindAPI", "eglQueryAPI")},
	// ---- libOpenSLES.so ----
	{lib: "libOpenSLES.so", cl: ClassAndroidLib, prefixes: []string{"SL_", "IID_"},
		exact: set("slCreateEngine", "slInterfaceGetLength", "slInterfaceAcquire",
			"slInterfaceRelease")},
	// ---- libOpenMAXAL.so ----
	{lib: "libOpenMAXAL.so", cl: ClassAndroidLib, prefixes: []string{"XA_"},
		exact: set("xaCreateEngine", "xaInterfaceGetLength", "xaInterfaceAcquire", "xaInterfaceRelease")},
	// ---- libmediandk.so ----
	{lib: "libmediandk.so", cl: ClassAndroidLib, prefixes: []string{"AMedia"},
		exact: set("AMediaCodec_createDecoderByType", "AMediaCodec_createEncoderByType",
			"AMediaCodec_createInputSurface", "AMediaCodec_delete", "AMediaCodec_configure",
			"AMediaCodec_start", "AMediaCodec_stop", "AMediaCodec_flush", "AMediaCodec_release",
			"AMediaCodec_releaseInputBuffer", "AMediaCodec_releaseOutputBuffer",
			"AMediaCodec_dequeueInputBuffer", "AMediaCodec_dequeueOutputBuffer",
			"AMediaCodec_queueInputBuffer", "AMediaCodec_getInputBuffers", "AMediaCodec_getOutputBuffers",
			"AMediaCodec_start", "AMediaCodec_stop", "AMediaCodec_flush", "AMediaCodec_release",
			"AMediaCodec_releaseInputBuffer", "AMediaCodec_releaseOutputBuffer",
			"AMediaCodec_dequeueInputBuffer", "AMediaCodec_dequeueOutputBuffer",
			"AMediaCodec_queueInputBuffer", "AMediaCodec_getInputBuffers", "AMediaCodec_getOutputBuffers",
			"AMediaCodec_getOutputFormat", "AMediaCodec_getInputFormat", "AMediaCodec_getBufferInfo",
			"AMediaCodec_setParameters", "AMediaCodec_setOnFrameRenderedListener",
			"AMediaExtractor_new", "AMediaExtractor_delete", "AMediaExtractor_setDataSourceFd",
			"AMediaExtractor_setDataSource", "AMediaExtractor_getSampleSize", "AMediaExtractor_getSampleTime",
			"AMediaExtractor_getSampleTrackIndex", "AMediaExtractor_getSampleFlags",
			"AMediaExtractor_selectTrack", "AMediaExtractor_unselectTrack", "AMediaExtractor_readSampleData",
			"AMediaExtractor_advance", "AMediaExtractor_getTrackCount", "AMediaExtractor_getSampleFormat",
			"AMediaExtractor_getTrackFormat", "AMediaExtractor_seekTo", "AMediaFormat_new",
			"AMediaFormat_delete", "AMediaFormat_copy", "AMediaFormat_getInt32", "AMediaFormat_getInt64",
			"AMediaFormat_getFloat", "AMediaFormat_getString", "AMediaFormat_getBuffer",
			"AMediaFormat_getSize", "AMediaFormat_getRect", "AMediaFormat_setInt32", "AMediaFormat_setInt64",
			"AMediaFormat_setFloat", "AMediaFormat_setString", "AMediaFormat_setBuffer",
			"AMediaFormat_setSize", "AMediaFormat_setRect", "AMediaFormat_setByteBuffer",
			"AMediaFormat_getByteBuffer", "AMediaFormat_isEqual", "AMediaFormat_toString",
			"AMediaCrypto_new", "AMediaCrypto_delete", "AMediaCrypto_isCryptoSchemeSupported",
			"AMediaCrypto_requiresSecureDecoderComponent", "AMediaCrypto_setMediaDrmSession",
			"AMediaCodecInfo_getCodecCapabilities", "AMediaCodecInfo_getSupportedTypes",
			"AMediaCodecList_getCodecInfoAt", "AMediaCodecList_getNumCodecs",
			"AMediaCodecList_isEncoderSupported", "AMediaCodecList_isDecoderSupported",
			// AMediaFormat key constants are exported as data symbols.
			"AMEDIAMIMETYPE_VIDEO_AVC", "AMEDIAMIMETYPE_VIDEO_HEVC",
			"AMEDIAMIMETYPE_VIDEO_VP8", "AMEDIAMIMETYPE_VIDEO_VP9",
			"AMEDIAMIMETYPE_AUDIO_AAC", "AMEDIAMIMETYPE_AUDIO_MP3",
			"AMEDIAMIMETYPE_AUDIO_AC3", "AMEDIAMIMETYPE_AUDIO_EAC3",
			"AMEDIAMIMETYPE_AUDIO_FLAC", "AMEDIAMIMETYPE_AUDIO_OPUS",
			"AMEDIAMIMETYPE_AUDIO_RAW", "AMEDIAMIMETYPE_AUDIO_VORBIS",
			"AMEDIAFORMAT_KEY_AAC_PROFILE", "AMEDIAFORMAT_KEY_AAC_SBR_MODE",
			"AMEDIAFORMAT_KEY_AUDIO_SAMPLE_RATE", "AMEDIAFORMAT_KEY_BIT_RATE",
			"AMEDIAFORMAT_KEY_CHANNEL_COUNT", "AMEDIAFORMAT_KEY_COLOR_FORMAT",
			"AMEDIAFORMAT_KEY_COLOR_RANGE", "AMEDIAFORMAT_KEY_COLOR_STANDARD",
			"AMEDIAFORMAT_KEY_COLOR_TRANSFER", "AMEDIAFORMAT_KEY_COMPLEXITY",
			"AMEDIAFORMAT_KEY_COVERED", "AMEDIAFORMAT_KEY_CROP_BOTTOM",
			"AMEDIAFORMAT_KEY_CROP_LEFT", "AMEDIAFORMAT_KEY_CROP_RIGHT",
			"AMEDIAFORMAT_KEY_CROP_TOP", "AMEDIAFORMAT_KEY_DISPLAY_CROP",
			"AMEDIAFORMAT_KEY_DURATION", "AMEDIAFORMAT_KEY_FLAG_CODEC_CONFIG",
			"AMEDIAFORMAT_KEY_FLAG_PARTIAL_FRAME", "AMEDIAFORMAT_KEY_FRAME_RATE",
			"AMEDIAFORMAT_KEY_HDR_STATIC_INFO", "AMEDIAFORMAT_KEY_HEIGHT",
			"AMEDIAFORMAT_KEY_I_FRAME_INTERVAL", "AMEDIAFORMAT_KEY_IS_ADTS",
			"AMEDIAFORMAT_KEY_IS_AUTOSELECT", "AMEDIAFORMAT_KEY_IS_DEFAULT",
			"AMEDIAFORMAT_KEY_IS_FORCED_SUBTITLE", "AMEDIAFORMAT_KEY_IS_SYNC_FRAME",
			"AMEDIAFORMAT_KEY_KEY_SIZE", "AMEDIAFORMAT_KEY_LANGUAGE",
			"AMEDIAFORMAT_KEY_LEVEL", "AMEDIAFORMAT_KEY_MAX_BIT_RATE",
			"AMEDIAFORMAT_KEY_MAX_CROP_BOTTOM", "AMEDIAFORMAT_KEY_MAX_CROP_LEFT",
			"AMEDIAFORMAT_KEY_MAX_CROP_RIGHT", "AMEDIAFORMAT_KEY_MAX_CROP_TOP",
			"AMEDIAFORMAT_KEY_MAX_HEIGHT", "AMEDIAFORMAT_KEY_MAX_INPUT_SIZE",
			"AMEDIAFORMAT_KEY_MAX_WIDTH", "AMEDIAFORMAT_KEY_MIME",
			"AMEDIAFORMAT_KEY_MPEG2_STREAM_HEADER", "AMEDIAFORMAT_KEY_PCM_ENCODING",
			"AMEDIAFORMAT_KEY_PICTURE_LAYOUT", "AMEDIAFORMAT_KEY_PROFILE",
			"AMEDIAFORMAT_KEY_PROFILE_LEVEL", "AMEDIAFORMAT_KEY_PUSHBLOB_VERSION",
			"AMEDIAFORMAT_KEY_REPEAT_PREVIOUS_FRAME_AFTER", "AMEDIAFORMAT_KEY_ROTATION",
			"AMEDIAFORMAT_KEY_SAMPLE_RATE", "AMEDIAFORMAT_KEY_SLICE_HEIGHT",
			"AMEDIAFORMAT_KEY_STRIDE", "AMEDIAFORMAT_KEY_TEMPORAL_LAYERING",
			"AMEDIAFORMAT_KEY_TILE_HEIGHT", "AMEDIAFORMAT_KEY_TILE_WIDTH",
			"AMEDIAFORMAT_KEY_TRACK_INDEX", "AMEDIAFORMAT_KEY_TRACK_LANGUAGE",
			"AMEDIAFORMAT_KEY_TRACK_TYPE", "AMEDIAFORMAT_KEY_WIDTH",
			"AMEDIAFORMAT_KEY_CSD", "AMEDIAFORMAT_KEY_PCM_ENCODING",
			"AMEDIAFORMAT_KEY_SLICE_WIDTH")},
	// ---- libjnigraphics.so ----
	{lib: "libjnigraphics.so", cl: ClassAndroidLib, prefixes: []string{"AndroidBitmap"},
		exact: set("AndroidBitmap_getInfo", "AndroidBitmap_lockPixels", "AndroidBitmap_unlockPixels",
			"AndroidBitmap_compress")},
}

// classForLibrary maps a version-provider library name to an import class.
func classForLibrary(lib string) ImportClass {
	switch lib {
	case "libc.so", "libm.so", "libdl.so", "liblog.so":
		return ClassHost
	default:
		return ClassAndroidLib
	}
}

// ClassifyImports assigns every import a provider library and class. Versioned
// symbols are classified by their library; unversioned symbols are matched
// against the curated provider rules.
func (b *Binary) ClassifyImports() {
	libc := providerRule{lib: "libc.so", cl: ClassHost, prefixes: []string{
		"pthread_", "__pthread_", "mem", "str", "wc", "wmem", "fput", "fget", "fopen",
		"fclose", "fread", "fwrite", "malloc", "calloc", "realloc", "free", "mmap",
		"munmap", "mprotect", "madvise", "open", "close", "read", "write", "pread",
		"pwrite", "lseek", "fcntl", "ioctl", "socket", "bind", "listen", "accept",
		"connect", "send", "recv", "sendmsg", "recvmsg", "getaddrinfo", "getnameinfo",
		"select", "poll", "epoll", "eventfd", "pipe", "dup", "fork", "wait", "exec",
		"posix_", "clock_", "gettimeofday", "setitimer", "getitimer", "signal",
		"sigaction", "sigprocmask", "raise", "kill", "alarm", "getpid", "getuid",
		"getgid", "geteuid", "getegid", "getppid", "stat", "fstat", "lstat", "chmod",
		"chown", "access", "readlink", "symlink", "link", "unlink", "rename", "mkdir",
		"rmdir", "chdir", "getcwd", "umask", "truncate", "futimens", "utimensat",
		"nanosleep", "usleep", "sleep", "getrusage", "getrlimit", "setrlimit",
		"sysconf", "getpagesize", "gethostname", "uname", "isatty", "ttyname",
		"tcgetattr", "tcsetattr", "cfmakeraw", "getenv", "setenv", "unsetenv",
		"qsort", "bsearch", "rand", "srand", "random", "srandom", "abs", "labs",
		"llabs", "atoi", "atol", "atoll", "atof", "scanf", "printf", "sprintf",
		"snprintf", "vsprintf", "vsnprintf", "vprintf", "vscanf", "fprintf", "vfprintf",
		"sscanf", "strtol", "strtoll", "strtoul", "strtoull", "strtod", "strtof",
		"strtold", "cerror",
	}, exact: set(
		"__android_log_write", "gettid", "sched_getaffinity", "sched_setaffinity",
		"sched_yield", "sched_getcpu", "arc4random", "arc4random_buf", "getauxval",
		"getrandom", "prctl", "clone", "clone3", "syscall", "mincore", "munlockall",
		"mlockall", "mremap", "memset", "memcpy", "memmove", "memcmp", "memchr",
		"memccpy", "strlen", "strcmp", "strncmp", "strcpy", "strncpy", "strcat",
		"strncat", "strchr", "strrchr", "strstr", "strcasestr", "strtok", "strdup",
		"strndup", "strerror", "strerror_r", "strsignal", "strncpy", "strtoimax",
		"strtoumax", "wcstof", "wcstold", "wcstoll", "wcstoull", "wcstol", "wcstoul",
		"exit", "_exit", "abort", "atexit", "on_exit", "__cxa_atexit", "__cxa_finalize",
		"__cxa_pure_virtual", "__cxa_guard_acquire", "__cxa_guard_release",
		"__cxa_guard_abort", "__gxx_personality_v0", "_Unwind_Backtrace",
		"_Unwind_RaiseException", "_Unwind_GetIP", "__aeabi_unwind_cpp_pr0",
		"__aeabi_unwind_cpp_pr1", "__dso_handle", "__stack_chk_fail", "__stack_chk_guard",
		"__errno", "__errno_location", "errno", "pthread_create", "pthread_join",
		"pthread_detach", "pthread_mutex_init", "pthread_mutex_lock",
		"pthread_mutex_unlock", "pthread_mutex_destroy", "pthread_mutex_trylock",
		"pthread_cond_init", "pthread_cond_wait", "pthread_cond_timedwait",
		"pthread_cond_signal", "pthread_cond_broadcast", "pthread_cond_destroy",
		"pthread_once", "pthread_key_create", "pthread_key_delete", "pthread_getspecific",
		"pthread_setspecific", "pthread_self", "pthread_equal", "pthread_rwlock_init",
		"pthread_rwlock_rdlock", "pthread_rwlock_wrlock", "pthread_rwlock_unlock",
		"pthread_rwlock_tryrdlock", "pthread_rwlock_trywrlock", "pthread_rwlock_destroy",
		"pthread_attr_init", "pthread_attr_setdetachstate", "pthread_attr_destroy",
		"pthread_barrier_init", "pthread_barrier_wait", "pthread_barrier_destroy",
		"pthread_atfork", "pthread_kill", "pthread_sigmask", "pthread_cleanup_push",
		"sigemptyset", "sigfillset", "sigaddset", "sigdelset", "sigismember",
		"clock", "time", "gmtime", "gmtime_r", "localtime", "localtime_r", "mktime",
		"ctime", "asctime", "difftime", "tzset", "strftime", "gettimeofday",
		"in6addr_any", "in6addr_loopback", "inet_pton", "inet_ntop", "htons", "ntohs",
		"htonl", "ntohl", "getprotobyname", "gethostbyname", "setsockopt", "getsockopt",
		"shutdown", "getpeername", "getsockname", "socketpair", "setsid", "setpgid",
		"setpgrp", "getpgrp", "setuid", "setgid", "seteuid", "setegid", "getuid",
		// bionic fortify stubs (libc exports the _chk variants).
		"__memcpy_chk", "__memset_chk", "__memmove_chk", "__strcpy_chk",
		"__strncpy_chk", "__strcat_chk", "__strncat_chk", "__stpcpy_chk",
		"__stpncpy_chk", "__sprintf_chk", "__snprintf_chk", "__vsprintf_chk",
		"__vsnprintf_chk", "__fprintf_chk", "__vfprintf_chk", "__read_chk",
		"__pread_chk", "__write_chk", "__pwrite_chk", "__recv_chk",
		"__recvfrom_chk", "__poll_chk", "__ppoll_chk", "__getcwd_chk",
		"__realpath_chk", "__FD_CLR_chk", "__FD_SET_chk", "__FD_ISSET_chk",
		"__confstr_chk", "__getlogin_chk", "__getgroups_chk", "__getresuid_chk",
		"__open_chk", "__openat_chk", "__readlink_chk", "__snprintf_chk",
		"__socket_chk", "__ttyname_chk", "__tempnam_chk",
		// setjmp/longjmp family.
		"setjmp", "longjmp", "_setjmp", "_longjmp", "sigsetjmp", "siglongjmp",
		"__sigsetjmp",
		// misc + data symbols exported by libc.
		"getentropy", "localeconv", "setlocale", "strcoll", "strxfrm", "strsignal",
		"fcvt", "ecvt", "gcvt", "getopt", "getopt_long", "getopt_long_only",
		"stdin", "stdout", "stderr", "environ", "optarg", "optind", "opterr",
		"optopt", "__progname", "using_default_sig_sets", "_stack_chk_guard")}

	// Versioned symbols are authoritative; faster short-circuit.
	for _, s := range b.Imports {
		if s.VersionLib != "" {
			s.Provider = s.VersionLib
			s.Class = classForLibrary(s.VersionLib)
			continue
		}
		if libc.match(s.Name) {
			s.Provider = "libc.so"
			s.Class = ClassHost
			continue
		}
		matched := false
		for _, r := range providerRules {
			if r.match(s.Name) {
				s.Provider = r.lib
				s.Class = r.cl
				matched = true
				break
			}
		}
		if !matched {
			// Try the needed libraries by name to keep provider stable.
			for _, n := range b.Needed {
				if rule, ok := ruleForNeeded(n); ok && rule.match(s.Name) {
					s.Provider = n
					s.Class = rule.cl
					matched = true
					break
				}
			}
		}
		if !matched {
			s.Class = ClassStub
		}
	}
}

func ruleForNeeded(lib string) (providerRule, bool) {
	for _, r := range providerRules {
		if r.lib == lib {
			return r, true
		}
	}
	return providerRule{}, false
}

// ClassificationReport tallies imports per class and provider.
type ClassificationReport struct {
	ByClass    map[ImportClass]int
	ByProvider map[string]int
	Stubs      []*Symbol
}

// ReportClassification summarizes the import classification.
func (b *Binary) ReportClassification() *ClassificationReport {
	r := &ClassificationReport{
		ByClass:    map[ImportClass]int{},
		ByProvider: map[string]int{},
	}
	for _, s := range b.Imports {
		r.ByClass[s.Class]++
		r.ByProvider[s.Provider]++
		if s.Class == ClassStub {
			r.Stubs = append(r.Stubs, s)
		}
	}
	return r
}

// PrintClassification writes a human-readable classification summary.
func (r *ClassificationReport) PrintClassification(w io.Writer) {
	fmt.Fprintf(w, "imports by class:\n")
	for _, c := range []ImportClass{ClassHost, ClassAndroidLib, ClassStub, ClassUnclassified} {
		fmt.Fprintf(w, "  %-13s %d\n", c.String()+":", r.ByClass[c])
	}
	if len(r.ByProvider) > 0 {
		fmt.Fprintf(w, "by provider library:\n")
		libs := make([]string, 0, len(r.ByProvider))
		for lib := range r.ByProvider {
			libs = append(libs, lib)
		}
		sort.Strings(libs)
		for _, lib := range libs {
			if lib == "" {
				continue // unresolved stubs are listed below
			}
			fmt.Fprintf(w, "  %-20s %d\n", lib, r.ByProvider[lib])
		}
	}
	if len(r.Stubs) > 0 {
		fmt.Fprintf(w, "stub (resolve-or-fail-loudly) imports:\n")
		for _, s := range r.Stubs {
			if s.Bind == "WEAK" {
				fmt.Fprintf(w, "  %s%s  (weak -> bind zero)\n", s.Name, versionSuffix(s))
			} else {
				fmt.Fprintf(w, "  %s%s\n", s.Name, versionSuffix(s))
			}
		}
	}
}

func set(vals ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(vals))
	for _, v := range vals {
		m[v] = struct{}{}
	}
	return m
}

func versionSuffix(s *Symbol) string {
	if s.Version != "" {
		return "@" + s.Version
	}
	return ""
}
