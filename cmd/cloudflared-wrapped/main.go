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
	ID   string `json:"id"`
	Name string `json:"name"`
}

type apiResponse struct {
	Success bool        `json:"success"`
	Result  []dnsRecord `json:"result"`
	Errors  []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type createPayload struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl"`
}

func main() {
	configPath := envOr("CONFIG_PATH", "/etc/cloudflared/config.yml")
	credsPath := envOr("CREDENTIALS_DIR", "/etc/cloudflared") + "/credentials.json"
	tunnelName := os.Getenv("TUNNEL_NAME")
	apiToken := os.Getenv("CF_API_TOKEN")
	accountID := os.Getenv("CF_ACCOUNT_ID")
	zoneID := os.Getenv("CF_ZONE_ID")
	mode := envOr("MODE", "incremental")

	tunnelID := ""
	if tunnelName != "" && apiToken != "" && accountID != "" {
		id, err := ensureTunnel(apiToken, accountID, tunnelName, credsPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[tunnel] WARN: ensure failed: %v\n", err)
		} else {
			tunnelID = id
		}
	}

	if apiToken == "" || zoneID == "" {
		fmt.Println("[sync] skipped (CF_API_TOKEN/CF_ZONE_ID not set)")
		execCloudflared(configPath, tunnelID, credsPath)
	}

	cfg, err := parseConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[sync] WARN: failed to parse config: %v\n", err)
		execCloudflared(configPath, tunnelID, credsPath)
	}

	syncTunnelID := tunnelID
	if syncTunnelID == "" {
		syncTunnelID = cfg.Tunnel
	}
	target := syncTunnelID + ".cfargotunnel.com"
	desired := desiredHostnames(cfg)

	fmt.Printf("[sync] tunnel=%s mode=%s hostnames=%d\n", syncTunnelID, mode, len(desired))

	start := time.Now()
	if err := sync(apiToken, zoneID, target, desired, mode); err != nil {
		fmt.Fprintf(os.Stderr, "[sync] WARN: dns sync failed in %s: %v\n", time.Since(start).Round(time.Millisecond), err)
	} else {
		fmt.Printf("[sync] dns sync ok in %s\n", time.Since(start).Round(time.Millisecond))
	}

	execCloudflared(configPath, tunnelID, credsPath)
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
			fmt.Printf("[tunnel] using existing credentials.json tunnel=%s\n", c.TunnelID)
			return c.TunnelID, nil
		}
	}

	id, accountTag, err := lookupTunnel(token, accountID, name)
	if err != nil {
		return "", fmt.Errorf("lookup: %w", err)
	}

	if id != "" {
		fmt.Printf("[tunnel] adopting existing tunnel name=%s id=%s\n", name, id)
		secret, err := fetchTunnelSecret(token, accountID, id)
		if err != nil {
			return "", fmt.Errorf("fetch token: %w", err)
		}
		if err := writeCredentials(credsPath, credentials{accountTag, id, name, secret}); err != nil {
			return "", err
		}
		return id, nil
	}

	fmt.Printf("[tunnel] creating new tunnel name=%s\n", name)
	id, accountTag, secret, err := createTunnel(token, accountID, name)
	if err != nil {
		return "", fmt.Errorf("create: %w", err)
	}
	if err := writeCredentials(credsPath, credentials{accountTag, id, name, secret}); err != nil {
		return "", err
	}
	fmt.Printf("[tunnel] created tunnel id=%s\n", id)
	return id, nil
}

func lookupTunnel(token, accountID, name string) (id, accountTag string, err error) {
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel?name=%s&is_deleted=false", accountID, name)
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
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel/%s/token", accountID, tunnelID)
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

	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel", accountID)
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

func sync(token, zoneID, target string, desired map[string]bool, mode string) error {
	existing, err := listRecords(token, zoneID, target)
	if err != nil {
		return fmt.Errorf("list records: %w", err)
	}

	var created, ok, errored int

	for host := range desired {
		if _, exists := existing[host]; exists {
			fmt.Printf("  ok      %s\n", host)
			ok++
		} else {
			fmt.Printf("  create  %s\n", host)
			if err := createRecord(token, zoneID, host, target); err != nil {
				fmt.Fprintf(os.Stderr, "  ERROR   %s: %v\n", host, err)
				errored++
			} else {
				created++
			}
		}
	}

	var deleted int

	if mode == "complete" {
		for host, id := range existing {
			if !desired[host] {
				fmt.Printf("  delete  %s\n", host)
				if err := deleteRecord(token, zoneID, id); err != nil {
					fmt.Fprintf(os.Stderr, "  ERROR   %s: %v\n", host, err)
					errored++
				} else {
					deleted++
				}
			}
		}
	}

	fmt.Printf("[sync] summary: ok=%d created=%d deleted=%d errors=%d\n", ok, created, deleted, errored)

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
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records?type=CNAME&content=%s&per_page=500", zoneID, target)

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

func createRecord(token, zoneID, hostname, target string) error {
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records", zoneID)

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
		return err
	}

	var apiResp apiResponse
	if err := json.Unmarshal(resp, &apiResp); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	if !apiResp.Success {
		return fmt.Errorf("api error: %v", apiResp.Errors)
	}
	return nil
}

func deleteRecord(token, zoneID, recordID string) error {
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s", zoneID, recordID)

	resp, err := cfRequest("DELETE", url, token, nil)
	if err != nil {
		return err
	}

	var apiResp apiResponse
	if err := json.Unmarshal(resp, &apiResp); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	if !apiResp.Success {
		return fmt.Errorf("api error: %v", apiResp.Errors)
	}
	return nil
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

	fmt.Println("[entrypoint] launching cloudflared tunnel")
	args := []string{"cloudflared", "tunnel", "--config", configPath}
	if tunnelID != "" {
		args = append(args, "--credentials-file", credsPath, "run", tunnelID)
	} else {
		args = append(args, "run")
	}
	if err := syscall.Exec(bin, args, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "[entrypoint] exec failed: %v\n", err)
		os.Exit(1)
	}
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
