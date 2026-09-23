// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package opsutil

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

const signalHelperEnv = "OPSUTIL_SIGNAL_HELPER"

// The scenarios run in a re-exec'd child because the property under test
// is whether the process dies on a signal, which would kill the test binary.
func TestSignalContextHelperProcess(t *testing.T) {
	mode := os.Getenv(signalHelperEnv)
	if mode == "" {
		t.Skip("helper process only")
	}
	ctx, cancel := SignalContext()
	switch mode {
	case "second-signal":
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
			os.Exit(3)
		}
	case "after-cancel":
		cancel()
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	time.Sleep(10 * time.Second)
	os.Exit(0)
}

func runSignalHelper(t *testing.T, mode string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSignalContextHelperProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), signalHelperEnv+"="+mode)
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("helper %q: want death by SIGTERM, got err=%v", mode, err)
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
		t.Fatalf("helper %q: want death by SIGTERM, got %v", mode, exitErr)
	}
}

func TestSignalContextSecondSignalTerminates(t *testing.T) {
	runSignalHelper(t, "second-signal")
}

func TestSignalContextCancelRestoresDefault(t *testing.T) {
	runSignalHelper(t, "after-cancel")
}
