package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"rbxrt/internal/apk"
	"rbxrt/internal/hostabi"
	"rbxrt/internal/loader"
)

// The Java class names the engine looks up through JNI, as the APK's dex
// declares them. They are spelled the way the engine spells them in its
// FindClass/GetMethodID calls; a name that appears twice here is a typo.
const (
	classGLJava    = "com/roblox/engine/jni/NativeGLJavaInterface"
	classDevice    = "com/roblox/engine/jni/model/DeviceStaticParams"
	classPlatform  = "com/roblox/engine/jni/model/PlatformParams"
	classStartApp  = "com/roblox/engine/jni/autovalue/StartAppParams"
	classStartGame = "com/roblox/engine/jni/autovalue/StartGameParams"
	classInitArgs  = "com/roblox/engine/jni/autovalue/InitParams"
	classUser      = "com/roblox/engine/jni/user/NativeUserJavaInterface"
	classLocale    = "com/roblox/engine/jni/locale/NativeLocaleJavaInterface"
	classLogging   = "com/roblox/universalapp/logging/LoggingProtocol"

	// The activity the launch stands in for, and the framework objects it is
	// asked for. MainGameActivity is the APK class that declares
	// bootstrapTheApp() and getNativeHelper().
	classActivity       = "com/roblox/client/startup/MainGameActivity"
	classNativeHelper   = "com/roblox/client/startup/NativeHelper"
	classResources      = "android/content/res/Resources"
	classDisplayMetrics = "android/util/DisplayMetrics"
	classAssetManager   = "android/content/res/AssetManager"
	classConfiguration  = "android/content/res/Configuration"

	// androidx types the engine reads window insets through.
	classInsets     = "androidx/core/graphics/Insets"
	classInsetTypes = "androidx/core/view/WindowInsetsCompat$Type"
	classLoader     = "java/lang/ClassLoader"
)

var seededPlatform uintptr

// processStart is the reference point for the process timestamp the logging
// protocol reports.
var processStart = time.Now()

// rbxrtDataDir is the directory the engine may write to. On Android it is the
// app's internal data dir; here it is a scratch directory the launch creates.
func rbxrtDataDir() string {
	if d := os.Getenv("RBXRT_DATA"); d != "" {
		return d
	}
	return "/tmp/opencode/rbxrt-data"
}

// ensureDataDir creates the directories the Java side tells the engine it may
// use. The engine checks them with boost::filesystem::status while it is setting
// up its components, and this build of Boost reports a missing path by throwing,
// which unwinds out of the setup before it finishes. Creating them is part of
// standing in for the app; a failure is reported rather than ignored, because a
// missing directory would otherwise look like a mysterious crash much later.
func ensureDataDir() error {
	for _, name := range []string{"assets", "android", "StudioContent"} {
		path := filepath.Join(rbxrtDataDir(), name)
		if err := os.MkdirAll(path, 0o755); err != nil {
			return fmt.Errorf("create data directory %s: %w", path, err)
		}
	}
	// The asset-path native callback canonicalizes the Android platform asset
	// directory beneath the APK asset root during StartApp.
	if err := os.MkdirAll(filepath.Join(rbxrtDataDir(), "assets", "android"), 0o755); err != nil {
		return fmt.Errorf("create platform asset directory: %w", err)
	}
	if os.Getenv("RBXRT_SKIP_ASSET_STAGING") != "" {
		return nil
	}
	// Roblox looks up ExtraContent through the filesystem (unlike ordinary
	// packaged assets, which use AAssetManager), as <asset folder>/ExtraContent:
	// the same place it has in the APK's assets/. When that directory exists the
	// engine adds it as a content root, which is where rbxasset://models/...
	// resolves the bundled core-script patches (UniversalApp.rbxm); without it
	// StartApp finds no verified patch and aborts. Mirror the APK subtree there.
	apkPath := os.Getenv("RBXRT_APK")
	if apkPath == "" {
		apkPath = "roblox.apk"
	}
	archive, err := apk.OpenArchive(apkPath)
	if err != nil {
		return fmt.Errorf("open APK for ExtraContent: %w", err)
	}
	defer archive.Close()
	const prefix = "assets/ExtraContent/"
	for _, entry := range archive.Entries() {
		if !strings.HasPrefix(entry, prefix) {
			continue
		}
		rel := strings.TrimPrefix(entry, prefix)
		if rel == "" || filepath.IsAbs(rel) || filepath.Clean(rel) != rel || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe ExtraContent APK path %q", entry)
		}
		dst := filepath.Join(rbxrtDataDir(), "assets", "ExtraContent", rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("create ExtraContent directory %s: %w", filepath.Dir(dst), err)
		}
		if err := archive.Extract(entry, dst); err != nil {
			return fmt.Errorf("extract APK entry %s: %w", entry, err)
		}
	}
	// Packaged Roblox assets are read relative to the asset folder supplied by
	// Java. The native engine's filesystem fallback resolves rbxasset:// paths
	// beneath <asset folder>/content, so mirror that APK subtree too (for example
	// UniversalAppPatchConfig is required before StartApp can continue).
	const contentPrefix = "assets/content/"
	for _, entry := range archive.Entries() {
		if !strings.HasPrefix(entry, contentPrefix) {
			continue
		}
		rel := strings.TrimPrefix(entry, contentPrefix)
		if rel == "" || filepath.IsAbs(rel) || filepath.Clean(rel) != rel || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe content APK path %q", entry)
		}
		// Stage into the process asset root as well as the AAsset APK source;
		// some native filesystem paths strip rbxasset:// without adding content/.
		dst := filepath.Join(rbxrtDataDir(), "assets", "content", rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("create content directory %s: %w", filepath.Dir(dst), err)
		}
		if err := archive.Extract(entry, dst); err != nil {
			return fmt.Errorf("extract APK entry %s: %w", entry, err)
		}
	}
	// The engine's OpenSSL loads its trust store from exe/cacert.pem (and
	// probes exe/ssl/cacert.pem) relative to the working directory, which is
	// the data directory during a boot. Without it every TLS handshake fails
	// certificate verification. Use the bundle the signed APK ships.
	for _, dst := range []string{
		filepath.Join(rbxrtDataDir(), "exe", "cacert.pem"),
		filepath.Join(rbxrtDataDir(), "exe", "ssl", "cacert.pem"),
	} {
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("create CA bundle directory: %w", err)
		}
		if err := archive.Extract("assets/ssl/cacert.pem", dst); err != nil {
			return fmt.Errorf("extract CA bundle: %w", err)
		}
	}
	const androidPrefix = "assets/android/"
	for _, entry := range archive.Entries() {
		if !strings.HasPrefix(entry, androidPrefix) {
			continue
		}
		rel := strings.TrimPrefix(entry, androidPrefix)
		if rel == "" || filepath.IsAbs(rel) || filepath.Clean(rel) != rel || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe Android asset APK path %q", entry)
		}
		dst := filepath.Join(rbxrtDataDir(), "assets", "android", rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("create Android asset directory %s: %w", filepath.Dir(dst), err)
		}
		if err := archive.Extract(entry, dst); err != nil {
			return fmt.Errorf("extract APK entry %s: %w", entry, err)
		}
	}
	return nil
}

// setJavaStringValues gives the launch's String sentinels real content. The
// engine reads them through GetStringUTFChars, and an unset handle reads as the
// empty string - a wrong answer rather than a missing one, which is exactly the
// kind of silence this runtime is built to avoid.
func setJavaStringValues(im *loader.Image) {
	apk := os.Getenv("RBXRT_APK")
	if apk == "" {
		apk = "roblox.apk"
	}
	libDir := ""
	if im != nil && im.Eng != nil {
		libDir = filepath.Dir(im.Eng.Path)
	}
	hostabi.SetJavaString(javaAssetPath, apk)
	hostabi.SetJavaString(javaNativeLib, libDir)
	hostabi.SetJavaString(javaInternalDir, rbxrtDataDir())
}

// seedJavaValues answers the Java-side reads the engine makes while starting up:
// the device parameters, the platform parameters, the locale and the (guest)
// user. Every value here is one the real app supplies from its own classes;
// anything the engine asks for that is not here reports itself through
// jobject.go instead of passing as a zero.
//
// The engine reads these objects by field, not by method, so the fields are the
// ones the APK's model classes declare: DeviceStaticParams has the eight
// android.os.Build strings and cpu64Bit, PlatformParams the display geometry and
// input capabilities.
func seedJavaValues() {
	str := hostabi.NewJavaString

	// The device, as android.os.Build reports it.
	dev := hostabi.NewFakeObject(classDevice)
	hostabi.SetFakeMethodResult(classGLJava, "getDeviceStaticParams", dev)
	hostabi.SetFakeField(classDevice, "appBuildVariant", str("Production"))
	hostabi.SetFakeField(classDevice, "appVersion", str("0.0.0.0"))
	hostabi.SetFakeField(classDevice, "cpu64Bit", 1)
	hostabi.SetFakeField(classDevice, "deviceName", str("rbxrt"))
	hostabi.SetFakeField(classDevice, "deviceSku", str("rbxrt"))
	hostabi.SetFakeField(classDevice, "manufacturer", str("rbxrt"))
	hostabi.SetFakeField(classDevice, "osVersion", str("Linux"))
	hostabi.SetFakeField(classDevice, "socModel", str("x86_64"))

	// The platform: one logical pixel per pixel, a desktop-sized panel in
	// millimetres, and all three input devices present.
	platform := hostabi.NewFakeObject(classPlatform)
	seededPlatform = platform
	hostabi.SetFakeField(classPlatform, "assetFolderPath", str(filepath.Join(rbxrtDataDir(), "assets")))
	hostabi.SetFakeField(classPlatform, "dpiScale", uintptr(math.Float32bits(1)))
	hostabi.SetFakeField(classPlatform, "isKeyboardDevice", 1)
	hostabi.SetFakeField(classPlatform, "isMouseDevice", 1)
	hostabi.SetFakeField(classPlatform, "isTouchDevice", 1)
	hostabi.SetFakeField(classPlatform, "viewportWidthMm", 101)
	hostabi.SetFakeField(classPlatform, "viewportHeightMm", 57)

	// The app-start parameters: the surface the engine renders into (the same
	// synthetic Surface the lifecycle reports) and the platform above. No VR
	// activity, which the engine treats as the non-VR case.
	hostabi.SetFakeMethodResult(classStartApp, "surface", javaSurface)
	hostabi.SetFakeMethodResult(classStartApp, "platformParams", platform)
	hostabi.SetFakeMethodResult(classStartApp, "vrContext", 0)

	// StartGameParams is the follow-up Java value used by the activity after
	// StartApp. Keep its optional join metadata empty, but provide the same
	// surface and platform objects so the native path can create the render
	// view without an ART array or Activity instance.
	hostabi.SetFakeMethodResult(classStartGame, "surface", javaSurface)
	hostabi.SetFakeMethodResult(classStartGame, "platformParams", platform)
	hostabi.SetFakeMethodResult(classStartGame, "vrContext", 0)
	for _, method := range []string{
		"gameId", "accessCode", "linkCode", "username", "referralPage",
		"launchData", "joinAttemptId", "joinAttemptOrigin", "reservedServerAccessCode",
		"callId", "isoContext", "gameJoinContext", "eventId",
	} {
		hostabi.SetFakeMethodResult(classStartGame, method, str(""))
	}
	for _, method := range []string{"userId", "conversationId", "referredByPlayerId", "placeId", "joinRequestType", "isUnder13"} {
		hostabi.SetFakeMethodResult(classStartGame, method, 0)
	}
	hostabi.SetFakeMethodResult(classStartGame, "gameId", str("118593852151835"))
	hostabi.SetFakeMethodResult(classStartGame, "placeId", uintptr(118593852151835))
	hostabi.SetFakeMethodResult(classStartGame, "userId", uintptr(1))
	hostabi.SetFakeMethodResult(classStartGame, "username", str("rbxrt"))

	// InitParams: where the client would sign in and what it reports itself as.
	// The URLs are the real site's; networking is not realized, so anything that
	// reaches a socket stops there rather than at a wrong address.
	hostabi.SetFakeMethodResult(classInitArgs, "platformParams", platform)
	hostabi.SetFakeMethodResult(classInitArgs, "baseURL", str("https://www.roblox.com"))
	hostabi.SetFakeMethodResult(classInitArgs, "userAgent", str("rbxrt/0.0.0.0"))
	hostabi.SetFakeMethodResult(classInitArgs, "isTablet", 0)
	hostabi.SetFakeMethodResult(classInitArgs, "isVrDevice", 0)
	hostabi.SetFakeMethodResult("java/util/Collections$EmptyList", "size", 0)
	hostabi.SetFakeMethodResult("java/util/Collections$EmptyList", "isEmpty", 1)
	// nativePostClientSettingsLoadedInitialization3 accepts the Java List
	// interface, and JNI method IDs are keyed by their declaring interface, not
	// the concrete Collections$EmptyList implementation.
	hostabi.SetFakeMethodResult("java/util/List", "size", 0)
	hostabi.SetFakeMethodResult("java/util/List", "isEmpty", 1)
	// Leave optional array/iterator accessors unset: native code treats a null
	// result as an empty Java list, while a fake array has no ART array header.
	hostabi.SetFakeMethodResult("java/util/Iterator", "hasNext", 0)
	// A class loader for the reflection the engine does (it asks the Java side
	// for one, then looks classes up through it). The object is real to the
	// engine's hands; loadClass() reports itself.
	loader := hostabi.NewFakeObject(classLoader)
	hostabi.SetFakeMethodResult(classGLJava, "getClassLoader", loader)
	hostabi.SetFakeMethodResult("com/snapchat/djinni/NativeObjectManager", "getClassLoader", loader)

	// Window insets: every type is present and each inset is zero, so the safe
	// area is the whole window. The type bits are androidx's own (Type.java).
	for name, bit := range map[string]uintptr{
		"statusBars":              1,
		"navigationBars":          2,
		"captionBar":              4,
		"ime":                     8,
		"systemGestures":          16,
		"mandatorySystemGestures": 32,
		"tappableElement":         64,
		"displayCutout":           128,
		"systemBars":              7,
	} {
		hostabi.SetFakeMethodResult(classInsetTypes, name, bit)
	}
	zeroInsets := hostabi.NewFakeObject(classInsets)
	for _, side := range []string{"left", "top", "right", "bottom"} {
		hostabi.SetFakeField(classInsets, side, 0)
	}
	hostabi.SetFakeMethodResult(classActivity, "getWaterfallInsets", zeroInsets)
	hostabi.SetFakeMethodResult(classActivity, "getWindowInsets", zeroInsets)

	// The activity the launch stands in for. The engine calls back into it
	// during the lifecycle (its Java half is what starts the app), so the handle
	// the launch passes needs a class name and answers for those calls: there is
	// no Java to do the work, the boot below drives the engine directly.
	hostabi.NameJavaObject(javaActivity, classActivity)
	hostabi.SetFakeMethodResult(classActivity, "bootstrapTheApp", 0)
	hostabi.SetFakeMethodResult(classActivity, "getNativeHelper",
		hostabi.NewFakeObject(classNativeHelper))
	// During StartApp the engine reports the flag-provider handshake outcome to
	// the activity. The launch has no persisted flag overrides, so report
	// success: the failure path exits the app before any graphics start.
	// StartApp's asynchronous flag callback carries a ByteBuffer; return a
	// non-null buffer sentinel so native code follows the loaded/success path.
	hostabi.SetFakeMethodResult(classNativeHelper, "gameActivity_onFlagsLoaded",
		hostabi.NewFakeObject("java/nio/ByteBuffer"))
	hostabi.SetFakeMethodResult(classNativeHelper, "gameActivity_onFlagsFailed", 0)

	// getResources() and the display it describes: the engine sizes its output
	// from these numbers. One logical pixel per pixel, matching the 1280x720
	// window the launch creates.
	resources := hostabi.NewFakeObject(classResources)
	hostabi.SetFakeMethodResult(classActivity, "getResources", resources)
	metrics := hostabi.NewFakeObject(classDisplayMetrics)
	hostabi.SetFakeMethodResult(classResources, "getDisplayMetrics", metrics)
	hostabi.SetFakeField(classDisplayMetrics, "density", uintptr(math.Float32bits(1)))
	hostabi.SetFakeField(classDisplayMetrics, "scaledDensity", uintptr(math.Float32bits(1)))
	hostabi.SetFakeField(classDisplayMetrics, "densityDpi", 160)
	hostabi.SetFakeField(classDisplayMetrics, "widthPixels", 1280)
	hostabi.SetFakeField(classDisplayMetrics, "heightPixels", 720)
	hostabi.SetFakeField(classDisplayMetrics, "xdpi", uintptr(math.Float32bits(160)))
	hostabi.SetFakeField(classDisplayMetrics, "ydpi", uintptr(math.Float32bits(160)))
	hostabi.SetFakeMethodResult(classResources, "getAssets",
		hostabi.NewFakeObject(classAssetManager))
	hostabi.SetFakeMethodResult(classResources, "getConfiguration",
		hostabi.NewFakeObject(classConfiguration))

	// No signed-in user: the engine runs as a guest.
	hostabi.SetFakeMethodResult(classUser, "getUserId", 0)
	hostabi.SetFakeMethodResult(classUser, "getIsUnder13", 0)
	hostabi.SetFakeMethodResult(classUser, "getUsername", str(""))
	hostabi.SetFakeMethodResult(classUser, "getDisplayName", str(""))
	hostabi.SetFakeMethodResult(classUser, "getAlternateName", str(""))
	hostabi.SetFakeMethodResult(classUser, "getPlatformName", str("Android"))
	hostabi.SetFakeMethodResult(classUser, "getMembershipType", 0)
	hostabi.SetFakeMethodResult(classUser, "getHasRobloxSubscription", 0)
	hostabi.SetFakeMethodResult(classUser, "getTheme", str("dark"))

	// The locale: the runtime formats dates and numbers in the C locale, so the
	// engine is told the same.
	hostabi.SetFakeMethodResult(classLocale, "getLocale", str("en_us"))
	hostabi.SetFakeMethodResult(classLocale, "getRobloxLocale", str("en_us"))
	hostabi.SetFakeMethodResult(classLocale, "getGameLocale", str("en_us"))

	// Milliseconds since the process started, which is the unit the protocol
	// stamps its records with.
	hostabi.SetFakeMethodFunc(classLogging, "getProcessTimestamp",
		func() uintptr { return uintptr(time.Since(processStart).Milliseconds()) })
}
