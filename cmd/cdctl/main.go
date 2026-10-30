// cdctl manages cdd's projects on the server: who may deploy what, where it runs, its limits.
//
//	cdctl project add NAME --repo OWNER/REPO [--branch main] [--environment production]
//	                      [--gitlab | --issuer URL] [--type node|static|dockerfile] [--port auto|N]
//	                      [--domain D] [--health /healthz] [--data /app/data] [--env-file F]
//	                      [--cpus 0.5] [--memory 256m] [--pids 128] [--max-bundle-mb 50]
//	cdctl project list | show NAME
//	cdctl stop NAME       stop the app and refuse its deploys; cdctl start NAME undoes it
//	cdctl project rm NAME [--purge]   take the app down for good (--purge: its data too)
//	cdctl top             CPU, memory and processes of every app and of the builder
//	cdctl domain setup --zone example.com --ip 1.2.3.4   let cdctl manage project names (Porkbun + Caddy)
//	cdctl domain NAME [--yes]      the project's A record (asks first), Caddy site, HTTPS check
//	cdctl domain rm NAME [--yes]   take the name away (the record only if cd created it)
//	cdctl caddy NAME      print a Caddy site block for the project's domain
//	cdctl version
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/Dmitry-Fedotov-Dev/cd/internal/project"
	"github.com/Dmitry-Fedotov-Dev/cd/internal/proxy"
)

var version = "dev"

func main() {
	config := "/etc/cdd/cdd.json"
	args := os.Args[1:]
	if len(args) >= 2 && args[0] == "--config" {
		config, args = args[1], args[2:]
	}
	cfg, err := project.LoadServer(config)
	if err != nil && len(args) > 0 && args[0] != "version" {
		die("%v", err)
	}
	store := project.Store{Dir: cfg.ProjectsDir}

	switch {
	case len(args) >= 3 && args[0] == "project" && args[1] == "add":
		add(cfg, store, args[2], args[3:])
	case len(args) >= 2 && args[0] == "domain" && args[1] == "setup":
		domainSetup(config, cfg, args[2:])
	case len(args) >= 3 && len(args) <= 4 && args[0] == "domain" && args[1] == "rm":
		disconnect(cfg, store, get(store, args[2]), yesFlag(args[3:]), true)
	case len(args) >= 2 && len(args) <= 3 && args[0] == "domain":
		connect(cfg, store, get(store, args[1]), yesFlag(args[2:]))
	case len(args) == 2 && args[0] == "project" && args[1] == "list":
		list(store)
	case len(args) == 3 && args[0] == "project" && args[1] == "show":
		p := get(store, args[2])
		b, _ := json.MarshalIndent(p, "", "  ")
		fmt.Println(string(b))
	case len(args) >= 3 && len(args) <= 4 && args[0] == "project" && args[1] == "rm":
		purge := len(args) == 4
		if purge && args[3] != "--purge" {
			die("unknown flag %s", args[3])
		}
		remove(cfg, store, get(store, args[2]), purge)
	case len(args) == 2 && args[0] == "stop":
		stop(store, cfg.StateDir, get(store, args[1]))
	case len(args) == 2 && args[0] == "start":
		start(store, get(store, args[1]))
	case len(args) == 1 && args[0] == "top":
		top(store)
	case len(args) == 2 && args[0] == "caddy":
		caddy(get(store, args[1]))
	case len(args) == 1 && args[0] == "version":
		fmt.Println(version)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
}

const usage = `usage:
  cdctl project add NAME --repo OWNER/REPO [--branch main] [--environment production]
        [--gitlab | --issuer URL] [--type node|static|dockerfile] [--port auto|N] [--domain D]
        [--health /healthz] [--data /app/data] [--env-file F]
        [--cpus 0.5] [--memory 256m] [--pids 128] [--max-bundle-mb 50]
  cdctl project list | show NAME
  cdctl stop NAME | start NAME
  cdctl project rm NAME [--purge]
  cdctl top
  cdctl domain setup --zone example.com --ip 1.2.3.4
  cdctl domain NAME [--yes] | domain rm NAME [--yes]
  cdctl caddy NAME
  cdctl version`

func yesFlag(rest []string) bool {
	if len(rest) == 1 && rest[0] != "--yes" {
		die("unknown flag %s", rest[0])
	}
	return len(rest) == 1
}

func add(cfg project.Server, store project.Store, name string, args []string) {
	fs := flag.NewFlagSet("project add", flag.ExitOnError)
	repo := fs.String("repo", "", "OWNER/REPO (GitHub) or group/project (GitLab)")
	branch := fs.String("branch", "main", "branch allowed to deploy")
	env := fs.String("environment", "", "also require this CI environment (e.g. production)")
	gitlab := fs.Bool("gitlab", false, "the repository is on gitlab.com")
	issuer := fs.String("issuer", "", "OIDC issuer for a self-hosted GitLab, e.g. https://gitlab.example.com")
	typ := fs.String("type", "node", "node | static | dockerfile")
	port := fs.String("port", "auto", "host port on 127.0.0.1, or auto (3100-3999)")
	domain := fs.String("domain", "", "public domain, e.g. app.example.com (DNS and Caddy after cdctl domain setup)")
	yes := fs.Bool("yes", false, "do not ask before creating or deleting DNS records")
	health := fs.String("health", "", "health path (default /healthz, / for static)")
	data := fs.String("data", "", "persistent directory inside the container, e.g. /app/data")
	envFile := fs.String("env-file", "", "runtime env file on the server")
	cpus := fs.String("cpus", "0.5", "CPU limit")
	memory := fs.String("memory", "256m", "memory limit (no swap)")
	pids := fs.Int("pids", 128, "process limit")
	maxMB := fs.Int("max-bundle-mb", 50, "largest accepted bundle")
	fs.Parse(args)
	if *repo == "" {
		die("--repo is required")
	}

	var rule project.Rule
	switch {
	case *issuer != "" || *gitlab:
		iss := *issuer
		if iss == "" {
			iss = project.GitLabIssuer
		}
		rule = project.Rule{"iss": iss, "project_path": *repo, "ref": *branch, "ref_type": "branch", "ref_protected": "true"}
	default:
		rule = project.Rule{"iss": project.GitHubIssuer, "repository": *repo, "ref": "refs/heads/" + *branch}
	}
	if *env != "" {
		rule["environment"] = *env
	}

	p, err := store.Get(name)
	var prev project.Project
	if err != nil {
		p = &project.Project{Name: name}
	} else {
		prev = *p
	}
	p.Allow = []project.Rule{rule}
	p.Type, p.Domain, p.DataMount, p.EnvFile = *typ, *domain, *data, *envFile
	p.CPUs, p.Memory, p.PIDs, p.MaxBundleMB = *cpus, *memory, *pids, *maxMB
	if *health != "" {
		p.Health = *health
	} else if *typ == "static" && p.Health == "" {
		p.Health = "/"
	}
	if *port == "auto" {
		if p.Port == 0 {
			p.Port = freePort(store)
		}
	} else {
		n, err := strconv.Atoi(*port)
		if err != nil {
			die("--port: %v", err)
		}
		p.Port = n
	}
	if prev.Domain != "" && !strings.EqualFold(prev.Domain, p.Domain) {
		disconnect(cfg, store, &prev, *yes, false) // the old name goes before the new one comes
		p.DNSRecordID = ""
	}
	if err := store.Put(p); err != nil {
		die("%v", err)
	}
	fmt.Printf("project %s: %s %s deploys to 127.0.0.1:%d (%s)\n", p.Name, *repo, *branch, p.Port, p.Type)
	switch {
	case p.Domain == "":
	case cfg.DNS != nil:
		connect(cfg, store, p, *yes)
	default:
		fmt.Printf("reverse proxy: cdctl caddy %s (or set up domains: cdctl domain setup)\n", p.Name)
	}
}

func freePort(store project.Store) int {
	used := map[int]bool{}
	if ps, err := store.List(); err == nil {
		for _, p := range ps {
			used[p.Port] = true
		}
	}
	for port := 3100; port <= 3999; port++ {
		if used[port] {
			continue
		}
		l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			continue
		}
		l.Close()
		return port
	}
	die("no free port in 3100-3999")
	return 0
}

func list(store project.Store) {
	ps, err := store.List()
	if err != nil {
		die("%v", err)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATE\tTYPE\tPORT\tDOMAIN\tALLOWED")
	for _, p := range ps {
		var who []string
		for _, r := range p.Allow {
			who = append(who, r["repository"]+r["project_path"]+"@"+strings.TrimPrefix(r["ref"], "refs/heads/"))
		}
		state := strings.TrimSpace(dockerOut("container", "inspect", "-f", "{{.State.Status}}", "cd-"+p.Name))
		if state == "" {
			state = "not deployed"
		}
		if p.Stopped {
			state += ", stopped by owner"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n", p.Name, state, p.Type, p.Port, p.Domain, strings.Join(who, ", "))
	}
	tw.Flush()
}

func caddy(p *project.Project) {
	if p.Domain == "" {
		die("project %s has no --domain", p.Name)
	}
	fmt.Print(proxy.Site(p.Name, p.Domain, p.Port))
}

// stop takes the app off the air at once and keeps it off: the container stops (and stays
// stopped across reboots), and the repository can no longer deploy or roll back.
func stop(store project.Store, stateDir string, p *project.Project) {
	p.Stopped = true
	if err := store.Put(p); err != nil {
		die("%v", err)
	}
	if err := docker("stop", "--time", "10", "cd-"+p.Name); err != nil {
		fmt.Printf("no running container cd-%s\n", p.Name)
	}
	fmt.Printf("%s is stopped; its deploys are refused. To bring it back: cdctl start %s\n", p.Name, p.Name)
	if building(stateDir, p) {
		fmt.Printf("a deploy of %s was in progress: run cdctl stop %s again when it ends\n", p.Name, p.Name)
	}
}

func start(store project.Store, p *project.Project) {
	p.Stopped = false
	if err := store.Put(p); err != nil {
		die("%v", err)
	}
	if err := docker("start", "cd-"+p.Name); err != nil {
		fmt.Printf("%s may deploy again; it has no container yet, the next deploy starts it\n", p.Name)
		return
	}
	fmt.Printf("%s is running and may deploy again\n", p.Name)
}

// remove takes the app down for good: container, network, images, release record and the
// permission to deploy, its name (Caddy site; the DNS record if cd created it, after asking).
// Its data stays unless purge.
func remove(cfg project.Server, store project.Store, p *project.Project, purge bool) {
	stateDir := cfg.StateDir
	disconnect(cfg, store, p, false, false)
	c := "cd-" + p.Name
	_ = docker("rm", "-f", c, c+"-old")
	_ = docker("network", "rm", c)
	for _, img := range strings.Fields(dockerOut("images", "-q", c)) {
		_ = docker("rmi", "-f", img)
	}
	dir := filepath.Join(stateDir, "projects", p.Name)
	if purge {
		_ = os.RemoveAll(dir)
	} else {
		_ = os.Remove(filepath.Join(dir, "release.json"))
	}
	if err := store.Delete(p.Name); err != nil {
		die("%v", err)
	}
	fmt.Printf("removed %s: container, images, permission to deploy\n", p.Name)
	if purge {
		fmt.Printf("its data is deleted (%s)\n", dir)
	} else {
		fmt.Printf("its data is kept in %s\n", filepath.Join(dir, "data"))
	}
	if p.Domain != "" && cfg.DNS == nil {
		fmt.Printf("%s still points here: remove its block from the reverse proxy\n", p.Domain)
	}
}

// top shows what every app and the builder use right now.
func top(store project.Store) {
	ps, err := store.List()
	if err != nil {
		die("%v", err)
	}
	names := []string{}
	for _, p := range ps {
		if running("cd-" + p.Name) {
			names = append(names, "cd-"+p.Name)
		}
	}
	if running("buildx_buildkit_cd-builder0") {
		names = append(names, "buildx_buildkit_cd-builder0")
	}
	if len(names) == 0 {
		fmt.Println("nothing is running")
		return
	}
	args := append([]string{"stats", "--no-stream", "--format",
		"table {{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.MemPerc}}\t{{.PIDs}}"}, names...)
	if err := docker(args...); err != nil {
		die("docker stats: %v", err)
	}
	fmt.Println("CPU % is of one core (50% = half a core); an app cannot go over its --cpus")
}

func running(c string) bool {
	return strings.TrimSpace(dockerOut("container", "inspect", "-f", "{{.State.Running}}", c)) == "true"
}

func building(stateDir string, p *project.Project) bool {
	m, _ := filepath.Glob(filepath.Join(stateDir, "projects", p.Name, "build-*"))
	return len(m) > 0
}

func docker(args ...string) error {
	cmd := exec.Command("docker", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func dockerOut(args ...string) string {
	b, _ := exec.Command("docker", args...).Output()
	return string(b)
}

func get(store project.Store, name string) *project.Project {
	p, err := store.Get(name)
	if err != nil {
		die("project %s: %v", name, err)
	}
	return p
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "cdctl: "+format+"\n", a...)
	os.Exit(1)
}
