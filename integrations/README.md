# Agent integrations

This directory contains harness-specific ingress adapters for AgentFlare. Each
integration observes one external agent harness, converts its lifecycle signals
into the normalized AgentFlare event model, and sends those events to the local
daemon.

The daemon remains harness-independent. Adding a new harness should add a new
directory here without adding harness knowledge to the daemon core or HID
driver.

## Current integrations

- codex/: Codex hook integration. It supports global and project-local
  installation, and global installation is the documented standard. This is
  the current working integration.
- claude/: reserved extension point. No implementation is installed yet.
- opencode/: reserved extension point. No implementation is installed yet.

## Boundary

An integration may observe and normalize lifecycle metadata. It must not own
LED colors, slot assignment, terminal timers, HID access, or task control. The
AgentFlare daemon owns those responsibilities.

See codex/README.md for the complete Codex installation procedure.
