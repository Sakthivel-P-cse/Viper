# Viper project instructions

Before building, implementing, refactoring, debugging, or testing this
repository, read [the Codex development plan](docs/codex-development-plan.md)
and use its current phase, task boundaries, model assignment, and acceptance
gate.

Project-specific rules:

- Treat docs/industry-grade/design.md as the architectural baseline and
  docs/industry-grade/storage.md as the durability contract. If the older
  proposal in documentation.md conflicts with either document, do not make a
  silent choice; surface the conflict and follow the industry-grade baseline.
- Use Sol high for architecture, Raft state transitions, persistence and
  recovery semantics, public protocol or persisted-format changes, security,
  concurrency, fault models, checker algorithms, cross-package changes, and
  final correctness reviews.
- Use Luna max only for bounded, low-risk work whose behavior and interfaces
  are already specified: adapters, fixtures, mechanical test cases, generated
  wiring, documentation, formatting, and CLI/demo presentation. Luna must
  stop and escalate when a task introduces a new invariant, protocol rule,
  security decision, persisted-format change, or cross-package contract.
- For split tasks, Sol high defines the contract and acceptance criteria,
  Luna max may implement the bounded slice, and Sol high reviews the result.
  Model selection never replaces tests, race detection, static analysis, or
  the phase gate.
- Treat model classification as pre-task guidance. When the user explicitly
  assigns a specific in-scope task, complete it end-to-end with the currently
  active model and do not pause merely to request a model switch. At the end,
  report the recommended model for the next task. Pause for safety, approval,
  or unresolved scope issues, not merely because another model would be
  preferable.
- Preserve unrelated worktree changes. Do not claim a phase is complete until
  its written acceptance criteria and checks pass.
