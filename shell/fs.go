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

	"mvdan.cc/sh/v3/interp"
)

// The interpreter asks about the instance's filesystem, not this machine's, so
// every handler below hands the question to the transport.

func (s *state) statHandler(ctx context.Context, name string, followSymlinks bool) (fs.FileInfo, error) {
	dir := interp.HandlerCtx(ctx).Dir
	if err := s.validate("stat", dir, name); err != nil {
		return nil, err
	}
	return s.cfg.Transport.Stat(ctx, dir, name, followSymlinks)
}

func (s *state) accessHandler(ctx context.Context, name string, mode interp.AccessMode) error {
	dir := interp.HandlerCtx(ctx).Dir
	if err := s.validate("access", dir, name); err != nil {
		return err
	}
	return s.cfg.Transport.Access(ctx, dir, name, accessMode(mode))
}

func (s *state) readDirHandler(ctx context.Context, name string) ([]fs.DirEntry, error) {
	dir := interp.HandlerCtx(ctx).Dir
	if err := s.validate("readdir", dir, name); err != nil {
		return nil, err
	}
	return s.cfg.Transport.ReadDir(ctx, dir, name)
}

func (s *state) open(ctx context.Context, name string, flag int, _ os.FileMode) (io.ReadWriteCloser, error) {
	hc := interp.HandlerCtx(ctx)
	if name != os.DevNull {
		if err := s.validate("open", hc.Dir, name); err != nil {
			return nil, err
		}
	}
	return s.cfg.Transport.Open(ctx, hc.Dir, name, flag, hc.Stderr)
}

// validate is a file question the transport has already said it cannot
// answer, the instance having no sh for it to ask with.
func (s *state) validate(op, dir, name string) error {
	if !s.noShell {
		return nil
	}
	return &fs.PathError{Op: op, Path: resolve(dir, name), Err: ErrNoShell}
}

// accessMode is the interpreter's way of asking, in ours.
func accessMode(mode interp.AccessMode) AccessMode {
	var want AccessMode
	if mode&interp.AccessRead != 0 {
		want |= AccessRead
	}
	if mode&interp.AccessWrite != 0 {
		want |= AccessWrite
	}
	if mode&interp.AccessExec != 0 {
		want |= AccessExec
	}
	return want
}
