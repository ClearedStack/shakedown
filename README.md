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

Try `testdata/tools/failing-tool` to see every condition fail with deterministic rule codes. Add `-json` for machine-readable output. Exit status: `0` pass, `1` fail, `2` bad usage or unreadable folder, `3` `needs_subtool`, `4` timeout.

### Run as a Muster worker

```text
shakedown run [-once] [-poll 2s] [-heartbeat 20s] [-timeout 30s] [-case-timeout 2s]
```

Worker mode reads `CONTRACT_VERSION` (`v1`), `MUSTER_URL` (default `http://muster.clearedstack.svc.cluster.local:8080`), `SHAKEDOWN_TOKEN_FILE` (default `/var/run/secrets/kubernetes.io/serviceaccount/token`), and `SHAKEDOWN_SPOOL_DIR` (default `/var/spool/cs`).

On each cycle Shakedown:

1. Drains any pending SHA-256-verified reports from `/var/spool/cs` before requesting new work.
2. Calls `POST /v1/lease` with its rotated Kubernetes ServiceAccount bearer token.
3. Rejects any upstream result carrying raw Sextant capture fields (`E_PARSE`), decodes the static bundle from `results.moldloft` in memory, or parks the item (`building -> parked`) when `results.moldloft` flags regulatory ambiguity.
4. Extends the lease via `POST /v1/heartbeat` while running the 5-condition suite under bounded per-case and per-bundle deadlines.
5. Atomically stages and delivers:
   - `POST /v1/complete` (`state: "building"`, `next: "chartroom"`) with Shakedown's report summary and Chartroom's closed `attestation` block (`5` passed conditions) when every condition passes;
   - `POST /v1/complete` (`state: "parked"`) when regulatory ambiguity blocks verification;
   - `POST /v1/fail` (`E_PARSE` or `E_TIMEOUT`) on definite rule failures or timeouts;
   - `POST /v1/needs_subtool` on unfamiliar valid inputs.

## What works today

- **Zero external dependencies:** Pure Go standard library (`module github.com/ClearedStack/shakedown`), including a bounded HTML5 parser ([`internal/htmlparse`](internal/htmlparse)) and a deterministic JavaScript lexer, parser, and bytecode-free AST interpreter ([`internal/js`](internal/js)) with a fixed UTC clock (`2000-01-01T00:00:00Z`), disabled `Math.random`, call-stack cap, and deadline cancellation.
- **5-condition verification suite ([`internal/check`](internal/check)):** Covers `offline`, `csp`, `tracking`, `examples`, `citations`, and clean-room `BUNDLE-HYGIENE` (49 rules, each pinned by a golden fixture in [`testdata/rules`](testdata/rules)).
- **Shared contract `v1` client and replay ([`internal/muster`](internal/muster)):** Strict HTTP client and stand-in replaying all 52 saved examples from `ClearedStack/muster/contract/v1/examples/`.
- **Atomic `/var/spool/cs` persistence ([`internal/spool`](internal/spool)):** Write-temp, `fsync`, rename, and canonical `sha256` verification so transient `503` or network outages never lose a completed step.
- **Hardened Kubernetes manifests ([`deploy/`](deploy), [`.ko.yaml`](.ko.yaml)):** `cs-workers` namespace, digest-pinned image, `runAsNonRoot: true`, `readOnlyRootFilesystem: true`, `capabilities.drop: ["ALL"]`, `seccompProfile: RuntimeDefault`, `30Mi` memory limit (`GOMEMLIMIT=24MiB`), `arm64` affinity excluding `cm3588-plus`, and `default-deny` plus Muster/DNS-only egress `NetworkPolicy`.

### What each condition checks

| Condition | What is checked |
| --- | --- |
| `offline` | Every asset loaded by HTML or CSS resolves inside the bundle. Scripts may not reference network APIs (`fetch`, `XMLHttpRequest`, `WebSocket`, `EventSource`, `Worker`, `importScripts`) or remote URLs. Standard `<a href>` links may point outside. |
| `csp` | Every HTML page has exactly one `Content-Security-Policy` `<meta>` tag in `<head>` before any script or stylesheet, requiring `default-src 'self'`, `connect-src 'self'`, and `form-action 'none'` with zero remote sources or inline scripts/handlers. |
| `tracking` | Zero password inputs, cookies, `sendBeacon`, or `<a ping>`. Any `localStorage`/`sessionStorage`/`indexedDB` usage requires `data-shakedown="storage-disclosure"` and `data-shakedown="clear-data"` on the entry page. |
| `examples` | Executes the bundle's calculation scripts in [`internal/js`](internal/js) against every case in `examples.json` (requiring at least one `worked` and one `boundary` case) and verifies exact JSON equality. |
| `citations` | Verifies an MIT `LICENSE`, `data-shakedown="disclaimer"`, `data-shakedown="scope-exclusions"`, and complete `data-shakedown="citation"` entries (`data-calculation`, `data-source`, `data-paragraph`, `data-revised`) covering every tested calculation. |

## What does not work yet

- **Real headless browser execution:** Verification is static HTML/CSS/CSP/AST inspection plus deterministic in-process JavaScript execution of calculation scripts; it does not launch a headless browser inside the 30 MiB container budget.
- **Pausing `building` items via `/v1/needs_subtool` in Muster:** Muster's `contract/v1/CONTRACT.md` currently permits `needs_subtool` transitions only from `recorded`, `observed`, and `approved`, so Muster answers `POST /v1/needs_subtool` on a `building` item with `409 move_not_allowed` until `{building -> needs_subtool -> building}` is added to Muster and Logbook. Shakedown drops permanent `409` spool entries safely without advancing unverified bundles to Chartroom.

## What Shakedown needs from other components

1. **Muster and Logbook:** Add `building -> needs_subtool` (and replay `needs_subtool -> building`) to `contract/v1/CONTRACT.md`, Muster's lifecycle table, and Logbook's pause state checks if `E_NEEDS_SUBTOOL` pauses from Stage 07 should auto-replay on redeploy.
2. **Moldloft (and Keelworks):** Populate `results.moldloft` with `{"files": {"shakedown.json": "...", "index.html": "...", ...}}` (UTF-8 strings or `{"base64": "..."}` objects within Muster's 64 KiB `complete` body limit, or agree on a contract extension if bundles exceed 64 KiB) using `shakedown.json` and `data-shakedown` HTML markers (or align Moldloft's `moldloft.json` / `tests/worked-examples.json` layout with Shakedown).
3. **Chartroom:** Already aligned on `results.shakedown.attestation.conditions` (five distinct condition names with `passed: true` and zero unknown fields).
4. **Lazarette, Keelworks, and Shipkeeper:** Build `./cmd/shakedown` via `ko`, push to `lazarette.clearedstack.svc.cluster.local:5000/shakedown`, and substitute the resulting `@sha256:...` digest into `deploy/shakedown.yaml` at deploy time (Shakedown is #10 in the platform deploy order).

## In this repository

| File | What it is |
| --- | --- |
| [`SPEC.md`](SPEC.md) | Component specification, boundaries, error codes, and resource budget. |
| [`PROMPT.md`](PROMPT.md) | Clean-room build prompt and rules. |
| [`component.json`](component.json) | ClearedStack naming registry entry for `shakedown`. |
| [`.ko.yaml`](.ko.yaml) | Rootless `ko` image build configuration pinned by upstream `sha256` digest. |
| [`deploy/`](deploy) | Hardened `cs-workers` Kubernetes manifests and `NetworkPolicy`. |
| [`cmd/shakedown`](cmd/shakedown) | CLI entry point (`check`, `run`, `version`). |
| [`internal/check`](internal/check) | Five-condition static and worked-example verifier. |
| [`internal/htmlparse`](internal/htmlparse) | Bounded standard-library HTML5 tokenizer and tree builder. |
| [`internal/js`](internal/js) | Deterministic standard-library JavaScript parser and interpreter. |
| [`internal/muster`](internal/muster) | Contract `v1` HTTP client and full test stand-in. |
| [`internal/spool`](internal/spool) | Atomic SHA-256-verified `/var/spool/cs` staging. |
| [`internal/worker`](internal/worker) | Lease, heartbeat, verification, and reporting loop. |
| [`testdata/`](testdata) | Sample tools, 49 rule fixtures, and 52 saved Muster contract `v1` examples. |

## License

[MIT](LICENSE). ClearedStack is an independent personal project. It is not affiliated with, endorsed by, or sponsored by the U.S. Department of Defense or any other government agency.
