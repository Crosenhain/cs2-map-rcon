package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestNormalizeWebPath(t *testing.T) {
	tests := map[string]string{
		"/":         "/",
		"":          "/",
		"cs2":       "/cs2/",
		"/cs2":      "/cs2/",
		"cs2/":      "/cs2/",
		"/cs2/":     "/cs2/",
		" /a/b ":    "/a/b/",
		"/a/b/rcon": "/a/b/rcon/",
		// Must never yield a protocol-relative redirect target.
		"//evil.example":   "/evil.example/",
		"///evil.example/": "/evil.example/",
		`/\evil.example`:   "/evil.example/",
		`\\evil.example`:   "/evil.example/",
		"//":               "/",
	}
	for in, want := range tests {
		if got := normalizeWebPath(in); got != want {
			t.Errorf("normalizeWebPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRouterCrossOriginProtection checks that browser cross-site POSTs are
// refused before reaching a handler, while same-origin and non-browser
// requests get through. A bad mapid makes rconHandler answer 400 without
// dialing RCON, so 400 means "reached the handler" and 403 means "blocked".
func TestRouterCrossOriginProtection(t *testing.T) {
	oldTargets, oldPath := rconTargets, webPath
	t.Cleanup(func() { rconTargets, webPath = oldTargets, oldPath })
	rconTargets = map[string]TargetServer{"srv": {Name: "srv", Address: "127.0.0.1:1"}}
	webPath = "/cs2/"

	router, err := newRouter([]string{"https://ha.example.com"})
	if err != nil {
		t.Fatalf("newRouter: %v", err)
	}

	body := url.Values{"target": {"srv"}, "mapid": {"bad map!"}}.Encode()
	tests := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"no browser headers (curl, Home Assistant)", nil, http.StatusBadRequest},
		{"same-origin browser POST", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://rcon.lan"}, http.StatusBadRequest},
		{"user-initiated navigation", map[string]string{"Sec-Fetch-Site": "none"}, http.StatusBadRequest},
		{"cross-site browser POST", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, http.StatusForbidden},
		{"same-site but cross-origin POST", map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "http://other.lan"}, http.StatusForbidden},
		{"old browser, mismatched Origin", map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"old browser, matching Origin", map[string]string{"Origin": "http://rcon.lan"}, http.StatusBadRequest},
		{"trusted origin", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://ha.example.com"}, http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://rcon.lan/cs2/rcon", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("got %d, want %d (body %q)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}

	// Cross-site GETs (e.g. status polling embedded elsewhere) stay allowed.
	req := httptest.NewRequest(http.MethodGet, "http://rcon.lan/cs2/api/status?target=nope", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Errorf("cross-site GET was blocked")
	}
}

func TestNewRouterRejectsBadTrustedOrigin(t *testing.T) {
	if _, err := newRouter([]string{"not a url"}); err == nil {
		t.Fatal("expected error for malformed trusted origin")
	}
}

func TestSteamPostChecksStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fail" {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"response":{"publishedfiledetails":[{"publishedfileid":"42","title":"Map"}]}}`))
	}))
	defer srv.Close()

	var det FileDetailsResponse
	if err := steamPost(srv.URL+"/fail", url.Values{}, &det); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("expected status error, got %v", err)
	}
	if err := steamPost(srv.URL+"/ok", url.Values{}, &det); err != nil {
		t.Fatalf("steamPost: %v", err)
	}
	if got := det.Response.PublishedFileDetails; len(got) != 1 || got[0].PublishedFileID != "42" {
		t.Fatalf("unexpected decode result: %+v", got)
	}
}
