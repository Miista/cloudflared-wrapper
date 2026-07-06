package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- parseZoneIDs ---

func TestParseZoneIDs(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{"abc123", []string{"abc123"}},
		{"abc123,def456", []string{"abc123", "def456"}},
		{"abc123, def456 , ghi789", []string{"abc123", "def456", "ghi789"}},
		{"", nil},
		{"  ", nil},
		{",,,", nil},
	}
	for _, tc := range cases {
		got := parseZoneIDs(tc.raw)
		if len(got) != len(tc.want) {
			t.Errorf("parseZoneIDs(%q) = %v, want %v", tc.raw, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("parseZoneIDs(%q)[%d] = %q, want %q", tc.raw, i, got[i], tc.want[i])
			}
		}
	}
}

// --- apexDomain ---

func TestApexDomain(t *testing.T) {
	cases := []struct {
		hostname string
		want     string
	}{
		{"links.guldmund.net", "guldmund.net"},
		{"links.palmund.net", "palmund.net"},
		{"a.b.c.example.com", "example.com"},
		{"example.com", "example.com"},
		{"sub.example.com", "example.com"},
		{"localhost", "localhost"},
		{"a.b", "a.b"},
	}
	for _, tc := range cases {
		got := apexDomain(tc.hostname)
		if got != tc.want {
			t.Errorf("apexDomain(%q) = %q, want %q", tc.hostname, got, tc.want)
		}
	}
}

// --- lookupZoneName (with fake HTTP server) ---

func zoneServer(t *testing.T, zones map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// expect GET /client/v4/zones/<id>
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/client/v4/zones/"), "/")
		id := parts[0]
		name, ok := zones[id]
		if !ok {
			w.WriteHeader(404)
			fmt.Fprintf(w, `{"success":false,"errors":[{"message":"zone not found"}]}`)
			return
		}
		resp := map[string]interface{}{
			"success": true,
			"result":  map[string]string{"name": name},
		}
		json.NewEncoder(w).Encode(resp)
	}))
}

func withCFBase(srv *httptest.Server, fn func()) {
	old := cfBase
	cfBase = srv.URL
	defer func() { cfBase = old }()
	fn()
}

func TestLookupZoneName(t *testing.T) {
	srv := zoneServer(t, map[string]string{
		"zone1": "palmund.net",
		"zone2": "guldmund.net",
	})
	defer srv.Close()

	// clear cache between tests
	zoneNameCache = map[string]string{}

	withCFBase(srv, func() {
		name, err := lookupZoneName("tok", "zone1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if name != "palmund.net" {
			t.Errorf("got %q, want palmund.net", name)
		}

		name, err = lookupZoneName("tok", "zone2")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if name != "guldmund.net" {
			t.Errorf("got %q, want guldmund.net", name)
		}
	})
}

func TestLookupZoneNameCached(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"result":  map[string]string{"name": "example.net"},
		})
	}))
	defer srv.Close()

	zoneNameCache = map[string]string{}
	withCFBase(srv, func() {
		lookupZoneName("tok", "zoneX")
		lookupZoneName("tok", "zoneX")
		lookupZoneName("tok", "zoneX")
	})
	if calls != 1 {
		t.Errorf("expected 1 API call due to caching, got %d", calls)
	}
}

func TestLookupZoneNameNotFound(t *testing.T) {
	srv := zoneServer(t, map[string]string{})
	defer srv.Close()

	zoneNameCache = map[string]string{}
	withCFBase(srv, func() {
		_, err := lookupZoneName("tok", "unknown")
		if err == nil {
			t.Error("expected error for unknown zone, got nil")
		}
	})
}

// --- matchZone ---

func TestMatchZone(t *testing.T) {
	srv := zoneServer(t, map[string]string{
		"zone-palmund": "palmund.net",
		"zone-guldmund": "guldmund.net",
	})
	defer srv.Close()

	zoneNameCache = map[string]string{}
	zones := []string{"zone-palmund", "zone-guldmund"}

	withCFBase(srv, func() {
		cases := []struct {
			hostname  string
			wantID    string
			wantMatch bool
		}{
			{"links.palmund.net", "zone-palmund", true},
			{"links.guldmund.net", "zone-guldmund", true},
			{"app.palmund.net", "zone-palmund", true},
			{"links.unknown.net", "", false},
			{"example.com", "", false},
		}
		for _, tc := range cases {
			id, ok := matchZone(tc.hostname, zones, "tok")
			if ok != tc.wantMatch || id != tc.wantID {
				t.Errorf("matchZone(%q) = (%q, %v), want (%q, %v)",
					tc.hostname, id, ok, tc.wantID, tc.wantMatch)
			}
		}
	})
}

// --- groupByZone ---
//
// This is the regression test for the "wrong zone" bug: a hostname whose apex
// does not match any configured zone must never appear in byZone and must
// therefore never be passed to sync(). Before the fix, main() called sync()
// with a single hardcoded zoneID regardless of whether the hostname's apex
// matched that zone, which caused records to be created under the wrong zone
// (e.g. "links.wrongzone.example" created as a subdomain of "right.example").

func TestGroupByZone_skipsUnmatchedHostname(t *testing.T) {
	srv := zoneServer(t, map[string]string{
		"zone-right": "right.example",
	})
	defer srv.Close()

	zoneNameCache = map[string]string{}
	zones := []string{"zone-right"}

	withCFBase(srv, func() {
		desired := map[string]bool{
			"app.right.example":  true,
			"app.wrong.example":  true, // different apex — must be skipped
			"app2.wrong.example": true, // another non-matching hostname
		}
		byZone := groupByZone(desired, zones, "tok")

		// only zone-right should have entries
		got := byZone["zone-right"]
		if !got["app.right.example"] {
			t.Error("expected app.right.example to be routed to zone-right")
		}
		if got["app.wrong.example"] {
			t.Error("app.wrong.example must NOT be routed to zone-right (wrong apex)")
		}
		if got["app2.wrong.example"] {
			t.Error("app2.wrong.example must NOT be routed to zone-right (wrong apex)")
		}
	})
}

func TestGroupByZone_multiZone(t *testing.T) {
	srv := zoneServer(t, map[string]string{
		"zone-a": "alpha.example",
		"zone-b": "beta.example",
	})
	defer srv.Close()

	zoneNameCache = map[string]string{}
	zones := []string{"zone-a", "zone-b"}

	withCFBase(srv, func() {
		desired := map[string]bool{
			"app.alpha.example":   true,
			"admin.alpha.example": true,
			"app.beta.example":    true,
			"other.gamma.example": true, // no zone — must be skipped
		}
		byZone := groupByZone(desired, zones, "tok")

		if !byZone["zone-a"]["app.alpha.example"] || !byZone["zone-a"]["admin.alpha.example"] {
			t.Error("alpha hostnames not routed to zone-a")
		}
		if !byZone["zone-b"]["app.beta.example"] {
			t.Error("beta hostname not routed to zone-b")
		}
		// cross-zone contamination check
		if byZone["zone-a"]["app.beta.example"] {
			t.Error("beta hostname must NOT appear in zone-a")
		}
		if byZone["zone-b"]["app.alpha.example"] {
			t.Error("alpha hostname must NOT appear in zone-b")
		}
		// skipped hostname must not appear anywhere
		for id, hosts := range byZone {
			if hosts["other.gamma.example"] {
				t.Errorf("gamma hostname must be skipped, but found in zone %s", id)
			}
		}
	})
}

func TestGroupByZone_allSkippedWhenNoZoneMatches(t *testing.T) {
	srv := zoneServer(t, map[string]string{
		"zone-a": "alpha.example",
	})
	defer srv.Close()

	zoneNameCache = map[string]string{}
	zones := []string{"zone-a"}

	withCFBase(srv, func() {
		desired := map[string]bool{
			"app.beta.example":  true,
			"app.gamma.example": true,
		}
		byZone := groupByZone(desired, zones, "tok")

		for id, hosts := range byZone {
			if len(hosts) > 0 {
				t.Errorf("expected no hostnames routed to any zone, got %v in %s", hosts, id)
			}
		}
	})
}

func TestMatchZoneSingleZone(t *testing.T) {
	// backward compat: single zone ID behaves identically to before
	srv := zoneServer(t, map[string]string{
		"zone-palmund": "palmund.net",
	})
	defer srv.Close()

	zoneNameCache = map[string]string{}
	zones := []string{"zone-palmund"}

	withCFBase(srv, func() {
		id, ok := matchZone("links.palmund.net", zones, "tok")
		if !ok || id != "zone-palmund" {
			t.Errorf("single-zone match failed: id=%q ok=%v", id, ok)
		}
		_, ok = matchZone("links.guldmund.net", zones, "tok")
		if ok {
			t.Error("expected no match for out-of-zone hostname with single zone")
		}
	})
}
