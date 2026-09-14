// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, Unikraft GmbH and The Unikraft CLI Authors.
// Licensed under the BSD-3-Clause License (the "License").
// You may not use this file except in compliance with the License.

package shell

import (
	"context"
	"io"
	"io/fs"
	"os"

	"unikraft.com/x/stdio"
)

// Config is a session's setup.
type Config struct {
	Instance  string
	Transport Transport
	Builtins  map[string]Builtin
	Dir       string
	Env       map[string]string
	Command   string
	Input     *os.File
}

// Streams are the three standard streams a single command is wired to.
type Streams = stdio.Stdio

// Transport is how the shell reaches the instance.
type Transport interface {
	// Exec runs one command, its status negated when it was signalled
	Exec(ctx context.Context, streams Streams, dir string, env map[string]string, args []string) (int, error)

	// Stat is what is at a path, the symlink itself when not following
	Stat(ctx context.Context, dir, name string, followSymlinks bool) (fs.FileInfo, error)

	// Access is whether the file can be used every way mode asks
	Access(ctx context.Context, dir, name string, mode AccessMode) error

	// ReadDir lists a directory, by name
	ReadDir(ctx context.Context, dir, name string) ([]fs.DirEntry, error)

	// Open streams a file one way, telling stderr what goes wrong after
	Open(ctx context.Context, dir, name string, flag int, stderr io.Writer) (io.ReadWriteCloser, error)

	// Environ is the instance's own environment, as NAME=value
	Environ(ctx context.Context) ([]string, error)
}

// AccessMode is what a caller wants to do with a file.
type AccessMode uint8

const (
	AccessRead AccessMode = 1 << iota
	AccessWrite
	AccessExec
)

// Builtin answers one ":" line here, not on the instance; args[0] is its name.
type Builtin interface {
	Run(ctx context.Context, streams Streams, args []string) (int, error)
}

// BuiltinFunc is a Builtin made of a function.
type BuiltinFunc func(ctx context.Context, streams Streams, args []string) (int, error)

func (f BuiltinFunc) Run(ctx context.Context, streams Streams, args []string) (int, error) {
	return f(ctx, streams, args)
}
