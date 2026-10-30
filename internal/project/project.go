// Package project holds the server config and the per-project files the owner manages with
// cdctl: who may deploy (claim rules), how to build (type) and how to run (port, limits).
package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Dmitry-Fedotov-Dev/cd/internal/oidc"
)

// Server is /etc/cdd/cdd.json.
type Server struct {
	Listen         string   `json:"listen"`            // 127.0.0.1:8790 — behind the host's reverse proxy
	PublicURL      string   `json:"public_url"`        // https://deploy.example.com — the token audience
	Issuers        []string `json:"issuers"`           // allowed OIDC issuers
	ProjectsDir    string   `json:"projects_dir"`      // /etc/cdd/projects
	StateDir       string   `json:"state_dir"`         // /var/lib/cdd
	BuildMinFreeMB int      `json:"build_min_free_mb"` // refuse to build below this much free RAM

	// Builds run the project's own code (npm install scripts, its build) — in a separate
	// BuildKit container with these limits, so a build cannot starve the host.
	// An empty build_memory builds with the plain docker builder, without limits.
	BuildMemory     string `json:"build_memory"`      // 1g
	BuildCPUs       string `json:"build_cpus"`        // 1
	BuildTimeoutMin int    `json:"build_timeout_min"` // 15
}

const (
	GitHubIssuer = "https://token.actions.githubusercontent.com"
	GitLabIssuer = "https://gitlab.com"
)

func DefaultServer() Server {
	return Server{
		Listen:         "127.0.0.1:8790",
		Issuers:        []string{GitHubIssuer, GitLabIssuer},
		ProjectsDir:    "/etc/cdd/projects",
		StateDir:       "/var/lib/cdd",
		BuildMinFreeMB: 700,

		BuildMemory:     "1g",
		BuildCPUs:       "1",
		BuildTimeoutMin: 15,
	}
}

func LoadServer(path string) (Server, error) {
	s := DefaultServer()
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	if s.PublicURL == "" {
		return s, fmt.Errorf("%s: public_url is required", path)
	}
	s.PublicURL = strings.TrimRight(s.PublicURL, "/")
	return s, nil
}

// Rule is one set of claims that must all match exactly, e.g.
// {"iss": GitHubIssuer, "repository": "me/app", "ref": "refs/heads/main"}.
type Rule map[string]string

// Project is /etc/cdd/projects/<name>.json.
type Project struct {
	Name   string `json:"name"`
	Allow  []Rule `json:"allow"`
	Type   string `json:"type"`             // node | static | dockerfile
	Port   int    `json:"port"`             // host port on 127.0.0.1
	Domain string `json:"domain,omitempty"` // informational: what the reverse proxy serves

	Health      string `json:"health"`               // health path, e.g. /healthz
	HealthSec   int    `json:"health_timeout_sec"`   // how long a new release may take to get healthy
	Node        string `json:"node,omitempty"`       // node image tag for type node/static builds
	StaticDir   string `json:"static_dir,omitempty"` // type static: build output to serve (dist)
	Dockerfile  string `json:"dockerfile,omitempty"` // type dockerfile: path inside the bundle
	DataMount   string `json:"data_mount,omitempty"` // persistent dir inside the container, e.g. /app/data
	EnvFile     string `json:"env_file,omitempty"`   // runtime env, kept on the server
	User        string `json:"user,omitempty"`       // container user; default 10001:10001
	MaxBundleMB int    `json:"max_bundle_mb"`

	// Stopped is set by `cdctl stop`: the container is stopped and the repository may not
	// deploy or roll back until `cdctl start`.
	Stopped bool `json:"stopped,omitempty"`

	CPUs   string `json:"cpus"`
	Memory string `json:"memory"`
	PIDs   int    `json:"pids"`
	TmpMB  int    `json:"tmp_mb"`
}

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,40}$`)

// ValidName reports whether s can be a project name (used in container and file names).
func ValidName(s string) bool { return nameRe.MatchString(s) }

func (p *Project) applyDefaults() {
	if p.Health == "" {
		p.Health = "/healthz"
	}
	if p.HealthSec == 0 {
		p.HealthSec = 90
	}
	if p.Node == "" {
		p.Node = "22-alpine"
	}
	if p.StaticDir == "" {
		p.StaticDir = "dist"
	}
	if p.Dockerfile == "" {
		p.Dockerfile = "Dockerfile"
	}
	if p.User == "" {
		p.User = "10001:10001"
	}
	if p.MaxBundleMB == 0 {
		p.MaxBundleMB = 50
	}
	if p.CPUs == "" {
		p.CPUs = "0.5"
	}
	if p.Memory == "" {
		p.Memory = "256m"
	}
	if p.PIDs == 0 {
		p.PIDs = 128
	}
	if p.TmpMB == 0 {
		p.TmpMB = 64
	}
}

func (p *Project) Validate() error {
	switch {
	case !ValidName(p.Name):
		return fmt.Errorf("bad project name %q (a-z, 0-9, -)", p.Name)
	case len(p.Allow) == 0:
		return errors.New("allow: at least one rule is required")
	case p.Port < 1024 || p.Port > 65535:
		return fmt.Errorf("port %d out of range", p.Port)
	case p.Type != "node" && p.Type != "static" && p.Type != "dockerfile":
		return fmt.Errorf("type %q: want node, static or dockerfile", p.Type)
	case !strings.HasPrefix(p.Health, "/"):
		return fmt.Errorf("health %q must start with /", p.Health)
	}
	for i, r := range p.Allow {
		if r["iss"] == "" {
			return fmt.Errorf("allow[%d]: iss is required", i)
		}
		if len(r) < 2 {
			return fmt.Errorf("allow[%d]: needs at least one claim besides iss (e.g. repository)", i)
		}
	}
	if p.DataMount != "" && !strings.HasPrefix(p.DataMount, "/") {
		return fmt.Errorf("data_mount %q must be absolute", p.DataMount)
	}
	return nil
}

// Allows reports whether the token's claims satisfy at least one rule.
func (p *Project) Allows(c oidc.Claims) bool {
	for _, r := range p.Allow {
		ok := true
		for k, want := range r {
			if c.String(k) != want {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// Store reads and writes project files in one directory.
type Store struct{ Dir string }

func (s Store) path(name string) string { return filepath.Join(s.Dir, name+".json") }

func (s Store) Get(name string) (*Project, error) {
	if !ValidName(name) {
		return nil, os.ErrNotExist
	}
	b, err := os.ReadFile(s.path(name))
	if err != nil {
		return nil, err
	}
	var p Project
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path(name), err)
	}
	p.applyDefaults()
	if p.Name != name {
		return nil, fmt.Errorf("%s: name %q does not match file name", s.path(name), p.Name)
	}
	return &p, p.Validate()
}

func (s Store) Put(p *Project) error {
	p.applyDefaults()
	if err := p.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o750); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(p.Name) + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(p.Name))
}

func (s Store) Delete(name string) error {
	if !ValidName(name) {
		return os.ErrNotExist
	}
	return os.Remove(s.path(name))
}

func (s Store) List() ([]*Project, error) {
	des, err := os.ReadDir(s.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Project
	for _, de := range des {
		name, ok := strings.CutSuffix(de.Name(), ".json")
		if !ok || de.IsDir() {
			continue
		}
		p, err := s.Get(name)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
