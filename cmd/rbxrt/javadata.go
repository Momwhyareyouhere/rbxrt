package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"rbxrt/internal/hostabi"
	"rbxrt/internal/loader"
)

// The Java class names the engine looks up through JNI, as the APK's dex
// declares them. They are spelled the way the engine spells them in its
// FindClass/GetMethodID calls; a name that appears twice here is a typo.
const (
	classGLJava   = "com/roblox/engine/jni/NativeGLJavaInterface"
	classDevice   = "com/roblox/engine/jni/model/DeviceStaticParams"
	classPlatform = "com/roblox/engine/jni/model/PlatformParams"
	classStartApp = "com/roblox/engine/jni/autovalue/StartAppParams"
	classInitArgs = "com/roblox/engine/jni/autovalue/InitParams"
	classUser     = "com/roblox/engine/jni/user/NativeUserJavaInterface"
	classLocale   = "com/roblox/engine/jni/locale/NativeLocaleJavaInterface"
	classLogging  = "com/roblox/universalapp/logging/LoggingProtocol"

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
	assets := filepath.Join(rbxrtDataDir(), "assets")
	if err := os.MkdirAll(assets, 0o755); err != nil {
		return fmt.Errorf("create data directory %s: %w", assets, err)
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

	// InitParams: where the client would sign in and what it reports itself as.
	// The URLs are the real site's; networking is not realized, so anything that
	// reaches a socket stops there rather than at a wrong address.
	hostabi.SetFakeMethodResult(classInitArgs, "platformParams", platform)
	hostabi.SetFakeMethodResult(classInitArgs, "baseURL", str("https://www.roblox.com"))
	hostabi.SetFakeMethodResult(classInitArgs, "userAgent", str("rbxrt/0.0.0.0"))
	hostabi.SetFakeMethodResult(classInitArgs, "isTablet", 0)
	hostabi.SetFakeMethodResult(classInitArgs, "isVrDevice", 0)

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
