package main

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gorcon/rcon"
	"github.com/gorcon/rcon/rcontest"
)

// setTestTargets swaps rconTargets for the duration of a test.
func setTestTargets(t *testing.T, targets map[string]TargetServer) {
	t.Helper()
	orig := rconTargets
	rconTargets = targets
	t.Cleanup(func() { rconTargets = orig })
}

// noDialTargets uses an address that fails instantly if dialed, so any
// handler that dials before validating returns 500 instead of the expected 4xx.
func noDialTargets(t *testing.T) {
	setTestTargets(t, map[string]TargetServer{
		"alpha": {Name: "alpha", Address: "dial-must-not-happen", Password: "pw"},
	})
}

func TestParseMapID(t *testing.T) {
	const wsURL = "https://steamcommunity.com/sharedfiles/filedetails/?id="
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		// Numeric workshop IDs
		{"workshop id", "3070290240", "host_workshop_map 3070290240", false},
		{"workshop id trimmed", "  3070290240\n", "host_workshop_map 3070290240", false},
		{"workshop id leading zeros", "000123", "host_workshop_map 123", false},
		{"workshop id max uint64", "18446744073709551615", "host_workshop_map 18446744073709551615", false},
		{"workshop id zero", "0", "", true},
		{"workshop id all zeros", "0000", "", true},
		{"workshop id overflow", "18446744073709551616", "", true},
		{"workshop id too long", "123456789012345678901", "", true},
		{"negative", "-5", "", true},
		{"plus sign", "+5", "", true},

		// Workshop URLs
		{"url sharedfiles", wsURL + "3070290240", "host_workshop_map 3070290240", false},
		{"url http", "http://steamcommunity.com/sharedfiles/filedetails/?id=42", "host_workshop_map 42", false},
		{"url www", "https://www.steamcommunity.com/sharedfiles/filedetails/?id=42", "host_workshop_map 42", false},
		{"url workshop path", "https://steamcommunity.com/workshop/filedetails/?id=42", "host_workshop_map 42", false},
		{"url no trailing slash", "https://steamcommunity.com/sharedfiles/filedetails?id=42", "host_workshop_map 42", false},
		{"url extra params", wsURL + "42&searchtext=dust", "host_workshop_map 42", false},
		{"url fragment", wsURL + "42#comments", "host_workshop_map 42", false},
		{"url uppercase host", "https://SteamCommunity.com/sharedfiles/filedetails/?id=42", "host_workshop_map 42", false},
		{"url trimmed", "  " + wsURL + "42\t", "host_workshop_map 42", false},
		{"url wrong host", "https://evil.example/sharedfiles/filedetails/?id=42", "", true},
		{"url host suffix", "https://steamcommunity.com.evil.example/sharedfiles/filedetails/?id=42", "", true},
		{"url host prefix", "https://evilsteamcommunity.com/sharedfiles/filedetails/?id=42", "", true},
		{"url userinfo trick", "https://steamcommunity.com@evil.example/sharedfiles/filedetails/?id=42", "", true},
		{"url userinfo", "https://user@steamcommunity.com/sharedfiles/filedetails/?id=42", "", true},
		{"url port", "https://steamcommunity.com:8443/sharedfiles/filedetails/?id=42", "", true},
		{"url ftp scheme", "ftp://steamcommunity.com/sharedfiles/filedetails/?id=42", "", true},
		{"url javascript scheme", "javascript:alert(1)", "", true},
		{"url no scheme", "steamcommunity.com/sharedfiles/filedetails/?id=42", "", true},
		{"url scheme relative", "//steamcommunity.com/sharedfiles/filedetails/?id=42", "", true},
		{"url wrong path", "https://steamcommunity.com/profiles/42?id=42", "", true},
		{"url extra path", "https://steamcommunity.com/sharedfiles/filedetails/x/?id=42", "", true},
		{"url no query", "https://steamcommunity.com/sharedfiles/filedetails/", "", true},
		{"url empty id", wsURL, "", true},
		{"url other param only", "https://steamcommunity.com/sharedfiles/filedetails/?foo=42", "", true},
		{"url duplicate id", wsURL + "42&id=43", "", true},
		{"url id zero", wsURL + "0", "", true},
		{"url id negative", wsURL + "-42", "", true},
		{"url id junk suffix", wsURL + "42abc", "", true},
		{"url id semicolon", wsURL + "42;quit", "", true},
		{"url id encoded semicolon", wsURL + "42%3Bquit", "", true},
		{"url id encoded space", wsURL + "42%20say%20hi", "", true},
		{"url id space", wsURL + "42 say hi", "", true},
		{"url id newline", wsURL + "42\nquit", "", true},
		{"url id too long", wsURL + "123456789012345678901", "", true},
		{"url too long", wsURL + "42&x=" + strings.Repeat("a", maxMapIDInputLen), "", true},

		// Official map names
		{"map name", "de_dust2", "map de_dust2", false},
		{"map name hyphen", "cs_office-v2", "map cs_office-v2", false},
		{"map name trimmed", " de_mirage \n", "map de_mirage", false},
		{"map name max length", strings.Repeat("a", maxMapNameLen), "map " + strings.Repeat("a", maxMapNameLen), false},
		{"map name too long", strings.Repeat("a", maxMapNameLen+1), "", true},
		{"map name leading hyphen", "-de_dust2", "", true},
		{"map name leading underscore", "_de_dust2", "", true},

		// Empty and injection attempts
		{"empty", "", "", true},
		{"whitespace only", "  \t\n ", "", true},
		{"inject semicolon", "de_dust2; quit", "", true},
		{"inject semicolon no space", "de_dust2;quit", "", true},
		{"inject after id", "123 ; say hi", "", true},
		{"inject newline", "de_dust2\nquit", "", true},
		{"inner space", "de dust2", "", true},
		{"quote", `de_dust2"`, "", true},
		{"path traversal", "../de_dust2", "", true},
		{"dot", "de_dust2.vpk", "", true},
		{"unicode", "de_düst2", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseMapID(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseMapID(%q) = %q, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseMapID(%q) error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("parseMapID(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseQuota(t *testing.T) {
	tests := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"0", 0, false},
		{"10", 10, false},
		{"64", 64, false},
		{" 20 ", 20, false},
		{"08", 8, false},
		{"65", 0, true},
		{"100", 0, true},
		{"1000", 0, true},
		{"99999999999999999999999", 0, true},
		{"-1", 0, true},
		{"+5", 0, true},
		{"", 0, true},
		{"   ", 0, true},
		{"abc", 0, true},
		{"5.5", 0, true},
		{"1e1", 0, true},
		{"0x10", 0, true},
		{"10; quit", 0, true},
	}
	for _, tt := range tests {
		got, err := parseQuota(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseQuota(%q) = %d, want error", tt.in, got)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("parseQuota(%q) = %d, %v; want %d", tt.in, got, err, tt.want)
		}
	}
}

func TestLookupTarget(t *testing.T) {
	setTestTargets(t, map[string]TargetServer{
		"alpha": {Name: "alpha", Address: "127.0.0.1:27015"},
	})
	tests := []struct {
		in      string
		wantErr bool
	}{
		{"alpha", false},
		{"  alpha\n", false},
		{"", true},
		{"   ", true},
		{"beta", true},
		{"ALPHA", true},
		{"alpha; quit", true},
	}
	for _, tt := range tests {
		got, err := lookupTarget(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("lookupTarget(%q) = %+v, want error", tt.in, got)
			}
			continue
		}
		if err != nil || got.Name != "alpha" {
			t.Errorf("lookupTarget(%q) = %+v, %v; want alpha", tt.in, got, err)
		}
	}
}

func TestHandlersRejectInvalidInput(t *testing.T) {
	noDialTargets(t)
	const form = "application/x-www-form-urlencoded"
	const jsonCT = "application/json"
	big := strings.Repeat("x", maxRequestBodyBytes+1)

	tests := []struct {
		name        string
		handler     http.HandlerFunc
		method      string
		url         string
		contentType string
		body        string
		want        int
	}{
		// rconHandler
		{"rcon GET", rconHandler, http.MethodGet, "/rcon?target=alpha&mapid=de_dust2", "", "", http.StatusMethodNotAllowed},
		{"rcon PUT", rconHandler, http.MethodPut, "/rcon", form, "target=alpha&mapid=de_dust2", http.StatusMethodNotAllowed},
		{"rcon missing target", rconHandler, http.MethodPost, "/rcon", form, "mapid=de_dust2", http.StatusBadRequest},
		{"rcon unknown target", rconHandler, http.MethodPost, "/rcon", form, "target=beta&mapid=de_dust2", http.StatusBadRequest},
		{"rcon missing mapid", rconHandler, http.MethodPost, "/rcon", form, "target=alpha", http.StatusBadRequest},
		{"rcon injection mapid", rconHandler, http.MethodPost, "/rcon", form, "target=alpha&mapid=de_dust2%3B+quit", http.StatusBadRequest},
		{"rcon zero mapid", rconHandler, http.MethodPost, "/rcon", form, "target=alpha&mapid=0", http.StatusBadRequest},
		{"rcon wrong host url", rconHandler, http.MethodPost, "/rcon", form, "target=alpha&mapid=" + "https%3A%2F%2Fevil.example%2Fsharedfiles%2Ffiledetails%2F%3Fid%3D42", http.StatusBadRequest},
		{"rcon query only", rconHandler, http.MethodPost, "/rcon?target=alpha&mapid=de_dust2", form, "", http.StatusBadRequest},
		{"rcon query target only", rconHandler, http.MethodPost, "/rcon?target=alpha", form, "mapid=de_dust2", http.StatusBadRequest},
		{"rcon malformed body", rconHandler, http.MethodPost, "/rcon", form, "target=alpha&mapid=%zz", http.StatusBadRequest},
		{"rcon oversize body", rconHandler, http.MethodPost, "/rcon", form, "target=alpha&mapid=de_dust2&pad=" + big, http.StatusRequestEntityTooLarge},

		// botsHandler
		{"bots GET", botsHandler, http.MethodGet, "/bots?target=alpha&action=kick", "", "", http.StatusMethodNotAllowed},
		{"bots unknown target", botsHandler, http.MethodPost, "/bots", form, "target=beta&action=kick", http.StatusBadRequest},
		{"bots missing action", botsHandler, http.MethodPost, "/bots", form, "target=alpha", http.StatusBadRequest},
		{"bots unknown action", botsHandler, http.MethodPost, "/bots", form, "target=alpha&action=reboot", http.StatusBadRequest},
		{"bots quota missing", botsHandler, http.MethodPost, "/bots", form, "target=alpha&action=quota", http.StatusBadRequest},
		{"bots quota too high", botsHandler, http.MethodPost, "/bots", form, "target=alpha&action=quota&quota=65", http.StatusBadRequest},
		{"bots quota negative", botsHandler, http.MethodPost, "/bots", form, "target=alpha&action=quota&quota=-1", http.StatusBadRequest},
		{"bots quota non-numeric", botsHandler, http.MethodPost, "/bots", form, "target=alpha&action=quota&quota=ten", http.StatusBadRequest},
		{"bots quota injection", botsHandler, http.MethodPost, "/bots", form, "target=alpha&action=quota&quota=5%3Bquit", http.StatusBadRequest},
		{"bots query only", botsHandler, http.MethodPost, "/bots?target=alpha&action=kick", form, "", http.StatusBadRequest},
		{"bots query quota only", botsHandler, http.MethodPost, "/bots?quota=10", form, "target=alpha&action=quota", http.StatusBadRequest},
		{"bots oversize body", botsHandler, http.MethodPost, "/bots", form, "target=alpha&action=kick&pad=" + big, http.StatusRequestEntityTooLarge},

		// apiLoadMapHandler
		{"api GET", apiLoadMapHandler, http.MethodGet, "/api/load", "", "", http.StatusMethodNotAllowed},
		{"api empty body", apiLoadMapHandler, http.MethodPost, "/api/load", jsonCT, "", http.StatusBadRequest},
		{"api invalid json", apiLoadMapHandler, http.MethodPost, "/api/load", jsonCT, `{"server":`, http.StatusBadRequest},
		{"api not an object", apiLoadMapHandler, http.MethodPost, "/api/load", jsonCT, `["alpha","de_dust2"]`, http.StatusBadRequest},
		{"api unknown server", apiLoadMapHandler, http.MethodPost, "/api/load", jsonCT, `{"server":"beta","map_id":"de_dust2"}`, http.StatusBadRequest},
		{"api missing map_id", apiLoadMapHandler, http.MethodPost, "/api/load", jsonCT, `{"server":"alpha"}`, http.StatusBadRequest},
		{"api bad map_id", apiLoadMapHandler, http.MethodPost, "/api/load", jsonCT, `{"server":"alpha","map_id":"de_dust2; quit"}`, http.StatusBadRequest},
		{"api trailing object", apiLoadMapHandler, http.MethodPost, "/api/load", jsonCT, `{"server":"alpha","map_id":"de_dust2"} {"server":"alpha"}`, http.StatusBadRequest},
		{"api trailing garbage", apiLoadMapHandler, http.MethodPost, "/api/load", jsonCT, `{"server":"alpha","map_id":"de_dust2"}xyz`, http.StatusBadRequest},
		{"api query only", apiLoadMapHandler, http.MethodPost, "/api/load?server=alpha&map_id=de_dust2", jsonCT, "", http.StatusBadRequest},
		{"api oversize body", apiLoadMapHandler, http.MethodPost, "/api/load", jsonCT, `{"server":"alpha","map_id":"de_dust2","pad":"` + big + `"}`, http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.url, strings.NewReader(tt.body))
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			rec := httptest.NewRecorder()
			tt.handler(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d (body: %q)", rec.Code, tt.want, rec.Body.String())
			}
			if tt.want == http.StatusMethodNotAllowed && rec.Header().Get("Allow") != http.MethodPost {
				t.Errorf("Allow header = %q, want POST", rec.Header().Get("Allow"))
			}
			if strings.TrimSpace(rec.Body.String()) == "" {
				t.Errorf("expected an error message in the response body")
			}
		})
	}
}

// fakeRCON starts an in-process RCON server that records executed commands.
func fakeRCON(t *testing.T) (addr string, commands func() []string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	srv := rcontest.NewServer(
		rcontest.SetSettings(rcontest.Settings{Password: "pw"}),
		rcontest.SetCommandHandler(func(c *rcontest.Context) {
			mu.Lock()
			got = append(got, c.Request().Body())
			mu.Unlock()
			_, _ = rcon.NewPacket(rcon.SERVERDATA_RESPONSE_VALUE, c.Request().ID, "").WriteTo(c.Conn())
		}),
	)
	t.Cleanup(srv.Close)
	return srv.Addr(), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

func TestHandlersSendValidatedCommands(t *testing.T) {
	const form = "application/x-www-form-urlencoded"
	tests := []struct {
		name        string
		handler     http.HandlerFunc
		url         string
		contentType string
		body        string
		wantStatus  int
		wantCmds    []string
	}{
		{"rcon workshop id", rconHandler, "/rcon", form, "target=alpha&mapid=3070290240", http.StatusSeeOther,
			[]string{"host_workshop_map 3070290240"}},
		{"rcon workshop url", rconHandler, "/rcon", form,
			"target=alpha&mapid=" + "https%3A%2F%2Fsteamcommunity.com%2Fsharedfiles%2Ffiledetails%2F%3Fid%3D3070290240",
			http.StatusSeeOther, []string{"host_workshop_map 3070290240"}},
		{"rcon map name trimmed", rconHandler, "/rcon", form, "target=+alpha+&mapid=+de_dust2+", http.StatusSeeOther,
			[]string{"map de_dust2"}},
		{"rcon body wins over query", rconHandler, "/rcon?mapid=de_nuke&target=beta", form, "target=alpha&mapid=de_dust2", http.StatusSeeOther,
			[]string{"map de_dust2"}},
		{"bots kick", botsHandler, "/bots", form, "target=alpha&action=kick", http.StatusSeeOther,
			[]string{"bot_quota 0"}},
		{"bots quota", botsHandler, "/bots", form, "target=alpha&action=quota&quota=08", http.StatusSeeOther,
			[]string{"bot_quota_mode fill", "bot_quota 8"}},
		{"api load extra fields", apiLoadMapHandler, "/api/load", "application/json",
			`{"server":"alpha","map_id":"3070290240","source":"home-assistant","extra":{"a":1}}` + "\n",
			http.StatusOK, []string{"host_workshop_map 3070290240"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr, commands := fakeRCON(t)
			setTestTargets(t, map[string]TargetServer{
				"alpha": {Name: "alpha", Address: addr, Password: "pw"},
			})
			req := httptest.NewRequest(http.MethodPost, tt.url, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", tt.contentType)
			rec := httptest.NewRecorder()
			tt.handler(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body: %q)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantStatus == http.StatusSeeOther && rec.Header().Get("Location") != webPath {
				t.Errorf("Location = %q, want %q", rec.Header().Get("Location"), webPath)
			}
			if got := commands(); strings.Join(got, "|") != strings.Join(tt.wantCmds, "|") {
				t.Errorf("commands = %q, want %q", got, tt.wantCmds)
			}
		})
	}
}

func TestRconHandlerAcceptsMultipart(t *testing.T) {
	addr, commands := fakeRCON(t)
	setTestTargets(t, map[string]TargetServer{
		"alpha": {Name: "alpha", Address: addr, Password: "pw"},
	})
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("target", "alpha")
	_ = mw.WriteField("mapid", "de_dust2")
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/rcon", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	rconHandler(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (body: %q)", rec.Code, rec.Body.String())
	}
	if got := commands(); len(got) != 1 || got[0] != "map de_dust2" {
		t.Errorf("commands = %q, want [map de_dust2]", got)
	}
}

func TestIndexHasWorkshopForm(t *testing.T) {
	setTestTargets(t, map[string]TargetServer{"alpha": {Name: "alpha"}})
	rec := httptest.NewRecorder()
	indexHandler(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	page := rec.Body.String()
	for _, want := range []string{
		`<form class="workshop-form toolbar-right" method="POST" action="rcon">`,
		`<input type="hidden" name="target" value="alpha"/>`,
		`name="mapid"`,
		fmt.Sprintf(`maxlength="%d"`, maxMapIDInputLen),
	} {
		if !strings.Contains(page, want) {
			t.Errorf("index page missing %q", want)
		}
	}
}

func TestIndexBotControls(t *testing.T) {
	setTestTargets(t, map[string]TargetServer{"alpha": {Name: "alpha"}})
	rec := httptest.NewRecorder()
	indexHandler(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	page := rec.Body.String()
	for _, want := range []string{
		`<select class="bots-select" name="quota" data-server="alpha"`,
		`<option value="1">1</option>`,
		`<option value="10" selected>10</option>`,
		`<option value="20">20</option>`,
		`name="action" value="quota">Set bots</button>`,
		`name="action" value="kick" title="Kick all bots (bot_quota 0)">Kick bots</button>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("index page missing %q", want)
		}
	}
	if strings.Contains(page, `<option value="21">`) || strings.Contains(page, `<option value="0">`) {
		t.Error("bot count options should be 1..20")
	}
}
