package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dmitry-Fedotov-Dev/cd/internal/project"
)

func testProject(typ string) *project.Project {
	return &project.Project{Name: "app", Type: typ, Port: 3100, CPUs: "0.5", Memory: "256m", PIDs: 128,
		TmpMB: 64, User: "10001:10001", Node: "22-alpine", StaticDir: "dist", Dockerfile: "Dockerfile"}
}

func TestRunArgsAreLocked(t *testing.T) {
	p := testProject("node")
	p.DataMount = "/app/data"
	a := RunArgs(p, "cd-app:abc", "/var/lib/cdd/projects/app/data")
	joined := strings.Join(a, " ")
	for _, want := range []string{"--read-only", "--cap-drop ALL", "--security-opt no-new-privileges",
		"--memory 256m", "--memory-swap 256m", "--pids-limit 128", "-p 127.0.0.1:3100:3000",
		"--user 10001:10001", "-v /var/lib/cdd/projects/app/data:/app/data"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
	for _, bad := range []string{"--privileged", "--network host", "docker.sock", "0.0.0.0:"} {
		if strings.Contains(joined, bad) {
			t.Errorf("unexpected %q", bad)
		}
	}
	if a[len(a)-1] != "cd-app:abc" {
		t.Error("image must be the last argument")
	}
}

func TestDockerfileNode(t *testing.T) {
	e := &Engine{}
	work := t.TempDir()
	src := filepath.Join(work, "src")
	os.MkdirAll(src, 0o755)
	p := testProject("node")
	if _, err := e.dockerfile(p, work, src); err == nil {
		t.Fatal("node without package.json accepted")
	}
	os.WriteFile(filepath.Join(src, "package.json"), []byte(`{"scripts":{"build":"x"}}`), 0o644)
	if _, err := e.dockerfile(p, work, src); err == nil {
		t.Fatal("node without start script accepted")
	}
	os.WriteFile(filepath.Join(src, "package.json"), []byte(`{"scripts":{"start":"node a.js"}}`), 0o644)
	p.DataMount = "/app/data"
	f, err := e.dockerfile(p, work, src)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f)
	for _, want := range []string{"FROM node:22-alpine", "max-old-space-size=192", "chown 10001:10001 /app/data", `CMD ["npm", "start"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("Dockerfile lacks %q:\n%s", want, b)
		}
	}
}

func TestDockerfileStaticAndOwn(t *testing.T) {
	e := &Engine{}
	work := t.TempDir()
	src := filepath.Join(work, "src")
	os.MkdirAll(src, 0o755)
	f, err := e.dockerfile(testProject("static"), work, src)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f)
	if !strings.Contains(string(b), "COPY . /srv") || strings.Contains(string(b), "npm ci") {
		t.Errorf("plain static:\n%s", b)
	}
	own := testProject("dockerfile")
	if _, err := e.dockerfile(own, work, src); err == nil {
		t.Fatal("missing Dockerfile accepted")
	}
	own.Dockerfile = "../../etc/passwd"
	if _, err := e.dockerfile(own, work, src); err == nil {
		t.Fatal("Dockerfile outside bundle accepted")
	}
}

func TestHeap(t *testing.T) {
	for in, want := range map[string]int{"256m": 192, "1g": 768, "64m": 64, "junk": 192} {
		if got := heapMB(in); got != want {
			t.Errorf("heapMB(%s)=%d want %d", in, got, want)
		}
	}
}
