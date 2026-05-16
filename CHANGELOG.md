# MERA Changelog

---

## Clean Install Test Sequence

Run these commands in order after a fresh `.\setup.ps1 -Minimal` to verify the installation:

```powershell
# 1. Verify binary is on PATH and version is reported correctly
mera -Version

# 2. System health check — expect no [FAIL] lines
mera -Doctor

# 3. Weighted health score — expect >= 60% on a Minimal install
mera -Health

# 4. Full installation smoke test — expect PASS on each item
mera -SmokeTest

# 5. Initialize MERA in a test project directory
mkdir C:\Temp\meratest; cd C:\Temp\meratest
mera -Init

# 6. Confirm models.json shows qwen2.5-coder:7b and FAST profile
Get-Content .\.mera\models.json

# 7. Confirm config.json shows schemaVersion:1 and no BOM
[System.IO.File]::ReadAllText('.\.mera\config.json') | ConvertFrom-Json | Select-Object schemaVersion, defaultModel

# 8. Dry-run a task (no code changes)
mera -DryRun dotnet "Add a simple health endpoint"

# 9. Confirm a session was created
mera -Sessions

# 10. Diag — confirm RAM is non-zero, PATH shows mera.exe
mera -Diag
```

Expected results on a Minimal install:
- `mera -Version` → `MERA Go v10.0.0`
- `mera -Doctor` → `[OK]` on all tool checks; `[WARN]` acceptable for optional tools (Aider, Python)
- `mera -Health` → `Health score: 60–100%`; `[WARN] Profile models` only if qwen2.5-coder:7b is not yet installed
- models.json `profile` field → `FAST`; all roles → `qwen2.5-coder:7b`
- `mera -Diag` RAM → non-zero float (e.g. `16.0 GB`)
- `mera -Diag` PATH → `mera.exe resolved via PATH`

---

## v10.0.0 — 2026-05-14

### Added — Release Packaging
- `build.ps1`: one-command release pipeline — compiles, checksums, generates manifest, zips
- `manifest.json`: machine-readable release manifest (version, files, checksums, schema requirements)
- `SHA256SUMS.txt`: per-file SHA-256 checksums for installer verification
- `VERSION.txt`: plain-text build metadata
- `MERA.SampleApp`: minimal ASP.NET Core project for smoke testing

### Added — Versioning
- `version.go`: `BuildVersion`, `BuildDate`, `InstallerVersion` vars (overridable via `-ldflags`)
- `mera -Version`: prints version, build date, schema versions, release manifest info

### Added — Session Tracking
- Every workflow run (orchestrate, dryRun) creates a session under `.mera/sessions/`
- Session ID format: `SESSION-YYYYMMDD-HHMMSS-XXXX`
- Session files: `summary.json`, `timeline.log`, `diff.patch` / `partial-diff.patch`, `report.md`
- `PhaseEvent` records phase name, status, duration, model used

### Added — Execution Observability
- Phase status printed at phase start: `[MERA] Phase: Planner  Status: Running  Elapsed: 00:03`
- Heartbeat goroutine prints `[MERA] Still working... (Phase) elapsed Xs` every `HeartbeatSeconds`
- `WorkflowReport` now includes `sessionId` and `version`

### Added — Abort Safety
- `setupAbortHandler()`: SIGINT (Ctrl+C) handler captures partial diff, writes session, exits 2
- Partial diff saved to `.mera/sessions/<id>/partial-diff.patch` on abort
- Prints rollback suggestion on abort

### Added — Session Replay
- `mera -Replay <session-id>`: full session timeline, artifacts, and partial diff
- `mera -Sessions`: list all stored session IDs
- Prefix matching: `mera -Replay SESSION-20260514` finds partial matches

### Added — Smoke Test
- `mera -SmokeTest`: exercises init, config validation, tool checks, Ollama API, code model, health, and sample DryRun
- Respects `MERA_AUTH_KEY` if set; runs without auth if not configured

### Added — Checksum Verification (setup.ps1)
- Installer reads `manifest.json` if present and verifies `mera.exe` checksum before install/update
- Version display in installer banner; upgrade notes when updating from older version

### Changed — Config Schema
- `Config` and `ModelConfig` now include `schemaVersion` field (transparent auto-migration in `loadConfig`/`loadModelConfig`)

### Fixed — Phase 10.2 Release Blockers
- **RAM detection**: replaced unreliable `wmic` with `Get-CimInstance Win32_ComputerSystem`; wmic retained as fallback — fixes "0.0 GB" RAM display on Windows 11
- **PATH diagnostic**: three-strategy detection (LookPath("mera.exe"), LookPath("mera"), exe-dir vs PATH comparison) — fixes false "not in PATH" when run via full path
- **Profile model compliance**: `checkProfileModelCompliance()` checks all role-assigned models against installed Ollama models; reported in `mera -Doctor` and as a weighted health component (weight 2)
- **Doctor model output**: deduplicates missing models; shows per-profile summary with `ollama pull` commands
- **Disk-aware model install**: `Test-DiskForProfile` only counts download size for models not yet on disk; skips check if all models already installed
- **Installer profile definitions**: Minimal profile correctly maps all roles to `qwen2.5-coder:7b`; optional Deep models (`deepseek-r1:14b`) tracked separately; `$MODEL_SIZE_GB` hashtable drives disk check
- **Install validation**: `Test-MeraGlobal` verifies mera.exe on PATH post-install; `Invoke-Validation` checks binary, PATH, models, and health score

### Fixed — Phase 10.4 Repo Scanning and File Targeting

- **Exclusion rules** (`util.go`, `index.go`): `skipDir` now excludes `.claude`, `TestResults`, `artifacts`, `packages`, `logs`, `tmp`, `temp` — `.claude\worktrees\...` files can never appear in File Scout results; new `skipFile()` strips `.min.js`, `.map`, `.dll`, `.exe`, `.pdb`, `.cache`, `.lock`, `.user`, `.suo`
- **Module-aware targeting** (`agents.go`): `prioritize()` now consults the solution module map and 12 name-variant patterns (`identity.api`, `hconnect.identity`, etc.); primary bucket (module match) fills first, secondary fills remaining slots
- **Solution-aware indexing** (`solution.go` — new): parses `.sln` and `.slnx` files to build a module→project-dir map; every `.` segment of the project name is indexed (`EcoSecretz.HConnect.Identity.API` → `identity`, `hconnect`, etc.)
- **Task keyword intelligence** (`confidence.go`): auth-task detection boosts `AuthController`, `LoginRequest`, `JwtService`, etc. filenames by +20; auth content signals (e.g. `[HttpPost`, `CheckPassword`) add up to +10; file-type bonus for controller/service/repository/dto/request filenames (+5 to +15)
- **Domain mismatch penalty** (`confidence.go`): frontend files scored –15 in backend tasks; test files scored –10 when task does not mention tests; task domain inferred from explicit frontend cues
- **File classification** (`util.go`): `classifyFile()` labels each file as `backend`, `frontend`, or `test`; shown in Evidence Report and Explain Selection
- **File Scout performance** (`agents.go`, `modelrouter.go`): Ollama candidate cap reduced 30→15; file snippet increased to 20 lines; FAST profile `FileScoutTimeout` = 45 s (separate from 60 s `AgentTimeout`); fallback message names the `ExplainSelection` command
- **Confidence scoring** (`confidence.go`): content scan depth 40→60 lines; content keyword cap 15→20 pts; auth content signals; all scoring paths unified through `clamp()`
- **ExplainSelection quality** (`report.go`): shows total indexed files, excluded count, backend/frontend/test breakdown; file type label next to each evidence entry
- **Actionable NO-GO** (`report.go`): `dryRun()` prints the blocking gate name and reason; for File Confidence/Discovery gates generates a `suggestRefinedTask()` output with exact `mera -DryRun` and `mera -ExplainSelection` commands
- **Profile honesty** (`modelrouter.go`, `report.go`): `modelModeLabel()` returns "Single-model fallback (qwen2.5-coder:7b)" when all roles share one model; shown in DryRun header and `-ProfileMode` output
- **Optional model recommendations** (`report.go`): `runDoctor()` prints upgrade suggestions (`phi4`, `llama3.1:8b`) when in single-model mode — informational only, never a failure

### Fixed — Phase 10.3 Release Polish
- **BOM root cause** (critical): `Set-Content -Encoding UTF8` in PS 5.1 writes a UTF-8 BOM that Go `json.Unmarshal` rejects; all JSON writes in setup.ps1 now use `[System.IO.File]::WriteAllText` with `[System.Text.UTF8Encoding]::new($false)` — fixes profile showing NORMAL/phi4/deepseek instead of the installed profile
- **models.json BOM fix**: `Write-ModelsJson`, `Write-ConfigJson`, and the upgrade migration path all write BOM-free JSON with `schemaVersion` field
- **Safe runtime defaults**: `safeDefaultModelConfig()` returns all roles as `qwen2.5-coder:7b` / FAST profile; used as fallback when models.json is absent or corrupt — prevents phi4/deepseek from appearing in error paths
- **Gap-fill strategy**: `loadModelConfig()` fills missing roles from the first non-empty model already in the config (not from `defaultModelConfig()`) — preserves the user's profile intent on partial configs
- **build.ps1 version parser**: regex `'^MERA\b.*?v(\d+\.\d+\.\d+)'` handles "MERA Go v10.0.0" format; semver fallback added — fixes version mismatch error during release build
- **Unicode/encoding**: all box-drawing characters (`==`, `[!]`, `->`, `OK`, `FAIL`) replaced with ASCII in all Go source files — fixes mojibake on Windows PS 5.1 console (CP1252)
- **Disk check pre-installed awareness**: `Test-DiskForProfile` queries Ollama API and skips download-size accounting for already-installed models
- **Config.maxSessions**: `Write-ConfigJson` writes `maxSessions: 50`; Go `Config` struct includes `MaxSessions int` (default 50) used by session pruner

---

## v9.0.0 — 2026-05-14

### Added — Diagnostics
- `mera -Diag`: full system snapshot (RAM, disk, tools, PATH, Ollama, configs, git)
- `mera -Health`: weighted health score 0–100% with 7 components
- `mera -Logs`: recent failures, fallbacks, gate blocks from `.mera/mera.log`
- `.mera/mera.log`: operational event log with 512 KB rotation
- Config schema versioning with transparent migration
- `setup.ps1 -Repair`: 9-check self-healing (PATH, binary, configs, Python, Aider, Ollama, models)
- Install checkpoints (`install.state.json`) with resume-from-interrupt

---

## v8.0.0

### Added — Installer
- `setup.ps1`: zero-friction Windows installer with hardware detection, profile selection, and validation

---

## v7.0.0

### Added — Model Routing
- Per-role model assignment (7 roles), performance profiles (FAST/NORMAL/DEEP/STRICT)
- `AIProvider` interface for local Ollama and future cloud backends

---

## v1.0.0 — v6.0.0

- Core workflow: Planner, Architect, File Scout, Security, Code (Aider), Diff Review
- Confidence engine, gates, DryRun, patch safety, learning memory, sprint recommendations
