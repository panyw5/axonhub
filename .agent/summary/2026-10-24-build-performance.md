# Build Performance Investigation: 2026-10-24

## Scope and Environment

- Host: macOS arm64, Apple M3 Max, 16 logical CPUs, 64 GiB memory.
- Frontend: Node v25.9.0, Vite 7.3.5, existing installed dependencies.
- Backend: Go 1.26.0, target windows/amd64, CGO_ENABLED=0, nomsgpack build tag.
- This is a macOS-hosted Windows cross-compilation measurement, not a native Windows measurement. Windows filesystem, antivirus, hardware, and download overhead are not represented.
- Source code and build configuration were not modified. No service was restarted, no active executable was replaced, and no lint or tests were run.
- A normal frontend build was copied into internal/server/static/dist. The comparison build used a temporary output directory instead.
- The attempted offline dependency installation stopped at pnpm's no-TTY modules-directory replacement guard. The existing modules directory was preserved. Dependency installation/download time is excluded from results.

## Measured Wall Times

| Stage | Seconds | Notes |
| --- | ---: | --- |
| Frontend baseline process | 24.72 | CPU profiling and lightweight phase/module diagnostics enabled |
| Remove previous embedded asset directory | 0.05 | Generated assets only |
| Copy frontend output into backend | 0.13 | Same copy operation as documented build |
| Windows backend, empty compilation cache | 24.44 | Existing downloaded module sources; new isolated GOCACHE |
| Windows backend, unchanged warm rebuild | 1.16 | Same binary path and cache; zero compiler/linker commands |
| Frontend direct-icon comparison process | 20.58 | Temporary import rewrite; repository sources unchanged |

The baseline build plus copy plus cold-cache backend totaled approximately 49.34 seconds, excluding downloads and diagnostic analysis between stages.

## Frontend Findings

- Baseline processed 24,764 modules, approximately 72.35 MiB of module source.
- Module processing took 19.10 seconds after buildStart. Chunk rendering through generateBundle took another 3.65 seconds.
- Main-isolate CPU samples attributed approximately 10.85 seconds to Rollup source files, 4.43 seconds to node:fs, and 3.48 seconds to garbage collection. These are sampled self times, not independent stage durations, and do not include all child-process/native work.

| Package | Baseline Parsed Modules |
| --- | ---: |
| lucide-react, across resolved versions | 6,608 |
| @tabler/icons-react | 6,077 |
| @lobehub/icons | 2,075 |
| es-toolkit | 1,081 |
| date-fns | 826 |
| antd | 627 |
| @lobehub/ui | 564 |
| @shikijs/langs | 513 |
| @shikijs/themes | 130 |

### Direct-Icon Comparison

- A temporary Vite plugin rewrote application-level named imports from lucide-react and @tabler/icons-react to their corresponding individual icon module files.
- It did not rewrite LobeHub imports or narrow supported model icons.
- Total processed modules decreased from 24,764 to 17,282, a reduction of 7,482 modules.
- Tabler modules decreased from 6,077 to 103; Lucide modules decreased from 6,608 to 5,100.
- Process time decreased from 24.72 to 20.58 seconds, approximately 16.7% in this single comparison.
- This is a single ordered comparison with profiling enabled, not a repeated benchmark. Filesystem cache effects and run-to-run variation are not controlled.
- A production implementation would need a maintainable import strategy and regression verification; the diagnostic rewrite is not installed in the project.

### LobeHub Namespace Import

- frontend/src/features/models/components/models-columns.tsx imports the entire @lobehub/icons namespace at line 5.
- At line 130 it selects Icons[iconName] using a runtime database value.
- Dynamic namespace access prevents ordinary named-export pruning: the root export includes features/hooks/metadata as well as brand icons.
- This is a stronger issue than simply having a handful of named brand-icon imports. The earlier static imported-name count did not account for this namespace use.
- A follow-up should preserve all valid stored icon names while separating the brand-icon registry from unrelated root exports, potentially using lazy loading.
- @lobehub/ui and @lobehub/fluent-emoji are connected peer dependencies. Removing their direct package.json entries alone will not remove the dependency chain.

### Highlighter Resources

- Application code uses Shiki's default full-bundle entry in code-block.tsx and masked-code-block.tsx; Streamdown also has its own highlighting dependency.
- Installed dependencies contain multiple Shiki generations. The build parsed 513 language modules and 130 theme modules across them.
- A focused language/theme registry would reduce this work, but should retain a plain-text fallback for unsupported user content and account for Streamdown too.

## Backend Findings

- Cold-cache graph contained 1,352 compilation actions plus one link action.
- Unchanged warm rebuild executed no compiler/linker commands and completed in 1.16 seconds.
- Existing global Go caches were not cleared or replaced.

| Package or Action | Observed Action Wall Seconds |
| --- | ---: |
| github.com/redis/go-redis/v9 | 5.98 |
| github.com/looplj/axonhub/internal/ent | 5.72 |
| modernc.org/sqlite/lib | 5.48 |
| github.com/looplj/axonhub/internal/server/gql | 4.30 |
| github.com/jackc/pgx/v5/pgtype | 4.11 |
| github.com/aws/aws-sdk-go-v2/service/s3 | 3.38 |
| Final link | 1.37 |

Compilation actions run concurrently, so their individual durations must not be summed as elapsed build time. These dependencies implement real functionality and are not proven redundant by their cost alone.

## Priorities

1. Replace icon barrel imports with a maintainable direct-import strategy; the diagnostic comparison demonstrated a reduction in work.
2. Refactor the dynamic LobeHub root namespace lookup without breaking persisted model icon names.
3. Narrow Shiki languages/themes across both direct consumers and Markdown rendering.
4. Preserve Go compilation caches and avoid unnecessary dependency reinstalls; the measured cold/warm difference was substantial.
5. Capture the same stages on the actual Windows host before attributing extra Windows latency to antivirus or filesystem behavior.

## Diagnostic Artifacts

- Temporary artifact directory: /private/tmp/axonhub-build-profile.txV4r1
- frontend.log, frontend-stats.json, frontend.cpuprofile
- frontend-direct.log, frontend-direct-stats.json, frontend-direct.cpuprofile
- frontend-profile.mjs: temporary baseline/comparison diagnostics, not project configuration
- windows-cold.log, windows-cold.json, windows-warm.log, windows-warm.json
- axonhub-windows-amd64.exe: verified as a PE32+ x86-64 Windows console executable; not run on this host
- go-cache/: isolated compilation cache used by both Windows builds
