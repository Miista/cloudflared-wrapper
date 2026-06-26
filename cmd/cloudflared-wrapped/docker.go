package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	dockerSocket  = "/var/run/docker.sock"
	labelHostname = "cloudflare.io/hostname"
)

// socketAvailable reports whether the Docker socket is mounted into the
// container. Its presence is the opt-in for label-based ingress discovery —
// no env var required.
func socketAvailable() bool {
	_, err := os.Stat(dockerSocket)
	return err == nil
}

// dockerHTTP is an http.Client that talks to the Docker Engine API over the
// Unix socket. We hand-roll this instead of pulling in the Docker SDK to keep
// the binary tiny and the distroless base intact.
var dockerHTTP = &http.Client{
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", dockerSocket)
		},
	},
}

type dockerContainer struct {
	Names  []string          `json:"Names"`
	Labels map[string]string `json:"Labels"`
	Ports  []struct {
		PrivatePort int    `json:"PrivatePort"`
		Type        string `json:"Type"`
	} `json:"Ports"`
}

// getContainers lists running containers via GET /containers/json. The
// response already carries Labels and Ports, so a single call is enough — no
// per-container inspect needed.
func getContainers() ([]dockerContainer, error) {
	resp, err := dockerHTTP.Get("http://unix/containers/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, string(data))
	}
	var containers []dockerContainer
	if err := json.Unmarshal(data, &containers); err != nil {
		return nil, fmt.Errorf("parse containers: %w", err)
	}
	return containers, nil
}

type ingressRule struct {
	Hostname string
	Service  string
}

// discoverIngress turns container labels into ingress rules. The only required
// label is cloudflare.io/hostname; the backend service is inferred from the
// container name plus its single exposed port. A container with 0 or >1 exposed
// ports (and no explicit :port in the hostname) is skipped with a loud log
// rather than aborting the whole wrapper.
func discoverIngress(containers []dockerContainer) []ingressRule {
	var rules []ingressRule
	for _, c := range containers {
		val := strings.TrimSpace(c.Labels[labelHostname])
		if val == "" {
			continue
		}
		name := containerName(c)
		if name == "" {
			fmt.Fprintf(os.Stderr, "[discover] skip %q: cannot determine container name\n", val)
			continue
		}

		host := val
		port := 0
		if i := strings.LastIndex(val, ":"); i != -1 {
			host = val[:i]
			p, err := strconv.Atoi(val[i+1:])
			if err != nil {
				fmt.Fprintf(os.Stderr, "[discover] skip %s: invalid port %q\n", name, val[i+1:])
				continue
			}
			port = p
		}

		if port == 0 {
			ports := exposedPorts(c)
			switch len(ports) {
			case 1:
				port = ports[0]
			case 0:
				fmt.Fprintf(os.Stderr, "[discover] skip %s: no exposed ports; specify %s: %s:<port>\n", name, labelHostname, host)
				continue
			default:
				fmt.Fprintf(os.Stderr, "[discover] skip %s: %d exposed ports %v; specify %s: %s:<port>\n", name, len(ports), ports, labelHostname, host)
				continue
			}
		}

		rule := ingressRule{
			Hostname: host,
			Service:  fmt.Sprintf("http://%s:%d", name, port),
		}
		fmt.Printf("[discover] %s -> %s\n", rule.Hostname, rule.Service)
		rules = append(rules, rule)
	}
	return rules
}

// containerName returns the container's primary name with the leading slash
// stripped — this is the DNS name cloudflared uses to reach it on the shared
// Docker network.
func containerName(c dockerContainer) string {
	if len(c.Names) == 0 {
		return ""
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

// exposedPorts returns the distinct TCP container ports.
func exposedPorts(c dockerContainer) []int {
	seen := map[int]bool{}
	var ports []int
	for _, p := range c.Ports {
		if p.Type != "tcp" || seen[p.PrivatePort] {
			continue
		}
		seen[p.PrivatePort] = true
		ports = append(ports, p.PrivatePort)
	}
	return ports
}

// writeMergedConfig reads the (read-only) base config.yml, merges the
// label-discovered ingress rules into it, and writes the result to dstPath —
// which becomes the config cloudflared actually runs. Ordering is: manual
// hostname rules first, then discovered rules, then exactly one catch-all
// last. Manual entries win on hostname conflict. Unknown top-level keys
// (tunnel, credentials-file, warp-routing, ...) are preserved.
func writeMergedConfig(srcPath, dstPath string, discovered []ingressRule) (string, error) {
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", srcPath, err)
	}
	root := map[string]interface{}{}
	if err := yaml.Unmarshal(data, &root); err != nil {
		return "", fmt.Errorf("parse %s: %w", srcPath, err)
	}

	rawIngress, _ := root["ingress"].([]interface{})
	var hostnamed, catchall []interface{}
	seen := map[string]bool{}
	for _, item := range rawIngress {
		m, ok := item.(map[string]interface{})
		if !ok {
			catchall = append(catchall, item)
			continue
		}
		if h, ok := m["hostname"].(string); ok && h != "" {
			hostnamed = append(hostnamed, m)
			seen[h] = true
		} else {
			catchall = append(catchall, m)
		}
	}

	for _, r := range discovered {
		if seen[r.Hostname] {
			fmt.Fprintf(os.Stderr, "[discover] %s already defined in config.yml; manual entry wins, skipping label\n", r.Hostname)
			continue
		}
		hostnamed = append(hostnamed, map[string]interface{}{
			"hostname": r.Hostname,
			"service":  r.Service,
		})
		seen[r.Hostname] = true
	}

	if len(catchall) == 0 {
		catchall = append(catchall, map[string]interface{}{"service": "http_status:404"})
	}

	root["ingress"] = append(hostnamed, catchall...)

	out, err := yaml.Marshal(root)
	if err != nil {
		return "", fmt.Errorf("marshal merged config: %w", err)
	}
	if err := os.WriteFile(dstPath, out, 0644); err != nil {
		return "", fmt.Errorf("write %s: %w", dstPath, err)
	}
	return dstPath, nil
}
