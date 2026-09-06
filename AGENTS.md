## General

- Keep AGENTS.md instructions concise and omit terminal periods
- Keep changes focused and separate unrelated cleanup from behavior changes
- Prefer direct code and small amounts of duplication over premature generalization
- Make state transitions, side effects, ownership, cleanup, external-format assumptions, and non-obvious decisions explicit in names or comments
- Comment intent and invariants without narrating obvious mechanics
- Handle errors explicitly, preserving causes and earlier failures, and explain intentionally ignored errors
- Keep concurrency bounded and make synchronization, cancellation, and completion explicit
- Test observable behavior and relevant failures and add regression tests for bug fixes when practical
- Structure tests with explicit `Given`, `When`, and `Then` sections
- Keep important inputs and expected outcomes visible and hide only mechanical test setup
- Define test helpers before the tests that use them

## Go

- Optimize code for a human maintainer with some Go knowledge, favoring clarity and explicit behavior
- Follow idiomatic Go style including naming and formatting
- Use descriptive domain names and conventional short names in small, obvious scopes
- Name functions for their behavior, prefer verbs for actions and `newX` for constructors
- Keep the main flow left-aligned and extract helpers only for meaningful operations or distinct responsibilities
- Define interfaces at the consumer only for a concrete substitution or testing need
- Keep call sites self-explanatory, especially for booleans and adjacent values of the same type
- Prefer the standard library and existing dependencies and justify additions

## Workflow

- Use Make targets for Go work, running `make agent-init` first to configure agent-writeable caches
- Run `make check` before handing off code changes and report any checks not run
- Run `make test-e2e` for changes to release packaging, installation, or device integration
- Run `make ci` to reproduce the complete CI suite locally
- Keep local and CI checks behind shared Make targets and pin analysis-tool versions compatible with the project Go version
- Ensure Make recipes propagate tool failures
- Verify new gates reject representative violations in an isolated checkout
- Require native checks and the ARM VM test before publishing releases
- Fix violations rather than weakening checks and keep suppressions narrow and explained

## Project

- Preserve read-only access to the Nickel database
- Prioritize partial failures, interrupted conversion, existing-file preservation, and uncertain reading status over coverage percentages
