package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Dmitry-Fedotov-Dev/cd/internal/dns"
	"github.com/Dmitry-Fedotov-Dev/cd/internal/project"
	"github.com/Dmitry-Fedotov-Dev/cd/internal/proxy"
)

var stdin = bufio.NewReader(os.Stdin)

// confirm asks a yes/no question; anything but yes (or no terminal answer) is no.
func confirm(yes bool, format string, a ...any) bool {
	q := fmt.Sprintf(format, a...)
	if yes {
		fmt.Println(q + " yes (--yes)")
		return true
	}
	fmt.Print(q + " [y/N] ")
	line, _ := stdin.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes", "д", "да":
		return true
	}
	return false
}

func readSecret(prompt string) string {
	fmt.Print(prompt)
	stty := func(arg string) {
		c := exec.Command("stty", arg)
		c.Stdin = os.Stdin
		_ = c.Run()
	}
	stty("-echo")
	defer func() { stty("echo"); fmt.Println() }()
	line, _ := stdin.ReadString('\n')
	return strings.TrimSpace(line)
}

func caddyOf(cfg project.Server) proxy.Caddy {
	return proxy.Caddy{Caddyfile: cfg.Proxy.Caddyfile, SitesDir: cfg.Proxy.SitesDir}
}

func porkbun(cfg project.Server) *dns.Porkbun {
	keys, err := dns.LoadKeys(cfg.DNS.KeysFile)
	if err != nil {
		die("DNS keys: %v (cdctl domain setup)", err)
	}
	return &dns.Porkbun{Keys: keys}
}

// domainSetup: cdctl domain setup --zone example.com --ip 1.2.3.4
func domainSetup(configPath string, cfg project.Server, args []string) {
	fs := flag.NewFlagSet("domain setup", flag.ExitOnError)
	zone := fs.String("zone", "", "the zone projects get names in, e.g. example.com")
	ip := fs.String("ip", "", "this server's public IPv4 address")
	keysFile := fs.String("keys", "/etc/cdd/porkbun.json", "where to keep the Porkbun API keys (root only)")
	fs.Parse(args)
	if *zone == "" || net.ParseIP(*ip).To4() == nil {
		die("usage: cdctl domain setup --zone example.com --ip 1.2.3.4")
	}
	*zone = strings.ToLower(strings.TrimSuffix(*zone, "."))

	keys, err := dns.LoadKeys(*keysFile)
	if err != nil || confirm(false, "Porkbun keys are already saved in %s. Replace them?", *keysFile) {
		fmt.Println("Porkbun → Account → API Access: create a key (and turn on API Access for", *zone+").")
		keys = dns.Keys{APIKey: readSecret("API key (pk1_…, not shown): "), SecretKey: readSecret("Secret API key (sk1_…, not shown): ")}
	}
	pb := &dns.Porkbun{Keys: keys}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	recs, err := pb.Records(ctx, *zone)
	if err != nil {
		die("the keys do not work for %s: %v", *zone, err)
	}
	fmt.Printf("keys work: %s has %d records\n", *zone, len(recs))
	if err := dns.SaveKeys(*keysFile, keys); err != nil {
		die("%v", err)
	}

	cfg.DNS = &project.DNS{Provider: "porkbun", Zone: *zone, IP: *ip, KeysFile: *keysFile}
	if err := project.SaveServer(configPath, cfg); err != nil {
		die("%v", err)
	}
	fmt.Printf("projects get names under %s, pointing to %s\n", *zone, *ip)

	backup, err := caddyOf(cfg).EnsureImport()
	switch {
	case err != nil:
		die("caddy: %v", err)
	case backup != "":
		fmt.Printf("Caddy now imports %s/*.caddy (backup of the Caddyfile: %s)\n", cfg.Proxy.SitesDir, backup)
	default:
		fmt.Printf("Caddy already imports %s/*.caddy\n", cfg.Proxy.SitesDir)
	}
}

// connect gives a project its public name: the A record (after asking), the Caddy site, HTTPS.
func connect(cfg project.Server, store project.Store, p *project.Project, yes bool) {
	if p.Domain == "" {
		die("project %s has no domain: cdctl project add %s … --domain NAME.%s", p.Name, p.Name, zoneHint(cfg))
	}
	if cfg.DNS == nil {
		die("domains are not set up on this server: cdctl domain setup (or put cdctl caddy %s into your proxy by hand)", p.Name)
	}
	sub, err := dns.Sub(p.Domain, cfg.DNS.Zone)
	if err != nil {
		die("%v", err)
	}
	if ps, err := store.List(); err == nil {
		for _, o := range ps {
			if o.Name != p.Name && strings.EqualFold(o.Domain, p.Domain) {
				die("%s is already the domain of project %s", p.Domain, o.Name)
			}
		}
	}

	pb := porkbun(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	recs, err := pb.Records(ctx, cfg.DNS.Zone)
	if err != nil {
		die("%v", err)
	}
	action, err := dns.Plan(recs, p.Domain, cfg.DNS.IP)
	if err != nil {
		die("%v", err)
	}
	switch action {
	case "exists":
		if p.DNSRecordID != "" {
			fmt.Printf("DNS: %s → %s (created by cd)\n", p.Domain, cfg.DNS.IP)
		} else {
			fmt.Printf("DNS: %s → %s already exists (made by hand: cd will never delete it)\n", p.Domain, cfg.DNS.IP)
		}
	case "create":
		if !confirm(yes, "Create the DNS record %s → %s in Porkbun?", p.Domain, cfg.DNS.IP) {
			die("nothing changed")
		}
		id, err := pb.CreateA(ctx, cfg.DNS.Zone, sub, cfg.DNS.IP, "cd:"+p.Name)
		if err != nil {
			die("%v", err)
		}
		p.DNSRecordID = id
		if err := store.Put(p); err != nil {
			die("the record %s was created but not saved in the project (%v): delete it in Porkbun by hand", id, err)
		}
		fmt.Printf("DNS: created %s → %s\n", p.Domain, cfg.DNS.IP)
	}

	if err := caddyOf(cfg).Put(p.Name, proxy.Site(p.Name, p.Domain, p.Port)); err != nil {
		die("caddy: %v", err)
	}
	fmt.Printf("Caddy: %s → 127.0.0.1:%d\n", p.Domain, p.Port)
	waitHTTPS(p.Domain, 90*time.Second)
}

// disconnect takes the name away: the Caddy site, and the A record if cd created it.
func disconnect(cfg project.Server, store project.Store, p *project.Project, yes, keepProject bool) {
	c := caddyOf(cfg)
	if c.Has(p.Name) {
		if err := c.Remove(p.Name); err != nil {
			die("caddy: %v", err)
		}
		fmt.Printf("Caddy: %s removed\n", p.Domain)
	}
	if p.DNSRecordID == "" {
		if p.Domain != "" {
			fmt.Printf("DNS: %s was not created by cd; left as it is\n", p.Domain)
		}
		return
	}
	if cfg.DNS == nil {
		fmt.Printf("DNS: the record of %s (id %s) stays: domains are not set up here\n", p.Domain, p.DNSRecordID)
		return
	}
	if !confirm(yes, "Delete the DNS record %s (created by cd)?", p.Domain) {
		fmt.Printf("DNS: %s kept\n", p.Domain)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := porkbun(cfg).Delete(ctx, cfg.DNS.Zone, p.DNSRecordID); err != nil {
		die("DNS: %v", err)
	}
	fmt.Printf("DNS: %s deleted\n", p.Domain)
	p.DNSRecordID = ""
	if keepProject {
		if err := store.Put(p); err != nil {
			die("%v", err)
		}
	}
}

// waitHTTPS waits until the domain answers over HTTPS with a valid certificate.
func waitHTTPS(domain string, limit time.Duration) {
	fmt.Printf("HTTPS: waiting for the certificate of %s", domain)
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{}}}
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		resp, err := c.Get("https://" + domain + "/")
		if err == nil {
			resp.Body.Close()
			fmt.Printf(" ok (HTTP %d)\n", resp.StatusCode)
			if resp.StatusCode == http.StatusBadGateway {
				fmt.Println("  502: the address works, the app is not running yet — its first deploy starts it")
			}
			return
		}
		fmt.Print(".")
		time.Sleep(5 * time.Second)
	}
	fmt.Printf("\n  not yet: DNS may take a few minutes. Check later: curl -sI https://%s\n", domain)
}

func zoneHint(cfg project.Server) string {
	if cfg.DNS != nil {
		return cfg.DNS.Zone
	}
	return "example.com"
}
