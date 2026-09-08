# Repository-owned Windows build entry

The Windows Phase 0 executor no longer configures or compiles a standalone CMake
project, extracts compile flags, or chooses compiler options. No legacy fallback
exists. Other Agent shell execution is governed by instructions, not a general
shell sandbox that can distinguish every compiler invocation.

Register `repository_root`, `build_contract: seekdb-phase0-v1`, and the reviewed
`build_script_sha256` in the host-owned Windows profile. The entry is always
`<repository_root>/build.ps1`. A script change requires registration again.
Existing profiles are deliberately not auto-registered: the current SeekDB
script does not yet implement the probe contract. This requires a repository
integration and review before Windows probe execution can resume.

The executor invokes the script in a child PowerShell process with only
`-WorkAssistantRequest <job-local JSON path>`. The process-only execution policy
does not change machine policy. The request contains:

- `contract`: `seekdb-phase0-v1`
- `source_directory`: frozen probe snapshot directory
- `build_directory`: fresh job-local output directory
- `snapshot_sha256`: immutable source identity
- `sqlite_include`, `sqlite_library`: previously configured pinned dependencies

The repository must integrate the probe as a target sharing the product build
configuration. A wrapper that merely recreates the independent CMake project is
not an acceptable implementation. Do not change product defaults to make a
probe pass. Validate the product through the same build entry after integration.

On success the entry emits `build/sqlite_path_probe.exe`, its extracted
`build/extracted.manifest`, and `build/build-result.json` containing matching
`contract`, `snapshot_sha256`, and `build_script_sha256`. Missing artifacts,
nonzero exit, or a mismatched receipt stop execution; no alternate build command
is attempted. Script output is retained in the executor log. The executor still
owns the serialized policy matrix, pinned SQLite DLL verification, and policy
restoration. Retry counts remain unlimited; same-job receipts remain idempotent.
