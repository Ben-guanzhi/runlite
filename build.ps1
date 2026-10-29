# build.ps1 - build a "copy it and run" runlite.
#
#   .\build.ps1            gates -> compile -> portable zip
#   .\build.ps1 -Verify    also unzip into a temp dir and run it with a clean PATH
#   .\build.ps1 -GUI       also build/pack the Fyne desktop app (separate module, separate zip)
#   .\build.ps1 -SkipGates compile/pack only
#
# Only windows/amd64: that is the one target actually exercised on this machine.
# A cross-compiled binary nobody ran is not evidence.
#
# Keep this file ASCII-only. Windows PowerShell 5.1 reads .ps1 as ANSI, so a
# UTF-8 Chinese comment silently breaks the parser (hit here on the first run).

param(
    [switch]$SkipGates,
    [switch]$Verify,
    [switch]$GUI,
    [string]$OutDir = "dist"
)

$ErrorActionPreference = "Stop"
Set-Location -Path $PSScriptRoot

# Read a UTF-8-emitting child without PowerShell mangling it.
# PS 5.1 decodes a child's stdout with the console codepage (GBK on this box),
# so `& exe help` hands back mojibake before you even write a file - the first
# artifact shipped usage.txt as a wall of '?'. Start-Process hands the child an
# inherited file handle instead, so the bytes in that file are exactly what Go
# wrote, and we decode them as UTF-8 ourselves.
function Invoke-Exe {
    param([string]$Exe, [string[]]$ChildArgs)
    $outFile = [System.IO.Path]::GetTempFileName()
    try {
        $p = Start-Process -FilePath $Exe -ArgumentList $ChildArgs -NoNewWindow -Wait -PassThru `
            -RedirectStandardOutput $outFile
        @{ Exit = $p.ExitCode; Out = [System.IO.File]::ReadAllText($outFile, [System.Text.Encoding]::UTF8) }
    }
    finally {
        Remove-Item -Force $outFile -ErrorAction SilentlyContinue
    }
}

# Single source of truth for the version: the constant the daemon reports at
# /api/health and `runlite version` prints. Duplicating the number here would
# let the zip name and the running daemon disagree.
$src = Get-Content -Raw -Encoding UTF8 "internal/api/api.go"
if ($src -notmatch 'const\s+Version\s*=\s*"([^"]+)"') {
    throw "version constant not found in internal/api/api.go"
}
$version = $Matches[1]
$stamp = (Get-Date).ToUniversalTime().ToString("yyyyMMdd-HHmmZ")
$name = "runlite-$version-windows-amd64"
$stage = Join-Path $OutDir $name
$exe = Join-Path $stage "runlite.exe"
$zip = Join-Path $OutDir "$name-$stamp.zip"

if (-not $SkipGates) {
    Write-Host "== gates: gofmt / go vet / go test ==" -ForegroundColor Cyan
    $format = gofmt -l .
    if ($format) { throw "not formatted:`n$format" }
    go vet ./...
    if ($LASTEXITCODE -ne 0) { throw "go vet failed" }
    go test -count=1 ./...
    if ($LASTEXITCODE -ne 0) { throw "go test failed" }
    # The desktop app is a second module, so the gates above never see it:
    # ./... in the root module stops at the module boundary. Without this the
    # GUI could ship red.
    if ($GUI) {
        Write-Host "== gates: runlite/gui module ==" -ForegroundColor Cyan
        Push-Location gui
        try {
            $gofmtGui = gofmt -l .
            if ($gofmtGui) { throw "gui module not formatted:`n$gofmtGui" }
            go vet ./...
            if ($LASTEXITCODE -ne 0) { throw "gui go vet failed" }
            go test -count=1 ./...
            if ($LASTEXITCODE -ne 0) { throw "gui go test failed" }
        }
        finally { Pop-Location }
    }
}

Write-Host "== building $name ==" -ForegroundColor Cyan
if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
New-Item -ItemType Directory -Force -Path $stage | Out-Null

# CGO_ENABLED=0: everything talks to the OS through syscall, no C runtime needed,
# so the build works even where MinGW is absent.
$env:CGO_ENABLED = "0"
$env:GOOS = "windows"
$env:GOARCH = "amd64"
go build -ldflags "-s -w" -o $exe ./cmd/runlite
if ($LASTEXITCODE -ne 0) { throw "compile failed" }

# The usage note is printed by the binary itself, so the text in the zip can
# never drift away from the flags that actually exist.
Write-Host "== generating usage.txt ==" -ForegroundColor Cyan
$help = Invoke-Exe $exe @("help")
if ($help.Exit -ne 0) { throw "the freshly built exe cannot even print help" }
# Exit code 0 is not enough: the whole value of this file is the Chinese text,
# and a codepage wreck keeps the exit code clean. Check the content.
if ($help.Out.Length -lt 200) { throw ("help output is only {0} chars - usage.txt would be near-empty" -f $help.Out.Length) }
# The CJK range is written as an escape, not literal characters: this file must
# stay ASCII-only (see the header note about the ANSI parser).
# CJK range as an escape, not literal characters: this file must stay ASCII-only.
if ($help.Out -notmatch "[\u4e00-\u9fff]") { throw "help output has no Chinese characters - capture is still mangled" }
if ($help.Out -match "\?\?\?\?") { throw "help output contains '????' - console codepage ate it" }
[System.IO.File]::WriteAllText((Join-Path $stage "usage.txt"), $help.Out, (New-Object System.Text.UTF8Encoding($false)))

# The zip is named after this version; the binary inside has to agree, or a
# support conversation about "the 0.1.0 zip" is about a build we cannot locate.
$ver = Invoke-Exe $exe @("version")
if ($ver.Exit -ne 0) { throw "runlite version failed in the freshly built exe" }
if ($ver.Out -notmatch [regex]::Escape($version)) {
    throw ("the exe reports '{0}' but the artifact is named {1}" -f $ver.Out.Trim(), $name)
}
Write-Host ("   version needle ok: {0}" -f $ver.Out.Trim())

# Package the folder, not its loose contents: unpacking should drop one named
# directory you can double-click into, not spray runlite.exe across wherever
# the extractor happened to run.
Compress-Archive -Path $stage -DestinationPath $zip -Force
$sha = (Get-FileHash -Algorithm SHA256 $zip).Hash
Write-Host ("== artifact == {0}  {1:N1} KB" -f $zip, ((Get-Item $zip).Length / 1KB)) -ForegroundColor Green
Write-Host ("   SHA256 {0}" -f $sha)

# ---------------------------------------------------------------- GUI --------
# The desktop app is built last and packed into its own zip: it is the only
# artifact that needs a C toolchain, and nobody should lose the CLI because the
# GUI failed.
if ($GUI) {
    Write-Host "== gui: runlite-gui (Fyne) ==" -ForegroundColor Cyan
    if (-not (Get-Command gcc -ErrorAction SilentlyContinue)) {
        throw "the GUI needs CGO for OpenGL and there is no gcc on PATH (MinGW-w64 or Strawberry C); build the CLI without -GUI"
    }
    $guiName = "runlite-gui-$version-windows-amd64"
    $guiStage = Join-Path $OutDir $guiName
    $guiExeRel = Join-Path $guiStage "runlite-gui.exe"
    $guiExe = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot $guiExeRel))
    $guiZip = Join-Path $OutDir "$guiName-$stamp.zip"

    if (Test-Path $guiStage) { Remove-Item -Recurse -Force $guiStage }
    New-Item -ItemType Directory -Force -Path $guiStage | Out-Null

    # CGO_ENABLED=1 is scoped to this one build and restored in the finally:
    # the CLI above was compiled with 0 and stays that way. -H windowsgui keeps
    # Windows from opening a black console behind the window (a GUI-subsystem
    # exe gets no console at all).
    $env:CGO_ENABLED = "1"
    try {
        Push-Location gui
        try {
            go build -ldflags "-s -w -H windowsgui" -o $guiExe .
            $guiBuildExit = $LASTEXITCODE
        }
        finally { Pop-Location }
    }
    finally { $env:CGO_ENABLED = "0" }
    if ($guiBuildExit -ne 0) { throw "gui compile failed" }
    if (-not (Test-Path $guiExe)) { throw ("gui build reported success but {0} is not there" -f $guiExe) }

    # The subsystem flag is the whole point of -H windowsgui, and a wrong flag
    # still yields a perfectly runnable exe - so check the binary, not the flags
    # we typed. debug/pe reads it straight out of the PE optional header;
    # 2 = WINDOWS_GUI, 3 = WINDOWS_CUI.
    $peDir = Join-Path ([System.IO.Path]::GetTempPath()) ("runlite-pecheck-" + [Guid]::NewGuid().ToString("N"))
    New-Item -ItemType Directory -Force -Path $peDir | Out-Null
    $peMain = Join-Path $peDir "main.go"
    [System.IO.File]::WriteAllText($peMain, @'
package main

import (
	"debug/pe"
	"fmt"
	"os"
)

func main() {
	f, err := pe.Open(os.Args[1])
	if err != nil {
		fmt.Println("ERR", err)
		os.Exit(1)
	}
	defer f.Close()
	oh, ok := f.OptionalHeader.(*pe.OptionalHeader64)
	if !ok {
		if o32, ok32 := f.OptionalHeader.(*pe.OptionalHeader32); ok32 {
			fmt.Println("SUBSYSTEM", o32.Subsystem)
			return
		}
		fmt.Println("ERR no windows optional header")
		os.Exit(1)
	}
	fmt.Println("SUBSYSTEM", oh.Subsystem)
}
'@, (New-Object System.Text.UTF8Encoding($false)))
$pe = Invoke-Exe "go" @("run", $peMain, $guiExe)
Remove-Item -Recurse -Force $peDir -ErrorAction SilentlyContinue
if ($pe.Exit -ne 0) { throw ("cannot read the gui exe's PE header: {0}" -f $pe.Out.Trim()) }
if ($pe.Out -notmatch "SUBSYSTEM 2\b") {
    throw ("-H windowsgui did not take: subsystem is '{0}' (want 2 = WINDOWS_GUI)" -f $pe.Out.Trim())
}
Write-Host ("   subsystem check ok: {0}" -f $pe.Out.Trim())

# The GUI auto-starts the daemon on demand (same path as `runlite daemon start`),
# which needs the CLI binary to spawn `runlite daemon exec`. Ship runlite.exe next to
# runlite-gui.exe so a fresh extract double-clicks straight into a working daemon -
# this is what lets "double-click the GUI" just work without a terminal first.
Copy-Item $exe (Join-Path $guiStage "runlite.exe") -Force

# Same reason the CLI zip carries usage.txt: the artifact should say what it is.
[System.IO.File]::WriteAllText((Join-Path $guiStage "README-gui.txt"), (@"
runlite-gui $version (windows/amd64)

This is a client of the same daemon as the CLI. Double-clicking runlite-gui.exe
starts the daemon for you if it is not already running (same path as
`runlite daemon start`), then connects - so no terminal is needed first.
That needs runlite.exe sitting next to this file (shipped in this zip), or
runlite on PATH.

"daemon not running" and "project won't start" stay two different things: the
former is a red screen with a Retry button, the latter is a project row in
failed state. The GUI only auto-starts the daemon; it never starts your
projects.

Needs the Visual C++ runtime that Go links against for OpenGL; on a machine
without it the exe exits immediately. That is why it is a separate zip.
"@), (New-Object System.Text.UTF8Encoding($false)))

Compress-Archive -Path $guiStage -DestinationPath $guiZip -Force
$guiSha = (Get-FileHash -Algorithm SHA256 $guiZip).Hash
Write-Host ("== gui artifact == {0}  {1:N1} MB" -f $guiZip, ((Get-Item $guiZip).Length / 1MB)) -ForegroundColor Green
Write-Host ("   SHA256 {0}" -f $guiSha)

# Sizes are the honest cost of the GUI: record them next to the CLI numbers so a
# "why is the desktop zip 40x bigger" question has an answer in the build log.
$cliMods = (go list -m all | Measure-Object -Line).Lines
Push-Location gui
try { $guiMods = (go list -m all | Measure-Object -Line).Lines }
finally { Pop-Location }
Write-Host ("   modules: cli={0}  gui={1}" -f $cliMods, $guiMods)
Write-Host ("   exe size: cli {0:N1} MB  gui {1:N1} MB" -f `
    ((Get-Item $exe).Length / 1MB), ((Get-Item $guiExe).Length / 1MB))
}

if ($Verify) {
    Write-Host "== verify on a clean PATH ==" -ForegroundColor Cyan
    $tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("runlite-verify-" + [Guid]::NewGuid().ToString("N"))
    Expand-Archive -Path $zip -DestinationPath $tmp
    $bin = Join-Path $tmp (Join-Path $name "runlite.exe")
    $envHome = Join-Path $tmp "home"
    $proj = Join-Path $tmp "proj"
    New-Item -ItemType Directory -Force -Path $proj | Out-Null
    $oldPath = $env:PATH
    try {
        # Only system directories: go, mvn, java, node, python all become
        # unresolvable - this is what "copied onto a fresh machine" means.
        $env:PATH = "$env:SystemRoot\System32;$env:SystemRoot"
        $env:RUNLITE_HOME = $envHome
        # The user's everyday daemon sits on the preferred port and a second
        # home must not fight it for the fact source. Give this temp home its
        # own port; if it happens to be taken, Listen() falls back and records
        # the real port in daemon.json, so the flow still works.
        $listener = New-Object System.Net.Sockets.TcpListener([System.Net.IPAddress]::Loopback, 0)
        $listener.Start(); $env:RUNLITE_PORT = "$($listener.LocalEndpoint.Port)"; $listener.Stop()
        Write-Host ("-- temp home {0}, port {1} --" -f $envHome, $env:RUNLITE_PORT)
        Write-Host "-- doctor (no java/mvn on PATH) --"
        $doc = Invoke-Exe $bin @("doctor")
        if ($doc.Exit -ne 0) { throw "doctor failed on a clean PATH" }
        Write-Host $doc.Out
        # M7 acceptance is not "doctor ran", it is "doctor told the user what to
        # do next". The needle below is the Chinese label for "what to do", kept
        # as a regex escape so this file stays ASCII-only (see header). If the
        # advice is ever dropped this gate goes red instead of shipping a box
        # where nothing is installed.
        if ($doc.Out -notmatch "\u600e\u4e48\u529e") { throw "doctor gave no actionable advice for the missing tools" }
        if ($doc.Out -notmatch "runlite add") { throw "doctor advice does not mention the --cmd escape hatch" }
        Write-Host "-- add / start / status / logs / stop / remove --"
        & $bin add --cmd "ping -n 20 127.0.0.1" $proj
        if ($LASTEXITCODE -ne 0) { throw "add failed" }
        & $bin start $proj
        if ($LASTEXITCODE -ne 0) { throw "start failed" }
        Start-Sleep -Seconds 2
        # rc 0 is not proof the run is tracked: ask the daemon and require an
        # active state carrying a real pid.
        $st = Invoke-Exe $bin @("status", $proj, "-json")
        if ($st.Exit -ne 0) { throw "status failed" }
        $view = $st.Out | ConvertFrom-Json
        if (-not $view.run -or $view.run.pid -le 0) { throw ("status has no live pid: {0}" -f $st.Out.Trim()) }
        if ($view.run.state -notmatch "^(starting|running)$") { throw ("expected an active state, got '{0}'" -f $view.run.state) }
        # The log pipe is the product: ping writes lines, so they must be readable.
        # Needle is "TTL=" - it only appears in ping's own replies, never in the
        # header runlite prints, and it survives any console codepage because it
        # is ASCII. ("Pinging" is English-Windows wording; a Chinese ping writes
        # TTL= too, so this gate does not depend on the machine's locale.)
        $lg = Invoke-Exe $bin @("logs", $proj, "-n", "50")
        if ($lg.Exit -ne 0 -or $lg.Out -notmatch "TTL=\d+") { throw ("log pipe never carried the child's output: {0}" -f $lg.Out.Trim()) }
        Write-Host ("   tracked pid {0}, state {1}, log starts with '{2}'" -f `
            $view.run.pid, $view.run.state, (($lg.Out -split "`r?`n" | Where-Object { $_ })[0]))
        $sp = Invoke-Exe $bin @("stop", $proj)
        if ($sp.Exit -ne 0) { throw "stop failed" }
        if ($sp.Out -notmatch "stopped") { throw ("stop did not report the end state: {0}" -f $sp.Out.Trim()) }
        Write-Host ("   stop said: {0}" -f $sp.Out.Trim())
        # A runlite-initiated stop must not carry an exit code: that number is the
        # byproduct of our own kill (a killed ping comes back as 1) and reads like a
        # crash. The needle words are Chinese, written as escapes - this file stays
        # ASCII-only.
        if ($sp.Out -match "\u9000\u51fa\u7801") {
            throw ("stop reported an exit code for a project runlite itself ended: {0}" -f $sp.Out.Trim())
        }
        # Second stop must be a no-op with rc 0 - scripts call stop unconditionally.
        $sp2 = Invoke-Exe $bin @("stop", $proj)
        if ($sp2.Exit -ne 0) {
            throw ("the second stop returned rc {0}, expected a no-op: {1}" -f $sp2.Exit, $sp2.Out.Trim())
        }
        & $bin remove $proj
        if ($LASTEXITCODE -ne 0) { throw "remove failed" }
    }
    finally {
        $env:PATH = $oldPath
        $env:RUNLITE_HOME = $null
        $env:RUNLITE_PORT = $null
        # The daemon is a detached child; kill it by the PID it wrote down
        # instead of leaving it to outlive the temp dir.
        $dn = Join-Path $envHome "daemon.json"
        if (Test-Path $dn) {
            $dpid = (Get-Content -Raw $dn | ConvertFrom-Json).pid
            if ($dpid) { Stop-Process -Id $dpid -Force -ErrorAction SilentlyContinue }
        }
        Start-Sleep -Seconds 1
        Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
    }
    Write-Host "== verify done, temp dir cleaned ==" -ForegroundColor Green
}
