// Copyright (c) 2026 Canonical Ltd
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License version 3 as
// published by the Free Software Foundation.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package daemon_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/canonical/pebble/client"
	"github.com/canonical/pebble/internals/daemon"
	"github.com/canonical/pebble/internals/logger"
	"github.com/canonical/pebble/internals/overlord/pairingstate"
	"github.com/canonical/pebble/internals/plan"
	"github.com/canonical/pebble/internals/reaper"
)

// TestShutdownDelayReduction verifies that shutting down Pebble while an exec
// command is running does not take an excessive amount of time. This test was
// added to verify the fix for the 1-second shutdown delay issue reported by
// Polar Signals analysis.
//
// The test starts a simple echo command (which exits quickly) and then
// immediately shuts down the daemon. We expect the shutdown to complete within
// a reasonable time (500ms), demonstrating that well-behaved processes don't
// incur the full WaitDelay penalty.
func TestShutdownDelayReduction(t *testing.T) {
	// Set up test environment
	logger.SetLogger(logger.New(&bytes.Buffer{}, "[test] "))
	plan.RegisterSectionExtension(pairingstate.PairingField, &pairingstate.SectionExtension{})
	defer plan.UnregisterSectionExtension(pairingstate.PairingField)

	err := reaper.Start()
	if err != nil {
		t.Fatalf("cannot start reaper: %v", err)
	}
	defer func() {
		if err := reaper.Stop(); err != nil {
			t.Fatalf("cannot stop reaper: %v", err)
		}
	}()

	// Create temporary directory for Pebble
	tmpDir := t.TempDir()
	socketPath := tmpDir + "/.pebble.socket"

	// Start daemon
	d, err := daemon.New(&daemon.Options{
		Dir:        tmpDir,
		SocketPath: socketPath,
	})
	if err != nil {
		t.Fatalf("cannot create daemon: %v", err)
	}

	err = d.Init()
	if err != nil {
		t.Fatalf("cannot init daemon: %v", err)
	}

	d.Start()

	// Create client
	c, err := client.New(&client.Config{Socket: socketPath})
	if err != nil {
		t.Fatalf("cannot create client: %v", err)
	}

	// Start an exec command that will complete quickly
	// We use a simple command that exits cleanly after receiving any input
	process, err := c.Exec(&client.ExecOptions{
		Command: []string{"/bin/sh", "-c", "trap 'exit 0' TERM; read line; echo \"$line\""},
		Stdin:   bytes.NewReader([]byte("hello\n")),
		Stdout:  &bytes.Buffer{},
	})
	if err != nil {
		t.Fatalf("cannot start exec: %v", err)
	}

	// Give the exec command a moment to start
	time.Sleep(50 * time.Millisecond)

	// Now shutdown the daemon while the exec is running and measure how long it takes
	startShutdown := time.Now()
	err = d.Stop(nil)
	shutdownDuration := time.Since(startShutdown)

	if err != nil {
		t.Errorf("daemon shutdown failed: %v", err)
	}

	// Clean up - try to wait for the process (it may already be done)
	_ = process.Wait()

	// Verify shutdown was reasonably fast
	// With the fix (100ms WaitDelay), shutdown should complete within 500ms
	// Without the fix (1s WaitDelay), shutdown would take >1s
	maxExpectedDuration := 500 * time.Millisecond
	if shutdownDuration > maxExpectedDuration {
		t.Errorf("Shutdown took too long: %v (expected <%v)", shutdownDuration, maxExpectedDuration)
		t.Error("This suggests the exec command WaitDelay may still be too high")
	} else {
		t.Logf("Shutdown completed in %v (well under %v limit)", shutdownDuration, maxExpectedDuration)
	}
}
