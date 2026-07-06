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
