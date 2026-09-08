$ErrorActionPreference = 'Stop'
$p = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('__PAYLOAD_BASE64__')) | ConvertFrom-Json
$cfg = $p.profile
$root = 'C:\work-assistant'
$policyPath = 'HKLM:\SYSTEM\CurrentControlSet\Control\FileSystem'
$result = @{status='unavailable';profile='windows_seekdb_phase0';snapshot_sha256=$p.snapshot_sha256;message=''}
$lease = $null
$original = $null
$journal = Join-Path $root 'policy-recovery.json'
$job = Join-Path $root ('jobs\'+$p.job_id)
function Save-JSON($path,$value) {
    $tmp=$path+'.tmp'
    $data=[Text.UTF8Encoding]::new($false).GetBytes(($value | ConvertTo-Json -Depth 8 -Compress))
    $stream=[IO.File]::Open($tmp,[IO.FileMode]::Create,[IO.FileAccess]::Write,[IO.FileShare]::None)
    try{$stream.Write($data,0,$data.Length);$stream.Flush($true)}finally{$stream.Dispose()}
    Move-Item -LiteralPath $tmp -Destination $path -Force
}
function Restore-Policy($saved) {
    Set-ItemProperty -LiteralPath $policyPath -Name LongPathsEnabled -Type DWord -Value ([int]$saved.value)
    if ((Get-ItemPropertyValue -LiteralPath $policyPath -Name LongPathsEnabled) -ne [int]$saved.value) {throw 'LongPathsEnabled restoration verification failed'}
}
try {
    if($p.job_id -notmatch '^[A-Za-z0-9_-]{1,160}$'){throw 'Invalid job identity'}
    if($p.parent_task_id -notmatch '^[A-Za-z0-9_-]{1,160}$'){throw 'Invalid parent identity'}
    # Only an explicitly registered repository build.ps1 may build this target.
    # Missing support is an integration issue, never a reason to guess flags.
    if($cfg.build_contract -ne 'seekdb-phase0-v1' -or !$cfg.repository_root -or $cfg.build_script_sha256 -notmatch '^[a-fA-F0-9]{64}$'){throw 'Repository build entry not registered for Phase 0; integrate and review build.ps1 support first. No direct CMake fallback.'}
    $buildScript=Join-Path $cfg.repository_root 'build.ps1'
    if(!(Test-Path -LiteralPath $buildScript -PathType Leaf)){throw 'Registered repository build.ps1 is missing'}
    $buildScriptHash=(Get-FileHash -LiteralPath $buildScript -Algorithm SHA256).Hash.ToLowerInvariant()
    if($buildScriptHash -ne $cfg.build_script_sha256.ToLowerInvariant()){throw 'Repository build.ps1 changed; review and register its new hash before execution'}
    Write-Output ('REPOSITORY_BUILD_ENTRY='+$buildScript)
    Write-Output ('REPOSITORY_BUILD_SHA256='+$buildScriptHash)
    $headerHash=(Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $cfg.sqlite_include 'sqlite3.h')).Hash.ToLowerInvariant()
    $libraryHash=(Get-FileHash -Algorithm SHA256 -LiteralPath $cfg.sqlite_library).Hash.ToLowerInvariant()
	$vendor=Split-Path (Split-Path $cfg.sqlite_library -Parent) -Parent
	$dll=Join-Path $vendor 'bin\sqlite3.dll'
	$dllHash=(Get-FileHash -Algorithm SHA256 -LiteralPath $dll).Hash.ToLowerInvariant()
	if($dllHash -ne $cfg.sqlite_dll_sha256.ToLowerInvariant()){throw 'Pinned SQLite DLL checksum mismatch'}
    if($headerHash -ne $cfg.sqlite_header_sha256.ToLowerInvariant() -or $libraryHash -ne $cfg.sqlite_library_sha256.ToLowerInvariant()){throw 'Pinned SQLite dependency checksum mismatch; no fallback library selected'}
    if(!$cfg.allow_policy_switch){throw 'This profile requires explicit permission to switch and restore LongPathsEnabled'}
    if($p.mode -eq 'preflight') {
        $result.status='passed';$result.message='Windows channel, configured toolchain and pinned SQLite dependencies are available.'
    } elseif($p.mode -eq 'execute') {
        New-Item -ItemType Directory -Path $root -Force | Out-Null
        $lease=[IO.File]::Open((Join-Path $root 'executor.lock'),[IO.FileMode]::OpenOrCreate,[IO.FileAccess]::ReadWrite,[IO.FileShare]::None)
        # Recover an interrupted previous execution before making further changes.
        if(Test-Path -LiteralPath $journal){
            $saved=Get-Content -LiteralPath $journal -Raw | ConvertFrom-Json
            Restore-Policy $saved
            Move-Item -LiteralPath $journal -Destination (Join-Path $root ('policy-recovered-'+[guid]::NewGuid().ToString('N')+'.json'))
        }
        if(Test-Path -LiteralPath $job){throw 'Job directory already exists; refusing duplicate execution'}
        New-Item -ItemType Directory -Path (Join-Path $job 'source') | Out-Null
        Save-JSON (Join-Path $job 'request.json') $p
        Save-JSON (Join-Path $job 'status.json') @{state='RUNNING';snapshot=$p.snapshot_sha256}
        $allowed=@('CMakeLists.txt','path_fixture.h','path_fixture_test.cpp','phase0.manifest','README.md','sqlite_path_probe.cpp')
        if($p.files.Count -ne 6){throw 'Incomplete source snapshot'}
        $seen=@{}
        foreach($file in $p.files){
            if($file.name -cnotin $allowed -or $seen.ContainsKey($file.name)){throw 'Invalid source snapshot filename'}
            $seen[$file.name]=$true
            [IO.File]::WriteAllBytes((Join-Path (Join-Path $job 'source') $file.name),[Convert]::FromBase64String($file.data))
        }
        $result.status='failed'
        Write-Output ('SOURCE_SNAPSHOT_SHA256='+$p.snapshot_sha256)
        $build=Join-Path $job 'build'
        # This versioned request is data, not a model-supplied command line.
        # The repository owns all configure/compile/link options and must emit
        # build/sqlite_path_probe.exe and build/extracted.manifest.
        $request=Join-Path $job 'repository-build-request.json'
        Save-JSON $request @{contract='seekdb-phase0-v1';source_directory=(Join-Path $job 'source');build_directory=$build;snapshot_sha256=$p.snapshot_sha256;sqlite_include=$cfg.sqlite_include;sqlite_library=$cfg.sqlite_library}
        & powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $buildScript -WorkAssistantRequest $request
        if($LASTEXITCODE -ne 0){throw 'Repository build.ps1 failed; inspect its output. No alternate build invocation attempted.'}
        $receiptPath=Join-Path $build 'build-result.json'
        if(!(Test-Path -LiteralPath $receiptPath)){throw 'Repository build entry did not return build-result.json'}
        $receipt=Get-Content -LiteralPath $receiptPath -Raw | ConvertFrom-Json
        if($receipt.contract -ne 'seekdb-phase0-v1' -or $receipt.snapshot_sha256 -ne $p.snapshot_sha256 -or $receipt.build_script_sha256 -ne $buildScriptHash){throw 'Repository build receipt does not match the requested source and registered entry'}
        $exe=Join-Path $build 'sqlite_path_probe.exe'
        if(!(Test-Path -LiteralPath $exe)){throw 'Probe executable missing'}
        $manifest=Get-Content -LiteralPath (Join-Path $build 'extracted.manifest') -Raw
        if($manifest -notmatch 'longPathAware[^>]*>\s*true\s*<'){throw 'Probe manifest does not declare longPathAware=true'}
        Write-Output ('PROBE_SHA256='+(Get-FileHash -Algorithm SHA256 -LiteralPath $exe).Hash)
        # Dynamic SQLite must be the configured vendor DLL, not a DLL found on PATH.
        Copy-Item -LiteralPath $dll -Destination $build
        Write-Output ('SQLITE_DLL_SHA256='+$dllHash)
        $original=@{value=[int](Get-ItemPropertyValue -LiteralPath $policyPath -Name LongPathsEnabled);job=$p.job_id}
        Save-JSON $journal $original
        $cases=Join-Path $job 'cases'
        New-Item -ItemType Directory -Path $cases | Out-Null
        $failed=@()
        foreach($policy in @(0,1)){
            Set-ItemProperty -LiteralPath $policyPath -Name LongPathsEnabled -Type DWord -Value $policy
            if((Get-ItemPropertyValue -LiteralPath $policyPath -Name LongPathsEnabled) -ne $policy){throw 'Policy switch verification failed'}
            $log=Join-Path $job ('policy-'+$policy+'.log')
            & $exe $cases $policy *> $log
            $exit=$LASTEXITCODE
            Get-Content -LiteralPath $log
            if($exit -ne 0 -or !(Select-String -LiteralPath $log -SimpleMatch ('SQLITE_EXTENDED_MATRIX_PASS policy='+$policy) -Quiet)){$failed+=('policy='+$policy+', exit='+$exit)}
        }
        if($failed.Count -gt 0){throw ('Windows Phase 0 failed: '+($failed -join '; '))}
        $result.status='passed';$result.message='Windows Phase 0 probe passed under both policy settings. This is not final SeekDB acceptance; review the exact dependency and snapshot evidence.'
    } else {throw 'Unknown executor mode'}
} catch {
    $result.message=$_.Exception.Message
} finally {
    if($original -ne $null){
        try {
            Restore-Policy $original
            Move-Item -LiteralPath $journal -Destination (Join-Path $job 'policy-restored.json')
            Write-Output ('POLICY_RESTORED='+$original.value)
        } catch {
            $result.status='unavailable';$result.message='Policy restoration failed; recovery journal retained. Stop further tests and restore the VM policy.'
        }
    }
    if($lease -ne $null){
        if(Test-Path -LiteralPath $job){try{Save-JSON (Join-Path $job 'status.json') $result}catch{$result.status='unavailable';$result.message='Cannot persist Windows result; inspect job directory before retrying.'}}
        $lease.Dispose()
    }
}
Write-Output ('WORK_ASSISTANT_RESULT='+($result | ConvertTo-Json -Compress -Depth 6))
