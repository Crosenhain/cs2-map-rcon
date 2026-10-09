package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorcon/rcon"
	"github.com/gorcon/rcon/rcontest"
)

// kickTestStatusOutput is a realistic `status` reply from a CS2 dedicated
// server: the "[NoChan]" placeholder, bots, humans (one with an h:mm:ss
// connection time) and names with spaces, apostrophes and HTML.
const kickTestStatusOutput = `Server:  Running [0.0.0.0:27015]
Client:  Disconnected
@ Current  :  game
source   : console
hostname : Friday Night 5v5
spawn    : 1
version  : 1.40.9.2/14092 10283 secure  public
steamid  : [A:1:3954651142:31254] (90219348727681030)
udp/ip   : 0.0.0.0:27015 (local: 192.168.1.10:27015) (public IP from Steam: 203.0.113.10)
os/type  : Linux dedicated
players  : 4 humans, 2 bots (16 max) (not hibernating) (unreserved)
loaded spawngroup(  1)  : SV:  [1: de_dust2 | main lump | mapload]

---------players--------
  id     time ping loss      state   rate adr name
65535 [NoChan]    0    0 challenging      0unknown ''
    1      BOT    0    0     active      0 'Rezan'
    2      BOT    0    0     active      0 'Vladimir'
    3    12:34   24    0     active 786432 203.0.113.7:27005 'Alice'
    4    05:01   41    1     active 786432 198.51.100.23:27005 'Big Bob the Builder'
    5  1:02:03   18    0     active 1000000 192.0.2.44:27005 'O'Brien'
    6    00:12   55    0   spawning 786432 203.0.113.99:27005 '<img src=x onerror=alert(1)>'
#end
`

func TestParseStatusPlayers(t *testing.T) {
	tests := []struct {
		name   string
		status string
		want   []Player
	}{
		{
			name:   "full dedicated server status",
			status: kickTestStatusOutput,
			want: []Player{
				{ID: 3, Name: "Alice"},
				{ID: 4, Name: "Big Bob the Builder"},
				{ID: 5, Name: "O'Brien"},
				{ID: 6, Name: "<img src=x onerror=alert(1)>"},
			},
		},
		{
			name: "empty server with only the placeholder row",
			status: `players  : 0 humans, 0 bots (16 max) (hibernating) (unreserved)
---------players--------
  id     time ping loss      state   rate adr name
65535 [NoChan]    0    0 challenging      0unknown ''
#end
`,
			want: []Player{},
		},
		{
			name: "bots only",
			status: `---------players--------
  id     time ping loss      state   rate adr name
65535 [NoChan]    0    0 challenging      0unknown ''
    1      BOT    0    0     active      0 'Rezan'
    2      BOT    0    0     active      0 'Some Bot With Spaces'
    3      BOT    0    0     active      0 'DemoRecorder'
#end
`,
			want: []Player{},
		},
		{
			name: "quotes, spaces and other awkward names",
			status: `---------players--------
  id     time ping loss      state   rate adr name
    0    01:00   10    0     active 786432 203.0.113.1:27005 ''quoted''
    1    01:00   10    0     active 786432 203.0.113.2:27005 'it's "fine" & <ok>'
    2    01:00   10    0     active 786432 203.0.113.3:27005 '  leading and trailing  '
    7    01:00   10    0     active 786432 203.0.113.4:27005 '886 // k1na# HFTH'
    8    01:00   10    0     active 786432 203.0.113.5:27005 '♕KINGSLAINE مقاومة'
    9    01:00   10    0     active 786432 203.0.113.6:27005 ''
#end
`,
			want: []Player{
				{ID: 0, Name: "'quoted'"},
				{ID: 1, Name: `it's "fine" & <ok>`},
				{ID: 2, Name: "  leading and trailing  "},
				{ID: 7, Name: "886 // k1na# HFTH"},
				{ID: 8, Name: "♕KINGSLAINE مقاومة"},
				{ID: 9, Name: ""},
			},
		},
		{
			name: "listen server loopback and run-together rate/address",
			status: `---------players--------
  id     time ping loss      state   rate adr name
    0    10:00    0    0     active 786432 loopback:0 'Host Player'
    1    10:00    5    0     active 1000000203.0.113.9:27005 'Glued'
#end
`,
			want: []Player{
				{ID: 0, Name: "Host Player"},
				{ID: 1, Name: "Glued"},
			},
		},
		{
			name:   "CRLF line endings and tabs",
			status: "---------players--------\r\n  id     time ping loss      state   rate adr name\r\n\t3\t12:34\t24\t0\tactive\t786432\t203.0.113.7:27005\t'Tabby Cat'\r\n#end\r\n",
			want:   []Player{{ID: 3, Name: "Tabby Cat"}},
		},
		{
			name: "malformed lines are skipped",
			status: `---------players--------
  id     time ping loss      state   rate adr name
garbage line without any quotes
    3    12:34   24    0     active 786432 203.0.113.7:27005 'unterminated
    4    12:34   24    0     active 786432 203.0.113.7:27005 'trailing' junk
   xx    12:34   24    0     active 786432 203.0.113.7:27005 'bad id'
   -5    12:34   24    0     active 786432 203.0.113.7:27005 'negative id'
   +5    12:34   24    0     active 786432 203.0.113.7:27005 'plus id'
    6    soon   24    0     active 786432 203.0.113.7:27005 'bad time'
    7    12:34   xx    0     active 786432 203.0.113.7:27005 'bad ping'
    8    12:34 'too few columns'
    9    12:34   24    0     active 786432 203.0.113.7:27005 extra 'too many columns'
65535    12:34   24    0     active 786432 203.0.113.7:27005 'placeholder id'
99999999999999999999 12:34 24 0 active 786432 203.0.113.7:27005 'huge id'
'
''
   10    12:34   24    0     active 786432 203.0.113.7:27005 'Valid Player'
#end
`,
			want: []Player{{ID: 10, Name: "Valid Player"}},
		},
		{
			name: "rows after #end are ignored",
			status: `---------players--------
  id     time ping loss      state   rate adr name
    3    12:34   24    0     active 786432 203.0.113.7:27005 'Inside'
#end
    4    12:34   24    0     active 786432 203.0.113.8:27005 'Outside'
`,
			want: []Player{{ID: 3, Name: "Inside"}},
		},
		{
			name: "rows before the players section are ignored",
			status: `hostname : '3 12:34 24 0 active 786432 1.2.3.4:5 'sneaky'
    4    12:34   24    0     active 786432 203.0.113.8:27005 'Before Header'
`,
			want: []Player{},
		},
		{
			name: "missing #end still parses",
			status: `---------players--------
  id     time ping loss      state   rate adr name
    3    12:34   24    0     active 786432 203.0.113.7:27005 'Alice'`,
			want: []Player{{ID: 3, Name: "Alice"}},
		},
		{
			name:   "invalid UTF-8 in a name is replaced",
			status: "---------players--------\n    3    12:34   24    0     active 786432 203.0.113.7:27005 'bad\xffname'\n#end\n",
			want:   []Player{{ID: 3, Name: "bad\uFFFDname"}},
		},
		{
			name: "output without a players section",
			status: `hostname : Friday Night 5v5
players  : 0 humans, 0 bots (16 max) (hibernating) (unreserved)
loaded spawngroup(  1)  : SV:  [1: de_dust2 | main lump | mapload]
`,
			want: []Player{},
		},
		{
			name:   "unknown command reply",
			status: "Unknown command 'status'!\n",
			want:   []Player{},
		},
		{
			name:   "empty output",
			status: "",
			want:   []Player{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseStatusPlayers(tt.status)
			if got == nil {
				t.Fatal("parseStatusPlayers returned nil; want a non-nil slice so it encodes as []")
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseStatusPlayers() = %#v\nwant %#v", got, tt.want)
			}
		})
	}
}

func TestParseKickUserID(t *testing.T) {
	tests := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{in: "0", want: 0},
		{in: "7", want: 7},
		{in: "00012", want: 12},
		{in: "65534", want: 65534},
		{in: "", wantErr: true},
		{in: "abc", wantErr: true},
		{in: "-1", wantErr: true},
		{in: "+5", wantErr: true},
		{in: " 5", wantErr: true},
		{in: "1.5", wantErr: true},
		{in: "0x10", wantErr: true},
		{in: "5;quit", wantErr: true},
		{in: "٣", wantErr: true}, // non-ASCII digit
		{in: "65535", wantErr: true},
		{in: "99999", wantErr: true},
		{in: "123456", wantErr: true},
	}
	for _, tt := range tests {
		got, err := parseKickUserID(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseKickUserID(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("parseKickUserID(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

// kickTestTargets swaps rconTargets/webPath for the duration of a test.
func kickTestTargets(t *testing.T, targets map[string]TargetServer) {
	t.Helper()
	oldTargets, oldWebPath := rconTargets, webPath
	rconTargets, webPath = targets, "/"
	t.Cleanup(func() { rconTargets, webPath = oldTargets, oldWebPath })
}

// kickTestSilentListener accepts TCP connections, counts them and holds them
// open without ever replying. A handler that dialled it would block on the
// RCON auth reply until gorcon's 5s timeout, so a fast response plus a zero
// count proves no RCON connection was attempted.
func kickTestSilentListener(t *testing.T) (addr string, accepted func() int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var (
		count atomic.Int32
		mu    sync.Mutex
		conns []net.Conn
		done  = make(chan struct{})
	)
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			count.Add(1)
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return ln.Addr().String(), count.Load
}

// kickTestRCON is a fake CS2 RCON server that answers `status` with a canned
// reply and records every command it receives.
type kickTestRCON struct {
	mu       sync.Mutex
	commands []string
}

func newKickTestRCON(t *testing.T, password, status string) (*kickTestRCON, string) {
	t.Helper()
	f := &kickTestRCON{}
	srv := rcontest.NewServer(
		rcontest.SetSettings(rcontest.Settings{Password: password}),
		rcontest.SetCommandHandler(func(c *rcontest.Context) {
			cmd := c.Request().Body()
			f.mu.Lock()
			f.commands = append(f.commands, cmd)
			f.mu.Unlock()
			body := ""
			if cmd == "status" {
				body = status
			}
			_, _ = rcon.NewPacket(rcon.SERVERDATA_RESPONSE_VALUE, c.Request().ID, body).WriteTo(c.Conn())
		}),
	)
	t.Cleanup(srv.Close)
	return f, srv.Addr()
}

func (f *kickTestRCON) Commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}

func kickTestPost(t *testing.T, target string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	kickHandler(rec, req)
	return rec
}

func TestKickHandlerRejectsWithoutDialling(t *testing.T) {
	addr, accepted := kickTestSilentListener(t)
	kickTestTargets(t, map[string]TargetServer{
		"srv": {Name: "srv", Address: addr, Password: "pw"},
	})

	tests := []struct {
		name     string
		method   string
		url      string
		body     string
		wantCode int
	}{
		{name: "GET not allowed", method: http.MethodGet, url: "/kick?target=srv&userid=3", wantCode: http.StatusMethodNotAllowed},
		{name: "PUT not allowed", method: http.MethodPut, url: "/kick", body: "target=srv&userid=3", wantCode: http.StatusMethodNotAllowed},
		{name: "unknown target", method: http.MethodPost, url: "/kick", body: "target=nope&userid=3", wantCode: http.StatusBadRequest},
		{name: "missing target", method: http.MethodPost, url: "/kick", body: "userid=3", wantCode: http.StatusBadRequest},
		{name: "query string is ignored", method: http.MethodPost, url: "/kick?target=srv&userid=3", body: "", wantCode: http.StatusBadRequest},
		{name: "empty userid is a no-op", method: http.MethodPost, url: "/kick", body: "target=srv&userid=&name=", wantCode: http.StatusSeeOther},
		{name: "missing userid is a no-op", method: http.MethodPost, url: "/kick", body: "target=srv", wantCode: http.StatusSeeOther},
		{name: "blank userid is a no-op", method: http.MethodPost, url: "/kick", body: "target=srv&userid=+++", wantCode: http.StatusSeeOther},
		{name: "userid only in query is a no-op", method: http.MethodPost, url: "/kick?userid=3", body: "target=srv", wantCode: http.StatusSeeOther},
		{name: "non-numeric userid", method: http.MethodPost, url: "/kick", body: "target=srv&userid=abc", wantCode: http.StatusBadRequest},
		{name: "injection attempt in userid", method: http.MethodPost, url: "/kick", body: "target=srv&userid=" + url.QueryEscape("3;quit"), wantCode: http.StatusBadRequest},
		{name: "negative userid", method: http.MethodPost, url: "/kick", body: "target=srv&userid=-1", wantCode: http.StatusBadRequest},
		{name: "userid out of range", method: http.MethodPost, url: "/kick", body: "target=srv&userid=65535", wantCode: http.StatusBadRequest},
		{name: "userid too long", method: http.MethodPost, url: "/kick", body: "target=srv&userid=0000001", wantCode: http.StatusBadRequest},
		{name: "oversize body", method: http.MethodPost, url: "/kick", body: "target=srv&userid=3&name=" + strings.Repeat("A", 2*maxRequestBodyBytes), wantCode: http.StatusRequestEntityTooLarge},
		{name: "malformed form encoding", method: http.MethodPost, url: "/kick", body: "target=srv&userid=%zz", wantCode: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.url, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()

			start := time.Now()
			kickHandler(rec, req)
			elapsed := time.Since(start)

			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tt.wantCode, rec.Body.String())
			}
			if tt.wantCode == http.StatusSeeOther {
				if loc := rec.Header().Get("Location"); loc != "/" {
					t.Errorf("Location = %q, want %q", loc, "/")
				}
			}
			if tt.wantCode == http.StatusMethodNotAllowed {
				if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
					t.Errorf("Allow = %q, want %q", allow, http.MethodPost)
				}
			}
			if elapsed > 2*time.Second {
				t.Errorf("handler took %v; it should not have attempted an RCON connection", elapsed)
			}
			if n := accepted(); n != 0 {
				t.Errorf("RCON listener accepted %d connection(s); want none", n)
			}
		})
	}
}

func TestKickHandlerAgainstFakeServer(t *testing.T) {
	tests := []struct {
		name         string
		form         url.Values
		wantCode     int
		wantKick     string // expected kick command, "" for none
		wantBodyPart string
	}{
		{
			name:     "kicks matching id and name",
			form:     url.Values{"userid": {"5"}, "name": {"O'Brien"}},
			wantCode: http.StatusSeeOther,
			wantKick: "kickid 5",
		},
		{
			name:     "kicks player with HTML in name",
			form:     url.Values{"userid": {"6"}, "name": {"<img src=x onerror=alert(1)>"}},
			wantCode: http.StatusSeeOther,
			wantKick: "kickid 6",
		},
		{
			name:     "kicks by id when no name is supplied",
			form:     url.Values{"userid": {"3"}},
			wantCode: http.StatusSeeOther,
			wantKick: "kickid 3",
		},
		{
			name:         "stale id no longer on the server",
			form:         url.Values{"userid": {"9"}, "name": {"Gone"}},
			wantCode:     http.StatusConflict,
			wantBodyPart: "no longer on the server",
		},
		{
			name:         "id reused by a different player",
			form:         url.Values{"userid": {"4"}, "name": {"Alice"}},
			wantCode:     http.StatusConflict,
			wantBodyPart: "different player",
		},
		{
			name:         "bots are not kickable here",
			form:         url.Values{"userid": {"1"}, "name": {"Rezan"}},
			wantCode:     http.StatusConflict,
			wantBodyPart: "no longer on the server",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake, addr := newKickTestRCON(t, "pw", kickTestStatusOutput)
			kickTestTargets(t, map[string]TargetServer{
				"srv": {Name: "srv", Address: addr, Password: "pw"},
			})

			form := url.Values{"target": {"srv"}}
			for k, v := range tt.form {
				form[k] = v
			}
			rec := kickTestPost(t, "/kick", form.Encode())

			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tt.wantCode, rec.Body.String())
			}
			if tt.wantBodyPart != "" && !strings.Contains(rec.Body.String(), tt.wantBodyPart) {
				t.Errorf("body %q does not contain %q", rec.Body.String(), tt.wantBodyPart)
			}
			if tt.wantCode == http.StatusSeeOther {
				if loc := rec.Header().Get("Location"); loc != "/" {
					t.Errorf("Location = %q, want %q", loc, "/")
				}
			}

			want := []string{"status"}
			if tt.wantKick != "" {
				want = append(want, tt.wantKick)
			}
			if got := fake.Commands(); !reflect.DeepEqual(got, want) {
				t.Errorf("RCON commands = %q, want %q", got, want)
			}
		})
	}
}

func TestAPIStatusIncludesPlayerList(t *testing.T) {
	_, addr := newKickTestRCON(t, "pw", kickTestStatusOutput)
	kickTestTargets(t, map[string]TargetServer{
		"srv": {Name: "srv", Address: addr, Password: "pw"},
	})

	rec := httptest.NewRecorder()
	apiStatusHandler(rec, httptest.NewRequest(http.MethodGet, "/api/status?target=srv", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if strings.Contains(rec.Body.String(), "<img") {
		t.Errorf("raw HTML leaked into JSON body: %s", rec.Body.String())
	}

	var got ServerStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := ServerStatus{
		Hostname: "Friday Night 5v5",
		Map:      "de_dust2",
		Players:  "4 humans, 2 bots",
		PlayerList: []Player{
			{ID: 3, Name: "Alice"},
			{ID: 4, Name: "Big Bob the Builder"},
			{ID: 5, Name: "O'Brien"},
			{ID: 6, Name: "<img src=x onerror=alert(1)>"},
		},
		Online: true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("status = %#v\nwant %#v", got, want)
	}
}

func TestAPIStatusAlwaysJSON(t *testing.T) {
	// A port that refuses connections, so the dial fails immediately.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	closedAddr := ln.Addr().String()
	_ = ln.Close()

	kickTestTargets(t, map[string]TargetServer{
		"down": {Name: "down", Address: closedAddr, Password: "pw"},
	})

	tests := []struct {
		name      string
		method    string
		url       string
		wantCode  int
		wantError string
	}{
		{name: "wrong method", method: http.MethodPost, url: "/api/status?target=down", wantCode: http.StatusMethodNotAllowed, wantError: "Method not allowed"},
		{name: "unknown target", method: http.MethodGet, url: "/api/status?target=nope", wantCode: http.StatusBadRequest, wantError: "Unknown server"},
		{name: "connection failed", method: http.MethodGet, url: "/api/status?target=down", wantCode: http.StatusOK, wantError: "Connection failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			apiStatusHandler(rec, httptest.NewRequest(tt.method, tt.url, nil))

			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			if !strings.Contains(rec.Body.String(), `"player_list":[]`) {
				t.Errorf("body %q should contain an empty player_list array", rec.Body.String())
			}
			var got ServerStatus
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode %q: %v", rec.Body.String(), err)
			}
			if got.Online || got.Error != tt.wantError {
				t.Errorf("got online=%v error=%q, want online=false error=%q", got.Online, got.Error, tt.wantError)
			}
		})
	}
}
