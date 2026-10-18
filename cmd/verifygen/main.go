package main

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/prepaser/mytracker/internal/atomicfile"
	"github.com/prepaser/mytracker/internal/browserverify"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("verifygen", flag.ContinueOnError)
	out := fs.String("out", "index.html", "output HTML file")
	roots := fs.String("tsa-roots", "", "optional public TSA trust roots PEM")
	policy := fs.String("tsa-policy", "", "comma-separated allowed TSA policy OIDs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	repo, err := findRoot()
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "mytracker-verifygen-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	env := os.Environ()
	for _, key := range []string{"GOOS=", "GOARCH=", "CGO_ENABLED="} {
		filtered := env[:0]
		for _, entry := range env {
			if !strings.HasPrefix(entry, key) {
				filtered = append(filtered, entry)
			}
		}
		env = filtered
	}
	env = append(env, "GOOS=js", "GOARCH=wasm", "CGO_ENABLED=0")
	build := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", filepath.Join(tmp, "verify.wasm"), "./cmd/verifywasm")
	build.Dir, build.Env, build.Stderr = repo, env, os.Stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("build verifier: %w", err)
	}
	rootCmd := exec.Command("go", "env", "GOROOT")
	rootCmd.Dir, rootCmd.Env = repo, env
	goRoot, err := rootCmd.Output()
	if err != nil {
		return fmt.Errorf("find Go runtime: %w", err)
	}
	runtime, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(goRoot)), "lib", "wasm", "wasm_exec.js"))
	if err != nil {
		return err
	}
	goLicense, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(goRoot)), "LICENSE"))
	if os.IsNotExist(err) {
		goLicense, err = os.ReadFile("/usr/share/licenses/go/LICENSE")
	}
	if err != nil {
		return err
	}
	runtime = append([]byte("/*\n"+string(goLicense)+"\n*/\n"), runtime...)
	var notices strings.Builder
	notices.WriteString("mytracker\nSPDX-License-Identifier: AGPL-3.0-only\nSource: https://github.com/prepaser/mytracker\n\n")
	for _, name := range []string{"LICENSE", "THIRD_PARTY_NOTICES"} {
		data, err := os.ReadFile(filepath.Join(repo, name))
		if err != nil {
			return err
		}
		notices.Write(data)
		notices.WriteByte('\n')
	}
	runtime = append([]byte("/*\n"+notices.String()+"\n*/\n"), runtime...)
	wasm, err := os.ReadFile(filepath.Join(tmp, "verify.wasm"))
	if err != nil {
		return err
	}
	worker := string(runtime) + "\nconst wasmBase64 = " + quote(base64.StdEncoding.EncodeToString(wasm)) + ";\n" + workerLoader
	workerJSON := quote(worker)
	defaults := map[string]any{"roots_pem": "", "policy_oids": []string{}, "providers": []string{"https://blockstream.info/api", "https://mempool.space/api"}, "min_confirmations": 6, "online": false}
	if *roots != "" {
		f, err := os.Open(*roots)
		if err != nil {
			return err
		}
		pem, readErr := io.ReadAll(io.LimitReader(f, (1<<20)+1))
		_ = f.Close()
		if readErr != nil {
			return readErr
		}
		if len(pem) > 1<<20 || !x509.NewCertPool().AppendCertsFromPEM(pem) {
			return fmt.Errorf("invalid TSA roots PEM (maximum 1 MiB)")
		}
		public, err := publicRoots(pem)
		if err != nil {
			return err
		}
		defaults["roots_pem"] = string(public)
	}
	if *policy != "" {
		oids := strings.Split(*policy, ",")
		for i := range oids {
			oids[i] = strings.TrimSpace(oids[i])
		}
		defaults["policy_oids"] = oids
	}
	options, _ := json.Marshal(defaults)
	var verificationOptions browserverify.Options
	if err := json.Unmarshal(options, &verificationOptions); err != nil {
		return err
	}
	if err := browserverify.ValidateOptions(verificationOptions); err != nil {
		return err
	}
	template, err := os.ReadFile(filepath.Join(repo, "internal", "verifyui", "assets", "index.html"))
	if err != nil {
		return err
	}
	html := string(template)
	for _, item := range [][2]string{{"{{WORKER_JS_JSON}}", workerJSON}, {"{{DEFAULT_OPTIONS_JSON}}", string(options)}} {
		if strings.Count(html, item[0]) != 1 {
			return fmt.Errorf("invalid verifier template: %s", item[0])
		}
		html = strings.Replace(html, item[0], item[1], 1)
	}
	if err := atomicfile.WriteFunc(*out, 0644, func(f *os.File) error { _, err := io.WriteString(f, html); return err }); err != nil {
		return err
	}
	fmt.Printf("Generated %s (%d bytes)\n", *out, len(html))
	return nil
}

func publicRoots(data []byte) ([]byte, error) {
	var public []byte
	for len(bytes.TrimSpace(data)) > 0 {
		block, rest := pem.Decode(data)
		if block == nil || block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("TSA roots must contain only public certificates")
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return nil, fmt.Errorf("invalid TSA certificate")
		}
		public = append(public, pem.EncodeToMemory(block)...)
		data = rest
	}
	if len(public) == 0 {
		return nil, fmt.Errorf("no TSA certificates")
	}
	return public, nil
}

func quote(value string) string { b, _ := json.Marshal(value); return string(b) }

func findRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "internal", "verifyui", "assets", "index.html")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("run verifygen inside the mytracker source tree")
		}
		dir = parent
	}
}

const workerLoader = `
const originalFetch = globalThis.fetch.bind(globalThis);
globalThis.fetch = (url, options = {}) => originalFetch(url, {...options, mode: 'cors', credentials: 'omit', redirect: 'error', referrerPolicy: 'no-referrer'});
(async () => {
  try {
    const bytes = Uint8Array.from(atob(wasmBase64), c => c.charCodeAt(0));
    const go = new Go();
    const {instance} = await WebAssembly.instantiate(bytes, go.importObject);
    await go.run(instance);
  } catch {
    postMessage({type: 'error', message: 'verifier_runtime_failed'});
  }
})();
`
