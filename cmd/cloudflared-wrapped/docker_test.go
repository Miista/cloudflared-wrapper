package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func tcpPorts(ports ...int) []struct {
	PrivatePort int    `json:"PrivatePort"`
	Type        string `json:"Type"`
} {
	var out []struct {
		PrivatePort int    `json:"PrivatePort"`
		Type        string `json:"Type"`
	}
	for _, p := range ports {
		out = append(out, struct {
			PrivatePort int    `json:"PrivatePort"`
			Type        string `json:"Type"`
		}{p, "tcp"})
	}
	return out
}

func TestDiscoverIngress(t *testing.T) {
	containers := []dockerContainer{
		// single exposed port -> inferred
		{Names: []string{"/app"}, Labels: map[string]string{labelHostname: "app.example.com"}, Ports: tcpPorts(8080)},
		// explicit :port wins, no inference needed
		{Names: []string{"/api"}, Labels: map[string]string{labelHostname: "api.example.com:3000"}, Ports: tcpPorts(80, 443)},
		// multiple ports, no explicit port -> skipped
		{Names: []string{"/multi"}, Labels: map[string]string{labelHostname: "multi.example.com"}, Ports: tcpPorts(80, 443)},
		// zero ports, no explicit port -> skipped
		{Names: []string{"/noport"}, Labels: map[string]string{labelHostname: "noport.example.com"}},
		// no label -> ignored
		{Names: []string{"/plain"}, Labels: map[string]string{}, Ports: tcpPorts(8080)},
		// duplicate port entries (IPv4+IPv6) collapse to one -> inferred
		{Names: []string{"/dup"}, Labels: map[string]string{labelHostname: "dup.example.com"}, Ports: tcpPorts(5000, 5000)},
	}

	got := discoverIngress(containers)
	want := []ingressRule{
		{Hostname: "app.example.com", Service: "http://app:8080"},
		{Hostname: "api.example.com", Service: "http://api:3000"},
		{Hostname: "dup.example.com", Service: "http://dup:5000"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("discoverIngress mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func hostNetwork(mode string) struct {
	NetworkMode string `json:"NetworkMode"`
} {
	return struct {
		NetworkMode string `json:"NetworkMode"`
	}{mode}
}

func TestDiscoverIngressHostNetwork(t *testing.T) {
	containers := []dockerContainer{
		// host-network, single port -> host.docker.internal
		{Names: []string{"/ha"}, Labels: map[string]string{labelHostname: "ha.example.com"}, Ports: tcpPorts(8123), HostConfig: hostNetwork("host")},
		// host-network, explicit :port -> host.docker.internal
		{Names: []string{"/svc"}, Labels: map[string]string{labelHostname: "svc.example.com:9000"}, Ports: tcpPorts(80, 443), HostConfig: hostNetwork("host")},
		// host-network, multiple ports, no explicit port -> skipped
		{Names: []string{"/multi"}, Labels: map[string]string{labelHostname: "multi.example.com"}, Ports: tcpPorts(80, 443), HostConfig: hostNetwork("host")},
		// bridge network -> container name as before
		{Names: []string{"/app"}, Labels: map[string]string{labelHostname: "app.example.com"}, Ports: tcpPorts(8080), HostConfig: hostNetwork("bridge")},
	}

	got := discoverIngress(containers)
	want := []ingressRule{
		{Hostname: "ha.example.com", Service: "http://host.docker.internal:8123"},
		{Hostname: "svc.example.com", Service: "http://host.docker.internal:9000"},
		{Hostname: "app.example.com", Service: "http://app:8080"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("discoverIngress host-network mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestWriteMergedConfig(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "config.yml")
	dst := filepath.Join(dir, "merged.yml")

	base := `tunnel: my-tunnel-uuid
credentials-file: /etc/cloudflared/credentials.json
ingress:
  - hostname: manual.example.com
    service: http://manual:9000
  - hostname: app.example.com
    service: http://manual-app:1234
  - service: http_status:404
`
	if err := os.WriteFile(src, []byte(base), 0644); err != nil {
		t.Fatal(err)
	}

	discovered := []ingressRule{
		{Hostname: "new.example.com", Service: "http://new:8080"},
		{Hostname: "app.example.com", Service: "http://app:80"}, // conflicts with manual -> dropped
	}

	out, err := writeMergedConfig(src, dst, discovered)
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]interface{}
	if err := yaml.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}

	// top-level keys preserved
	if root["tunnel"] != "my-tunnel-uuid" {
		t.Errorf("tunnel key not preserved: %v", root["tunnel"])
	}
	if root["credentials-file"] != "/etc/cloudflared/credentials.json" {
		t.Errorf("credentials-file not preserved: %v", root["credentials-file"])
	}

	ingress, _ := root["ingress"].([]interface{})
	var hostnames []string
	for _, item := range ingress {
		m := item.(map[string]interface{})
		if h, ok := m["hostname"].(string); ok {
			hostnames = append(hostnames, h)
		}
	}
	// manual first (in order), then discovered new one; conflict dropped
	wantHosts := []string{"manual.example.com", "app.example.com", "new.example.com"}
	if !reflect.DeepEqual(hostnames, wantHosts) {
		t.Errorf("hostname order mismatch\n got: %v\nwant: %v", hostnames, wantHosts)
	}

	// app.example.com must keep the MANUAL service, not the discovered one
	for _, item := range ingress {
		m := item.(map[string]interface{})
		if m["hostname"] == "app.example.com" && m["service"] != "http://manual-app:1234" {
			t.Errorf("conflict not resolved to manual: %v", m["service"])
		}
	}

	// exactly one catch-all, and it is last
	last := ingress[len(ingress)-1].(map[string]interface{})
	if last["service"] != "http_status:404" || last["hostname"] != nil {
		t.Errorf("catch-all not last: %v", last)
	}
	if n := strings.Count(string(data), "http_status:404"); n != 1 {
		t.Errorf("expected exactly one catch-all, found %d", n)
	}
}

func TestWriteMergedConfigMissingBase(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "does-not-exist.yml")
	dst := filepath.Join(dir, "merged.yml")

	out, err := writeMergedConfig(src, dst, []ingressRule{{Hostname: "x.example.com", Service: "http://x:80"}})
	if err != nil {
		t.Fatalf("expected synthesis from labels, got error: %v", err)
	}
	data, _ := os.ReadFile(out)
	s := string(data)
	if !strings.Contains(s, "x.example.com") || !strings.Contains(s, "http://x:80") {
		t.Errorf("discovered rule missing from synthesized config:\n%s", s)
	}
	if !strings.Contains(s, "http_status:404") {
		t.Errorf("catch-all missing from synthesized config:\n%s", s)
	}
}

func TestWriteMergedConfigAddsCatchall(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "config.yml")
	dst := filepath.Join(dir, "merged.yml")

	// base with no catch-all and no ingress at all
	if err := os.WriteFile(src, []byte("ingress: []\n"), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := writeMergedConfig(src, dst, []ingressRule{{Hostname: "x.example.com", Service: "http://x:80"}})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(out)
	if !strings.Contains(string(data), "http_status:404") {
		t.Errorf("catch-all not appended when missing:\n%s", data)
	}
}
