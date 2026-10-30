// Package proxy publishes cd's projects through the host's Caddy: one site file per project in
// its own directory, imported once from the main Caddyfile. Every change is validated before
// Caddy reloads, and undone if it does not validate; the rest of the Caddyfile is never edited
// except to add that one import line (after a backup).
package proxy

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Caddy struct {
	Caddyfile string // /etc/caddy/Caddyfile
	SitesDir  string // /etc/caddy/cd

	// Run executes a command (caddy validate, the reload); exec by default, replaced in tests.
	Run func(name string, args ...string) error
}

// Site is the site block for a project.
func Site(name, domain string, port int) string {
	return fmt.Sprintf(`# cd project %[1]s (managed by cdctl)
%[2]s {
	encode zstd gzip
	request_body {
		max_size 10MB
	}
	reverse_proxy 127.0.0.1:%[3]d
}
`, name, domain, port)
}

func (c Caddy) importLine() string { return "import " + filepath.Join(c.SitesDir, "*.caddy") }

func (c Caddy) run(name string, args ...string) error {
	if c.Run != nil {
		return c.Run(name, args...)
	}
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		return fmt.Errorf("%s: %s", name, lines[len(lines)-1])
	}
	return nil
}

func (c Caddy) validate() error {
	return c.run("caddy", "validate", "--config", c.Caddyfile, "--adapter", "caddyfile")
}

func (c Caddy) reload() error { return c.run("systemctl", "reload", "caddy") }

// Imported tells whether the main Caddyfile already imports the sites directory.
func (c Caddy) Imported() (bool, error) {
	b, err := os.ReadFile(c.Caddyfile)
	if err != nil {
		return false, err
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) == c.importLine() {
			return true, nil
		}
	}
	return false, nil
}

// EnsureImport adds the import line once, after backing the Caddyfile up. Returns the backup
// path when it changed the file.
func (c Caddy) EnsureImport() (backup string, err error) {
	if ok, err := c.Imported(); err != nil || ok {
		return "", err
	}
	if err := os.MkdirAll(c.SitesDir, 0o755); err != nil {
		return "", err
	}
	orig, err := os.ReadFile(c.Caddyfile)
	if err != nil {
		return "", err
	}
	backup = c.Caddyfile + ".bak-cd-" + time.Now().Format("20060102-150405")
	if err := os.WriteFile(backup, orig, 0o644); err != nil {
		return "", err
	}
	add := "\n# cd projects (cdctl)\n" + c.importLine() + "\n"
	if err := writeKeepMode(c.Caddyfile, append(orig, []byte(add)...)); err != nil {
		return "", err
	}
	if err := c.validate(); err != nil {
		_ = writeKeepMode(c.Caddyfile, orig)
		return "", fmt.Errorf("the Caddyfile with the import does not validate, left as it was: %w", err)
	}
	return backup, c.reload()
}

func (c Caddy) path(name string) string { return filepath.Join(c.SitesDir, name+".caddy") }

// Has tells whether the project has a site file.
func (c Caddy) Has(name string) bool {
	_, err := os.Stat(c.path(name))
	return err == nil
}

// Put writes the project's site file, validates and reloads; on a validation error the
// previous file (or none) is put back and Caddy is not reloaded.
func (c Caddy) Put(name, site string) error {
	if ok, err := c.Imported(); err != nil || !ok {
		return errors.New("the Caddyfile does not import " + c.SitesDir + ": run cdctl domain setup")
	}
	p := c.path(name)
	old, readErr := os.ReadFile(p)
	if err := os.WriteFile(p, []byte(site), 0o644); err != nil {
		return err
	}
	if err := c.validate(); err != nil {
		if readErr == nil {
			_ = os.WriteFile(p, old, 0o644)
		} else {
			_ = os.Remove(p)
		}
		return fmt.Errorf("the site does not validate, nothing changed: %w", err)
	}
	return c.reload()
}

// Remove deletes the project's site file and reloads.
func (c Caddy) Remove(name string) error {
	p := c.path(name)
	old, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		return err
	}
	if err := c.validate(); err != nil {
		_ = os.WriteFile(p, old, 0o644)
		return fmt.Errorf("Caddy does not validate without the site, put it back: %w", err)
	}
	return c.reload()
}

func writeKeepMode(path string, b []byte) error {
	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	return os.WriteFile(path, b, mode)
}
