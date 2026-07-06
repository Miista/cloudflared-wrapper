package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

type config struct {
	Tunnel  string `yaml:"tunnel"`
	Ingress []struct {
		Hostname string `yaml:"hostname"`
	} `yaml:"ingress"`
}

type dnsRecord struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Content string `json:"content"`
}

type apiResponse struct {
	Success bool        `json:"success"`
	Result  []dnsRecord `json:"result"`
	Errors  []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type apiStatus struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
}

const errCodeRecordAlreadyExists = 81053

// cfBase is the Cloudflare API base URL. Overridden in tests to point at a fake server.
var cfBase = "%s"

func hasErrorCode(resp []byte, code int) bool {
	var s apiStatus
	if json.Unmarshal(resp, &s) != nil {
		return false
	}
	for _, e := range s.Errors {
		if e.Code == code {
			return true
		}
	}
	return false
}

func checkStatus(resp []byte) error {
	var s apiStatus
	if err := json.Unmarshal(resp, &s); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	if !s.Success {
		return fmt.Errorf("api error: %v", s.Errors)
	}
	return nil
}

type createPayload struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl"`
}

func main() {
	// Feature 0 — drop-in passthrough. If the user already gave cloudflared what
	// it needs directly, forward it untouched and skip all wrapper logic. This
	// is what makes the image a true drop-in replacement: an explicit command
	// wins over everything, and a remote-managed TUNNEL_TOKEN runs as-is.
	if len(os.Args) > 1 {
		fmt.Println("[entrypoint] Passthrough: forwarding command to cloudflared")
		execPassthrough(os.Args[1:])
	}
	if os.Getenv("TUNNEL_TOKEN") != "" {
		fmt.Println("[entrypoint] Passthrough: TUNNEL_TOKEN set, running token tunnel")
		execPassthrough([]string{"tunnel", "run"})
	}

	configPath := envOr("CONFIG_PATH", "/etc/cloudflared/config.yml")
	credsDir := envOr("CREDENTIALS_DIR", "/var/lib/cloudflared")
	credsPath := credsDir + "/credentials.json"
	tunnelName := os.Getenv("TUNNEL_NAME")
	apiToken := os.Getenv("CF_API_TOKEN")
	accountID := os.Getenv("CF_ACCOUNT_ID")
	zoneID := os.Getenv("CF_ZONE_ID")
	mode := envOr("MODE", "incremental")

	tunnelID := ""
	if tunnelName != "" && apiToken != "" && accountID != "" {
		id, err := ensureTunnel(apiToken, accountID, tunnelName, credsPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[tunnel] Ensure failed: %v\n", err)
			os.Exit(1)
		}
		tunnelID = id
	}

	// Feature 1 — discover ingress from Docker labels. Activated purely by the
	// socket being mounted; independent of any Cloudflare credentials. Three
	// outcomes: socket absent (feature off), socket read OK, socket read failed.
	var discovered []ingressRule
	socketPresent := socketAvailable()
	discoverOK := false
	if socketPresent {
		containers, err := getContainers()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[discover] WARN: socket present but unreadable: %v\n", err)
		} else {
			discovered = discoverIngress(containers)
			discoverOK = true
		}
	}

	// Build the config cloudflared runs. Generate a merged config when we have
	// label rules to add, or when the base config.yml is missing but we know the
	// tunnel id (auto mode) and must synthesize at least a catch-all so
	// cloudflared can start. Otherwise run the user's config.yml untouched.
	effectiveConfigPath := configPath
	if len(discovered) > 0 || (!fileExists(configPath) && tunnelID != "") {
		merged, err := writeMergedConfig(configPath, "/tmp/config.yml", discovered)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[discover] WARN: config generation failed, using base config: %v\n", err)
		} else {
			effectiveConfigPath = merged
			if len(discovered) > 0 {
				fmt.Printf("[discover] merged %d label-discovered rule(s) into %s\n", len(discovered), merged)
			} else {
				fmt.Printf("[entrypoint] no config.yml found; generated minimal config at %s\n", merged)
			}
		}
	}

	zoneIDs := parseZoneIDs(zoneID)

	if apiToken == "" || len(zoneIDs) == 0 {
		fmt.Println("[sync] Skipped (CF_API_TOKEN/CF_ZONE_ID not set)")
		execCloudflared(effectiveConfigPath, tunnelID, credsPath)
	}

	cfg, err := parseConfig(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			// No base config — fine in auto mode; ingress comes from labels.
			cfg = &config{}
		} else {
			fmt.Fprintf(os.Stderr, "[sync] WARN: Failed to parse config: %v\n", err)
			execCloudflared(effectiveConfigPath, tunnelID, credsPath)
		}
	}

	// Guardrail: in complete mode we delete CNAMEs not in the desired set. If
	// the socket is mounted but we couldn't read it, the desired set is missing
	// the label-discovered hosts — deleting against it could wipe live records.
	// Fall back to incremental for this run. (Socket simply absent is fine —
	// the user opted out of feature 1, so complete is trustworthy.)
	if mode == "complete" && socketPresent && !discoverOK {
		fmt.Fprintln(os.Stderr, "[sync] WARN: socket unreadable; downgrading complete->incremental to avoid deleting records")
		mode = "incremental"
	}

	syncTunnelID := tunnelID
	if syncTunnelID == "" {
		syncTunnelID = cfg.Tunnel
	}
	target := syncTunnelID + ".cfargotunnel.com"
	desired := desiredHostnames(cfg)
	for _, r := range discovered {
		desired[r.Hostname] = true
	}

	// Group hostnames by zone ID. Each hostname's apex is looked up in the
	// zone list; hostnames with no matching zone are explicitly skipped.
	byZone := make(map[string]map[string]bool)
	for _, id := range zoneIDs {
		byZone[id] = make(map[string]bool)
	}
	for host := range desired {
		id, ok := matchZone(host, zoneIDs, apiToken)
		if !ok {
			fmt.Printf("[sync] SKIP    %s (apex not in CF_ZONE_ID list)\n", host)
			continue
		}
		byZone[id][host] = true
	}

	fmt.Printf("[sync] tunnel=%s mode=%s hostnames=%d zones=%d\n", syncTunnelID, mode, len(desired), len(zoneIDs))

	start := time.Now()
	var syncErr error
	for _, id := range zoneIDs {
		hosts := byZone[id]
		if err := sync(apiToken, id, target, hosts, mode); err != nil {
			fmt.Fprintf(os.Stderr, "[sync] WARN: DNS sync failed for zone %s: %v\n", id, err)
			syncErr = err
		}
	}
	if syncErr == nil {
		fmt.Printf("[sync] DNS sync OK in %s\n", time.Since(start).Round(time.Millisecond))
	} else {
		fmt.Fprintf(os.Stderr, "[sync] WARN: DNS sync completed with errors in %s\n", time.Since(start).Round(time.Millisecond))
	}

	execCloudflared(effectiveConfigPath, tunnelID, credsPath)
}

type credentials struct {
	AccountTag   string `json:"AccountTag"`
	TunnelID     string `json:"TunnelID"`
	TunnelName   string `json:"TunnelName"`
	TunnelSecret string `json:"TunnelSecret"`
}

func ensureTunnel(token, accountID, name, credsPath string) (string, error) {
	if data, err := os.ReadFile(credsPath); err == nil {
		var c credentials
		if err := json.Unmarshal(data, &c); err == nil && c.TunnelID != "" {
			if c.TunnelName != name {
				fmt.Printf("[tunnel] credentials.json is for tunnel %q but TUNNEL_NAME=%q; switching\n", c.TunnelName, name)
			} else if !tunnelExists(token, accountID, c.TunnelID) {
				fmt.Printf("[tunnel] credentials.json tunnel id=%s no longer exists; recreating\n", c.TunnelID)
			} else {
				fmt.Printf("[tunnel] Using existing credentials.json tunnel=%s\n", c.TunnelID)
				return c.TunnelID, nil
			}
		}
	}

	id, accountTag, err := lookupTunnel(token, accountID, name)
	if err != nil {
		return "", fmt.Errorf("lookup: %w", err)
	}

	if id != "" {
		fmt.Printf("[tunnel] Adopting existing tunnel name=%s id=%s\n", name, id)
		secret, err := fetchTunnelSecret(token, accountID, id)
		if err != nil {
			return "", fmt.Errorf("fetch token: %w", err)
		}
		if err := writeCredentials(credsPath, credentials{accountTag, id, name, secret}); err != nil {
			return "", err
		}
		return id, nil
	}

	fmt.Printf("[tunnel] Creating new tunnel name=%s\n", name)
	id, accountTag, secret, err := createTunnel(token, accountID, name)
	if err != nil {
		return "", fmt.Errorf("create: %w", err)
	}
	if err := writeCredentials(credsPath, credentials{accountTag, id, name, secret}); err != nil {
		return "", err
	}
	fmt.Printf("[tunnel] Created tunnel id=%s\n", id)
	return id, nil
}

func tunnelExists(token, accountID, tunnelID string) bool {
	url := fmt.Sprintf("%s/client/v4/accounts/%s/cfd_tunnel/%s", cfBase, accountID, tunnelID)
	resp, err := cfRequest("GET", url, token, nil)
	if err != nil {
		return false
	}
	var parsed struct {
		Success bool `json:"success"`
		Result  struct {
			DeletedAt *string `json:"deleted_at"`
		} `json:"result"`
	}
	if json.Unmarshal(resp, &parsed) != nil || !parsed.Success {
		return false
	}
	return parsed.Result.DeletedAt == nil
}

func lookupTunnel(token, accountID, name string) (id, accountTag string, err error) {
	url := fmt.Sprintf("%s/client/v4/accounts/%s/cfd_tunnel?name=%s&is_deleted=false", cfBase, accountID, name)
	resp, err := cfRequest("GET", url, token, nil)
	if err != nil {
		return "", "", err
	}
	var parsed struct {
		Success bool `json:"success"`
		Result  []struct {
			ID         string `json:"id"`
			AccountTag string `json:"account_tag"`
		} `json:"result"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(resp, &parsed); err != nil {
		return "", "", fmt.Errorf("parse response: %w", err)
	}
	if !parsed.Success {
		return "", "", fmt.Errorf("api error: %v", parsed.Errors)
	}
	if len(parsed.Result) == 0 {
		return "", "", nil
	}
	return parsed.Result[0].ID, parsed.Result[0].AccountTag, nil
}

func fetchTunnelSecret(token, accountID, tunnelID string) (string, error) {
	url := fmt.Sprintf("%s/client/v4/accounts/%s/cfd_tunnel/%s/token", cfBase, accountID, tunnelID)
	resp, err := cfRequest("GET", url, token, nil)
	if err != nil {
		return "", err
	}
	var parsed struct {
		Success bool   `json:"success"`
		Result  string `json:"result"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(resp, &parsed); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	if !parsed.Success {
		return "", fmt.Errorf("api error: %v", parsed.Errors)
	}
	decoded, err := base64.StdEncoding.DecodeString(parsed.Result)
	if err != nil {
		return "", fmt.Errorf("decode token: %w", err)
	}
	var t struct {
		S string `json:"s"`
	}
	if err := json.Unmarshal(decoded, &t); err != nil {
		return "", fmt.Errorf("parse token: %w", err)
	}
	if t.S == "" {
		return "", fmt.Errorf("token missing secret")
	}
	return t.S, nil
}

func createTunnel(token, accountID, name string) (id, accountTag, secret string, err error) {
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return "", "", "", err
	}
	secret = base64.StdEncoding.EncodeToString(secretBytes)

	payload, _ := json.Marshal(map[string]string{
		"name":          name,
		"tunnel_secret": secret,
		"config_src":    "local",
	})

	url := fmt.Sprintf("%s/client/v4/accounts/%s/cfd_tunnel", cfBase, accountID)
	resp, err := cfRequest("POST", url, token, payload)
	if err != nil {
		return "", "", "", err
	}
	var parsed struct {
		Success bool `json:"success"`
		Result  struct {
			ID         string `json:"id"`
			AccountTag string `json:"account_tag"`
		} `json:"result"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(resp, &parsed); err != nil {
		return "", "", "", fmt.Errorf("parse response: %w", err)
	}
	if !parsed.Success {
		return "", "", "", fmt.Errorf("api error: %v", parsed.Errors)
	}
	return parsed.Result.ID, parsed.Result.AccountTag, secret, nil
}

func writeCredentials(path string, c credentials) error {
	data, _ := json.MarshalIndent(c, "", "  ")
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// parseZoneIDs splits CF_ZONE_ID on commas and trims whitespace.
func parseZoneIDs(raw string) []string {
	var ids []string
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			ids = append(ids, s)
		}
	}
	return ids
}

// apexDomain returns the last two labels of a hostname (e.g. "links.guldmund.net" → "guldmund.net").
func apexDomain(hostname string) string {
	parts := strings.Split(hostname, ".")
	if len(parts) < 2 {
		return hostname
	}
	return strings.Join(parts[len(parts)-2:], ".")
}

// matchZone returns the zone ID from zoneIDs whose apex matches hostname's apex,
// querying the Cloudflare API for each zone ID's name on demand (cached per call).
// Returns ("", false) if no zone matches.
func matchZone(hostname string, zoneIDs []string, token string) (string, bool) {
	apex := apexDomain(hostname)
	for _, id := range zoneIDs {
		name, err := lookupZoneName(token, id)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[sync] WARN: could not look up zone %s: %v\n", id, err)
			continue
		}
		if name == apex {
			return id, true
		}
	}
	return "", false
}

var zoneNameCache = map[string]string{}

// lookupZoneName fetches the zone name for a given zone ID, with in-process caching.
func lookupZoneName(token, zoneID string) (string, error) {
	if name, ok := zoneNameCache[zoneID]; ok {
		return name, nil
	}
	url := fmt.Sprintf("%s/client/v4/zones/%s", cfBase, zoneID)
	resp, err := cfRequest("GET", url, token, nil)
	if err != nil {
		return "", err
	}
	var parsed struct {
		Success bool `json:"success"`
		Result  struct {
			Name string `json:"name"`
		} `json:"result"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(resp, &parsed); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	if !parsed.Success {
		return "", fmt.Errorf("api error: %v", parsed.Errors)
	}
	zoneNameCache[zoneID] = parsed.Result.Name
	return parsed.Result.Name, nil
}

func sync(token, zoneID, target string, desired map[string]bool, mode string) error {
	existing, err := listRecords(token, zoneID, target)
	if err != nil {
		return fmt.Errorf("list records: %w", err)
	}

	var created, updated, ok, errored int

	for host := range desired {
		if _, exists := existing[host]; exists {
			fmt.Printf("  OK      %s\n", host)
			ok++
			continue
		}
		action, from, err := upsertRecord(token, zoneID, host, target)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ERROR   %s: %v\n", host, err)
			errored++
			continue
		}
		switch action {
		case "create":
			fmt.Printf("  Create  %s\n", host)
			created++
		case "update":
			fmt.Printf("  Update  %s from %s to %s\n", host, from, target)
			updated++
		}
	}

	var deleted int

	if mode == "complete" {
		for host, id := range existing {
			if !desired[host] {
				fmt.Printf("  Delete  %s\n", host)
				if err := deleteRecord(token, zoneID, id); err != nil {
					fmt.Fprintf(os.Stderr, "  ERROR   %s: %v\n", host, err)
					errored++
				} else {
					deleted++
				}
			}
		}
	}

	fmt.Printf("[sync] Summary: ok=%d created=%d updated=%d deleted=%d errors=%d\n", ok, created, updated, deleted, errored)

	if errored > 0 {
		return fmt.Errorf("%d record(s) failed", errored)
	}
	return nil
}

func parseConfig(path string) (*config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func desiredHostnames(cfg *config) map[string]bool {
	hosts := make(map[string]bool)
	for _, ing := range cfg.Ingress {
		if ing.Hostname != "" {
			hosts[ing.Hostname] = true
		}
	}
	return hosts
}

func listRecords(token, zoneID, target string) (map[string]string, error) {
	url := fmt.Sprintf("%s/client/v4/zones/%s/dns_records?type=CNAME&content=%s&per_page=500", cfBase, zoneID, target)

	resp, err := cfRequest("GET", url, token, nil)
	if err != nil {
		return nil, err
	}

	var apiResp apiResponse
	if err := json.Unmarshal(resp, &apiResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if !apiResp.Success {
		return nil, fmt.Errorf("api error: %v", apiResp.Errors)
	}

	records := make(map[string]string)
	for _, r := range apiResp.Result {
		records[r.Name] = r.ID
	}
	return records, nil
}

func upsertRecord(token, zoneID, hostname, target string) (string, string, error) {
	url := fmt.Sprintf("%s/client/v4/zones/%s/dns_records", cfBase, zoneID)

	payload := createPayload{
		Type:    "CNAME",
		Name:    hostname,
		Content: target,
		Proxied: true,
		TTL:     1,
	}
	body, _ := json.Marshal(payload)

	resp, err := cfRequest("POST", url, token, body)
	if err != nil {
		if hasErrorCode(resp, errCodeRecordAlreadyExists) {
			from, err := repointRecord(token, zoneID, hostname, target)
			if err != nil {
				return "", "", err
			}
			return "update", from, nil
		}
		return "", "", err
	}
	if err := checkStatus(resp); err != nil {
		return "", "", err
	}
	return "create", "", nil
}

func repointRecord(token, zoneID, hostname, target string) (string, error) {
	url := fmt.Sprintf("%s/client/v4/zones/%s/dns_records?type=CNAME&name=%s", cfBase, zoneID, hostname)
	resp, err := cfRequest("GET", url, token, nil)
	if err != nil {
		return "", fmt.Errorf("lookup conflict: %w", err)
	}
	var found struct {
		Success bool        `json:"success"`
		Result  []dnsRecord `json:"result"`
	}
	if err := json.Unmarshal(resp, &found); err != nil {
		return "", fmt.Errorf("parse lookup: %w", err)
	}
	if !found.Success || len(found.Result) == 0 {
		return "", fmt.Errorf("conflict record not found for %s", hostname)
	}

	existing := found.Result[0]
	updateURL := fmt.Sprintf("%s/client/v4/zones/%s/dns_records/%s", cfBase, zoneID, existing.ID)
	payload, _ := json.Marshal(createPayload{
		Type:    "CNAME",
		Name:    hostname,
		Content: target,
		Proxied: true,
		TTL:     1,
	})
	upResp, err := cfRequest("PUT", updateURL, token, payload)
	if err != nil {
		return "", fmt.Errorf("repoint: %w", err)
	}
	if err := checkStatus(upResp); err != nil {
		return "", fmt.Errorf("repoint: %w", err)
	}
	return existing.Content, nil
}

func deleteRecord(token, zoneID, recordID string) error {
	url := fmt.Sprintf("%s/client/v4/zones/%s/dns_records/%s", cfBase, zoneID, recordID)

	resp, err := cfRequest("DELETE", url, token, nil)
	if err != nil {
		return err
	}
	return checkStatus(resp)
}

func cfRequest(method, url, token string, body []byte) ([]byte, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return data, fmt.Errorf("http %d: %s", resp.StatusCode, string(data))
	}
	return data, nil
}

func execCloudflared(configPath, tunnelID, credsPath string) {
	bin, err := findBinary("cloudflared")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[entrypoint] cloudflared not found: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("[entrypoint] Launching cloudflared tunnel")
	args := []string{"cloudflared", "tunnel", "--config", configPath}
	if tunnelID != "" {
		args = append(args, "--credentials-file", credsPath, "run", tunnelID)
	} else {
		args = append(args, "run")
	}
	if err := syscall.Exec(bin, args, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "[entrypoint] Exec failed: %v\n", err)
		os.Exit(1)
	}
}

// execPassthrough forwards to cloudflared untouched (Feature 0). It mirrors the
// official image's entrypoint (`cloudflared --no-autoupdate ...`) so a no-change
// image swap behaves identically.
func execPassthrough(args []string) {
	bin, err := findBinary("cloudflared")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[entrypoint] cloudflared not found: %v\n", err)
		os.Exit(1)
	}
	full := append([]string{"cloudflared", "--no-autoupdate"}, args...)
	if err := syscall.Exec(bin, full, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "[entrypoint] Exec failed: %v\n", err)
		os.Exit(1)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func findBinary(name string) (string, error) {
	paths := []string{"/usr/local/bin/" + name, "/usr/bin/" + name, "/bin/" + name}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not in %v", name, paths)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
