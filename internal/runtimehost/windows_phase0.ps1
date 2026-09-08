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
    # Hashes identify transferred inputs; they are not approval allowlists.
    if($p.repository_sha256 -notmatch '^[a-f0-9]{64}$'){throw 'Repository snapshot identity missing'}
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
        Save-JSON (Join-Path $job 'request.json') @{job_id=$p.job_id;parent_task_id=$p.parent_task_id;repository_sha256=$p.repository_sha256;snapshot_sha256=$p.snapshot_sha256}
        Save-JSON (Join-Path $job 'status.json') @{state='RUNNING';snapshot=$p.snapshot_sha256}
        $source=Join-Path $job 'source'
        $archive=Join-Path $job 'repository.tar.gz'
        [IO.File]::WriteAllBytes($archive,[Convert]::FromBase64String($p.repository_archive))
        if((Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash.ToLowerInvariant() -ne $p.repository_sha256){throw 'Repository transfer checksum mismatch'}
        & tar.exe -xzf $archive -C $source
        if($LASTEXITCODE -ne 0){throw 'Cannot extract repository snapshot'}
        $deps=Split-Path (Split-Path $vendor -Parent) -Parent
        New-Item -ItemType Directory -Path (Join-Path $source 'deps') -Force | Out-Null
        $dependencyLink=Join-Path $source 'deps\3rd'
        if(Test-Path -LiteralPath $dependencyLink){throw 'Source snapshot must not replace host dependencies'}
        New-Item -ItemType Junction -Path $dependencyLink -Target $deps | Out-Null
        $allowed=@('CMakeLists.txt','path_fixture.h','path_fixture_test.cpp','phase0.manifest','README.md','sqlite_path_probe.cpp')
        if($p.files.Count -ne 6){throw 'Incomplete source snapshot'}
        $seen=@{}
        foreach($file in $p.files){
            if($file.name -cnotin $allowed -or $seen.ContainsKey($file.name)){throw 'Invalid source snapshot filename'}
            $seen[$file.name]=$true
            $probeFile=Join-Path (Join-Path $source 'tools\windows\long_path_phase0') $file.name
            $actual=[Convert]::ToBase64String([IO.File]::ReadAllBytes($probeFile))
            if($actual -cne $file.data){throw 'Probe changed during repository snapshot'}
        }
        $result.status='failed'
        Write-Output ('SOURCE_SNAPSHOT_SHA256='+$p.snapshot_sha256)
        $build=Join-Path $source 'build_phase0'
        $buildScript=Join-Path $source 'build.ps1'
        Write-Output ('REPOSITORY_SHA256='+$p.repository_sha256)
        Write-Output ('REPOSITORY_BUILD_SHA256='+(Get-FileHash -LiteralPath $buildScript -Algorithm SHA256).Hash)
        $buildLog=Join-Path $job 'repository-build.log'
        $buildErr=Join-Path $job 'repository-build.stderr.log'
        Write-Output ('BUILD_COMMAND=powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "'+$buildScript+'" phase0 -j 4')
        $buildProcess=Start-Process -FilePath 'powershell.exe' -ArgumentList @('-NoProfile','-NonInteractive','-ExecutionPolicy','Bypass','-File',('"'+$buildScript+'"'),'phase0','-j','4') -WorkingDirectory $source -RedirectStandardOutput $buildLog -RedirectStandardError $buildErr -NoNewWindow -Wait -PassThru
        $buildExit=$buildProcess.ExitCode
        Get-Content -LiteralPath $buildLog -Tail 160
        Get-Content -LiteralPath $buildErr -Tail 80
        Write-Output ('BUILD_EXIT_CODE='+$buildExit)
        if($buildExit -ne 0){throw ('Repository build.ps1 failed, exit='+$buildExit+'; logs: '+$buildLog+' and '+$buildErr+'. No alternate build invocation attempted.')}
        $exe=Join-Path $build 'sqlite_path_probe.exe'
        if(!(Test-Path -LiteralPath $exe)){throw 'Probe executable missing'}
    Add-Type -TypeDefinition @'
using System;
using System.ComponentModel;
using System.Runtime.InteropServices;
public static class SeekDBManifest {
    [DllImport("kernel32.dll", CharSet=CharSet.Unicode, SetLastError=true)]
    static extern IntPtr LoadLibraryEx(string file, IntPtr reserved, uint flags);
    [DllImport("kernel32.dll", SetLastError=true)]
    static extern IntPtr FindResource(IntPtr module, IntPtr name, IntPtr type);
    [DllImport("kernel32.dll", SetLastError=true)]
    static extern uint SizeofResource(IntPtr module, IntPtr resource);
    [DllImport("kernel32.dll", SetLastError=true)]
    static extern IntPtr LoadResource(IntPtr module, IntPtr resource);
    [DllImport("kernel32.dll", SetLastError=true)]
    static extern IntPtr LockResource(IntPtr resource);
    [DllImport("kernel32.dll")] static extern bool FreeLibrary(IntPtr module);
    public static byte[] Read(string file) {
        var module = LoadLibraryEx(file, IntPtr.Zero, 2); // AS_DATAFILE: never execute.
        if (module == IntPtr.Zero) throw new Win32Exception();
        try {
            var resource = FindResource(module, new IntPtr(1), new IntPtr(24));
            if (resource == IntPtr.Zero) throw new Win32Exception();
            var size = SizeofResource(module, resource);
            if (size == 0 || size > 1048576) throw new InvalidOperationException("Invalid manifest size");
            var loaded = LoadResource(module, resource);
            if (loaded == IntPtr.Zero) throw new Win32Exception();
            var data = LockResource(loaded);
            if (data == IntPtr.Zero) throw new Win32Exception();
            var bytes = new byte[(int)size];
            Marshal.Copy(data, bytes, 0, bytes.Length);
            return bytes;
        } finally { FreeLibrary(module); }
    }
}
'@
    [IO.File]::WriteAllBytes((Join-Path $build 'extracted.manifest'), [SeekDBManifest]::Read($exe))
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
            # Native stderr must not terminate the PowerShell loop before we
            # record the exit code and run the other authorized policy case.
            $probeErr=Join-Path $job ('policy-'+$policy+'.stderr.log')
            Write-Output ('PROBE_COMMAND="'+$exe+'" "'+$cases+'" '+$policy)
            $probeProcess=Start-Process -FilePath $exe -ArgumentList @(('"'+$cases+'"'),[string]$policy) -RedirectStandardOutput $log -RedirectStandardError $probeErr -NoNewWindow -Wait -PassThru
            $exit=$probeProcess.ExitCode
            Get-Content -LiteralPath $log
            Get-Content -LiteralPath $probeErr
            Write-Output ('PROBE_EXIT_CODE policy='+$policy+' exit='+$exit)
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
