//go:build e2e

// End-to-end on a Linux host with Docker: a fake OIDC issuer, the real server, engine and
// client (deploy.sh). Everything it creates is named cd-e2e-* and removed at the end.
//
//	go test -c -tags e2e -o cd-e2e ./internal/e2e && sudo ./cd-e2e -test.v -client ./deploy.sh
package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dmitry-Fedotov-Dev/cd/internal/engine"
	"github.com/Dmitry-Fedotov-Dev/cd/internal/oidc"
	"github.com/Dmitry-Fedotov-Dev/cd/internal/project"
	"github.com/Dmitry-Fedotov-Dev/cd/internal/server"
)

var client = flag.String("client", "deploy.sh", "path to the cd client")

type issuer struct {
	url string
	key *rsa.PrivateKey
}

func newIssuer(t *testing.T) *issuer {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	is := &issuer{key: key}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	is.url = srv.URL
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"jwks_uri": srv.URL + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "e2e",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
	})
	return is
}

func (is *issuer) token(aud, repo string) string {
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	h := enc(map[string]string{"alg": "RS256", "kid": "e2e"})
	b := enc(map[string]any{"iss": is.url, "aud": aud, "exp": time.Now().Add(5 * time.Minute).Unix(),
		"repository": repo, "ref": "refs/heads/main", "actor": "e2e", "run_id": "1"})
	sum := sha256.Sum256([]byte(h + "." + b))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, is.key, crypto.SHA256, sum[:])
	return h + "." + b + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func tgz(t *testing.T, files map[string]string) string {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: "app/" + name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	f := filepath.Join(t.TempDir(), "bundle.tgz")
	os.WriteFile(f, buf.Bytes(), 0o644)
	return f
}

const nodeServer = `const http = require('http'), fs = require('fs');
const V = %q;
http.createServer((req, res) => {
  if (req.url === '/healthz') { %s }
  if (req.url === '/count') {
    const f = '/app/data/count';
    const n = (fs.existsSync(f) ? +fs.readFileSync(f, 'utf8') : 0) + 1;
    fs.writeFileSync(f, String(n));
    return res.end(V + ' ' + n);
  }
  res.end(V);
}).listen(process.env.PORT, process.env.HOST);
`

func nodeApp(t *testing.T, version string, healthy bool) string {
	health := `return res.end('{"ok":true}');`
	if !healthy {
		health = `res.statusCode = 500; return res.end('broken');`
	}
	return tgz(t, map[string]string{
		"package.json": `{"name":"e2e","version":"1.0.0","scripts":{"start":"node server.js"}}`,
		"server.js":    fmt.Sprintf(nodeServer, version, health),
	})
}

func get(t *testing.T, url string) string {
	resp, err := http.Get(url)
	if err != nil {
		return "ERR " + err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestEndToEnd(t *testing.T) {
	is := newIssuer(t)
	dir := t.TempDir()
	store := project.Store{Dir: filepath.Join(dir, "projects")}
	rule := []project.Rule{{"iss": is.url, "repository": "me/app", "ref": "refs/heads/main"}}
	must(t, store.Put(&project.Project{Name: "cd-e2e-static", Type: "static", Port: 3991, Health: "/", Allow: rule}))
	must(t, store.Put(&project.Project{Name: "cd-e2e-node", Type: "node", Port: 3992, DataMount: "/app/data", Allow: rule}))

	t.Cleanup(func() {
		for _, n := range []string{"cd-e2e-static", "cd-e2e-node"} {
			exec.Command("docker", "rm", "-f", "cd-"+n, "cd-"+n+"-old").Run()
			out, _ := exec.Command("docker", "images", "-q", "cd-"+n).Output()
			for _, id := range strings.Fields(string(out)) {
				exec.Command("docker", "rmi", "-f", id).Run()
			}
			exec.Command("docker", "network", "rm", "cd-"+n).Run()
		}
		exec.Command("docker", "buildx", "rm", "cd-e2e-builder").Run()
	})

	var api *httptest.Server
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := &server.Server{
			Store:    store,
			Verifier: &oidc.Verifier{Issuers: []string{is.url}, Audience: api.URL},
			Engine:   eng,
			Log:      slog.New(slog.NewTextHandler(os.Stderr, nil)),
			Version:  "e2e",
		}
		s.Handler().ServeHTTP(w, r)
	}))
	defer api.Close()
	eng.StateDir = filepath.Join(dir, "state")

	run := func(project, repo, bundle string, args ...string) (int, string) {
		cmd := exec.Command("bash", append([]string{*client}, args...)...)
		cmd.Env = append(os.Environ(), "CD_SERVER="+api.URL, "CD_PROJECT="+project,
			"CD_TOKEN="+is.token(api.URL, repo), "CD_BUNDLE="+bundle, "CD_SHA="+randomSHA())
		out, _ := cmd.CombinedOutput()
		return cmd.ProcessState.ExitCode(), string(out)
	}
	expect := func(name string, code int, want int, out string) {
		t.Helper()
		if code != want {
			t.Fatalf("%s: exit %d, want %d\n%s", name, code, want, out)
		}
		t.Logf("%s: exit %d ok", name, code)
	}

	site := tgz(t, map[string]string{"index.html": "<p>static e2e</p>"})
	code, out := run("cd-e2e-static", "evil/app", site)
	expect("foreign repo is denied", code, 5, out)
	code, out = run("cd-e2e-static", "me/app", site)
	expect("static deploy", code, 0, out)
	if body := get(t, "http://127.0.0.1:3991/"); !strings.Contains(body, "static e2e") {
		t.Fatalf("static site: %q", body)
	}

	code, out = run("cd-e2e-node", "me/app", nodeApp(t, "v1", true))
	expect("node v1", code, 0, out)
	get(t, "http://127.0.0.1:3992/count")
	if body := get(t, "http://127.0.0.1:3992/count"); body != "v1 2" {
		t.Fatalf("v1 count: %q", body)
	}

	code, out = run("cd-e2e-node", "me/app", nodeApp(t, "v2-broken", false))
	expect("broken v2 rolls back", code, 1, out)
	if body := get(t, "http://127.0.0.1:3992/"); body != "v1" {
		t.Fatalf("after failed deploy: %q", body)
	}

	code, out = run("cd-e2e-node", "me/app", nodeApp(t, "v3", true))
	expect("node v3", code, 0, out)
	if body := get(t, "http://127.0.0.1:3992/count"); body != "v3 3" {
		t.Fatalf("data survived deploy: %q", body)
	}

	code, out = run("cd-e2e-node", "me/app", "", "rollback")
	expect("rollback", code, 0, out)
	if body := get(t, "http://127.0.0.1:3992/"); body != "v1" {
		t.Fatalf("after rollback: %q", body)
	}

	code, out = run("cd-e2e-node", "me/app", "", "status")
	expect("status", code, 0, out)
	t.Log(out)

	bad := tgz(t, map[string]string{"server.js": "x"})
	code, out = run("cd-e2e-node", "me/app", bad)
	expect("bundle without package.json is rejected", code, 2, out)

	insp, _ := exec.Command("docker", "inspect", "-f",
		"{{.HostConfig.ReadonlyRootfs}} {{.HostConfig.CapDrop}} {{.HostConfig.Memory}} {{.HostConfig.PidsLimit}} {{.Config.User}} {{.HostConfig.Privileged}}",
		"cd-cd-e2e-node").Output()
	if got := strings.TrimSpace(string(insp)); got != "true [ALL] 268435456 128 10001:10001 false" {
		t.Fatalf("container hardening: %q", got)
	}
	lim, _ := exec.Command("docker", "container", "inspect", "-f", "{{.HostConfig.Memory}} {{.HostConfig.CpuQuota}}",
		"buildx_buildkit_cd-e2e-builder0").Output()
	if got := strings.TrimSpace(string(lim)); got != "1073741824 100000" {
		t.Fatalf("builder limits: %q", got)
	}
}

var eng = &engine.Engine{BuildMinFreeMB: 300, Builder: "cd-e2e-builder", BuildMemory: "1g", BuildCPUs: "1",
	BuildTimeout: 10 * time.Minute}

func randomSHA() string {
	b := make([]byte, 20)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
