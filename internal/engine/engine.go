// Package engine builds a release from an unpacked bundle and swaps it in with the docker
// CLI: one container per project, fixed limits, health check, automatic rollback.
package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/Dmitry-Fedotov-Dev/cd/internal/bundle"
	"github.com/Dmitry-Fedotov-Dev/cd/internal/project"
	"github.com/Dmitry-Fedotov-Dev/cd/templates"
)

// Result codes are the protocol's exit codes (see docs/PROTOCOL.md).
const (
	OK       = 0 // the new release is serving
	Failed   = 1 // build or start failed; the previous release is serving
	Rejected = 2 // the bundle itself is unacceptable
	Busy     = 3 // another deploy is running or the server is short of resources; retry later
	BadInput = 4 // unknown action or project, bad parameters
)

type Result struct {
	Code int
	Msg  string
}

// Engine runs at most one deploy (or rollback) at a time.
type Engine struct {
	StateDir       string
	BuildMinFreeMB int
	Docker         string // docker binary, "docker" by default
	HTTP           *http.Client

	// Builder is the buildx builder for releases, created on first use with BuildMemory and
	// BuildCPUs as its container limits. Empty: plain `docker build`, no limits.
	Builder      string
	BuildMemory  string
	BuildCPUs    string
	BuildTimeout time.Duration // 0: only the request's own deadline

	mu sync.Mutex
}

// State is <state>/projects/<name>/release.json.
type State struct {
	Image      string    `json:"image"`
	SHA        string    `json:"sha"`
	At         time.Time `json:"at"`
	Previous   string    `json:"previous,omitempty"`
	PrevSHA    string    `json:"previous_sha,omitempty"`
	DeployedBy string    `json:"deployed_by,omitempty"`
}

func (e *Engine) dir(p *project.Project) string {
	return filepath.Join(e.StateDir, "projects", p.Name)
}

func container(p *project.Project) string { return "cd-" + p.Name }

// Deploy unpacks the bundle, builds the image and swaps the container. Progress goes to out.
func (e *Engine) Deploy(ctx context.Context, p *project.Project, sha, by string, src io.Reader, out io.Writer) Result {
	if !e.mu.TryLock() {
		return Result{Busy, "another deploy is running"}
	}
	defer e.mu.Unlock()
	if r, ok := e.checkResources(); !ok {
		return r
	}

	work := filepath.Join(e.dir(p), "build-"+time.Now().UTC().Format("20060102-150405"))
	srcDir := filepath.Join(work, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		return Result{Failed, err.Error()}
	}
	defer os.RemoveAll(work)

	step(out, "unpack")
	if err := bundle.Extract(src, srcDir, bundle.DefaultLimits); err != nil {
		if errors.Is(err, bundle.ErrRejected) {
			return Result{Rejected, err.Error()}
		}
		return Result{Failed, "unpack: " + err.Error()}
	}

	dockerfile, err := e.dockerfile(p, work, srcDir)
	if err != nil {
		return Result{Rejected, err.Error()}
	}
	image := fmt.Sprintf("cd-%s:%s", p.Name, sha)
	step(out, "build "+image)
	if r := e.build(ctx, out, dockerfile, image, srcDir, "cd.project="+p.Name, "cd.sha="+sha); r.Code != OK {
		return r
	}

	st, _ := e.state(p)
	if r := e.swap(ctx, p, image, out); r.Code != OK {
		return r
	}
	next := State{Image: image, SHA: sha, At: time.Now().UTC(), DeployedBy: by}
	if st.Image != "" && st.Image != image {
		next.Previous, next.PrevSHA = st.Image, st.SHA
	} else {
		next.Previous, next.PrevSHA = st.Previous, st.PrevSHA
	}
	if err := e.saveState(p, next); err != nil {
		fmt.Fprintf(out, "warning: state not saved: %v\n", err)
	}
	e.pruneImages(ctx, p, next)
	return Result{OK, "deployed " + short(sha)}
}

// Rollback swaps the previous image back in.
func (e *Engine) Rollback(ctx context.Context, p *project.Project, out io.Writer) Result {
	if !e.mu.TryLock() {
		return Result{Busy, "another deploy is running"}
	}
	defer e.mu.Unlock()
	st, err := e.state(p)
	if err != nil || st.Previous == "" {
		return Result{Failed, "no previous release to roll back to"}
	}
	if r := e.swap(ctx, p, st.Previous, out); r.Code != OK {
		return r
	}
	next := State{Image: st.Previous, SHA: st.PrevSHA, At: time.Now().UTC(), Previous: st.Image, PrevSHA: st.SHA, DeployedBy: "rollback"}
	if err := e.saveState(p, next); err != nil {
		fmt.Fprintf(out, "warning: state not saved: %v\n", err)
	}
	return Result{OK, "rolled back to " + short(st.PrevSHA)}
}

// Status prints the release, the container state, health and usage.
func (e *Engine) Status(ctx context.Context, p *project.Project, out io.Writer) Result {
	st, _ := e.state(p)
	fmt.Fprintf(out, "project     %s (%s)\n", p.Name, p.Type)
	if p.Domain != "" {
		fmt.Fprintf(out, "domain      https://%s\n", p.Domain)
	}
	fmt.Fprintf(out, "release     %s  %s  by %s\n", orDash(short(st.SHA)), orDash(fmtTime(st.At)), orDash(st.DeployedBy))
	fmt.Fprintf(out, "previous    %s\n", orDash(short(st.PrevSHA)))
	state := strings.TrimSpace(e.output(ctx, "container", "inspect", "-f", "{{.State.Status}} since {{.State.StartedAt}} restarts={{.RestartCount}}", container(p)))
	fmt.Fprintf(out, "container   %s\n", orDash(state))
	code, body := e.probe(ctx, p)
	fmt.Fprintf(out, "health      %s %d %s\n", p.Health, code, strings.TrimSpace(body))
	usage := strings.TrimSpace(e.output(ctx, "stats", "--no-stream", "--format", "{{.CPUPerc}} CPU, {{.MemUsage}}", container(p)))
	fmt.Fprintf(out, "usage       %s (limits: cpus %s, memory %s, pids %d)\n", orDash(usage), p.CPUs, p.Memory, p.PIDs)
	if code < 200 || code >= 400 {
		return Result{Failed, "unhealthy"}
	}
	return Result{OK, "healthy"}
}

// Logs prints the last n lines of the container log.
func (e *Engine) Logs(ctx context.Context, p *project.Project, n int, out io.Writer) Result {
	if err := e.docker(ctx, out, "logs", "--tail", strconv.Itoa(n), container(p)); err != nil {
		return Result{Failed, "no logs"}
	}
	return Result{OK, ""}
}

// swap replaces the running container with one from image; on failure it restores the old one.
func (e *Engine) swap(ctx context.Context, p *project.Project, image string, out io.Writer) Result {
	name, old := container(p), container(p)+"-old"
	if err := e.prepare(ctx, p, out); err != nil {
		return Result{Failed, err.Error()}
	}
	_ = e.quiet(ctx, "rm", "-f", old)
	hadOld := e.quiet(ctx, "container", "inspect", name) == nil
	if hadOld {
		step(out, "stop current release")
		_ = e.quiet(ctx, "stop", "-t", "20", name)
		if err := e.quiet(ctx, "rename", name, old); err != nil {
			return Result{Failed, "rename: " + err.Error()}
		}
	}
	step(out, "start "+image)
	if err := e.docker(ctx, out, append([]string{"run"}, RunArgs(p, image, e.dataDir(p))...)...); err != nil {
		e.restore(ctx, p, hadOld, out)
		return Result{Failed, "container did not start"}
	}
	step(out, fmt.Sprintf("wait for %s (up to %ds)", p.Health, p.HealthSec))
	if err := e.waitHealthy(ctx, p); err != nil {
		fmt.Fprintf(out, "health check failed: %v\n--- last log lines of the new release ---\n", err)
		_ = e.docker(ctx, out, "logs", "--tail", "60", name)
		e.restore(ctx, p, hadOld, out)
		return Result{Failed, "health check failed, previous release restored"}
	}
	if hadOld {
		_ = e.quiet(ctx, "rm", "-f", old)
	}
	fmt.Fprintln(out, "healthy")
	return Result{OK, ""}
}

func (e *Engine) restore(ctx context.Context, p *project.Project, hadOld bool, out io.Writer) {
	name, old := container(p), container(p)+"-old"
	_ = e.quiet(ctx, "rm", "-f", name)
	if !hadOld {
		return
	}
	step(out, "restore previous release")
	if err := e.quiet(ctx, "rename", old, name); err == nil {
		_ = e.quiet(ctx, "start", name)
		if err := e.waitHealthy(ctx, p); err != nil {
			fmt.Fprintf(out, "previous release is not healthy either: %v\n", err)
		}
	}
}

// prepare creates the project's network and data directory.
func (e *Engine) prepare(ctx context.Context, p *project.Project, out io.Writer) error {
	net := container(p)
	if e.quiet(ctx, "network", "inspect", net) != nil {
		if err := e.docker(ctx, out, "network", "create", "--label", "cd.project="+p.Name, net); err != nil {
			return fmt.Errorf("network: %w", err)
		}
	}
	if p.DataMount == "" {
		return nil
	}
	d := e.dataDir(p)
	if err := os.MkdirAll(d, 0o750); err != nil {
		return err
	}
	uid, gid, ok := parseUser(p.User)
	if ok {
		return chownTree(d, uid, gid)
	}
	return nil
}

func (e *Engine) dataDir(p *project.Project) string {
	if p.DataMount == "" {
		return ""
	}
	return filepath.Join(e.dir(p), "data")
}

// RunArgs is everything after `docker run`: the container's limits are fixed here, not in the project.
func RunArgs(p *project.Project, image, dataDir string) []string {
	a := []string{
		"-d", "--name", container(p), "--restart", "unless-stopped",
		"--network", container(p),
		"-p", fmt.Sprintf("127.0.0.1:%d:3000", p.Port),
		"--read-only", "--tmpfs", fmt.Sprintf("/tmp:rw,size=%dm,mode=1777", p.TmpMB),
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--cpus", p.CPUs, "--memory", p.Memory, "--memory-swap", p.Memory,
		"--pids-limit", strconv.Itoa(p.PIDs), "--ulimit", "nofile=4096:8192",
		"--user", p.User,
		"--log-driver", "json-file", "--log-opt", "max-size=10m", "--log-opt", "max-file=5",
		"--label", "cd.project=" + p.Name,
		"-e", "PORT=3000", "-e", "HOST=0.0.0.0",
	}
	if p.EnvFile != "" {
		a = append(a, "--env-file", p.EnvFile)
	}
	if dataDir != "" {
		a = append(a, "-v", dataDir+":"+p.DataMount)
	}
	return append(a, image)
}

func (e *Engine) dockerfile(p *project.Project, work, srcDir string) (string, error) {
	switch p.Type {
	case "dockerfile":
		f := filepath.Join(srcDir, filepath.FromSlash(p.Dockerfile))
		if rel, err := filepath.Rel(srcDir, f); err != nil || strings.HasPrefix(rel, "..") {
			return "", fmt.Errorf("dockerfile %q is outside the bundle", p.Dockerfile)
		}
		if fi, err := os.Lstat(f); err != nil || !fi.Mode().IsRegular() {
			return "", fmt.Errorf("bundle has no %s", p.Dockerfile)
		}
		return f, nil
	case "node", "static":
	default:
		return "", fmt.Errorf("unknown type %q", p.Type)
	}
	hasPackage := false
	if b, err := os.ReadFile(filepath.Join(srcDir, "package.json")); err == nil {
		hasPackage = true
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
		}
		if json.Unmarshal(b, &pkg) != nil {
			return "", errors.New("package.json is not valid JSON")
		}
		if p.Type == "node" && pkg.Scripts["start"] == "" {
			return "", errors.New(`type node needs "scripts.start" in package.json`)
		}
		if p.Type == "static" && pkg.Scripts["build"] == "" {
			return "", errors.New(`type static with package.json needs "scripts.build"`)
		}
	} else if p.Type == "node" {
		return "", errors.New("type node needs package.json")
	}
	tpl, err := template.ParseFS(templates.FS, p.Type+"/Dockerfile.tmpl")
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	err = tpl.Execute(&buf, map[string]any{
		"Node": p.Node, "Heap": heapMB(p.Memory), "DataMount": p.DataMount, "User": p.User,
		"HasPackage": hasPackage, "StaticDir": strings.Trim(p.StaticDir, "/"),
	})
	if err != nil {
		return "", err
	}
	f := filepath.Join(work, "Dockerfile")
	return f, os.WriteFile(f, buf.Bytes(), 0o644)
}

// heapMB leaves a quarter of the container's memory to everything that is not the JS heap.
func heapMB(mem string) int {
	m := strings.ToLower(strings.TrimSpace(mem))
	mult := 1
	switch {
	case strings.HasSuffix(m, "g"):
		m, mult = strings.TrimSuffix(m, "g"), 1024
	case strings.HasSuffix(m, "m"):
		m = strings.TrimSuffix(m, "m")
	default:
		return 192
	}
	n, err := strconv.Atoi(m)
	if err != nil || n <= 0 {
		return 192
	}
	return max(64, n*mult*3/4)
}

func (e *Engine) waitHealthy(ctx context.Context, p *project.Project) error {
	deadline := time.Now().Add(time.Duration(p.HealthSec) * time.Second)
	var last string
	for time.Now().Before(deadline) {
		running, restarts, _ := strings.Cut(strings.TrimSpace(e.output(ctx, "container", "inspect", "-f", "{{.State.Running}} {{.RestartCount}}", container(p))), " ")
		if running == "false" {
			return errors.New("container exited")
		}
		if restarts != "" && restarts != "0" {
			return errors.New("container keeps crashing")
		}
		code, body := e.probe(ctx, p)
		if code >= 200 && code < 400 {
			return nil
		}
		last = fmt.Sprintf("%d %s", code, strings.TrimSpace(body))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("no healthy answer in %ds (last: %s)", p.HealthSec, last)
}

func (e *Engine) probe(ctx context.Context, p *project.Project) (int, string) {
	client := e.HTTP
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", p.Port, p.Health), nil)
	if err != nil {
		return 0, err.Error()
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "no answer"
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	return resp.StatusCode, string(b)
}

// build makes the image. The bundle's code runs here (npm install scripts, its build), so with
// a Builder it runs in that BuildKit container under its memory/CPU limits, and every build
// has a deadline.
func (e *Engine) build(ctx context.Context, out io.Writer, dockerfile, image, dir string, labels ...string) Result {
	if e.BuildTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.BuildTimeout)
		defer cancel()
	}
	args := []string{"build", "--pull", "-f", dockerfile, "-t", image}
	if e.Builder != "" {
		if err := e.ensureBuilder(ctx, out); err != nil {
			return Result{Failed, "builder: " + err.Error()}
		}
		args = []string{"buildx", "build", "--builder", e.Builder, "--load", "--progress", "plain",
			"--pull", "-f", dockerfile, "-t", image}
	}
	for _, l := range labels {
		args = append(args, "--label", l)
	}
	if err := e.docker(ctx, out, append(args, dir)...); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return Result{Failed, fmt.Sprintf("build took longer than %s", e.BuildTimeout)}
		}
		return Result{Failed, "build failed"}
	}
	return Result{OK, ""}
}

// ensureBuilder creates the limited BuildKit builder once. Its limits are fixed at creation:
// after changing build_memory/build_cpus, `docker buildx rm <builder>` and the next build
// recreates it.
func (e *Engine) ensureBuilder(ctx context.Context, out io.Writer) error {
	if e.quiet(ctx, "buildx", "inspect", e.Builder) == nil {
		return nil
	}
	cpus, err := strconv.ParseFloat(e.BuildCPUs, 64)
	if err != nil || cpus <= 0 {
		return fmt.Errorf("build_cpus %q is not a number", e.BuildCPUs)
	}
	step(out, "create builder "+e.Builder+" (memory "+e.BuildMemory+", cpus "+e.BuildCPUs+")")
	return e.docker(ctx, out, "buildx", "create", "--name", e.Builder, "--driver", "docker-container",
		"--driver-opt", "memory="+e.BuildMemory,
		"--driver-opt", "memory-swap="+e.BuildMemory,
		"--driver-opt", "cpu-period=100000",
		"--driver-opt", fmt.Sprintf("cpu-quota=%d", int(cpus*100000)),
		"--bootstrap")
}

func (e *Engine) checkResources() (Result, bool) {
	if e.BuildMinFreeMB <= 0 {
		return Result{}, true
	}
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return Result{}, true
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if kb, ok := strings.CutPrefix(sc.Text(), "MemAvailable:"); ok {
			n, _ := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(kb), "kB")))
			if n/1024 < e.BuildMinFreeMB {
				return Result{Busy, fmt.Sprintf("only %d MB of free memory, need %d MB to build", n/1024, e.BuildMinFreeMB)}, false
			}
		}
	}
	return Result{}, true
}

func (e *Engine) pruneImages(ctx context.Context, p *project.Project, st State) {
	keep := map[string]bool{st.Image: true, st.Previous: true}
	for _, img := range strings.Fields(e.output(ctx, "images", "--format", "{{.Repository}}:{{.Tag}}", "cd-"+p.Name)) {
		if !keep[img] {
			_ = e.quiet(ctx, "rmi", img)
		}
	}
}

func (e *Engine) state(p *project.Project) (State, error) {
	var st State
	b, err := os.ReadFile(filepath.Join(e.dir(p), "release.json"))
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(b, &st)
}

func (e *Engine) saveState(p *project.Project, st State) error {
	b, _ := json.MarshalIndent(st, "", "  ")
	f := filepath.Join(e.dir(p), "release.json")
	if err := os.WriteFile(f+".tmp", append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(f+".tmp", f)
}

func (e *Engine) bin() string {
	if e.Docker != "" {
		return e.Docker
	}
	return "docker"
}

// docker runs a docker command with its output streamed to out.
func (e *Engine) docker(ctx context.Context, out io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, e.bin(), args...)
	cmd.Stdout, cmd.Stderr = out, out
	return cmd.Run()
}

func (e *Engine) quiet(ctx context.Context, args ...string) error {
	return exec.CommandContext(ctx, e.bin(), args...).Run()
}

func (e *Engine) output(ctx context.Context, args ...string) string {
	b, _ := exec.CommandContext(ctx, e.bin(), args...).Output()
	return string(b)
}

func step(out io.Writer, s string) { fmt.Fprintf(out, "==> %s\n", s) }

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

func parseUser(u string) (int, int, bool) {
	a, b, ok := strings.Cut(u, ":")
	uid, err1 := strconv.Atoi(a)
	gid, err2 := strconv.Atoi(b)
	return uid, gid, ok && err1 == nil && err2 == nil
}
