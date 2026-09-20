# internal/shutdown

## Purpose

Turns a termination signal into a cancelled context and closes registered resources in a
defined order, so each `main` does not hand-roll the sequence and leak a connection or
hang past the container's grace period.

## Contents

- `shutdown.go` — `Context` (a context cancelled on SIGINT/SIGTERM, plus its `stop`
  function), and `Group` with `Add` and `Close`.
- `shutdown_test.go` — unit tests, including ordering, the hung-closer path and error
  aggregation.

## How it fits in

Used by every binary's `main` and nothing else. It depends only on the standard library.

## Gotchas

- **The `stop` function returned by `Context` must be called**, normally by `defer` in
  `main`. Until it is, default signal handling is suppressed, so a second Ctrl-C during a
  slow shutdown will not kill the process.
- **`Group` closes in reverse registration order**, matching `defer` and usually matching
  correctness: an HTTP server registered after the database must stop accepting requests
  before the database goes away.
- **`Add` after `Close` panics.** That is a programmer error, and the alternative is a
  resource that silently never gets closed.
- **The timeout in `Close` is shared across all closers, not per closer.** Two consequences
  are deliberate: a closer that ignores its context and blocks anyway is abandoned rather
  than waited on, and a closer that consumes the whole budget leaves the rest invoked
  best-effort and reported as incomplete.
- **An abandoned close function keeps running** until the process exits moments later. It
  must therefore not write to anything whose lifetime ends when `Close` returns.
- Every failure is reported, joined, so one noisy resource cannot hide another.
