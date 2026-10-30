package proxy

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fake records the commands; validation fails while a site file contains "BROKEN".
func fake(t *testing.T) (Caddy, *[]string) {
	t.Helper()
	dir := t.TempDir()
	cf := filepath.Join(dir, "Caddyfile")
	os.WriteFile(cf, []byte("shop.example.com {\n\trespond ok\n}\n"), 0o640)
	var ran []string
	c := Caddy{Caddyfile: cf, SitesDir: filepath.Join(dir, "cd")}
	c.Run = func(name string, args ...string) error {
		ran = append(ran, name+" "+strings.Join(args, " "))
		if name == "caddy" {
			m, _ := filepath.Glob(filepath.Join(c.SitesDir, "*.caddy"))
			for _, f := range m {
				if b, _ := os.ReadFile(f); strings.Contains(string(b), "BROKEN") {
					return errors.New("invalid")
				}
			}
		}
		return nil
	}
	return c, &ran
}

func TestCaddyLifecycle(t *testing.T) {
	c, ran := fake(t)
	if err := c.Put("app", Site("app", "app.example.com", 3100)); err == nil {
		t.Fatal("Put before the import line must fail")
	}
	backup, err := c.EnsureImport()
	if err != nil || backup == "" {
		t.Fatalf("EnsureImport: %q %v", backup, err)
	}
	b, _ := os.ReadFile(c.Caddyfile)
	if !strings.HasPrefix(string(b), "shop.example.com {") || !strings.Contains(string(b), "import "+filepath.Join(c.SitesDir, "*.caddy")) {
		t.Fatalf("Caddyfile: %q", b)
	}
	if st, _ := os.Stat(c.Caddyfile); runtime.GOOS != "windows" && st.Mode().Perm() != 0o640 {
		t.Fatalf("mode changed to %v", st.Mode().Perm())
	}
	if again, err := c.EnsureImport(); err != nil || again != "" {
		t.Fatalf("second EnsureImport changed the file: %q %v", again, err)
	}

	if err := c.Put("app", Site("app", "app.example.com", 3100)); err != nil || !c.Has("app") {
		t.Fatalf("Put: %v", err)
	}
	if err := c.Put("app", "BROKEN"); err == nil {
		t.Fatal("a site that does not validate was accepted")
	}
	if b, _ := os.ReadFile(c.path("app")); !strings.Contains(string(b), "reverse_proxy 127.0.0.1:3100") {
		t.Fatalf("the previous site was not restored: %q", b)
	}
	if err := c.Put("new", "BROKEN"); err == nil || c.Has("new") {
		t.Fatal("a new broken site must be removed")
	}

	reloads := 0
	for _, r := range *ran {
		if strings.HasPrefix(r, "systemctl reload caddy") {
			reloads++
		}
	}
	if reloads != 2 { // the import and the good site; never after a failed validation
		t.Fatalf("reloads: %d (%v)", reloads, *ran)
	}
	if err := c.Remove("app"); err != nil || c.Has("app") {
		t.Fatalf("Remove: %v", err)
	}
	if err := c.Remove("app"); err != nil {
		t.Fatalf("Remove twice: %v", err)
	}
}
