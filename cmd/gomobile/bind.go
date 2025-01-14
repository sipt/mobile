// Copyright 2015 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/tools/go/packages"
)

var cmdBind = &command{
	run:   runBind,
	Name:  "bind",
	Usage: "[-target android|ios] [-bootclasspath <path>] [-classpath <path>] [-o output] [build flags] [package]",
	Short: "build a library for Android and iOS",
	Long: `
Bind generates language bindings for the package named by the import
path, and compiles a library for the named target system.

The -target flag takes a target system name, either android (the
default) or ios.

For -target android, the bind command produces an AAR (Android ARchive)
file that archives the precompiled Java API stub classes, the compiled
shared libraries, and all asset files in the /assets subdirectory under
the package directory. The output is named '<package_name>.aar' by
default. This AAR file is commonly used for binary distribution of an
Android library project and most Android IDEs support AAR import. For
example, in Android Studio (1.2+), an AAR file can be imported using
the module import wizard (File > New > New Module > Import .JAR or
.AAR package), and setting it as a new dependency
(File > Project Structure > Dependencies).  This requires 'javac'
(version 1.7+) and Android SDK (API level 15 or newer) to build the
library for Android. The environment variable ANDROID_HOME must be set
to the path to Android SDK. Use the -javapkg flag to specify the Java
package prefix for the generated classes.

By default, -target=android builds shared libraries for all supported
instruction sets (arm, arm64, 386, amd64). A subset of instruction sets
can be selected by specifying target type with the architecture name. E.g.,
-target=android/arm,android/386.

For -target ios, gomobile must be run on an OS X machine with Xcode
installed. The generated Objective-C types can be prefixed with the -prefix
flag.

For -target android, the -bootclasspath and -classpath flags are used to
control the bootstrap classpath and the classpath for Go wrappers to Java
classes.

The -v flag provides verbose output, including the list of packages built.

The build flags -a, -n, -x, -gcflags, -ldflags, -tags, -trimpath, and -work
are shared with the build command. For documentation, see 'go help build'.
`,
}

func runBind(cmd *command) error {
	cleanup, err := buildEnvInit()
	if err != nil {
		return err
	}
	defer cleanup()

	args := cmd.flag.Args()

	targetOS, targetArchs, err := parseBuildTarget(buildTarget)
	if err != nil {
		return fmt.Errorf(`invalid -target=%q: %v`, buildTarget, err)
	}

	if bindJavaPkg != "" && targetOS != "android" {
		return fmt.Errorf("-javapkg is supported only for android target")
	}
	if bindPrefix != "" && targetOS != "ios" && targetOS != "macos" {
		return fmt.Errorf("-prefix is supported only for ios/macos target")
	}

	if targetOS == "android" {
		if _, err := ndkRoot(); err != nil {
			return err
		}
	}

	var gobind string
	if !buildN {
		gobind, err = exec.LookPath("gobind")
		if err != nil {
			return errors.New("gobind was not found. Please run gomobile init before trying again.")
		}
	} else {
		gobind = "gobind"
	}

	if len(args) == 0 {
		args = append(args, ".")
	}
	pkgs, err := importPackages(args, targetOS)
	if err != nil {
		return err
	}

	// check if any of the package is main
	for _, pkg := range pkgs {
		if pkg.Name == "main" {
			return fmt.Errorf("binding 'main' package (%s) is not supported", pkg.PkgPath)
		}
	}

	switch targetOS {
	case "android":
		return goAndroidBind(gobind, pkgs, targetArchs)
	case "ios":
		if !xcodeAvailable() {
			return fmt.Errorf("-target=ios requires XCode")
		}
		return goIOSBind(gobind, pkgs, targetArchs)
	case "macos":
		if !xcodeAvailable() {
			return fmt.Errorf("-target=macos requires XCode")
		}
		return goMacOSBind(gobind, pkgs, targetArchs)
	default:
		return fmt.Errorf(`invalid -target=%q`, buildTarget)
	}
}

func importPackages(args []string, targetOS string) ([]*packages.Package, error) {
	config := packagesConfig(targetOS)
	return packages.Load(config, args...)
}

var (
	bindPrefix        string // -prefix
	bindJavaPkg       string // -javapkg
	bindClasspath     string // -classpath
	bindBootClasspath string // -bootclasspath
)

func init() {
	// bind command specific commands.
	cmdBind.flag.StringVar(&bindJavaPkg, "javapkg", "",
		"specifies custom Java package path prefix. Valid only with -target=android.")
	cmdBind.flag.StringVar(&bindPrefix, "prefix", "",
		"custom Objective-C name prefix. Valid only with -target=ios.")
	cmdBind.flag.StringVar(&bindClasspath, "classpath", "", "The classpath for imported Java classes. Valid only with -target=android.")
	cmdBind.flag.StringVar(&bindBootClasspath, "bootclasspath", "", "The bootstrap classpath for imported Java classes. Valid only with -target=android.")
}

func bootClasspath() (string, error) {
	if bindBootClasspath != "" {
		return bindBootClasspath, nil
	}
	apiPath, err := androidAPIPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(apiPath, "android.jar"), nil
}

func copyFile(dst, src string) error {
	if buildX {
		printcmd("cp %s %s", src, dst)
	}
	return writeFile(dst, func(w io.Writer) error {
		if buildN {
			return nil
		}
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()

		if _, err := io.Copy(w, f); err != nil {
			return fmt.Errorf("cp %s %s failed: %v", src, dst, err)
		}
		return nil
	})
}

func writeFile(filename string, generate func(io.Writer) error) error {
	if buildV {
		fmt.Fprintf(os.Stderr, "write %s\n", filename)
	}

	if err := mkdir(filepath.Dir(filename)); err != nil {
		return err
	}

	if buildN {
		return generate(ioutil.Discard)
	}

	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()

	return generate(f)
}

func packagesConfig(targetOS string) *packages.Config {
	config := &packages.Config{}
	// Add CGO_ENABLED=1 explicitly since Cgo is disabled when GOOS is different from host OS.
	var goos = targetOS
	if goos == "macos" || goos == "ios" {
		goos = "darwin"
	}
	config.Env = append(os.Environ(), "GOARCH=arm64", "GOOS="+goos, "CGO_ENABLED=1")
	tags := buildTags
	if targetOS == "ios" {
		tags = append(tags, "ios")
	}
	if len(tags) > 0 {
		config.BuildFlags = []string{"-tags=" + strings.Join(tags, ",")}
	}
	return config
}

// getModuleVersions returns a module information at the directory src.
func getModuleVersions(targetOS string, targetArch string, src string) (*modfile.File, error) {
	cmd := exec.Command("go", "list")
	cmd.Env = append(os.Environ(), "GOOS="+targetOS, "GOARCH="+targetArch)

	tags := buildTags
	if targetOS == "darwin" {
		tags = append(tags, "ios")
	}
	// TODO(hyangah): probably we don't need to add all the dependencies.
	cmd.Args = append(cmd.Args, "-m", "-json", "-tags="+strings.Join(tags, ","), "all")
	cmd.Dir = src

	output, err := cmd.Output()
	if err != nil {
		// Module information is not available at src.
		return nil, nil
	}

	type Module struct {
		Main    bool
		Path    string
		Version string
		Dir     string
		Replace *Module
	}

	f := &modfile.File{}
	f.AddModuleStmt("gobind")
	e := json.NewDecoder(bytes.NewReader(output))
	for {
		var mod *Module
		err := e.Decode(&mod)
		if err != nil && err != io.EOF {
			return nil, err
		}
		if mod != nil {
			if mod.Replace != nil {
				p, v := mod.Replace.Path, mod.Replace.Version
				if modfile.IsDirectoryPath(p) {
					// replaced by a local directory
					p = mod.Replace.Dir
				}
				f.AddReplace(mod.Path, mod.Version, p, v)
			} else {
				// When the version part is empty, the module is local and mod.Dir represents the location.
				if v := mod.Version; v == "" {
					f.AddReplace(mod.Path, mod.Version, mod.Dir, "")
				} else {
					f.AddRequire(mod.Path, v)
				}
			}
		}
		if err == io.EOF {
			break
		}
	}
	return f, nil
}

// writeGoMod writes go.mod file at $WORK/src when Go modules are used.
func writeGoMod(targetOS string, targetArch string) error {
	m, err := areGoModulesUsed()
	if err != nil {
		return err
	}
	// If Go modules are not used, go.mod should not be created because the dependencies might not be compatible with Go modules.
	if !m {
		return nil
	}

	return writeFile(filepath.Join(tmpdir, "src", "go.mod"), func(w io.Writer) error {
		f, err := getModuleVersions(targetOS, targetArch, ".")
		if err != nil {
			return err
		}
		if f == nil {
			return nil
		}
		bs, err := f.Format()
		if err != nil {
			return err
		}
		if _, err := w.Write(bs); err != nil {
			return err
		}
		return nil
	})
}

func areGoModulesUsed() (bool, error) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return false, err
	}
	outstr := strings.TrimSpace(string(out))
	if outstr == "" {
		return false, nil
	}
	return true, nil
}
