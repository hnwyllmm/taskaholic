# Agent-owned Windows work

The dev Windows profile can opt in with autonomous=true. An approved development
run then receives run-scoped exec/upload tools; read-only/reviewer runs do not.
The Agent chooses files, packaging, remote paths, build commands, tests and VM
environment changes. The old six-file Phase 0 workflow is disabled for this
profile. No per-script approval, registry policy recipe or 64 MiB archive limit
applies to the generic path.

The Codex host sandbox remains workspace-write with host networking disabled.
A mailbox client inside the session sends data to the runtime broker. The broker
only invokes the operator-configured Windows transport, never a model-supplied
host command. Credentials stay in the transport process. Uploaded files must be
regular files inside the task source worktree; rooted filesystem operations
reject traversal and symlink escapes. Git metadata is not an upload source.
The Agent may create an archive in that worktree and upload it.

Uploads stream in chunks without whole-file Base64 JSON. Local and remote SHA256
must match before the remote destination is replaced. Interrupted uploads may
leave a uniquely named .upload-* file; do not assume an interrupted exec had no
remote side effects. No automatic replay occurs.

Each operation records request, stdout, stderr and result under
WorkRoot/vm-operations/<run>/<operation>. Logs are copied into the run mailbox for
the client; start/finish events appear in task activity. Operations serialize on
the runtime's Windows slot. The mailbox is only serviced while its approved
Agent run is active; pause/cancellation closes it and cancels the transport.

Windows uses the existing VM account, with the user's authorization to control
that VM. This is not a new restricted Windows account or a host-admin grant.
VM reachability is unchanged. Linux host writes, credentials and PR publication
remain under their existing boundaries. Plan changes still require replan.

Client usage is supplied in each run:

- python3 <client.py> exec --script '<PowerShell>'
- python3 <client.py> exec --file <script inside task worktree>
- python3 <client.py> upload <file inside task worktree> <Windows destination>

For native Windows commands, scripts should explicitly propagate the native exit
code when appropriate. The returned process status is not a product test verdict.
