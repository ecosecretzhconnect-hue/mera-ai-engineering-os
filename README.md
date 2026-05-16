# MERA Go v10 Bundle

Go rewrite foundation for MERA as a universal multi-project AI Engineering OS.

## Includes
- heartbeat/progress UX
- performance modes: fast, deep, analyze, code
- evidence-oriented execution contract
- stack fingerprint config
- patch/secret/boundary/blast-radius validation gates
- atomic snapshot and rollback
- workflow-style multi-agent orchestration, not parallel model execution
- Aider integration with smaller map-token defaults
- project detection for .NET, Node, Python, frontend builds
- no auto-commit, no auto-push, no auto-deploy

## Build and install

```powershell
cd D:\AI\MERA\mera_go_v10_bundle
powershell -ExecutionPolicy Bypass -File .\build.ps1
powershell -ExecutionPolicy Bypass -File .\install-go.ps1
```

Restart PowerShell.

## Commands

```powershell
mera -Doctor
mera -Init
mera -Scan
mera -Fingerprint
mera -Plan Identity "Fix login issue"
mera -Analyze Identity "Explain auth flow"
mera -Code Identity "Add FromBody to auth DTOs"
mera -Fast Hostel "Create owner hostel form"
mera -Deep Finance "Analyze Razorpay orchestration"
mera -Orchestrate Identity "Fix login issue"
mera -Validate
mera -Snapshot
mera -Rollback
mera -BoundaryCheck Identity
mera -BlastRadius
mera -Mission
```

## Hardware requirements

| RAM   | Recommended profile | Notes |
|-------|---------------------|-------|
| 8 GB  | Minimal             | One model handles all roles; slowest |
| 12 GB | Fast                | phi4 + qwen2.5-coder:7b + llama3.1:8b |
| 16 GB | Balanced            | qwen2.5-coder:14b; best everyday choice |
| 32 GB+| Deep                | Adds optional deepseek-coder-v2 for diff review |

CPU inference is supported (no GPU required). GPU dramatically improves throughput.

## Model profiles

| Profile  | Models pulled | Disk (~) | MERA profile | Use when |
|----------|--------------|----------|--------------|----------|
| Minimal  | qwen2.5-coder:7b | 5 GB | FAST | Low disk / testing |
| Fast     | + phi4, llama3.1:8b | 19 GB | FAST | Dev laptop, 12 GB RAM |
| Balanced | phi4, llama3.1:8b, qwen2.5-coder:14b | 23 GB | NORMAL | **Recommended for most users** |
| Deep     | Balanced + deepseek-coder-v2 (optional) | 32 GB | DEEP | High-RAM workstations |

**Fast vs Minimal guidance:**
- Use **Minimal** if you have under 10 GB free disk or just want to evaluate MERA quickly. All seven agent roles share one model — quality is reduced but it works end-to-end.
- Use **Fast** for regular development work on a laptop. phi4 handles planning/analysis, qwen2.5-coder:7b handles code edits, llama3.1:8b handles diff review and security.
- Use **Balanced** for production use. The 14b coder model produces significantly better diffs.

After install, change profiles any time:
```powershell
mera -SetProfile FAST|NORMAL|DEEP|STRICT
mera -SetModel code qwen2.5-coder:14b
```

## Failure modes and recovery

### MERA is stuck / no output for a long time
A heartbeat prints every 15 seconds when a subprocess is silent. If it stops:
1. Press `Ctrl+C` — MERA kills the subprocess, saves a partial session, and exits with code 2.
2. Check the partial diff: `mera -Replay` (shows the most recent session).
3. Undo any partial changes: `mera -Rollback`.
4. Retry with a lighter profile: `mera -Fast <module> "task"`.

### How to rollback changes
```powershell
mera -Rollback         # reverts to the last snapshot (git stash pop equivalent)
git diff               # inspect what changed before deciding
```
Snapshots are stored in `.mera/snapshots/`. If the snapshot is gone, use `git checkout -- .`.

### How to replay a session
```powershell
mera -Sessions                     # list all stored sessions
mera -Replay SESSION-20260514-...  # full timeline, artifacts, partial diff
```
Prefix matching works: `mera -Replay SESSION-20260514` finds all sessions from that date (fails if ambiguous).

### How to repair an installation
```powershell
powershell -ExecutionPolicy Bypass -File .\setup.ps1 -Repair
```
Checks and fixes: binary, PATH, config files, Python, Aider, Ollama, missing models (9 checks).

### How to clean a stale session lock
If MERA crashed and left `.mera/session.lock`:
```powershell
Remove-Item .mera\session.lock
```
The lock auto-expires after 2 hours. MERA will warn and clear it on next run.

### How to run the smoke test
```powershell
mera -SmokeTest
```
Runs 7 checks: init, config, models, tool PATH, Ollama API, code model, health score, and a sample DryRun. Does not require a git repo or `MERA_AUTH_KEY`.

### How to verify checksum after download
```powershell
# Verify manually
Get-FileHash .\outputs\mera.exe -Algorithm SHA256
# Compare against SHA256SUMS.txt or manifest.json checksums."mera.exe"
```
The installer (`setup.ps1`) verifies the checksum automatically when `manifest.json` is present.

### Config file is corrupt / MERA fails to start
MERA automatically tries `.mera/config.json.bak` if `config.json` is corrupt. If both are corrupt:
```powershell
Remove-Item .mera\config.json
mera -Init    # regenerates defaults
```

## Recommended workflow

```powershell
cd D:\Projects\EcoSecretz.HConnect
git checkout -b feature/my-task
mera -Plan Identity "Fix login issue"
mera -Code Identity "Fix login issue"
mera -Validate
git status
git diff --cached
git commit
```
