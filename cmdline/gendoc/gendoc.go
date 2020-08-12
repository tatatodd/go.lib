// Copyright 2015 The Vanadium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Command gendoc generates godoc comments describing the usage of tools based
// on the cmdline package.
//
// Usage:
//   go run gendoc.go [flags] <pkg> [args]
//
// <pkg> is the package path for the tool.
//
// [args] are the arguments to pass to the tool to produce usage output.  If no
// args are given, runs "<tool> help ..."
//
// The gendoc command itself is not based on the cmdline library to avoid
// non-trivial bootstrapping.
//
//go:generate go run . -go-flag-pkg v.io/x/lib/cmdline/gendoc -h
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	flagEnv          string
	flagInstall      string
	flagOut          string
	flagPostProcess  bool
	flagStderr       bool
	flagGoFlagPkg    bool
	flagTags         string
	copyrightNotice  string
	goInstallCommand string
)

func main() {
	flag.StringVar(&flagEnv, "env", "os", `Environment variables to set before running command.  If "os", grabs vars from the underlying OS.  If empty, doesn't set any vars.  Otherwise vars are expected to be comma-separated entries of the form KEY1=VALUE1,KEY2=VALUE2,...`)
	flag.StringVar(&flagInstall, "install", "", "Comma separated list of packages to install before running command.  All commands that are built will be on the PATH.")
	flag.StringVar(&flagOut, "out", "./doc.go", "Path to the output file.")
	flag.BoolVar(&flagStderr, "use-stderr", false, "If set, read usage output from stderr rather than stdout; it also ignores the exit status of the command.")
	flag.BoolVar(&flagPostProcess, "postprocess-output", false, "If set, the help/usage output will be post processed to remove absolute path names that contain the build directory.")
	flag.BoolVar(&flagGoFlagPkg, "go-flag-pkg", false, "Set if the command is using the standard go flag package, it sets both use-stderr and postprocess-output to true")
	flag.StringVar(&flagTags, "tags", "", "Tags for go build, also added as build constraints in the generated output file.")
	flag.StringVar(&copyrightNotice, "copyright-notice", "", "File containing the copyright notice to be prepended to the autogenerated documentation; if specified as an empty string then no copyright notice will be used.")
	flag.StringVar(&goInstallCommand, "build-cmd", "", "Comand to use for building/installing commands whose usage is to be documented, it must accept the same flags as 'go install'.")
	flag.Parse()
	if flagGoFlagPkg {
		flagStderr, flagPostProcess = true, true
	}
	if err := generate(flagStderr, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func determineBinaryName(pkg string) (string, error) {
	var listOut bytes.Buffer
	listCmd := exec.Command("go", "list", pkg)
	listCmd.Stdout = &listOut
	if err := listCmd.Run(); err != nil {
		msg := fmt.Sprintf("%q failed: %v\n%v\n", strings.Join(listCmd.Args, " "), err, listOut.String())
		return "", errors.New(msg)
	}
	return filepath.Base(strings.TrimSpace(listOut.String())), nil
}

func generate(readStderr bool, args []string) error {
	if got, want := len(args), 1; got < want {
		return fmt.Errorf("gendoc requires at least one argument\nusage: gendoc <pkg> [args]")
	}
	pkg, args := args[0], args[1:]

	// Find out the binary name from the pkg name, include a package
	// name of '.'.
	binName, err := determineBinaryName(pkg)
	if err != nil {
		return err
	}

	// Build the binary into a temporary directory
	tmpDir, err := ioutil.TempDir("", "")
	if err != nil {
		return fmt.Errorf("TempDir() failed: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Install all packages in a temporary directory.
	pkgs := []string{pkg}
	if flagInstall != "" {
		pkgs = append(pkgs, strings.Split(flagInstall, ",")...)
	}

	installCmd := append([]string{}, "go", "install")
	if len(goInstallCommand) > 0 {
		installCmd = strings.Split(goInstallCommand, " ")
	}

	for _, installPkg := range pkgs {
		installArgs := append(installCmd, "-tags="+flagTags, installPkg)
		installCmd := exec.Command(installArgs[0], installArgs[1:]...)
		installCmd.Env = append(os.Environ(), "GOBIN="+tmpDir)
		if err := installCmd.Run(); err != nil {
			msg := fmt.Sprintf("%q failed: %v\n", strings.Join(installCmd.Args, " "), err)
			return errors.New(msg)
		}
	}

	// Run the binary to generate documentation.
	var out bytes.Buffer
	if len(args) == 0 {
		args = []string{"help", "..."}
	}
	runCmd := exec.Command(filepath.Join(tmpDir, binName), args...)
	runCmd.Dir = tmpDir
	if readStderr {
		runCmd.Stderr = &out
	} else {
		runCmd.Stdout = &out
	}
	runCmd.Env = runEnviron(tmpDir)
	if err := runCmd.Run(); err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok || !readStderr {
			msg := fmt.Sprintf("%q failed: %v\n%v\n", strings.Join(runCmd.Args, " "), err, out.String())
			return errors.New(msg)
		}
		fmt.Printf("ignoring exit error: %v\n", exitErr)
	}
	if flagPostProcess {

	}
	var tagsConstraint string
	if flagTags != "" {
		tagsConstraint = fmt.Sprintf("// +build %s\n\n", flagTags)
	}

	copyright := `// Copyright 2018 The Vanadium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

`
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "copyright-notice" {
			copyright = ""
		}
	})

	if len(copyright) == 0 {
		if len(copyrightNotice) > 0 {
			buf, err := ioutil.ReadFile(copyrightNotice)
			if err != nil {
				msg := fmt.Sprintf("failed to read copyright notice file: %v: %v", copyrightNotice, err)
				return errors.New(msg)
			}
			copyright = string(buf)
		}
	}
	doc := fmt.Sprintf(`%s// This file was auto-generated via go generate.
// DO NOT UPDATE MANUALLY

%s/*
%s*/
package main
`, copyright, tagsConstraint, postProcess(flagPostProcess, tmpDir, out.String()))

	// Write the result to the output file.
	path, perm := flagOut, os.FileMode(0644)
	if err := ioutil.WriteFile(path, []byte(doc), perm); err != nil {
		msg := fmt.Sprintf("WriteFile(%v, %v) failed: %v\n", path, perm, err)
		return errors.New(msg)
	}
	return nil
}

func postProcess(postProcessFlag bool, tmpDir string, body string) string {
	out := suppressParallelFlag(body)
	if !postProcessFlag {
		return out
	}
	out = strings.Replace(out, tmpDir+string(filepath.Separator), "", -1)
	return out
}

// suppressParallelFlag replaces the default value of the test.parallel flag
// with the literal string "<number of threads>". The default value of the
// test.parallel flag is GOMAXPROCS, which (since Go1.5) is set to the number
// of logical CPU threads on the current system. This causes problems with the
// vanadium-go-generate test, which requires that the output of gendoc is the
// same on all systems.
func suppressParallelFlag(input string) string {
	pattern := regexp.MustCompile(`(?m:(^ -test\.parallel=)(?:\d)+$)`)
	return pattern.ReplaceAllString(input, "$1<number of threads>")
}

// runEnviron returns the environment variables to use when running the command
// to retrieve full help information.
func runEnviron(binDir string) []string {
	// Never return nil, which signals exec.Command to use os.Environ.
	in, out := strings.Split(flagEnv, ","), make([]string, 0)
	if flagEnv == "os" {
		in = os.Environ()
	}
	updatedPath := false
	for _, e := range in {
		if e == "" {
			continue
		}
		if strings.HasPrefix(e, "PATH=") {
			e = "PATH=" + binDir +
				string(os.PathListSeparator) + e[5:]
			updatedPath = true
		}
		out = append(out, e)
	}
	if !updatedPath {
		out = append(out, "PATH="+binDir)
	}
	out = append(out, "CMDLINE_STYLE=godoc")
	return out
}
