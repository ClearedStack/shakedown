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

> **Status: design.** The first milestone is built: a command that checks one folder of static files against the five listing conditions. The worker itself (leasing from Muster, the spool, signing, deployment) is not built. See [What works today](#what-works-today).

Shakedown is stage 07 of the ClearedStack pipeline. ClearedStack is an open pipeline that finds ideas for government-contracting tools, screens and scores them, and builds each one from the public rule. Shakedown is meant to be understood and used without the rest.

## What works today

One command. It reads a folder of static files, checks it against the five listing conditions, and prints pass or fail for each.

```
go run ./cmd/shakedown check testdata/tools/passing-tool
```

```
shakedown v0.1.0-draft (contract v1)

offline    pass
csp        pass
tracking   pass
examples   pass
citations  pass

verdict    pass
```

Try `testdata/tools/failing-tool` to see every condition fail, with the reason for each. Add `-json` for a report a program can read. The exit status is 0 for pass, 1 for fail, 2 for bad usage or a folder that cannot be read, 3 for `needs_subtool` and 4 for a timeout.

Run the tests with `go test ./...`. They need Go 1.26 or later and do not use the network once the two dependencies are downloaded.

### What each condition checks

The check reads files. It does not open a browser. Each condition is a set of rules, and every rule has a saved example in [`testdata/rules`](testdata/rules): the passing sample tool with one change laid over it, and the exact report that change must produce.

| Condition | What is checked today |
| --- | --- |
| `offline` | Everything a page or style sheet loads is a file inside the folder. No script names a network call (`fetch`, `XMLHttpRequest`, `WebSocket` and the like) or holds an outside address. Ordinary links (`<a href>`) may point outside. |
| `csp` | Every page has one `Content-Security-Policy` meta element, in the head, before anything loads, with `default-src 'self'`, `connect-src 'self'` and `form-action 'none'` exactly. No other directive may allow an outside source. |
| `tracking` | No password field, no cookies, no `sendBeacon`, no `ping` attribute. If a script uses browser storage, the entry page must say so and carry a clear-data button. |
| `examples` | The tool's own calculation script is run against each saved example, and the result must equal the expected value exactly. There must be at least one worked case and one boundary case. |
| `citations` | An MIT `LICENSE` file. A non-advice disclaimer and a statement of what is out of scope on the entry page. A citation, with regulation, paragraph and revision date, for every calculation the examples exercise. |

A case with no rule (an unknown file type, template, policy source, inline script, and so on) is reported as `needs_subtool` with `E_NEEDS_SUBTOOL`. It is never guessed.

### The folder layout it expects

`SPEC.md` does not say how a built tool lays out its files, so this milestone had to choose. The choice is provisional and is raised as a question on the pull request.

- `shakedown.json`: names the contract version, the template, the entry page, the scripts that hold the calculation and the function to call, and the examples file.
- An examples file: a list of cases, each with a name, a kind (`worked`, `boundary` or `synthetic`), the calculation it exercises, an input and the expected result.
- Pages mark the parts the check must find with a `data-shakedown` attribute: `citation`, `disclaimer`, `scope-exclusions`, `storage-disclosure` and `clear-data`.
- `LICENSE` at the top of the folder.

[`testdata/tools/passing-tool`](testdata/tools/passing-tool) is a complete example. Its regulation, rates and figures are made up.

### Limits to know about

- The script scan reads names and strings as they are written. Code that builds a name while it runs is not seen. The content security policy is what stops that in the browser.
- The check cannot tell whether a cited source is real, official or public, or whether a disclaimer says the right thing. It checks that they are present and complete.
- Calculation scripts run in an embedded JavaScript engine with a fixed clock, UTC, and no `Math.random`. Each example has a deadline. There is no hard limit on the memory a script may take.
- Measured on the two sample tools, the command peaks at about 13 MiB of memory. That is a measurement on small inputs, not a guarantee of the 30 MiB budget.

### Not yet done

Everything in `SPEC.md` beyond the command above:

- Leasing work from Muster (`lease`, `heartbeat`, `complete`, `fail`, `needs_subtool`), lane order, and the in-memory stand-in for Muster.
- Staging a signed result in `/var/spool/cs`.
- Moving an item to `parked` when a regulation is ambiguous.
- Loading a tool in a real browser with the network cut.
- Building the image with `ko`, and running under the Kubernetes limits (unprivileged, read-only filesystem, 30 MiB, network policy). None of this has been exercised.
- Rejecting fixtures that hold real or controlled data. There is no rule for recognising such data yet.

## In this repository

| File | What it is |
| --- | --- |
| [`SPEC.md`](SPEC.md) | The specification: what Shakedown does, its interfaces, how it fails and how it is kept safe. |
| [`PROMPT.md`](PROMPT.md) | The prompt to start a build session with. It says what to build and the rules to build it under. |
| [`component.json`](component.json) | This component’s entry in the ClearedStack naming registry. |
| [`brand/`](brand) | The Shakedown logo and mark, for light and dark backgrounds. |
| [`cmd/shakedown`](cmd/shakedown) | The command. |
| [`internal/check`](internal/check) | The five conditions and their rules. |
| [`testdata/`](testdata) | The two sample tools and one saved example per rule. All made up. |
| [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md) | The open-source libraries used, and their licenses. |

## At a glance

| | |
| --- | --- |
| **Stage** | 07 of 10 in the pipeline |
| **Formerly** | `cs-07-verify` |
| **Namespace** | `cs-workers` |
| **Container image** | `lazarette.clearedstack.svc.cluster.local:5000/shakedown` |
| **Memory budget** | 30 MiB |
| **Depends on** | [Muster](https://github.com/ClearedStack/muster) |
| **Depended on by** | Nothing else in the platform |

### Boundaries

- Leases built tools from Muster
- Returns a pass or fail on each of the five conditions

## Names

`shakedown` is this component’s identifier everywhere: the repository, the workload, the container image. The one place names are written down is the [naming registry](https://clearedstack.com/platform/registry.json), kept in [`clearedstack.com`](https://github.com/ClearedStack/clearedstack.com/blob/main/src/data/registry.json). `component.json` here is a copy of this component’s entry. If the two disagree, the registry is right.

## Ground rules

- **No AI when it runs.** The same input gives the same output on any machine.
- **Stop instead of guessing.** A case with no rule is paused and reported, never improvised.
- **Clean room.** Written from scratch. No code, text or design is copied from another project.
- **Public sources and made-up data only.** Nothing classified, controlled or taken from a real contract, in code, tests, issues or commit messages.

## License

[MIT](LICENSE). ClearedStack is an independent personal project. It is not affiliated with, endorsed by, or sponsored by the U.S. Department of Defense or any other government agency.
