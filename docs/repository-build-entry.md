# Repository-owned Windows build entry

Legacy executor documentation. For profiles with autonomous=true (dev win11),
this path is disabled; see autonomous-windows.md. The Agent owns upload and
execution using generic tools, without snapshot size or fixed-target gates.

The approved development Agent may edit build.ps1 and build configuration in its
task worktree, then request Windows validation without a PR, extra review, or
script-hash registration. Plan, repository and host-operation grants are unchanged.

The runtime snapshots current Git tracked and non-ignored untracked source,
including working-tree edits and deletions, excluding Git metadata and symlinks.
The compressed snapshot is persisted and transferred to a fresh Windows job
directory. Its SHA256 proves transfer identity, not approval. Probe files are
checked against the six-file evidence snapshot to detect concurrent edits.

The executor uses the existing host dependencies and invokes only:

```powershell
powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File <job>/source/build.ps1 phase0 -j 4
```

Expected output: source/build_phase0/sqlite_path_probe.exe. The executor reads
the embedded manifest itself, verifies the pinned SQLite inputs, runs the
serialized policy 0/1 matrix and restores the prior setting. No special
WorkAssistantRequest interface, build-result.json, script allowlist, or manual
registration is required. Existing profile registration fields are ignored.

Compiler flags and target definitions remain owned by the repository. A missing
target or build failure returns actionable stdout/stderr/exit evidence to the
original developer session; the operator does not repair product code.

Isolation here means a separate source/build directory in the dedicated Windows
VM. The existing WinRM account still runs the job; this change does not implement
a new non-administrator Windows process sandbox. Agents receive no host
credentials or new Linux privileges. Repository scripts must not install tools,
alter host configuration or bypass the granted workflow. Stronger Windows
account isolation is separate work, not a new per-script approval requirement.
