# Repository instructions for AI developers

Read [ARCHITECTURE.md](ARCHITECTURE.md) and [DEVELOPMENT.md](DEVELOPMENT.md) before changing behavior. They describe ownership, feature locations, state contracts, validation, migrations and distribution. Read the relevant module README when changing networking or quality behavior.

## Working rules

- Inspect the worktree and preserve existing user changes. Identify the observable requirement, primary owner, actual callers and affected state before editing.
- Keep business rules in their owning domain/application service. HTTP encodes protocols, frontend owns interaction, storage supplies atomic mechanisms, and the composition root owns lifecycle. Do not reintroduce broad writers or duplicate policies.
- A directory can contain several logical responsibilities. Gateway includes execution, account selection and admission cooperation; moving files alone does not resolve ownership.
- Preserve authorization on execution and resource access, identity/generation/CAS checks, bounded physical attempts, independent completion facts and explicit resource handoff. Cancellation must not erase already accepted work or charges.
- Owned tasks must be cancelable and joined. Handle failure, EOF, timeout, repeated close, page unmount and late responses through the existing lifecycle contract.
- For persistent changes, cover SQLite/PostgreSQL compatibility, current transaction checks, concurrent instances, old data and rollback limits. Never clear business data to make a migration or test pass.
- Keep implementation comments and durable documentation focused on current contracts. Update the existing guide when a boundary changes.
- Start with isolated configuration, data directories and credentials; an environment database URL can override the file. Preserve local audit journals and stable instance identities during recovery.
- Treat interface defaults, empty values, stream terminal events, old clients and duplicate submissions as compatibility contracts. A request ID is not an idempotency guarantee.

## Repository contents

- Commit product code, required generated contracts, maintained tests, synthetic/minimal fixtures and durable project documentation.
- Keep plans, task logs, review rounds, raw test/benchmark output, screenshots, traces, database exports and one-off probes outside the repository. Do not force-add ignored files.
- Never commit live credentials, private configuration, cookies, signed URLs, prompts/responses or identifying operational data. Tests are subject to the same rule; synthetic data should be visibly fictional.
- Review staged content and run `python3 scripts/check-repository.py --staged`. Ignore rules do not remove tracked files or old commits. Do not push a development history containing private work records as a way to publish a clean final tree.
- Preserve shared history unless rewriting it was explicitly requested. Make local, reviewable changes first; publishing/deploying must follow the user's authorized scope.
- The GHCR workflow publishes on pushes to main and version tags. Check workflow conditions before pushing. Before removing ignored directories, check processes, mounts, cron and configuration references.

## Validation

- Fast repository/backend boundary checks: `python3 scripts/check-repository.py`, `bash scripts/verify_test.sh`, `make verify-check`.
- Backend changes: relevant `go test` packages, then appropriate `go vet`/build; use `-race -timeout 30m` for concurrency/lifecycle changes. `make verify` and `make verify-full` are broader backend checks.
- Frontend changes: `pnpm test`, `pnpm lint`, `pnpm build` in `frontend/`; verify interactive/lifecycle changes in a browser.
- Public API annotations: run `make swagger` and review the generated contract.
- Use disposable, isolated databases/Redis for integration tests. Check actual test environment variables and report skipped dependencies. Live inference and load scripts consume real upstream resources and require an authorized target.
- Report the behavior changed, validation actually run, and material compatibility limits. Do not commit execution transcripts as evidence.
