# AgentFlare Go engineering guide

This file defines the engineering standards for Go code in this repository. Keep product
requirements and architecture decisions in dedicated documents, not here.

## Repository operating notes

- Treat `README.md` as the user-facing operational documentation and
  `firmware/README.md` as the sole firmware authority. Keep them aligned when
  product behaviour changes.
- The KM16-Pro HID interface controls physical LEDs. Do not start the daemon, run a
  hardware preflight, or send a command that can alter the device unless the user has
  explicitly requested a hardware interaction in the current task.
- Keep ordinary tests independent of a physical keyboard. Use the injected transport fakes
  for driver behaviour and reserve hardware smoke tests for explicit opt-in verification.
- A successful socket API response confirms the Core accepted a desired state; it is not a
  HID completion barrier. Integration tests that assert device output must synchronize on a
  fake-transport observation or acknowledgement event.
- Preserve the safety invariant after an uncertain HID transaction: close the session and
  do not send further requests, including LED cleanup or matrix-effect restore.

## Change philosophy

- Inspect the relevant code, tests, and local conventions before editing.
- Make the smallest coherent change that fully addresses the task.
- Preserve existing architecture. Do not add layers, packages, interfaces, configuration,
  dependencies, or extension points for hypothetical future needs.
- Do not mix unrelated refactors, formatting churn, dependency upgrades, or renames into a
  focused change.
- Preserve unrelated user changes in a dirty working tree.
- When behavior changes, update relevant tests and user-facing documentation.
- Never imply that an unexecuted check passed. State exactly what ran and what did not.

## Toolchain and validation

- Use the Go version declared in `go.mod`. Toolchain upgrades are deliberate changes.
- Format every changed Go file with `gofmt`.
- Run the narrowest relevant checks first.
- Before completion, run the documented full validation suite when the change is broad,
  cross-cutting, concurrency-sensitive, or required by CI. Otherwise run relevant package
  checks and explain the scope.
- Typical checks are `go test ./...`, `go vet ./...`, `go test -race ./...`, and
  `go build ./...`. Use `-race` for changes involving goroutines, channels, timers,
  callbacks, or shared state.
- Run `go mod tidy` when imports or dependencies change, then inspect `go.mod` and `go.sum`.
- Run `govulncheck ./...` for dependency changes and release preparation when available.

## Idiomatic design and packages

- Prefer direct, readable, idiomatic Go over patterns imported from other ecosystems.
- Keep packages coherent and acyclic. Use short, lower-case package names.
- Do not create catch-all packages such as `util`, `common`, `helpers`, `types`, or
  `interfaces`.
- Keep identifiers unexported unless another package genuinely needs them. Place
  repository-private packages under `internal/`.
- Keep `main` and `cmd/` thin: process wiring belongs there; testable behavior does not.
- Add an interface when the consumer depends on behavior rather than a concrete representation.
  Typical reasons are multiple implementations or isolating an external or system boundary.
  Define it in the consuming package.
- Prefer composition and concrete types. Use generics only when they reduce meaningful
  duplication without hiding domain behavior.
- Avoid reflection and `unsafe` unless necessary. Isolate and document either when used.

## Naming, documentation, and errors

- Let `gofmt` decide formatting. Do not enforce a custom line limit or hand-align code.
- Follow Go initialism conventions: `ID`, `URL`, `HTTP`, `JSON`, and `HID`.
- Avoid package stutter: prefer `device.Controller` to `device.DeviceController`.
- Name code for its responsibility. Use short names only in a small, obvious scope.
- Exported API should have useful Go documentation. Do not add comments that merely restate
  the identifier. Comments explain purpose, invariants, trade-offs, or surprises.
- Return expected operational errors. Panic only for violated internal invariants indicating a
  programmer defect.
- Wrap errors with operation context and `%w`; classify them with `errors.Is` or `errors.As`,
  never by matching text.
- Handle every error deliberately. Libraries return errors; process boundaries decide whether
  and how to log them. Do not log and return the same error.
- Do not call `log.Fatal` or `os.Exit` outside the top-level command boundary.
- Use lower-case error messages without trailing punctuation. Check close, flush, and sync
  errors when failure could lose or corrupt data.

## Logging, context, and time

- Use the standard library's `log/slog` for structured application logging. Configure it at the
  composition root and pass `*slog.Logger` explicitly where operational logging is needed.
- Use stable, low-cardinality structured attributes. Never log secrets, credentials, or tokens.
  Avoid complete untrusted payloads and personal data; when operationally required, minimize
  and redact them explicitly.
- Add another logging library only when profiling or a concrete integration requirement shows
  that `slog` is insufficient; document the reason.
- Pass `context.Context` as the first parameter of blocking or cancellable operations. Do not
  store contexts in structs or replace an available caller context with `context.Background()`.
- Propagate cancellation and deadlines. Use context values only for request-scoped metadata.
- Make time-dependent behavior testable. Prefer injecting a duration or function over a clock
  interface unless several time operations genuinely need a shared abstraction.
- Do not use `time.Sleep` for synchronization.

## Concurrency, I/O, and input safety

- Start synchronously. Add concurrency only for a concrete benefit.
- Every goroutine needs a clear owner, bounded lifetime, and shutdown strategy. Handle errors
  explicitly where applicable.
- The sending side owns and closes a channel. Protect mutable shared state through a clear owner
  or explicit synchronization; keep locks small and never hold one across slow external I/O
  without a documented invariant.
- When processing an unbounded or externally sized input set, bound concurrency explicitly.
  Do not spawn one goroutine per item.
- Avoid goroutine leaks on cancellation, errors, partial startup, and shutdown. Bound queues,
  retries, buffers, and scanners; make retry backoff cancellable.
- Arrange cleanup immediately after acquiring a resource. Ensure I/O can stop through context,
  deadlines, or bounded timeouts, and handle partial reads and writes correctly.
- Treat external input as untrusted. Validate lengths, formats, paths, numeric ranges, and
  protocol state before allocating resources or causing side effects.
- Make ownership of mutable slices, maps, and byte buffers clear. Copy data retained beyond a
  caller or asynchronous boundary. Never rely on map iteration order.

## Testing

- Prefer the standard `testing` package. Test observable behavior and package contracts.
- Keep tests next to the code they cover. Use tables when they improve clarity, not by default.
- Use `t.Helper()`, `t.Cleanup()`, and `t.TempDir()` for shared helpers, cleanup, and files.
- Use `t.Parallel()` only when every dependency is isolated and concurrency-safe.
- Unit tests must not depend on wall-clock sleeps, execution order, network access, local
  services, or physical hardware. Keep integration and hardware checks explicit opt-in.
- Add meaningful regression tests for reproducible bugs. Use fuzzing for parsers, decoders,
  protocol boundaries, and other untrusted structured input when appropriate.
- Do not hide flaky tests behind retries. Remove the source of nondeterminism.

## Dependencies, workspaces, and platform code

- Prefer the standard library, but use a focused maintained dependency over substantial
  security-sensitive or platform-specific reinvention.
- Review new dependencies for concrete benefit, maintenance, license, transitive weight,
  platform support, and security history. Do not edit `go.sum` manually.
- Do not commit local-path `replace` directives. Do not add `go.work` to a single-module repo.
- Commit `go.work` only for an intentional multi-module repository, never with machine-specific
  paths or local replacements. Where modules must remain independently usable, CI should also
  build and test them with `GOWORK=off`.
- Isolate platform-specific and CGO code behind small packages or build constraints. Keep Go
  ownership and cleanup of C resources explicit, and compile or test supported platform
  combinations in CI where the toolchain permits it.
- Do not edit generated files manually. Change their source and rerun the documented generator.
