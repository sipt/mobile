// Copyright 2015 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"golang.org/x/tools/go/packages"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
)

func goMacOSBind(gobind string, pkgs []*packages.Package, archs []string) error {
	// Run gobind to generate the bindings
	cmd := exec.Command(
		gobind,
		"-lang=go,objc",
		"-outdir="+tmpdir,
	)
	cmd.Env = append(cmd.Env, "GOOS=darwin")
	cmd.Env = append(cmd.Env, "CGO_ENABLED=1")
	tags := buildTags
	cmd.Args = append(cmd.Args, "-tags="+strings.Join(tags, ","))
	if bindPrefix != "" {
		cmd.Args = append(cmd.Args, "-prefix="+bindPrefix)
	}
	for _, p := range pkgs {
		cmd.Args = append(cmd.Args, p.PkgPath)
	}
	if err := runCmd(cmd); err != nil {
		return err
	}

	srcDir := filepath.Join(tmpdir, "src", "gobind")

	var name string
	var title string
	if buildO == "" {
		name = pkgs[0].Name
		title = strings.Title(name)
		buildO = title + ".framework"
	} else {
		if !strings.HasSuffix(buildO, ".framework") {
			return fmt.Errorf("static framework name %q missing .framework suffix", buildO)
		}
		base := filepath.Base(buildO)
		name = base[:len(base)-len(".framework")]
		title = strings.Title(name)
	}

	fileBases := make([]string, len(pkgs)+1)
	for i, pkg := range pkgs {
		fileBases[i] = bindPrefix + strings.Title(pkg.Name)
	}
	fileBases[len(fileBases)-1] = "Universe"

	cmd = exec.Command("xcrun", "lipo", "-create")

	modulesUsed, err := areGoModulesUsed()
	if err != nil {
		return err
	}

	for _, arch := range archs {
		if err := writeGoMod("darwin", arch); err != nil {
			return err
		}

		env := macosEnv[arch]
		// Add the generated packages to GOPATH for reverse bindings.
		gopath := fmt.Sprintf("GOPATH=%s%c%s", tmpdir, filepath.ListSeparator, goEnv("GOPATH"))
		env = append(env, gopath)

		// Run `go mod tidy` to force to create go.sum.
		// Without go.sum, `go build` fails as of Go 1.16.
		if modulesUsed {
			if err := goModTidyAt(filepath.Join(tmpdir, "src"), env); err != nil {
				return err
			}
		}

		path, err := goIOSBindArchive(name, env, filepath.Join(tmpdir, "src"))
		if err != nil {
			return fmt.Errorf("darwin-%s: %v", arch, err)
		}
		cmd.Args = append(cmd.Args, "-arch", archClang(arch), path)
	}

	// Build static framework output directory.
	if err := removeAll(buildO); err != nil {
		return err
	}
	headers := buildO + "/Versions/A/Headers"
	if err := mkdir(headers); err != nil {
		return err
	}
	if err := symlink("A", buildO+"/Versions/Current"); err != nil {
		return err
	}
	if err := symlink("Versions/Current/Headers", buildO+"/Headers"); err != nil {
		return err
	}
	if err := symlink("Versions/Current/"+title, buildO+"/"+title); err != nil {
		return err
	}

	cmd.Args = append(cmd.Args, "-o", buildO+"/Versions/A/"+title)
	fmt.Println(cmd.String())
	if err := runCmd(cmd); err != nil {
		return err
	}

	// Copy header file next to output archive.
	headerFiles := make([]string, len(fileBases))
	if len(fileBases) == 1 {
		headerFiles[0] = title + ".h"
		err := copyFile(
			headers+"/"+title+".h",
			srcDir+"/"+bindPrefix+title+".objc.h",
		)
		if err != nil {
			return err
		}
	} else {
		for i, fileBase := range fileBases {
			headerFiles[i] = fileBase + ".objc.h"
			err := copyFile(
				headers+"/"+fileBase+".objc.h",
				srcDir+"/"+fileBase+".objc.h")
			if err != nil {
				return err
			}
		}
		err := copyFile(
			headers+"/ref.h",
			srcDir+"/ref.h")
		if err != nil {
			return err
		}
		headerFiles = append(headerFiles, title+".h")
		err = writeFile(headers+"/"+title+".h", func(w io.Writer) error {
			return iosBindHeaderTmpl.Execute(w, map[string]interface{}{
				"pkgs": pkgs, "title": title, "bases": fileBases,
			})
		})
		if err != nil {
			return err
		}
	}

	resources := buildO + "/Versions/A/Resources"
	if err := mkdir(resources); err != nil {
		return err
	}
	if err := symlink("Versions/Current/Resources", buildO+"/Resources"); err != nil {
		return err
	}
	if err := writeFile(buildO+"/Resources/Info.plist", func(w io.Writer) error {
		_, err := w.Write([]byte(iosBindInfoPlist))
		return err
	}); err != nil {
		return err
	}

	var mmVals = struct {
		Module  string
		Headers []string
	}{
		Module:  title,
		Headers: headerFiles,
	}
	err = writeFile(buildO+"/Versions/A/Modules/module.modulemap", func(w io.Writer) error {
		return iosModuleMapTmpl.Execute(w, mmVals)
	})
	if err != nil {
		return err
	}
	return symlink("Versions/Current/Modules", buildO+"/Modules")
}
