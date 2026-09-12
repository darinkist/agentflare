# AgentFlare repository guide

These rules apply across the repository. More specific instructions in a
subdirectory take precedence for that subtree.

## Repository operation and safety

- Treat `README.md` as the user-facing operational documentation and
  `firmware/README.md` as the sole firmware authority. Keep them aligned when
  product behaviour changes.
- The KM16-Pro HID interface controls physical LEDs. Do not start the daemon,
  run a hardware preflight, or send a command that can alter the device unless
  the user explicitly requested hardware interaction in the current task.
- Keep ordinary tests independent of a physical keyboard. Use injected fakes
  and transports for device behaviour; reserve hardware smoke tests for
  explicit opt-in verification.
- A successful socket API response confirms that the Core accepted a desired
  state. It is not a HID completion barrier. Tests that assert device output
  must synchronize on a fake-transport observation or acknowledgement event.
- After an uncertain HID transaction, close the session and do not send
  further requests, including LED cleanup or matrix-effect restore.

## Change philosophy

- Keep product requirements and architecture decisions in dedicated documents,
  not in this guide.
- Inspect the relevant code, tests, documentation, and local conventions before
  editing.
- Make the smallest coherent change that fully addresses the task.
- Preserve existing architecture. Do not add layers, packages, interfaces,
  dependencies, or extension points for hypothetical future needs.
- Do not mix unrelated refactors, formatting churn, dependency upgrades, or
  renames into a focused change.
- Preserve unrelated user changes in a dirty working tree.
- When behaviour changes, update relevant tests and user-facing documentation.
- Never imply that an unexecuted check passed. State exactly what ran and what
  did not.

## Validation

- Run the narrowest relevant checks first.
- Before completion, run the documented full validation suite for broad,
  cross-cutting, concurrency-sensitive, or CI-critical changes. Otherwise run
  relevant package checks and explain their scope.
