# Developer-owned recovery

Manager schedules and enforces existing grants; the development Agent diagnoses
and repairs the task in its authorized worktree. This change does not implement
SeekDB fixes, install trial build scripts, register build-entry hashes, or grant
host administration.

## Continuation contract

Normally the Agent diagnoses and repairs within its current turn. To end a turn
without handing a recoverable failure to the user, it can submit:

```json
{
  "outcome": "blocked",
  "message": "Build failed; continue diagnosis in the authorized worktree",
  "artifacts": [],
  "recovery_request": {
    "evidence": "Actual command, exit code, saved log and observed result",
    "next_step": "A concrete next step within the existing approved scope"
  }
}
```

The other required fields follow the normal result schema. The new field is
nullable; old stored results without it remain valid. It is mutually exclusive
with environment, publishing, testing and plan-change requests. The supplied text
is work material, never executable code or a permission grant.

Manager verifies the current development run, approved plan and review identity.
It queues a system message to the same task and session, with a five-second
scheduling interval. There is no attempt cap or same-input rejection. Existing
durable messages/events provide restart recovery and runtime event deduplication.
User pause, superseding input, active environment jobs and uncertain publications
must not be bypassed. Each new run still passes existing execution-policy checks.

Known WebSocket disconnections during approved development use this continuation
path automatically. Generic failures, authentication errors and arbitrary timeouts
are not assumed safe to retry. The next turn must inspect previous side effects.
An explicit diagnosis can resume an automatically paused BLOCKED task without
invalidating its approved plan; a user-paused task remains paused.

## Environment evidence

An unavailable advertised Windows executor returns the prerequisite to the parent
without creating a permanently waiting child. A capability that disappears after
dispatch remains subject to the existing execution gate.

Native process stdout/stderr and exit codes are collected separately. Failure
stderr is retained even when the enclosing WinRM script exits zero. The parent
gets the report head and tail, preserving snapshot identity, final errors and
policy restoration evidence. Full reports remain in the environment child and
runtime job directory. Source-platform comments remain milestone-only.

## Boundaries and remaining work

Repository build scripts remain mandatory in the controlled Windows executor;
there is no independent CMake fallback. Other sandbox commands are guided by the
per-turn contract, not a universal shell interception mechanism.

Agents may prepare necessary build-entry changes in the approved worktree and
request review; they may not edit the host profile or self-register their hash.
Automated reviewer-approved build-entry registration is not implemented here.
Missing privileges and genuinely external prerequisites still need an explicit,
actionable dependency report, not endless identical environment requests.
