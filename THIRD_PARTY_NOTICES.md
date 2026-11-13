# Third-party notices

Shakedown is built from source with Go modules. Nothing is copied into this repository. These libraries are compiled into the binary; each keeps its own license and notice in its module, at the version pinned in `go.sum`.

| Module | Used for | License |
| --- | --- | --- |
| `github.com/dop251/goja` | Running a tool's calculation script against its worked examples, and parsing scripts for the static scan | MIT |
| `golang.org/x/net` | Parsing HTML | BSD 3-Clause |
| `github.com/dlclark/regexp2/v2` | Needed by goja | MIT |
| `github.com/go-sourcemap/sourcemap` | Needed by goja | BSD 2-Clause |
| `github.com/google/pprof` | Needed by goja | Apache 2.0 |
| `golang.org/x/text` | Needed by goja | BSD 3-Clause |
