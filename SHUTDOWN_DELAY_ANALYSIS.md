# Analysis: 1s Shutdown Delay with Exec Commands

## Issue Summary

The Polar Signals analysis has pointed out that Pebble experiences a 1-second delay during shutdown when exec commands are still running. This investigation analyzes the root cause and proposes a solution.

## Root Cause Analysis

### The WaitDelay Setting

In `/internals/overlord/cmdstate/handlers.go`, line 356:

```go
cmd.WaitDelay = waitDelay  // waitDelay = time.Second (line 45)
```

### What is WaitDelay?

According to Go's documentation, `exec.Cmd.WaitDelay` serves two purposes:

1. **Process Termination Timeout**: If a child process doesn't exit after context cancellation, wait up to `WaitDelay` before sending SIGKILL
2. **I/O Pipe Timeout**: If I/O pipes remain open after process exit (e.g., due to orphaned subprocesses), wait up to `WaitDelay` before forcefully closing them

The critical statement from the documentation:

> "The WaitDelay timer starts when either the associated Context is done or a call to Wait observes that the child process has exited, **whichever occurs first**."

This means the timer can start **before** the process even exits.

### The Shutdown Flow

When Pebble shuts down with an exec command running:

1. `daemon.Stop()` is called
2. The task's tomb context is killed: `ctx := tomb.Context(context.Background())` (line 123)
3. Context cancellation triggers `exec.CommandContext` to send SIGTERM to the process
4. **The `WaitDelay` timer starts immediately** when the context is cancelled
5. Even if the process exits quickly (e.g., in 10ms), the full 1-second delay may still apply if:
   - The I/O pipes haven't been fully drained yet
   - The websocket streams are still reading/writing
   - There are race conditions between process exit and pipe closure

### Why This Matters

The websocket-based I/O handling in Pebble is complex:

- **Terminal mode**: Uses `MirrorToWebsocket` with sophisticated polling logic (`ExecReaderToChannel`)
- **Non-terminal mode**: Uses separate goroutines for stdin/stdout/stderr pipes
- The code waits for output to be sent: `wgOutputSent.Wait()` (line 410)

This complexity means that even when a process exits cleanly after SIGTERM, the I/O pipes might not close immediately, causing Go to wait the full `WaitDelay` period.

## When Is This Behavior Intended?

The `WaitDelay` is valuable in two scenarios:

1. **Misbehaving processes**: If a process ignores or doesn't handle SIGTERM properly, we want to SIGKILL it after 1 second
2. **Orphaned subprocesses**: If a process spawns children and exits, leaving children with open pipes, we don't want to hang indefinitely

## When Is This Behavior NOT Intended?

The delay is NOT intended when:

1. A process exits cleanly and quickly after receiving SIGTERM
2. All I/O has been flushed and pipes are closing normally
3. The only reason for the delay is the timer itself

According to the issue description: "If this happens when the process doesn't cleanly exit, I think that's working as intended, but otherwise we should look at fixing it."

## Proposed Solutions

### Option 1: Reduce WaitDelay to 0 (Recommended for Well-Behaved Processes)

**Pros:**
- Eliminates artificial delays for well-behaved processes
- Processes that exit cleanly won't incur any unnecessary wait time
- I/O will still be read until EOF naturally

**Cons:**
- If a process spawns orphaned children that keep pipes open, we'll hang indefinitely
- No forced timeout for misbehaving processes

### Option 2: Reduce WaitDelay to 100-200ms

**Pros:**
- Short enough to avoid noticeable delays for clean exits
- Still provides protection against hanging on orphaned pipes
- Still forces SIGKILL for truly misbehaving processes

**Cons:**
- Some artificial delay remains (though much smaller)
- May not be enough time for complex cleanup in edge cases

### Option 3: Use Different WaitDelay Based on Context

**Pros:**
- Can use 0 or small delay for normal operation
- Can use longer delay (1s) for timeout scenarios
- Most flexible approach

**Cons:**
- More complex to implement
- Requires distinguishing between "shutdown" and "timeout" cancellation

### Option 4: Make WaitDelay Configurable

**Pros:**
- Users can tune based on their workload
- Can be set per-exec command

**Cons:**
- Adds configuration complexity
- Most users won't know what value to use

## Recommendation

**Option 2 is recommended**: Reduce `waitDelay` from `time.Second` to `100 * time.Millisecond`.

**Rationale:**
- 100ms is sufficient for well-behaved processes to exit and close pipes
- Still provides reasonable protection against orphaned subprocesses
- Reduces the worst-case shutdown delay from 1s to 100ms (90% improvement)
- Balances clean shutdown performance with robustness

If profiling shows that even 100ms is too long, we can further reduce to 50ms or implement Option 3 for more fine-grained control.

## Testing Strategy

1. **Test clean shutdown**: Start an exec command, then immediately shut down Pebble. Measure time.
2. **Test misbehaving process**: Start an exec that ignores SIGTERM. Verify SIGKILL is sent after WaitDelay.
3. **Test orphaned children**: Start an exec that spawns background processes. Verify cleanup.
4. **Test normal completion**: Ensure exec commands that complete normally aren't affected.

## Implementation Plan

1. Change `waitDelay` constant in `internals/overlord/cmdstate/handlers.go`
2. Also check `execWaitDelay` in `internals/overlord/checkstate/checkers.go` (same issue)
3. Add test cases to verify the behavior
4. Run existing test suite to ensure no regressions
5. Profile shutdown time before and after the change
