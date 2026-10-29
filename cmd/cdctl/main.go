// cdctl manages cdd's projects on the server: who may deploy what, where it runs, its limits.
//
//	cdctl project add NAME --repo OWNER/REPO [--branch main] [--environment production]
//	                      [--gitlab | --issuer URL] [--type node|static|dockerfile] [--port auto|N]
//	                      [--domain D] [--health /healthz] [--data /app/data] [--env-file F]
//	                      [--cpus 0.5] [--memory 256m] [--pids 128] [--max-bundle-mb 50]
//	cdctl project list | show NAME | rm NAME
//	cdctl caddy NAME      print a Caddy site block for the project's domain
//	cdctl version
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/Dmitry-Fedotov-Dev/cd/internal/project"
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
		add(store, args[2], args[3:])
	case len(args) == 2 && args[0] == "project" && args[1] == "list":
		list(store)
	case len(args) == 3 && args[0] == "project" && args[1] == "show":
		p := get(store, args[2])
		b, _ := json.MarshalIndent(p, "", "  ")
		fmt.Println(string(b))
	case len(args) == 3 && args[0] == "project" && args[1] == "rm":
		get(store, args[2])
		if err := store.Delete(args[2]); err != nil {
			die("%v", err)
		}
		fmt.Printf("removed %s from cdd. Its container keeps running: docker rm -f cd-%s\n", args[2], args[2])
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
  cdctl project list | show NAME | rm NAME
  cdctl caddy NAME
  cdctl version`

func add(store project.Store, name string, args []string) {
	fs := flag.NewFlagSet("project add", flag.ExitOnError)
	repo := fs.String("repo", "", "OWNER/REPO (GitHub) or group/project (GitLab)")
	branch := fs.String("branch", "main", "branch allowed to deploy")
	env := fs.String("environment", "", "also require this CI environment (e.g. production)")
	gitlab := fs.Bool("gitlab", false, "the repository is on gitlab.com")
	issuer := fs.String("issuer", "", "OIDC issuer for a self-hosted GitLab, e.g. https://gitlab.example.com")
	typ := fs.String("type", "node", "node | static | dockerfile")
	port := fs.String("port", "auto", "host port on 127.0.0.1, or auto (3100-3999)")
	domain := fs.String("domain", "", "public domain (for cdctl caddy)")
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
	if err != nil {
		p = &project.Project{Name: name}
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
	if err := store.Put(p); err != nil {
		die("%v", err)
	}
	fmt.Printf("project %s: %s %s deploys to 127.0.0.1:%d (%s)\n", p.Name, *repo, *branch, p.Port, p.Type)
	if p.Domain != "" {
		fmt.Printf("reverse proxy: cdctl caddy %s\n", p.Name)
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
	fmt.Fprintln(tw, "NAME\tTYPE\tPORT\tDOMAIN\tALLOWED")
	for _, p := range ps {
		var who []string
		for _, r := range p.Allow {
			who = append(who, r["repository"]+r["project_path"]+"@"+strings.TrimPrefix(r["ref"], "refs/heads/"))
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", p.Name, p.Type, p.Port, p.Domain, strings.Join(who, ", "))
	}
	tw.Flush()
}

func caddy(p *project.Project) {
	if p.Domain == "" {
		die("project %s has no --domain", p.Name)
	}
	fmt.Printf(`# cd project %[1]s
%[2]s {
	encode zstd gzip
	request_body {
		max_size 10MB
	}
	reverse_proxy 127.0.0.1:%[3]d
}
`, p.Name, p.Domain, p.Port)
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
