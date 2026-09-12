# Project-local Codex integration instructions

- `codex/hooks.json` is the canonical hook template. Activation must go through
  `integrations/codex/install.py` using its locked `uv` project, which creates
  the target `.codex/hooks.json`, materializes a permanent base-Python bridge
  command, installs the adjacent package, preserves unrelated hooks, and
  records a backup. Contributors should not instruct users to copy files by
  hand.
  Changes to the hook definition or bridge require normal Codex review and
  project trust.
- Hooks are an ingress boundary only. They may forward allowlisted lifecycle
  metadata to the existing AgentFlare service, but must not start, resume,
  interrupt, answer, or otherwise control the existing Codex task.
- Do not add direct HID access, LED logic, prompt content, tool content, or
  transcript persistence to this directory.
- Keep runtime state and logs outside the repository unless a test explicitly
  uses a temporary directory.
- Resolve repository-local paths from the repository root so hooks remain valid
  when Codex invokes them with a subdirectory as the working directory.
