<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="brand/logo-dark.svg">
    <img src="brand/logo-light.svg" alt="Shakedown" height="56">
  </picture>
</p>

<p align="center">
  <strong>Checks a built tool against the five listing conditions before it can be listed.</strong><br>
  Part of <a href="https://clearedstack.com">ClearedStack</a>
</p>

---

> **Status: ready to deploy (`v0.1.0-draft`, contract `v1`).** Shakedown builds with the Go standard library alone, verifies static bundles locally (`shakedown check`) or as a cluster worker (`shakedown run`), replays all 52 saved Muster contract `v1` examples, stages outbound reports atomically in `/var/spool/cs`, and ships hardened Kubernetes manifests in [`deploy/`](deploy).

Shakedown is stage 07 of the ClearedStack pipeline. ClearedStack is an open pipeline that finds ideas for government-contracting tools, screens and scores them, and builds each one from the public rule. Shakedown can also be run standalone on any local static bundle folder without a cluster.

## Commands

### Check a local folder

```text
go run ./cmd/shakedown check testdata/moldloft/calculator-rate-buildup
go run ./cmd/shakedown check testdata/tools/passing-tool
```

```text
shakedown v0.1.0-draft (contract v1)

offline    pass
csp        pass
tracking   pass
examples   pass
citations  pass

verdict    pass
```

Shakedown verifies both Moldloft repositories (`moldloft.json` across `calculator`, `checklist`, `validator`, and `decision-tree`) and standalone `shakedown.json` bundles. Try `testdata/tools/failing-tool` to see every condition fail with deterministic rule codes. Add `-json` for machine-readable output. Exit status: `0` pass, `1` fail, `2` bad usage or unreadable folder, `3` `needs_subtool`, `4` timeout.

### Replay seeded pipeline items locally

```text
go run ./cmd/shakedown replay [-timeout 30s] [-case-timeout 2s] [-allow-pause-building] seeds.jsonl
```

Runs `worker.Worker` deterministically in-process against the full Muster contract `v1` stand-in over a JSON array or JSONL stream of `SeedItem` records and writes canonical JSON outcomes (`state`, `held`, `next`, `shakedown`) to `stdout` without opening network sockets.

### Run as a Muster worker

```text
shakedown run [-once] [-listen :8080] [-poll 2s] [-heartbeat 20s] [-timeout 30s] [-case-timeout 2s]
```

Worker mode reads `CONTRACT_VERSION` (`v1`), `MUSTER_URL` (default `http://muster.clearedstack.svc.cluster.local:8080`), `SHAKEDOWN_TOKEN_FILE` (default `/var/run/secrets/kubernetes.io/serviceaccount/token`), `SHAKEDOWN_SPOOL_DIR` (default `/var/spool/cs`, falling back to a bounded single-slot in-memory spool if the container rootfs is read-only without a mounted volume), and `SHAKEDOWN_LISTEN_ADDR` (default `:8080` in-cluster, serving `GET /healthz`). When started with zero arguments inside Kubernetes (`KUBERNETES_SERVICE_HOST` set or `SHAKEDOWN_AUTORUN=1`), `shakedown` automatically enters worker mode with a default `24 MiB` Go heap limit (`debug.SetMemoryLimit`).

On each cycle Shakedown:

1. Drains any pending SHA-256-verified reports from `/var/spool/cs` before requesting new work.
2. Calls `POST /v1/lease` with its rotated Kubernetes ServiceAccount bearer token.
3. Rejects any upstream result carrying raw Sextant capture fields (`E_PARSE`), decodes the static bundle from `results.moldloft` in memory (accepting `publish` and `build` metadata alongside map-form or array-form `files`), or parks the item (`building -> parked`) when `results.moldloft` flags regulatory ambiguity.
4. Extends the lease via `POST /v1/heartbeat` while running the 5-condition suite under bounded per-case and per-bundle deadlines.
5. Atomically stages and delivers:
   - `POST /v1/complete` (`state: "building"`, `next: "chartroom"`) with Shakedown's report summary and Chartroom's closed `attestation` block (`5` passed conditions) when every condition passes;
   - `POST /v1/complete` (`state: "parked"`) when regulatory ambiguity blocks verification;
   - `POST /v1/fail` (`E_PARSE` or `E_TIMEOUT`) on definite rule failures or timeouts;
   - `POST /v1/needs_subtool` on unfamiliar valid inputs.

## What works today

- **Zero external dependencies:** Pure Go standard library (`module github.com/ClearedStack/shakedown`), including a linear-time bounded HTML5 parser ([`internal/htmlparse`](internal/htmlparse)) and a deterministic JavaScript lexer, parser, AST interpreter, and bounded in-memory DOM/Web Storage harness ([`internal/js`](internal/js)) supporting `BigInt` (`math/big`), `Map`, `Array.from`, `Function.prototype.call`/`apply`, `document`/`window`/`localStorage`/`setTimeout`, a fixed UTC clock (`2000-01-01T00:00:00Z`), disabled `Math.random`, per-VM call-stack and memory/allocation caps (`RangeError` well beneath the 24 MiB `GOMEMLIMIT`), deadline cancellation, and standard-library fuzz targets.
- **5-condition verification suite ([`internal/check`](internal/check)):** Covers `offline`, `csp`, `tracking`, `examples` (both headless engine verification and live DOM execution of `tests/index.html` and `index.html`), `citations`, and clean-room `BUNDLE-HYGIENE` (49 rules pinned by golden fixtures in [`testdata/rules`](testdata/rules), plus golden Moldloft bundles across all four stack templates in [`testdata/moldloft`](testdata/moldloft)).
- **Shared contract `v1` client and replay ([`internal/muster`](internal/muster)):** Strict HTTP client, full contract stand-in replaying all 52 saved examples from `ClearedStack/muster/contract/v1/examples/`, and offline `shakedown replay` CLI.
- **Atomic `/var/spool/cs` persistence ([`internal/spool`](internal/spool)):** Write-temp, `fsync`, rename, and canonical `sha256` verification so transient `503` or network outages never lose a completed step, with bounded single-slot in-memory fallback on unmounted read-only root filesystems.
- **GitOps and hardened Kubernetes manifests ([`app.yaml`](app.yaml), [`kustomization.yaml`](kustomization.yaml), [`deploy/`](deploy), [`.ko.yaml`](.ko.yaml)):** Compatible with both Keelworks' digest pinner and Shipkeeper's strict `app.yaml` renderer (`:8080/healthz` readiness/liveness, `cs-workers` namespace, digest-pinned image, `runAsNonRoot: true`, `readOnlyRootFilesystem: true`, `capabilities.drop: ["ALL"]`, `seccompProfile: RuntimeDefault`, `30Mi` memory limit with programmatic `24MiB` Go memory cap, `arm64` affinity excluding `cm3588-plus`, and `default-deny` plus Muster/DNS-only egress `NetworkPolicy`).

### What each condition checks

| Condition | What is checked |
| --- | --- |
| `offline` | Every asset loaded by HTML, CSS, or Markdown (`.md`) resolves inside the bundle, and `moldloft.json` file lists, byte counts, and SHA-256 digests match the bundle contents. Scripts may not reference network APIs (`fetch`, `XMLHttpRequest`, `WebSocket`, `EventSource`, `Worker`, `importScripts`) or remote URLs (except exact `data/tool.json` `citations[*].url` values mirrored inside `js/data.js`). Standard `<a href>` and Markdown `[text](https://...)` links may point outside. |
| `csp` | Every HTML page has exactly one `Content-Security-Policy` `<meta>` tag in `<head>` before any script or stylesheet, requiring `default-src 'self'`, `connect-src 'self'`, and `form-action 'none'` with zero remote sources or inline scripts/handlers. |
| `tracking` | Zero password inputs, cookies, `sendBeacon`, or `<a ping>`. Any `localStorage`/`sessionStorage`/`indexedDB` usage requires `data-shakedown="storage-disclosure"` and `data-shakedown="clear-data"` on the entry page. |
| `examples` | Executes every worked example in isolated [`internal/js`](internal/js) VMs: for `moldloft.json` bundles across `calculator`, `checklist`, `validator`, and `decision-tree`, verifies `js/data.js` == `data/tool.json` and `tests/worked-examples.js` == `tests/worked-examples.json`, runs `MoldloftCheck.run`, executes `tests/index.html` (`tests/browser.js`) in the bounded DOM, and drives `index.html` (`js/app.js`) form inputs, outputs, and reset handlers across every worked example; for `shakedown.json` bundles, requires at least one `worked` and one `boundary` case in `examples.json` and verifies exact JSON equality. |
| `citations` | Verifies an MIT `LICENSE`, scope exclusions, disclaimer/footer notice, and complete source citations covering every rule/calculation: semantic `<section aria-labelledby="h-scope">`, `<section aria-labelledby="h-sources">` (`<li id="cite-...">` + `<a href="#cite-...">`), `<footer>`, and `data/tool.json` bindings for `moldloft.json` bundles, or `data-shakedown` markers for `shakedown.json` bundles. |

## What does not work yet

- **Full browser layout/CSS engine:** Verification combines static HTML/CSS/CSP/Markdown/AST inspection with deterministic in-process JavaScript and bounded DOM execution (`index.html` and `tests/index.html`); it does not launch Chromium/WebKit or compute CSS pixel layout inside the 30 MiB container budget.
- **Pausing `building` items via `/v1/needs_subtool` in Muster:** Muster's `contract/v1/CONTRACT.md` currently permits `needs_subtool` transitions only from `recorded`, `observed`, and `approved`, so Muster answers `POST /v1/needs_subtool` on a `building` item with `409 move_not_allowed` until `{building -> needs_subtool -> building}` is added to Muster and Logbook. Shakedown drops permanent `409` spool entries safely without advancing unverified bundles to Chartroom.

## What Shakedown needs from other components

1. **Muster and Logbook:** Add `building -> needs_subtool` (and replay `needs_subtool -> building`) to `contract/v1/CONTRACT.md`, Muster's lifecycle table, and Logbook's pause state checks if `E_NEEDS_SUBTOOL` pauses from Stage 07 should auto-replay on redeploy.
2. **Moldloft (and Muster):** Shakedown natively verifies Moldloft's `moldloft.json` layout across all four stack templates (including `index.html` and `tests/index.html` DOM execution) and accepts `publish` and `build` metadata alongside `files` in `results.moldloft`. Moldloft currently writes `publish` and `build` metadata to `results.moldloft` while pushing the generated files to Plainforge (because Muster caps `POST /v1/complete` bodies at 64 KiB and Shakedown's `NetworkPolicy` forbids direct Plainforge egress). Moldloft and Muster need either `files` included in `results.moldloft` (with a matching Muster body limit when bundles approach 64 KiB) or an agreed contract mechanism to pass the static bundle bytes to Shakedown without opening Plainforge network access from `cs-workers`.
3. **Chartroom:** Already aligned on `results.shakedown.attestation.conditions` (five distinct condition names with `passed: true` and zero unknown fields).
4. **Lazarette, Keelworks, and Shipkeeper:** Build `./cmd/shakedown` via `ko`, push to `lazarette.clearedstack.svc.cluster.local:5000/shakedown`, and pin the resulting `sha256:...` digest into `app.yaml` / `deploy/shakedown.yaml` at deploy time (Shakedown is #10 in the platform deploy order).

## In this repository

| File | What it is |
| --- | --- |
| [`SPEC.md`](SPEC.md) | Component specification, boundaries, error codes, and resource budget. |
| [`PROMPT.md`](PROMPT.md) | Clean-room build prompt and rules. |
| [`component.json`](component.json) | ClearedStack naming registry entry for `shakedown`. |
| [`app.yaml`](app.yaml) | Declarative GitOps service descriptor for Keelworks and Shipkeeper. |
| [`kustomization.yaml`](kustomization.yaml) | Root Kustomize entrypoint referencing [`deploy/`](deploy). |
| [`.ko.yaml`](.ko.yaml) | Rootless `ko` image build configuration pinned by upstream `sha256` digest. |
| [`deploy/`](deploy) | Hardened `cs-workers` Kubernetes manifests and `NetworkPolicy`. |
| [`cmd/shakedown`](cmd/shakedown) | CLI entry point (`check`, `replay`, `run`, `version`). |
| [`internal/check`](internal/check) | Five-condition static, worked-example, and DOM UI verifier (`moldloft.json` and `shakedown.json`). |
| [`internal/htmlparse`](internal/htmlparse) | Bounded standard-library HTML5 tokenizer and tree builder. |
| [`internal/js`](internal/js) | Deterministic, memory-capped standard-library JavaScript interpreter and in-memory DOM. |
| [`internal/muster`](internal/muster) | Contract `v1` HTTP client and full test stand-in. |
| [`internal/spool`](internal/spool) | Atomic SHA-256-verified `/var/spool/cs` staging with read-only memory fallback. |
| [`internal/worker`](internal/worker) | Lease, heartbeat, verification, and reporting loop. |
| [`testdata/`](testdata) | Sample tools, 5 Moldloft golden bundles, 49 rule fixtures, and 52 saved Muster contract `v1` examples. |

## License

[MIT](LICENSE). ClearedStack is an independent personal project. It is not affiliated with, endorsed by, or sponsored by the U.S. Department of Defense or any other government agency.
