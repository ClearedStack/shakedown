package shakedown_test

import (
	"bytes"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestNoEmDashesInRepo(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if strings.ContainsRune(target, '\u2014') {
				t.Errorf("%s symlink target contains an em dash", path)
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".png", ".jpg", ".jpeg", ".gif", ".ico", ".pdf":
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.ContainsRune(string(raw), '\u2014') {
			t.Errorf("%s contains an em dash (U+2014)", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStandardLibraryOnly(t *testing.T) {
	goMod, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	text := string(goMod)
	if !strings.Contains(text, "module github.com/ClearedStack/shakedown\n") {
		t.Fatalf("unexpected module path in go.mod:\n%s", text)
	}
	if strings.Contains(text, "require") {
		t.Fatalf("go.mod must have zero third-party dependencies:\n%s", text)
	}
	if _, err := os.Stat("go.sum"); !os.IsNotExist(err) {
		t.Fatal("go.sum must not exist when using standard library only")
	}
	if _, err := os.Stat("Dockerfile"); !os.IsNotExist(err) {
		t.Fatal("Dockerfile must not exist; image is built with ko")
	}

	fset := token.NewFileSet()
	err = filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			pkg := strings.Trim(imp.Path.Value, `"`)
			if strings.HasPrefix(pkg, "github.com/ClearedStack/shakedown/") {
				continue
			}
			if strings.Contains(pkg, ".") {
				t.Errorf("%s imports external package %q", path, pkg)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNoSecretsInRepo(t *testing.T) {
	secretRE := regexp.MustCompile(`(?i)(-----BEGIN [A-Z ]*PRIVATE KEY-----|ghp_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}|AKIA[0-9A-Z]{16})`)
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if m := secretRE.Find(raw); m != nil {
			t.Errorf("%s matched secret pattern %q", path, string(m))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDeployManifestsAndKoConfig(t *testing.T) {
	koRaw, err := os.ReadFile(".ko.yaml")
	if err != nil {
		t.Fatalf("missing .ko.yaml: %v", err)
	}
	digestRE := regexp.MustCompile(`@sha256:[0-9a-f]{64}`)
	if !digestRE.Match(koRaw) {
		t.Error(".ko.yaml base image must be pinned by sha256 digest")
	}

	deployRaw, err := os.ReadFile(filepath.Join("deploy", "shakedown.yaml"))
	if err != nil {
		t.Fatalf("missing deploy/shakedown.yaml: %v", err)
	}
	dep := string(deployRaw)
	if !digestRE.Match(deployRaw) {
		t.Error("deploy/shakedown.yaml image must be pinned by sha256 digest")
	}
	requiredInDeploy := []string{
		"namespace: cs-workers",
		"serviceAccountName: shakedown",
		"runAsNonRoot: true",
		"readOnlyRootFilesystem: true",
		"allowPrivilegeEscalation: false",
		`drop: ["ALL"]`,
		"type: RuntimeDefault",
		"key: kubernetes.io/arch",
		`values: ["arm64"]`,
		"key: kubernetes.io/hostname",
		`values: ["cm3588-plus"]`,
		"memory: 30Mi",
		"mountPath: /var/spool/cs",
		"CONTRACT_VERSION",
		"containerPort: 8080",
		"path: /healthz",
		"readinessProbe:",
		"livenessProbe:",
	}
	for _, needle := range requiredInDeploy {
		if !strings.Contains(dep, needle) {
			t.Errorf("deploy/shakedown.yaml missing required setting %q", needle)
		}
	}

	npRaw, err := os.ReadFile(filepath.Join("deploy", "networkpolicy.yaml"))
	if err != nil {
		t.Fatalf("missing deploy/networkpolicy.yaml: %v", err)
	}
	np := string(npRaw)
	requiredInNP := []string{
		"name: default-deny",
		"namespace: cs-workers",
		"podSelector: {}",
		"ingress: []",
		"app.kubernetes.io/name: muster",
		"port: 8080",
		"port: 53",
	}
	for _, needle := range requiredInNP {
		if !strings.Contains(np, needle) {
			t.Errorf("deploy/networkpolicy.yaml missing required setting %q", needle)
		}
	}
}

func TestAppYAMLAndKustomization(t *testing.T) {
	appRaw, err := os.ReadFile("app.yaml")
	if err != nil {
		t.Fatalf("missing app.yaml: %v", err)
	}
	if bytes.ContainsRune(appRaw, '\r') || bytes.ContainsRune(appRaw, '\x00') || bytes.ContainsRune(appRaw, '\t') {
		t.Fatal("app.yaml must use LF line endings only with no tabs or NUL bytes")
	}
	appText := string(appRaw)
	digestLineRE := regexp.MustCompile(`^digest:\s*sha256:[0-9a-f]{64}$`)
	fields := map[string]string{}
	digestLines := 0
	for _, line := range strings.Split(strings.TrimRight(appText, "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if trimmed == "---" || trimmed == "..." {
			t.Fatalf("app.yaml must be a single document without %q markers", trimmed)
		}
		if strings.HasPrefix(line, "digest:") {
			digestLines++
			if !digestLineRE.MatchString(line) {
				t.Errorf("invalid digest line %q in app.yaml", line)
			}
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			t.Fatalf("malformed line in app.yaml: %q", line)
		}
		fields[strings.TrimSpace(key)] = strings.TrimSpace(val)
	}
	if digestLines != 1 {
		t.Fatalf("app.yaml must have exactly 1 top-level digest line, got %d", digestLines)
	}
	wantFields := map[string]string{
		"name":      "shakedown",
		"namespace": "cs-workers",
		"image":     "shakedown",
		"replicas":  "1",
		"port":      "8080",
		"memory":    "30Mi",
	}
	for k, want := range wantFields {
		if got := fields[k]; got != want {
			t.Errorf("app.yaml %s = %q, want %q", k, got, want)
		}
	}

	kustRaw, err := os.ReadFile("kustomization.yaml")
	if err != nil {
		t.Fatalf("missing kustomization.yaml: %v", err)
	}
	kust := string(kustRaw)
	for _, rel := range []string{"deploy/networkpolicy.yaml", "deploy/shakedown.yaml"} {
		if !strings.Contains(kust, rel) {
			t.Errorf("kustomization.yaml missing %q", rel)
		}
	}
}

func TestGofmtClean(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		formatted, err := format.Source(src)
		if err != nil {
			t.Errorf("%s failed go/format: %v", path, err)
			return nil
		}
		if !bytes.Equal(src, formatted) {
			t.Errorf("%s is not gofmt-formatted", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
