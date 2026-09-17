// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, Unikraft GmbH and The Unikraft CLI Authors.
// Licensed under the BSD-3-Clause License (the "License").
// You may not use this file except in compliance with the License.

package shell

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	xio "unikraft.com/x/io"
	"unikraft.com/x/log"
)

const (
	instanceProbeTimeout  = 3 * time.Second
	instanceProbeAttempts = 2
)

// environFallbacks fill in what an instance did not export. PATH is what a
// container image starts with when it sets none (Debian's ENV_SUPATH), so a
// shell finds what the image's own processes find; HOME and the ids are an
// instance's reality: one user, root, at the root of its filesystem.
var environFallbacks = map[string]string{
	"HOME": "/",
	"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	"UID":  "0",
	"EUID": "0",
	"GID":  "0",
}

var (
	errNotATerminal = errors.New("the shell needs a terminal to prompt on; give it a command line to run without one")
	errNoPrompt     = errors.New("the interactive prompt is not in this build; give the shell a command line")
)

// console is where the session writes, serialised so two commands cannot interleave a write.
type console struct {
	Out, Err io.Writer
}

type state struct {
	cfg     Config
	console console

	runner *interp.Runner

	interactive bool

	noShell bool

	tty   *os.File
	stdin *os.File
}

func Run(ctx context.Context, cfg Config, streams Streams) (int, error) {
	s, err := newState(ctx, cfg, streams)
	if err != nil {
		return 0, err
	}

	switch {
	case s.cfg.Command != "":
		return s.runSource(ctx, strings.NewReader(s.cfg.Command))
	case s.interactive:
		return 0, errNoPrompt
	default:
		return 0, errNotATerminal
	}
}

// newSession initializes the streams it writes on, the instance's environment,
// and the interpreter over both.
func newState(ctx context.Context, cfg Config, streams Streams) (*state, error) {
	if cfg.Transport == nil {
		return nil, fmt.Errorf("no transport to the instance")
	}
	if cfg.Dir == "" {
		cfg.Dir = "/"
	}

	s := &state{cfg: cfg}
	if f, ok := streams.Stdin.(*os.File); ok && xio.IsTTYReader(f) {
		s.tty = f
	}
	s.stdin = cmp.Or(s.tty, cfg.Input)
	s.interactive = cfg.Command == "" && s.tty != nil && xio.IsTTY(streams.Stdout)

	var terminal sync.Mutex
	s.console = console{
		Out: lockWriter(&terminal, streams.Stdout),
		Err: lockWriter(&terminal, streams.Stderr),
	}

	env, err := s.environ(ctx)
	if err != nil {
		return nil, err
	}

	in := streams.Stdin
	if cfg.Input != nil {
		in = cfg.Input
	}

	runner, err := interp.New(
		interp.StdIO(in, s.console.Out, s.console.Err),
		interp.Env(expand.ListEnviron(env...)),
		interp.Interactive(s.interactive),
		interp.CallHandler(keepLookupsRemote),
		interp.ExecHandlers(s.route),
		interp.StatHandler(s.statHandler),
		interp.AccessHandler(s.accessHandler),
		interp.ReadDirHandler2(s.readDirHandler),
		interp.OpenHandler(s.open),
	)
	if err != nil {
		return nil, err
	}

	runner.Dir = cfg.Dir
	runner.Reset()
	s.runner = runner
	return s, nil
}

func (s *state) runSource(ctx context.Context, src io.Reader) (int, error) {
	prog, err := syntax.NewParser().Parse(src, "")
	if err != nil {
		return 0, err
	}
	if err := unsupported(prog); err != nil {
		return 0, err
	}
	return exitStatus(s.runner.Run(ctx, prog))
}

// exitStatus separates a command's exit status from the interpreter failing.
func exitStatus(err error) (int, error) {
	if status, ok := errors.AsType[interp.ExitStatus](err); ok {
		return int(status), nil
	}
	return 0, err
}

// unsupported finds what the interpreter would have to answer from this machine
// and not the instance
func unsupported(prog *syntax.File) error {
	var err error
	syntax.Walk(prog, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.ProcSubst:
			err = fmt.Errorf("%s: process substitution is not supported, the pipe would be on this machine", n.Pos())
		case *syntax.Word:
			if len(n.Parts) == 0 {
				break
			}
			// A word opening with ~ and a name: bare ~ and ~/ are $HOME, and fine.
			if lit, ok := n.Parts[0].(*syntax.Lit); ok && len(lit.Value) > 1 && lit.Value[0] == '~' &&
				(lit.Value[1] == '_' || unicode.IsLetter(rune(lit.Value[1]))) {
				err = fmt.Errorf("%s: ~user is not supported, it would be this machine's user", n.Pos())
			}
		}
		return err == nil
	})
	return err
}

// remoteBuiltins are the builtins the interpreter knows the names of but does
// not implement
var remoteBuiltins = map[string]bool{
	"bg":       true,
	"bind":     true,
	"caller":   true,
	"compgen":  true,
	"complete": true,
	"compopt":  true,
	"disown":   true,
	"enable":   true,
	"fc":       true,
	"fg":       true,
	"history":  true,
	"jobs":     true,
	"kill":     true,
	"logout":   true,
	"newgrp":   true,
	"suspend":  true,
	"ulimit":   true,
	"umask":    true,
}

// keepLookupsRemote stops type, command -v and pwd -P from running locally, and
// sends the instance what the interpreter would otherwise refuse as a builtin.
//
// HACK: the interpreter resolves these as its own builtins before the exec
// handler is reached, so there is nowhere later to route them from. Rewriting
// the line as "sh -c" makes them commands again, which the handler does see.
func keepLookupsRemote(_ context.Context, args []string) ([]string, error) {
	var local bool
	switch args[0] {
	case "type":
		local = true
	case "command":
		// command <name> runs the interpreter's builtin directly, past this handler.
		local = len(args) > 1 && (args[1] == "-v" || args[1] == "-V" || remoteBuiltins[args[1]])
	case "pwd":
		local = slices.Contains(args[1:], "-P")
	default:
		local = remoteBuiltins[args[0]]
	}
	if !local {
		return args, nil
	}
	return append([]string{"sh", "-c", args[0] + ` "$@"`, "sh"}, args[1:]...), nil
}

func (s *state) environ(ctx context.Context) ([]string, error) {
	exported, err := s.cfg.Transport.Environ(ctx)
	switch {
	case errors.Is(err, ErrNoShell):
		// Nothing to probe with, but the session still opens: the next command
		// finds out for itself.
		s.noShell = true
	case err != nil:
		log.G(ctx).Debug().Err(err).Msg("could not read the instance environment")

		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("could not reach %s: %w", cmp.Or(s.cfg.Instance, "the instance"), err)
	}

	vars := map[string]string{}
	for _, record := range exported {
		if name, value, ok := strings.Cut(record, "="); ok {
			vars[name] = value
		}
	}
	maps.Copy(vars, s.cfg.Env)

	for name, fallback := range environFallbacks {
		if vars[name] == "" {
			vars[name] = fallback
		}
	}

	env := make([]string, 0, len(vars))
	for name, value := range vars {
		env = append(env, name+"="+value)
	}
	slices.Sort(env)
	return env, nil
}
