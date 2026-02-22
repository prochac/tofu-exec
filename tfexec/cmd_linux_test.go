// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package tfexec

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func Test_runTofuCmd_linux(t *testing.T) {
	// Checks runTofuCmd for race condition when using
	// go test -race -run Test_runTofuCmd_linux ./tfexec -tags=linux
	var buf bytes.Buffer

	tf := &Tofu{
		logger:   log.New(&buf, "", 0),
		execPath: "echo",
	}

	ctx, cancel := context.WithCancel(context.Background())

	cmd := tf.buildTofuCmd(ctx, nil, "hello tf-exec!")
	err := tf.runTofuCmd(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}

	// Cancel stops the leaked go routine which logs an error
	cancel()
	time.Sleep(time.Second)
	if strings.Contains(buf.String(), "error from kill") {
		t.Fatal("canceling context should not lead to logging an error")
	}
}

func TestGracefulShutdown_linux(t *testing.T) {
	td := t.TempDir()

	// Shell script that traps SIGINT, touches a marker file, and exits.
	// Uses background sleep + wait so the shell can process signals
	// between iterations (wait is interruptible by signals, sleep is not).
	script := filepath.Join(td, "trap.sh")
	err := os.WriteFile(script, []byte(`#!/bin/sh
trap 'touch "$1"; exit 0' INT
while true; do sleep 0.1 & wait $!; done
`), 0755)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("default kills immediately", func(t *testing.T) {
		sigFile := filepath.Join(t.TempDir(), "got_sigint")

		tf := &Tofu{
			execPath:   script,
			workingDir: td,
			logger:     log.New(io.Discard, "", 0),
		}
		tf.SetEnv(map[string]string{})

		ctx, cancel := context.WithCancel(context.Background())

		errCh := make(chan error, 1)
		go func() {
			cmd := tf.buildTofuCmd(ctx, nil, sigFile)
			errCh <- tf.runTofuCmd(ctx, cmd)
		}()

		// Give the process time to start and set up the trap.
		time.Sleep(500 * time.Millisecond)
		cancel()

		err := <-errCh
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got: %v", err)
		}

		// Process was killed with SIGKILL — trap never ran, no file.
		if _, err := os.Stat(sigFile); !os.IsNotExist(err) {
			t.Fatal("expected process to be killed without receiving SIGINT")
		}
	})

	t.Run("graceful sends SIGINT", func(t *testing.T) {
		sigFile := filepath.Join(t.TempDir(), "got_sigint")

		tf := &Tofu{
			execPath:                script,
			workingDir:              td,
			logger:                  log.New(io.Discard, "", 0),
			gracefulShutdownTimeout: 5 * time.Second,
		}
		tf.SetEnv(map[string]string{})

		ctx, cancel := context.WithCancel(context.Background())

		errCh := make(chan error, 1)
		go func() {
			cmd := tf.buildTofuCmd(ctx, nil, sigFile)
			errCh <- tf.runTofuCmd(ctx, cmd)
		}()

		// Give the process time to start and set up the trap.
		time.Sleep(500 * time.Millisecond)
		cancel()

		err := <-errCh
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got: %v", err)
		}

		// Process received SIGINT — trap ran and created the file.
		if _, err := os.Stat(sigFile); err != nil {
			t.Fatal("expected process to receive SIGINT for graceful shutdown")
		}
	})

	t.Run("force kills after timeout", func(t *testing.T) {
		// Script that traps SIGINT but refuses to exit — simulates a
		// stuck process that ignores the graceful shutdown signal.
		stubborn := filepath.Join(t.TempDir(), "stubborn.sh")
		err := os.WriteFile(stubborn, []byte(`#!/bin/sh
trap '' INT
while true; do sleep 0.1 & wait $!; done
`), 0755)
		if err != nil {
			t.Fatal(err)
		}

		tf := &Tofu{
			execPath:                stubborn,
			workingDir:              td,
			logger:                  log.New(io.Discard, "", 0),
			gracefulShutdownTimeout: 1 * time.Second,
		}
		tf.SetEnv(map[string]string{})

		ctx, cancel := context.WithCancel(context.Background())

		errCh := make(chan error, 1)
		go func() {
			cmd := tf.buildTofuCmd(ctx, nil)
			errCh <- tf.runTofuCmd(ctx, cmd)
		}()

		// Give the process time to start.
		time.Sleep(500 * time.Millisecond)

		start := time.Now()
		cancel()
		<-errCh
		elapsed := time.Since(start)

		// WaitDelay is 1s. The process ignores SIGINT, so Go's exec
		// machinery must SIGKILL it after the delay. Allow some slack
		// but assert it didn't hang.
		if elapsed > 3*time.Second {
			t.Fatalf("expected process to be force-killed after WaitDelay, but took %s", elapsed)
		}
	})
}
