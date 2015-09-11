// Copyright 2015 The Vanadium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cmdline

import (
	"bytes"
	"flag"
	"fmt"
	"go/doc"
	"io"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"v.io/x/lib/textutil"
)

const missingDescription = "No description available"

// helpRunner is a Runner that implements the "help" functionality.  Help is
// requested for the last command in rootPath, which must not be empty.
type helpRunner struct {
	rootPath []*Command
	*helpConfig
}

func makeHelpRunner(path []*Command, env *Env) helpRunner {
	return helpRunner{path, &helpConfig{
		env:   env,
		style: env.style(),
		width: env.width(),
	}}
}

// helpConfig holds configuration data for help.  The style and width may be
// overriden by flags if the command returned by newCommand is parsed.
type helpConfig struct {
	env   *Env
	style style
	width int
}

// Run implements the Runner interface method.
func (h helpRunner) Run(env *Env, args []string) error {
	w := textutil.NewUTF8LineWriter(env.Stdout, h.width)
	defer w.Flush()
	return runHelp(w, env.Stderr, args, h.rootPath, h.helpConfig)
}

// usageFunc is used as the implementation of the Env.Usage function.
func (h helpRunner) usageFunc(writer io.Writer) {
	w := textutil.NewUTF8LineWriter(writer, h.width)
	usage(w, h.rootPath, h.helpConfig, h.env.firstCall())
	w.Flush()
}

const helpName = "help"

// newCommand returns a new help command that uses h as its Runner.
func (h helpRunner) newCommand() *Command {
	help := &Command{
		Runner: h,
		Name:   helpName,
		Short:  "Display help for commands or topics",
		Long: `
Help with no args displays the usage of the parent command.

Help with args displays the usage of the specified sub-command or help topic.

"help ..." recursively displays help for all commands and topics.
`,
		ArgsName: "[command/topic ...]",
		ArgsLong: `
[command/topic ...] optionally identifies a specific sub-command or help topic.
`,
	}
	help.Flags.Var(&h.style, "style", `
The formatting style for help output:
   compact - Good for compact cmdline output.
   full    - Good for cmdline output, shows all global flags.
   godoc   - Good for godoc processing.
Override the default by setting the CMDLINE_STYLE environment variable.
`)
	help.Flags.IntVar(&h.width, "width", h.width, `
Format output to this target width in runes, or unlimited if width < 0.
Defaults to the terminal width if available.  Override the default by setting
the CMDLINE_WIDTH environment variable.
`)
	// Override default values, so that the godoc style shows good defaults.
	help.Flags.Lookup("style").DefValue = "compact"
	help.Flags.Lookup("width").DefValue = "<terminal width>"
	cleanTree(help)
	return help
}

// runHelp implements the run-time behavior of the help command.
func runHelp(w *textutil.LineWriter, stderr io.Writer, args []string, path []*Command, config *helpConfig) error {
	if len(args) == 0 {
		usage(w, path, config, config.env.firstCall())
		return nil
	}
	if args[0] == "..." {
		usageAll(w, path, config, config.env.firstCall())
		return nil
	}
	// Look for matching children.
	cmd, subName, subArgs := path[len(path)-1], args[0], args[1:]
	for _, child := range cmd.Children {
		if child.Name == subName {
			return runHelp(w, stderr, subArgs, append(path, child), config)
		}
	}
	if helpName == subName {
		help := helpRunner{path, config}.newCommand()
		return runHelp(w, stderr, subArgs, append(path, help), config)
	}
	if cmd.LookPath {
		// Look for a matching executable in PATH.
		subCmd := cmd.Name + "-" + subName
		if lookPath(subCmd, config.env.pathDirs()) {
			runner := binaryRunner{subCmd, pathName(config.env.prefix(), path)}
			env := config.env.clone()
			env.Vars["CMDLINE_STYLE"] = config.style.String()
			if len(subArgs) == 0 {
				return runner.Run(env, []string{"-help"})
			}
			return runner.Run(env, append([]string{helpName}, subArgs...))
		}
	}
	// Look for matching topic.
	for _, topic := range cmd.Topics {
		if topic.Name == subName {
			fmt.Fprintln(w, topic.Long)
			return nil
		}
	}
	fn := helpRunner{path, config}.usageFunc
	return usageErrorf(stderr, fn, "%s: unknown command or topic %q", pathName(config.env.prefix(), path), subName)
}

func godocHeader(path, short string) string {
	// The first rune must be uppercase for godoc to recognize the string as a
	// section header, which is linked to the table of contents.
	switch {
	case path == "":
		return firstRuneToUpper(short)
	case short == "":
		return firstRuneToUpper(path)
	}
	// Godoc has special heurisitics to extract headers from the comments, from
	// which it builds a nice table of contents.  Headers must be single
	// unindented lines with unindented paragraphs both before and after, and the
	// line must not include certain characters.
	//
	// We try our best to create a header that includes both the command path and
	// the short description, but if godoc won't extract a header out of the line,
	// we fall back to just returning the command path.
	//
	// For more details see the comments and implementation of doc.ToHTML:
	// http://golang.org/pkg/go/doc/#ToHTML
	header := firstRuneToUpper(path + " - " + short)
	var buf bytes.Buffer
	doc.ToHTML(&buf, "before\n\n"+header+"\n\nafter", nil)
	if !bytes.Contains(buf.Bytes(), []byte("<h")) {
		return firstRuneToUpper(path)
	}
	return header
}

func firstRuneToUpper(s string) string {
	if s == "" {
		return ""
	}
	r, n := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[n:]
}

func lineBreak(w *textutil.LineWriter, style style) {
	w.Flush()
	switch style {
	case styleCompact, styleFull:
		width := w.Width()
		if width < 0 {
			// If the user has chosen an "unlimited" word-wrapping width, we still
			// need a reasonable width for our visual line break.
			width = defaultWidth
		}
		fmt.Fprintln(w, strings.Repeat("=", width))
	case styleGoDoc:
		fmt.Fprintln(w)
	}
	w.Flush()
}

// needsHelpChild returns true if cmd needs a default help command to be
// appended to its children.  Every command that has children and doesn't
// already have a "help" command needs a help child.
func needsHelpChild(cmd *Command) bool {
	for _, child := range cmd.Children {
		if child.Name == helpName {
			return false
		}
	}
	return len(cmd.Children) > 0
}

// usageAll prints usage recursively via DFS from the path onward.
func usageAll(w *textutil.LineWriter, path []*Command, config *helpConfig, firstCall bool) {
	cmd, cmdPath := path[len(path)-1], pathName(config.env.prefix(), path)
	usage(w, path, config, firstCall)
	for _, child := range cmd.Children {
		usageAll(w, append(path, child), config, false)
	}
	if cmd.LookPath {
		subCmds := lookPathAll(cmd.Name, config.env.pathDirs(), cmd.subNames())
		for _, subCmd := range subCmds {
			runner := binaryRunner{subCmd, cmdPath}
			var buffer bytes.Buffer
			env := config.env.clone()
			env.Stdout = &buffer
			env.Stderr = &buffer
			env.Vars["CMDLINE_FIRST_CALL"] = "1"
			env.Vars["CMDLINE_STYLE"] = config.style.String()
			if err := runner.Run(env, []string{helpName, "..."}); err == nil {
				// The binary subcommand supports "help".
				if config.style == styleGoDoc {
					// The textutil package will discard any leading empty lines
					// produced by the child process output, so we need to
					// output it here.
					fmt.Fprintln(w)
				}
				fmt.Fprint(w, buffer.String())
				continue
			}
			buffer.Reset()
			if err := runner.Run(env, []string{"-help"}); err == nil {
				// The binary subcommand supports "-help".
				if config.style == styleGoDoc {
					// The textutil package will discard any leading empty lines
					// produced by the child process output, so we need to
					// output it here.
					fmt.Fprintln(w)
				}
				fmt.Fprint(w, buffer.String())
				continue
			}
			// The binary subcommand does not support "help" or "-help".
			lineBreak(w, config.style)
			fmt.Fprintln(w, godocHeader(cmdPath+" "+strings.TrimPrefix(subCmd, cmd.Name+"-"), missingDescription))
		}
	}
	if firstCall && needsHelpChild(cmd) {
		help := helpRunner{path, config}.newCommand()
		usageAll(w, append(path, help), config, false)
	}
	for _, topic := range cmd.Topics {
		lineBreak(w, config.style)
		w.ForceVerbatim(true)
		fmt.Fprintln(w, godocHeader(cmdPath+" "+topic.Name, topic.Short))
		w.ForceVerbatim(false)
		fmt.Fprintln(w)
		fmt.Fprintln(w, topic.Long)
	}
}

// usage prints the usage of the last command in path to w.  The bool firstCall
// is set to false when printing usage for multiple commands, and is used to
// avoid printing redundant information (e.g. help command, global flags).
func usage(w *textutil.LineWriter, path []*Command, config *helpConfig, firstCall bool) {
	cmd, cmdPath := path[len(path)-1], pathName(config.env.prefix(), path)
	if config.style == styleShort {
		fmt.Fprintln(w, cmd.Short)
		return
	}
	if !firstCall {
		lineBreak(w, config.style)
		w.ForceVerbatim(true)
		fmt.Fprintln(w, godocHeader(cmdPath, cmd.Short))
		w.ForceVerbatim(false)
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, cmd.Long)
	fmt.Fprintln(w)
	// Usage line.
	fmt.Fprintln(w, "Usage:")
	cmdPathF := "   " + cmdPath
	if countFlags(&cmd.Flags, nil, true) > 0 {
		cmdPathF += " [flags]"
	}
	if cmd.Runner != nil {
		if cmd.ArgsName != "" {
			fmt.Fprintln(w, cmdPathF, cmd.ArgsName)
		} else {
			fmt.Fprintln(w, cmdPathF)
		}
	}
	var subCmds []string
	if cmd.LookPath {
		subCmds = lookPathAll(cmd.Name, config.env.pathDirs(), cmd.subNames())
	}
	hasSubcommands := len(subCmds) > 0 || len(cmd.Children) > 0
	if hasSubcommands {
		fmt.Fprintln(w, cmdPathF, "<command>")
	}
	// Compute the name width.
	const minNameWidth = 11
	nameWidth := minNameWidth
	for _, child := range cmd.Children {
		if len(child.Name) > nameWidth {
			nameWidth = len(child.Name)
		}
	}
	for _, subCmd := range subCmds {
		length := len(strings.TrimPrefix(subCmd, cmd.Name+"-"))
		if length > nameWidth {
			nameWidth = length
		}
	}
	// Command header.
	if hasSubcommands {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "The", cmdPath, "commands are:")
		// Print as a table with aligned columns Name and Short.
		w.SetIndents(spaces(3), spaces(3+nameWidth+1))
	}
	printShort := func(name, short string) {
		fmt.Fprintf(w, "%-[1]*[2]s %[3]s", nameWidth, name, short)
		w.Flush()
	}
	// Built-in subcommands.
	if len(cmd.Children) > 0 {
		for _, child := range cmd.Children {
			printShort(child.Name, child.Short)
		}
	}
	// Binary subcommands.
	if len(subCmds) > 0 {
		for _, subCmd := range subCmds {
			runner := binaryRunner{subCmd, cmdPath}
			var buffer bytes.Buffer
			env := config.env.clone()
			env.Stdout = &buffer
			env.Stderr = &buffer
			env.Vars["CMDLINE_STYLE"] = "short"
			if err := runner.Run(env, []string{"-help"}); err == nil {
				// The binary subcommand supports "-help".
				printShort(strings.TrimPrefix(subCmd, cmd.Name+"-"), buffer.String())
			} else {
				// The binary subcommand does not support "-help".
				printShort(strings.TrimPrefix(subCmd, cmd.Name+"-"), missingDescription)
			}
		}
	}
	// Default help command.
	if firstCall && needsHelpChild(cmd) {
		help := helpRunner{path, config}.newCommand()
		printShort(help.Name, help.Short)
	}
	// Command footer.
	if hasSubcommands {
		w.SetIndents()
		if firstCall && config.style != styleGoDoc {
			fmt.Fprintf(w, "Run \"%s help [command]\" for command usage.\n", cmdPath)
		}
	}
	// Args.
	if cmd.Runner != nil && cmd.ArgsLong != "" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, cmd.ArgsLong)
	}
	// Help topics.
	if len(cmd.Topics) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "The", cmdPath, "additional help topics are:")
		nameWidth := minNameWidth
		for _, topic := range cmd.Topics {
			if len(topic.Name) > nameWidth {
				nameWidth = len(topic.Name)
			}
		}
		// Print as a table with aligned columns Name and Short.
		w.SetIndents(spaces(3), spaces(3+nameWidth+1))
		for _, topic := range cmd.Topics {
			fmt.Fprintf(w, "%-[1]*[2]s %[3]s", nameWidth, topic.Name, topic.Short)
			w.Flush()
		}
		w.SetIndents()
		if firstCall && config.style != styleGoDoc {
			fmt.Fprintf(w, "Run \"%s help [topic]\" for topic details.\n", cmdPath)
		}
	}
	flagsUsage(w, path, config, firstCall)
}

func flagsUsage(w *textutil.LineWriter, path []*Command, config *helpConfig, firstCall bool) {
	cmd, cmdPath := path[len(path)-1], pathName(config.env.prefix(), path)
	// Flags.
	if countFlags(&cmd.Flags, nil, true) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "The", cmdPath, "flags are:")
		printFlags(w, &cmd.Flags, config.style, nil, true)
	}
	// Only show global flags on the first call.
	if !firstCall {
		return
	}
	hasCompact := countFlags(globalFlags, nonHiddenGlobalFlags, true) > 0
	hasFull := countFlags(globalFlags, nonHiddenGlobalFlags, false) > 0
	if config.style != styleCompact {
		// Non-compact style, always show all global flags.
		if hasCompact || hasFull {
			fmt.Fprintln(w)
			fmt.Fprintln(w, "The global flags are:")
			printFlags(w, globalFlags, config.style, nonHiddenGlobalFlags, true)
			if hasCompact && hasFull {
				fmt.Fprintln(w)
			}
			printFlags(w, globalFlags, config.style, nonHiddenGlobalFlags, false)
		}
		return
	}
	// Compact style, only show compact flags and a reminder if there are more.
	if hasCompact {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "The global flags are:")
		printFlags(w, globalFlags, config.style, nonHiddenGlobalFlags, true)
	}
	if hasFull {
		fmt.Fprintln(w)
		fullhelp := fmt.Sprintf(`Run "%s help -style=full" to show all global flags.`, cmdPath)
		if len(cmd.Children) == 0 {
			if len(path) > 1 {
				parentPath := pathName(config.env.prefix(), path[:len(path)-1])
				fullhelp = fmt.Sprintf(`Run "%s help -style=full %s" to show all global flags.`, parentPath, cmd.Name)
			} else {
				fullhelp = fmt.Sprintf(`Run "CMDLINE_STYLE=full %s -help" to show all global flags.`, cmdPath)
			}
		}
		fmt.Fprintln(w, fullhelp)
	}
}

func countFlags(flags *flag.FlagSet, regexps []*regexp.Regexp, match bool) (num int) {
	flags.VisitAll(func(f *flag.Flag) {
		if match == matchRegexps(regexps, f.Name) {
			num++
		}
	})
	return
}

func printFlags(w *textutil.LineWriter, flags *flag.FlagSet, style style, regexps []*regexp.Regexp, match bool) {
	flags.VisitAll(func(f *flag.Flag) {
		if match != matchRegexps(regexps, f.Name) {
			return
		}
		value := f.Value.String()
		if style == styleGoDoc {
			// When using styleGoDoc we use the default value, so that e.g. regular
			// help will show "/usr/home/me/foo" while godoc will show "$HOME/foo".
			value = f.DefValue
		}
		fmt.Fprintf(w, " -%s=%v", f.Name, value)
		w.SetIndents(spaces(3))
		fmt.Fprintln(w, f.Usage)
		w.SetIndents()
	})
}

func spaces(count int) string {
	return strings.Repeat(" ", count)
}

func matchRegexps(regexps []*regexp.Regexp, name string) bool {
	// We distinguish nil regexps from empty regexps; the former means "all names
	// match", while the latter means "no names match".
	if regexps == nil {
		return true
	}
	for _, r := range regexps {
		if r.MatchString(name) {
			return true
		}
	}
	return false
}

var nonHiddenGlobalFlags []*regexp.Regexp

// HideGlobalFlagsExcept hides global flags from the default compact-style usage
// message, except for the given regexps.  Global flag names that match any of
// the regexps will still be shown in the compact usage message.  Multiple calls
// behave as if all regexps were provided in a single call.
//
// All global flags are always shown in non-compact style usage messages.
func HideGlobalFlagsExcept(regexps ...*regexp.Regexp) {
	// NOTE: nonHiddenGlobalFlags is used as the argument to matchRegexps, where
	// nil means "all names match" and empty means "no names match".
	nonHiddenGlobalFlags = append(nonHiddenGlobalFlags, regexps...)
	if nonHiddenGlobalFlags == nil {
		nonHiddenGlobalFlags = []*regexp.Regexp{}
	}
}