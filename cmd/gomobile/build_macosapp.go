// Copyright 2015 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"fmt"
	"golang.org/x/tools/go/packages"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

func goMacOSBuild(pkg *packages.Package, bundleID string, archs []string) (map[string]bool, error) {
	src := pkg.PkgPath
	if buildO != "" && !strings.HasSuffix(buildO, ".app") {
		return nil, fmt.Errorf("-o must have an .app for -target=ios")
	}

	productName := rfc1034Label(path.Base(pkg.PkgPath))
	if productName == "" {
		productName = "ProductName" // like xcode.
	}

	projPbxproj := new(bytes.Buffer)
	if err := projPbxprojTmpl.Execute(projPbxproj, projPbxprojTmplData{
		BitcodeEnabled: bitcodeEnabled,
	}); err != nil {
		return nil, err
	}

	infoplist := new(bytes.Buffer)
	if err := infoplistTmpl.Execute(infoplist, infoplistTmplData{
		// TODO: better bundle id.
		BundleID: bundleID + "." + productName,
		Name:     strings.Title(path.Base(pkg.PkgPath)),
	}); err != nil {
		return nil, err
	}

	files := []struct {
		name     string
		contents []byte
	}{
		{tmpdir + "/main.xcodeproj/project.pbxproj", projPbxproj.Bytes()},
		{tmpdir + "/main/Info.plist", infoplist.Bytes()},
		{tmpdir + "/main/Images.xcassets/AppIcon.appiconset/Contents.json", []byte(contentsJSON)},
	}

	for _, file := range files {
		if err := mkdir(filepath.Dir(file.name)); err != nil {
			return nil, err
		}
		if buildX {
			printcmd("echo \"%s\" > %s", file.contents, file.name)
		}
		if !buildN {
			if err := ioutil.WriteFile(file.name, file.contents, 0644); err != nil {
				return nil, err
			}
		}
	}

	// We are using lipo tool to build multiarchitecture binaries.
	cmd := exec.Command(
		"xcrun", "lipo",
		"-o", filepath.Join(tmpdir, "main/main"),
		"-create",
	)
	var nmpkgs map[string]bool
	for _, arch := range archs {
		path := filepath.Join(tmpdir, arch)
		// Disable DWARF; see golang.org/issues/25148.
		if err := goBuild(src, macosEnv[arch], "-ldflags=-w", "-o="+path); err != nil {
			return nil, err
		}
		if nmpkgs == nil {
			var err error
			nmpkgs, err = extractPkgs(darwinArmNM, path)
			if err != nil {
				return nil, err
			}
		}
		cmd.Args = append(cmd.Args, path)
	}

	if err := runCmd(cmd); err != nil {
		return nil, err
	}

	// TODO(jbd): Set the launcher icon.
	if err := iosCopyAssets(pkg, tmpdir); err != nil {
		return nil, err
	}

	// Detect the team ID
	teamID, err := detectTeamID()
	if err != nil {
		return nil, err
	}

	// Build and move the release build to the output directory.
	cmdStrings := []string{
		"xcodebuild",
		"-configuration", "Release",
		"-project", tmpdir + "/main.xcodeproj",
		"-allowProvisioningUpdates",
		"DEVELOPMENT_TEAM=" + teamID,
	}

	cmd = exec.Command("xcrun", cmdStrings...)
	if err := runCmd(cmd); err != nil {
		return nil, err
	}

	// TODO(jbd): Fallback to copying if renaming fails.
	if buildO == "" {
		n := pkg.PkgPath
		if n == "." {
			// use cwd name
			cwd, err := os.Getwd()
			if err != nil {
				return nil, fmt.Errorf("cannot create .app; cannot get the current working dir: %v", err)
			}
			n = cwd
		}
		n = path.Base(n)
		buildO = n + ".app"
	}
	if buildX {
		printcmd("mv %s %s", tmpdir+"/build/Release-iphoneos/main.app", buildO)
	}
	if !buildN {
		// if output already exists, remove.
		if err := os.RemoveAll(buildO); err != nil {
			return nil, err
		}
		if err := os.Rename(tmpdir+"/build/Release-iphoneos/main.app", buildO); err != nil {
			return nil, err
		}
	}
	return nmpkgs, nil
}
