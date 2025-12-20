package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorcon/rcon"
)

// --- Structs ---

type MapInfo struct {
	ID    string
	Title string
}

type TargetServer struct {
	Name         string
	Address      string
	Password     string
	CollectionID string
}

type ServerStatus struct {
	Hostname string `json:"hostname"`
	Map      string `json:"map"`
	Players  string `json:"players"` // "X humans, Y bots"
	Online   bool   `json:"online"`
	Error    string `json:"error,omitempty"`
}

type FileDetailsResponse struct {
	Response struct {
		PublishedFileDetails []struct {
			PublishedFileID string `json:"publishedfileid"`
			Title           string `json:"title"`
		} `json:"publishedfiledetails"`
	} `json:"response"`
}

type ItemDetailsResponse struct {
	Response struct {
		CollectionDetails []struct {
			Children []struct {
				PublishedFileID string `json:"publishedfileid"`
			} `json:"children"`
		} `json:"collectiondetails"`
	} `json:"response"`
}

// PageData is passed to the HTML template
type PageData struct {
	Servers []ServerViewData
}

type ServerViewData struct {
	Name string
	Maps []MapInfo
}

// --- Globals ---

var (
	steamAPIKey   = getSecret("STEAM_API_KEY", "/run/secrets/steam_api_key")
	rconTargets   = loadRconTargetsFromFile("/run/secrets/rcon_targets")
	refreshPeriod = 10 * time.Minute
	webPath       = "/"
)

// Cache now maps CollectionID -> List of Maps
var cache = struct {
	sync.RWMutex
	collections map[string][]MapInfo
}{collections: make(map[string][]MapInfo)}

// --- Helpers ---

func getSecret(envKey, filePath string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func loadRconTargetsFromFile(path string) map[string]TargetServer {
	file, err := os.Open(path)
	if err != nil {
		log.Printf("Error opening RCON targets file: %v", err)
		return map[string]TargetServer{}
	}
	defer file.Close()

	targets := make(map[string]TargetServer)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			parts := strings.SplitN(line, "=", 4)
			if len(parts) == 4 {
				name := strings.TrimSpace(parts[0])
				targets[name] = TargetServer{
					Name:         name,
					Address:      strings.TrimSpace(parts[1]),
					Password:     strings.TrimSpace(parts[2]),
					CollectionID: strings.TrimSpace(parts[3]),
				}
			} else {
				log.Printf("Skipping invalid config line: %s", line)
			}
		}
	}
	return targets
}

func fetchCollection(colID string) ([]string, error) {
	form := url.Values{}
	form.Set("collectioncount", "1")
	form.Set("publishedfileids[0]", colID)
	resp, err := http.PostForm("https://api.steampowered.com/ISteamRemoteStorage/GetCollectionDetails/v1/", form)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %v", err)
	}
	var col ItemDetailsResponse
	if err := json.Unmarshal(body, &col); err != nil {
		return nil, fmt.Errorf("decode error: %v", err)
	}
	ids := []string{}
	for _, c := range col.Response.CollectionDetails {
		for _, child := range c.Children {
			ids = append(ids, child.PublishedFileID)
		}
	}
	return ids, nil
}

func fetchDetails(ids []string) ([]MapInfo, error) {
	if len(ids) == 0 {
		return []MapInfo{}, nil
	}
	form := url.Values{}
	form.Set("itemcount", fmt.Sprintf("%d", len(ids)))
	for i, id := range ids {
		form.Set(fmt.Sprintf("publishedfileids[%d]", i), id)
	}
	resp, err := http.PostForm("https://api.steampowered.com/ISteamRemoteStorage/GetPublishedFileDetails/v1/", form)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %v", err)
	}
	var det FileDetailsResponse
	if err := json.Unmarshal(body, &det); err != nil {
		return nil, fmt.Errorf("decode error: %v", err)
	}
	out := []MapInfo{}
	for _, d := range det.Response.PublishedFileDetails {
		title := d.Title
		if title == "" {
			title = "[No Title]"
		}
		out = append(out, MapInfo{ID: d.PublishedFileID, Title: title})
	}
	return out, nil
}

func updater() {
	runUpdate()
	ticker := time.NewTicker(refreshPeriod)
	for range ticker.C {
		runUpdate()
	}
}

func runUpdate() {
	uniqueIDs := make(map[string]bool)
	for _, target := range rconTargets {
		if target.CollectionID != "" {
			uniqueIDs[target.CollectionID] = true
		}
	}
	tempCache := make(map[string][]MapInfo)
	for colID := range uniqueIDs {
		ids, err := fetchCollection(colID)
		if err != nil {
			log.Printf("Error fetching collection %s: %v", colID, err)
			continue
		}
		maps, err := fetchDetails(ids)
		if err != nil {
			log.Printf("Error fetching details for collection %s: %v", colID, err)
			continue
		}
		tempCache[colID] = maps
	}
	cache.Lock()
	cache.collections = tempCache
	cache.Unlock()
	log.Println("Map cache updated")
}

// --- HTML Template ---

var tpl = template.Must(template.New("index").Funcs(template.FuncMap{
	"quotas": func() []int { return []int{6, 8, 10, 14, 20} },
}).Parse(`
<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>CS2 RCON</title>
  <style>
    body { background-color: #1e1e1e; color: #f0f0f0; font-family: Arial, sans-serif; padding: 2rem; }
    h1 { text-align: center; }
    .tabs { display: flex; justify-content: center; gap: 1rem; margin-bottom: 1rem; flex-wrap: wrap; }
    .tabs button { padding: 0.75rem 1.5rem; background-color: #333; color: #fff; border: none; cursor: pointer; border-radius: 5px; }
    .tabs button.active { background-color: #4caf50; color: white; }
    .tabcontent { display: none; }
    .tabcontent.active { display: block; }

    /* Status Box Styles */
    .status-box {
      background-color: #2a2a2a;
      border: 1px solid #444;
      border-radius: 8px;
      padding: 1rem;
      margin-bottom: 1.5rem;
      text-align: center;
      min-height: 80px;
      display: flex;
      flex-direction: column;
      justify-content: center;
      align-items: center;
    }
    .status-loading { color: #888; font-style: italic; }
    .status-error { color: #ff6b6b; }
    .status-details { display: flex; gap: 2rem; flex-wrap: wrap; justify-content: center; }
    .status-item span { font-weight: bold; color: #4caf50; }

    .grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); gap: 1rem; margin-top: 2rem; }
    .grid form { display: flex; justify-content: center; }
    button.map-button { background-color: #333; color: #fff; border: 2px solid #555; border-radius: 10px; padding: 1rem; width: 100%; font-size: 1.1rem; cursor: pointer; transition: all 0.2s ease-in-out; }
    button.map-button:hover { background-color: #555; border-color: #888; }
    .bot-controls { display: flex; flex-wrap: wrap; gap: 0.75rem; align-items: center; justify-content: center; margin: 1rem 0 0.5rem 0; }
    .bot-button { background-color: #2f2f2f; color: #fff; border: 2px solid #555; border-radius: 8px; padding: 0.6rem 1rem; font-size: 0.95rem; cursor: pointer; transition: all 0.15s ease-in-out; }
    .bot-button:hover { background-color: #4a4a4a; border-color: #777; }
    .bot-button.danger { border-color: #aa3333; }
    .bot-button.danger:hover { background-color: #7a2a2a; border-color: #cc4444; }
  </style>
  <script>
    let pollInterval = null;

    function showTab(server) {
      // UI switching
      document.querySelectorAll('.tabcontent').forEach(el => el.classList.remove('active'));
      document.querySelectorAll('.tabs button').forEach(el => el.classList.remove('active'));
      document.getElementById(server).classList.add('active');
      document.getElementById('btn-' + server).classList.add('active');
      localStorage.setItem('lastSelectedServer', server);

      // Start Polling
      startPolling(server);
    }

    function startPolling(server) {
      if (pollInterval) clearInterval(pollInterval);
      fetchStatus(server); // immediate fetch
      pollInterval = setInterval(() => fetchStatus(server), 5000); // 5s interval
    }

    async function fetchStatus(server) {
      const box = document.getElementById('status-' + server);
      if (!box) return;

      try {
        const res = await fetch('api/status?target=' + encodeURIComponent(server));
        const data = await res.json();

        if (!data.online) {
           box.innerHTML = '<div class="status-error">OFFLINE or Unreachable (' + (data.error || 'Unknown') + ')</div>';
           return;
        }

        box.innerHTML =
          '<div class="status-details">' +
            '<div class="status-item">Host: <span>' + (data.hostname || 'Unknown') + '</span></div>' +
            '<div class="status-item">Map: <span>' + (data.map || 'Unknown') + '</span></div>' +
            '<div class="status-item">Players: <span>' + (data.players || '0') + '</span></div>' +
          '</div>';
      } catch (e) {
        box.innerHTML = '<div class="status-error">Error fetching status</div>';
      }
    }

    window.onload = function () {
      const last = localStorage.getItem('lastSelectedServer');
      const fallback = document.querySelector('.tabs button')?.id.replace('btn-', '') || '';
      if (last && document.getElementById(last)) { showTab(last); }
      else if (fallback) { showTab(fallback); }
    };
  </script>
</head>
<body>
<h1>CS2 RCON</h1>

<div class="tabs">
  {{range .Servers}}
  <button id="btn-{{.Name}}" onclick="showTab('{{.Name}}')">{{.Name}}</button>
  {{end}}
</div>

{{range .Servers}}
{{$srv := .}}
<div id="{{$srv.Name}}" class="tabcontent">

  <div id="status-{{$srv.Name}}" class="status-box">
    <div class="status-loading">Loading server status...</div>
  </div>

  <div class="bot-controls">
  <form method="POST" action="bots" style="display:inline;">
    <input type="hidden" name="target" value="{{$srv.Name}}"/>
    <input type="hidden" name="action" value="kick"/>
    <button class="bot-button danger" type="submit">Kick All Bots</button>
  </form>
  {{range $q := quotas}}
  <form method="POST" action="bots" style="display:inline;">
    <input type="hidden" name="target" value="{{$srv.Name}}"/>
    <input type="hidden" name="action" value="quota"/>
    <input type="hidden" name="quota" value="{{$q}}"/>
    <button class="bot-button" type="submit">Set Quota: {{$q}}</button>
  </form>
  {{end}}
</div>

  <div class="grid">
    {{range $srv.Maps}}
    <form method="POST" action="rcon">
      <input type="hidden" name="mapid" value="{{.ID}}"/>
      <input type="hidden" name="target" value="{{$srv.Name}}"/>
      <button class="map-button" type="submit">{{.Title}}</button>
    </form>
    {{else}}
    <p style="text-align:center;">No maps found for this collection.</p>
    {{end}}
  </div>
</div>
{{end}}
</body>
</html>
`))

// --- Handlers ---

func indexHandler(w http.ResponseWriter, r *http.Request) {
	cache.RLock()
	collections := cache.collections
	cache.RUnlock()

	var serverViews []ServerViewData
	keys := make([]string, 0, len(rconTargets))
	for k := range rconTargets {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, name := range keys {
		target := rconTargets[name]
		maps := collections[target.CollectionID]
		serverViews = append(serverViews, ServerViewData{Name: name, Maps: maps})
	}

	var buf bytes.Buffer
	if err := tpl.Execute(&buf, PageData{Servers: serverViews}); err != nil {
		log.Println("Template execute error:", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

func rconHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	targetName := r.FormValue("target")
	target, ok := rconTargets[targetName]
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	rconConn, err := rcon.Dial(target.Address, target.Password)
	if err != nil {
		log.Println("RCON dial error:", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer rconConn.Close()

	mapID := r.FormValue("mapid")
	if _, err := strconv.ParseUint(mapID, 10, 64); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	cmd := fmt.Sprintf("host_workshop_map %s", mapID)
	if _, err := rconConn.Execute(cmd); err != nil {
		log.Println("RCON exec error:", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, webPath, http.StatusSeeOther)
}

func botsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	targetName := r.FormValue("target")
	target, ok := rconTargets[targetName]
	if !ok {
		http.Error(w, "Unknown RCON target", http.StatusBadRequest)
		return
	}
	conn, err := rcon.Dial(target.Address, target.Password)
	if err != nil {
		log.Printf("botsHandler dial error: %v", err)
		http.Error(w, "RCON connection failed", http.StatusInternalServerError)
		return
	}
	defer conn.Close()

	action := r.FormValue("action")
	switch action {
	case "kick":
		if _, err := conn.Execute("bot_quota 0"); err != nil {
			log.Printf("botsHandler exec error: %v", err)
			http.Error(w, "RCON execution failed", http.StatusInternalServerError)
			return
		}
	case "quota":
		qstr := r.FormValue("quota")
		if _, err := strconv.ParseUint(qstr, 10, 64); err != nil {
			http.Error(w, "Invalid quota", http.StatusBadRequest)
			return
		}
		cmds := []string{"bot_quota_mode fill", fmt.Sprintf("bot_quota %s", qstr)}
		for _, c := range cmds {
			if _, err := conn.Execute(c); err != nil {
				log.Printf("botsHandler exec error: %v", err)
				http.Error(w, "RCON execution failed", http.StatusInternalServerError)
				return
			}
		}
	}
	http.Redirect(w, r, webPath, http.StatusSeeOther)
}

// Regex to parse 'status' output
var (
	reHostname    = regexp.MustCompile(`hostname\s*:\s*(.+)`)
	reMap         = regexp.MustCompile(`(?m)^map\s*:\s*([\w\-\._]+)`)                       // Standard "map : name"
	reMapFallback = regexp.MustCompile(`loaded spawngroup\(\s*1\).+?\[\d+:\s*([\w\-\._]+)`) // Fallback for CS2 hibernation
	rePlayers     = regexp.MustCompile(`players\s*:\s*(\d+\s+humans?,\s+\d+\s+bots?)`)      // Captures just "0 humans, 0 bots"
)

func apiStatusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	targetName := r.URL.Query().Get("target")
	target, ok := rconTargets[targetName]
	if !ok {
		http.Error(w, "Unknown server", http.StatusBadRequest)
		return
	}

	status := ServerStatus{Online: false}

	conn, err := rcon.Dial(target.Address, target.Password)
	if err != nil {
		status.Error = "Connection failed"
		json.NewEncoder(w).Encode(status)
		return
	}
	defer conn.Close()

	resp, err := conn.Execute("status")
	if err != nil {
		status.Error = "Command failed"
		json.NewEncoder(w).Encode(status)
		return
	}

	status.Online = true

	// Parse Hostname
	if m := reHostname.FindStringSubmatch(resp); len(m) > 1 {
		status.Hostname = strings.TrimSpace(m[1])
	}

	// Parse Map (Try standard first, then fallback)
	if m := reMap.FindStringSubmatch(resp); len(m) > 1 {
		status.Map = strings.TrimSpace(m[1])
	} else if m := reMapFallback.FindStringSubmatch(resp); len(m) > 1 {
		status.Map = strings.TrimSpace(m[1])
	} else {
		status.Map = "Unknown"
	}

	// Parse Players
	if m := rePlayers.FindStringSubmatch(resp); len(m) > 1 {
		status.Players = strings.TrimSpace(m[1])
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

func apiMapsHandler(w http.ResponseWriter, r *http.Request) {
	cache.RLock()
	cols := cache.collections
	cache.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cols)
}

func apiLoadMapHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Server string `json:"server"`
		MapID  string `json:"map_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	target, ok := rconTargets[req.Server]
	if !ok {
		http.Error(w, "Unknown server", http.StatusBadRequest)
		return
	}
	if _, err := strconv.ParseUint(req.MapID, 10, 64); err != nil {
		http.Error(w, "Invalid map_id", http.StatusBadRequest)
		return
	}
	conn, err := rcon.Dial(target.Address, target.Password)
	if err != nil {
		http.Error(w, "RCON connection failed", http.StatusInternalServerError)
		return
	}
	defer conn.Close()
	cmd := fmt.Sprintf("host_workshop_map %s", req.MapID)
	if _, err := conn.Execute(cmd); err != nil {
		http.Error(w, "RCON execution failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"status":"ok"}`)
}

func main() {
	if steamAPIKey == "" {
		log.Fatal("Missing STEAM_API_KEY secret.")
	}
	if len(rconTargets) == 0 {
		log.Fatal("No RCON targets configured.")
	}
	// For serving it as a subdirectory
	if os.Getenv("WEB_PATH") != "" {
		webPath = os.Getenv("WEB_PATH")
	}

	go updater()

	http.HandleFunc(webPath, indexHandler)
	http.HandleFunc(webPath+"rcon", rconHandler)
	http.HandleFunc(webPath+"bots", botsHandler)

	http.HandleFunc(webPath+"api/maps", apiMapsHandler)
	http.HandleFunc(webPath+"api/load", apiLoadMapHandler)
	http.HandleFunc(webPath+"api/status", apiStatusHandler) // New endpoint

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Println("Listening on :" + port)
	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
