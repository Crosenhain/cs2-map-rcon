package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
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

	// Workshop-only metadata from Steam (zero for official maps), used by the
	// UI to sort maps by collection order, popularity or date.
	Order         int   `json:",omitempty"` // position in the collection
	Subscriptions int64 `json:",omitempty"` // current subscribers
	Favorited     int64 `json:",omitempty"`
	TimeCreated   int64 `json:",omitempty"` // Unix seconds
	TimeUpdated   int64 `json:",omitempty"` // Unix seconds
}

type TargetServer struct {
	Name         string
	Address      string
	Password     string
	CollectionID string
}

type ServerStatus struct {
	Hostname   string   `json:"hostname"`
	Map        string   `json:"map"`
	Players    string   `json:"players"`     // "X humans, Y bots"
	PlayerList []Player `json:"player_list"` // human players, for the kick dropdown
	Online     bool     `json:"online"`
	Error      string   `json:"error,omitempty"`
}

type FileDetailsResponse struct {
	Response struct {
		PublishedFileDetails []struct {
			PublishedFileID string `json:"publishedfileid"`
			Title           string `json:"title"`
			Subscriptions   int64  `json:"subscriptions"`
			Favorited       int64  `json:"favorited"`
			TimeCreated     int64  `json:"time_created"`
			TimeUpdated     int64  `json:"time_updated"`
		} `json:"publishedfiledetails"`
	} `json:"response"`
}

type ItemDetailsResponse struct {
	Response struct {
		CollectionDetails []struct {
			Children []struct {
				PublishedFileID string `json:"publishedfileid"`
				SortOrder       int    `json:"sortorder"`
			} `json:"children"`
		} `json:"collectiondetails"`
	} `json:"response"`
}

// PageData is passed to the HTML template
type PageData struct {
	Servers []ServerViewData
}

type ServerViewData struct {
	Name         string
	Maps         []MapInfo
	OfficialMaps []MapInfo
}

// --- Globals ---

var (
	rconTargets   = loadRconTargetsFromFile("/run/secrets/rcon_targets")
	refreshPeriod = 10 * time.Minute
	webPath       = "/"
	debugMode     = strings.ToLower(os.Getenv("DEBUG")) == "true"

	// steamClient bounds Steam Web API calls so a hung request can't stall the updater.
	steamClient = &http.Client{Timeout: 15 * time.Second}
	// steamAPIBase is overridden in tests.
	steamAPIBase = "https://api.steampowered.com"
)

// Cache now maps CollectionID -> List of Maps
var cache = struct {
	sync.RWMutex
	collections   map[string][]MapInfo
	installedMaps map[string][]MapInfo // ServerName -> List of Maps
}{
	collections:   make(map[string][]MapInfo),
	installedMaps: make(map[string][]MapInfo),
}

// --- Helpers ---

// normalizeWebPath makes p start and end with "/" so routes like p+"rcon" work.
// Leading slashes and backslashes collapse to a single "/", because webPath is
// used as a redirect target and browsers treat "//host" or "/\host" as another site.
func normalizeWebPath(p string) string {
	p = "/" + strings.TrimLeft(strings.TrimSpace(p), `/\`)
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

func loadRconTargetsFromFile(path string) map[string]TargetServer {
	file, err := os.Open(path)
	if err != nil {
		log.Printf("Error opening RCON targets file: %v", err)
		return map[string]TargetServer{}
	}
	defer func() { _ = file.Close() }()

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
	if err := scanner.Err(); err != nil {
		// Don't run with a partially read config.
		log.Printf("Error reading RCON targets file: %v", err)
		return map[string]TargetServer{}
	}
	return targets
}

// reWorkshopListMap captures the map name in each "(name)" of ds_workshop_listmaps output.
var reWorkshopListMap = regexp.MustCompile(`\(([^)]+)\)`)

func fetchInstalledMaps(target TargetServer) ([]MapInfo, error) {
	if debugMode {
		log.Printf("[DEBUG] Dialing RCON for %s at %s...", target.Name, target.Address)
	}
	conn, err := rcon.Dial(target.Address, target.Password)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	workshopMapNames := make(map[string]bool)
	if wsResp, err := conn.Execute("ds_workshop_listmaps"); err == nil {
		if debugMode {
			log.Printf("[DEBUG] 'ds_workshop_listmaps' response from %s:\n%s", target.Name, wsResp)
		}
		matches := reWorkshopListMap.FindAllStringSubmatch(wsResp, -1)
		for _, m := range matches {
			workshopMapNames[m[1]] = true
		}
	}

	resp, err := conn.Execute("maps *")
	if err != nil {
		return nil, err
	}

	if debugMode {
		log.Printf("[DEBUG] 'maps *' response from %s:\n%s", target.Name, resp)
	}

	var maps []MapInfo
	seen := make(map[string]bool)
	scanner := bufio.NewScanner(strings.NewReader(resp))

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "---") || strings.Contains(line, " maps") {
			continue
		}

		// Split into words to handle potential prefixes like "PENDING:" or "(fs)"
		words := strings.Fields(line)
		for _, w := range words {
			mapName := w
			// Strip .vpk if present
			if strings.HasSuffix(strings.ToLower(w), ".vpk") {
				mapName = w[:len(w)-4]
			}

			// Filter out workshop maps identified by ds_workshop_listmaps
			if workshopMapNames[mapName] {
				if debugMode {
					log.Printf("[DEBUG] Skipping workshop map (from ds_workshop_listmaps): %s", mapName)
				}
				continue
			}

			// Filter out paths (editor/, ui/, prefabs/, etc.) and vanity/background maps
			if strings.Contains(mapName, "/") || strings.Contains(mapName, "\\") ||
				strings.HasSuffix(mapName, "_vanity") || strings.HasSuffix(mapName, "_skybox") ||
				strings.HasPrefix(mapName, "workshop_preview_") ||
				mapName == "error" || mapName == "graphics_settings" || mapName == "lobby_mapveto" {
				if debugMode {
					log.Printf("[DEBUG] Skipping internal or vanity map: %s", mapName)
				}
				continue
			}

			// Basic validation: must match expected map name pattern
			if isValidMapName(mapName) && !seen[mapName] {
				maps = append(maps, MapInfo{ID: mapName, Title: mapName})
				seen[mapName] = true
				if debugMode {
					log.Printf("[DEBUG] Added official map: %s", mapName)
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading 'maps *' output: %v", err)
	}

	// Sort maps alphabetically
	sort.Slice(maps, func(i, j int) bool {
		return maps[i].Title < maps[j].Title
	})

	return maps, nil
}

// steamPost POSTs form to a Steam Web API endpoint and decodes the JSON reply into out.
func steamPost(endpoint string, form url.Values, out any) error {
	resp, err := steamClient.PostForm(endpoint, form)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status: %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response body: %v", err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode error: %v", err)
	}
	return nil
}

func fetchCollection(colID string) ([]string, error) {
	form := url.Values{}
	form.Set("collectioncount", "1")
	form.Set("publishedfileids[0]", colID)
	var col ItemDetailsResponse
	if err := steamPost(steamAPIBase+"/ISteamRemoteStorage/GetCollectionDetails/v1/", form, &col); err != nil {
		return nil, err
	}
	ids := []string{}
	for _, c := range col.Response.CollectionDetails {
		// Return items in the collection's own listing order.
		children := c.Children
		sort.SliceStable(children, func(i, j int) bool { return children[i].SortOrder < children[j].SortOrder })
		for _, child := range children {
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
	var det FileDetailsResponse
	if err := steamPost(steamAPIBase+"/ISteamRemoteStorage/GetPublishedFileDetails/v1/", form, &det); err != nil {
		return nil, err
	}
	position := make(map[string]int, len(ids))
	for i, id := range ids {
		position[id] = i
	}
	out := []MapInfo{}
	for i, d := range det.Response.PublishedFileDetails {
		title := d.Title
		if title == "" {
			title = fmt.Sprintf("[No Title] (%s)", d.PublishedFileID)
		}
		order, ok := position[d.PublishedFileID]
		if !ok {
			order = i
		}
		out = append(out, MapInfo{
			ID:            d.PublishedFileID,
			Title:         title,
			Order:         order,
			Subscriptions: d.Subscriptions,
			Favorited:     d.Favorited,
			TimeCreated:   d.TimeCreated,
			TimeUpdated:   d.TimeUpdated,
		})
	}
	// Keep collection order even if Steam ever returns details out of order.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
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
	if debugMode {
		log.Println("[DEBUG] Starting map update...")
	}
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

	tempInstalledMaps := make(map[string][]MapInfo)
	for name, target := range rconTargets {
		installed, err := fetchInstalledMaps(target)
		if err != nil {
			log.Printf("Error fetching installed maps for %s: %v", name, err)
			continue
		}
		tempInstalledMaps[name] = installed
	}

	cache.Lock()
	cache.collections = tempCache
	cache.installedMaps = tempInstalledMaps
	cache.Unlock()
	log.Println("Map cache updated")
}

// --- HTML Template ---

var tpl = template.Must(template.New("index").Funcs(template.FuncMap{
	"botCounts": func() []int {
		counts := make([]int, 20)
		for i := range counts {
			counts[i] = i + 1
		}
		return counts
	},
}).Parse(`
<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>CS2 RCON</title>
  <style>
    *, *::before, *::after { box-sizing: border-box; }
    body { background-color: #1e1e1e; color: #f0f0f0; font-family: Arial, sans-serif; margin: 0; padding: 1.5rem; }
    h3 { margin: 1.25rem 0 0.5rem 0; font-size: 1.05rem; }
    .visually-hidden { position: absolute; width: 1px; height: 1px; margin: -1px; padding: 0; overflow: hidden; clip: rect(0 0 0 0); white-space: nowrap; border: 0; }

    /* Top bar: server tabs on the left, Keep awake on the right. */
    .topbar { display: flex; align-items: center; justify-content: space-between; gap: 0.75rem; margin-bottom: 1rem; }
    .tabs { display: flex; gap: 0.5rem; min-width: 0; overflow-x: auto; scrollbar-width: none; }
    .tabs button { flex: 0 0 auto; padding: 0.6rem 1.25rem; background-color: #333; color: #fff; border: none; cursor: pointer; border-radius: 5px; font-size: 0.95rem; }
    .tabs button.active { background-color: #4caf50; color: white; }
    #wake-btn { flex: 0 0 auto; }
    .tabcontent { display: none; }
    .tabcontent.active { display: block; }

    /* Status Box Styles */
    .status-box {
      background-color: #2a2a2a;
      border: 1px solid #444;
      border-radius: 8px;
      padding: 0.75rem 1rem;
      margin-bottom: 1rem;
      text-align: center;
      min-height: 3rem;
      display: flex;
      flex-direction: column;
      justify-content: center;
      align-items: center;
    }
    .status-loading { color: #888; font-style: italic; }
    .status-error { color: #ff6b6b; }
    .status-details { display: flex; gap: 0.25rem 1.5rem; flex-wrap: wrap; justify-content: center; }
    .status-item span { font-weight: bold; color: #4caf50; }

    /* Control rows: one group on the left, one on the right; they stack when narrow. */
    .toolbar { display: flex; flex-wrap: wrap; justify-content: space-between; align-items: center; gap: 0.5rem 1.5rem; margin-bottom: 0.75rem; }
    .toolbar-left, .toolbar-right { display: flex; align-items: center; gap: 0.5rem; margin: 0; flex: 1 1 340px; min-width: 0; }
    .toolbar-right { justify-content: flex-end; }
    .control-label { display: flex; align-items: center; gap: 0.5rem; color: #aaa; font-size: 0.9rem; }
    .kick-select, .bots-select { background-color: #2f2f2f; color: #fff; border: 2px solid #555; border-radius: 8px; padding: 0.6rem 0.75rem; font-size: 0.95rem; cursor: pointer; }
    .kick-select { flex: 1 1 auto; min-width: 0; max-width: 22rem; }
    .kick-select:hover, .bots-select:hover { border-color: #777; }
    .kick-select:focus-visible, .bots-select:focus-visible { outline: 2px solid #4caf50; outline-offset: 2px; }
    .bot-button { background-color: #2f2f2f; color: #fff; border: 2px solid #555; border-radius: 8px; padding: 0.6rem 1rem; font-size: 0.95rem; cursor: pointer; transition: all 0.15s ease-in-out; white-space: nowrap; }
    .bot-button:hover { background-color: #4a4a4a; border-color: #777; }
    .bot-button.danger { border-color: #aa3333; }
    .bot-button.danger:hover { background-color: #7a2a2a; border-color: #cc4444; }
    .bot-button.active { background-color: #4caf50; border-color: #4caf50; }
    .bot-button:disabled { opacity: 0.5; cursor: not-allowed; }
    .workshop-input, .map-filter { flex: 1 1 auto; min-width: 0; max-width: 22rem; background-color: #2a2a2a; color: #f0f0f0; border: 2px solid #555; border-radius: 8px; padding: 0.6rem 1rem; font-size: 0.95rem; }
    .map-sort { flex: 0 0 auto; background-color: #2f2f2f; color: #fff; border: 2px solid #555; border-radius: 8px; padding: 0.6rem 0.75rem; font-size: 0.95rem; cursor: pointer; }
    .map-sort:hover { border-color: #777; }
    .map-sort:focus-visible { outline: 2px solid #4caf50; outline-offset: 2px; }
    .workshop-input::placeholder, .map-filter::placeholder { color: #888; }
    .workshop-input:focus, .map-filter:focus { outline: none; border-color: #4caf50; }

    .grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(180px, 1fr)); gap: 0.6rem; }
    .grid form { display: flex; justify-content: center; margin: 0; }
    .grid form[hidden] { display: none; }
    .filter-empty { text-align: center; grid-column: 1 / -1; color: #888; margin: 0.5rem 0; }
    button.map-button { background-color: #333; color: #fff; border: 2px solid #555; border-radius: 8px; padding: 0.75rem; width: 100%; font-size: 1rem; line-height: 1.25; overflow-wrap: anywhere; cursor: pointer; transition: all 0.2s ease-in-out; }
    button.map-button:hover { background-color: #555; border-color: #888; }
    .map-meta { display: block; margin-top: 0.2rem; font-size: 0.75rem; color: #9a9a9a; }
    .map-meta[hidden] { display: none; }

    /* Phones: tighter spacing, stacked control rows, two map columns. */
    @media (max-width: 600px) {
      body { padding: 0.75rem; }
      .topbar { margin-bottom: 0.75rem; }
      .tabs button { padding: 0.5rem 0.9rem; }
      .status-box { padding: 0.5rem; margin-bottom: 0.75rem; font-size: 0.9rem; }
      .status-details { gap: 0.15rem 1rem; }
      .toolbar { gap: 0.5rem; }
      .toolbar-left, .toolbar-right { flex-basis: 100%; justify-content: flex-start; }
      .bot-button { padding: 0.45rem 0.7rem; font-size: 0.9rem; }
      /* 16px inputs stop iOS Safari zooming in on focus. */
      .kick-select, .bots-select, .map-sort { padding: 0.4rem 0.6rem; font-size: 16px; }
      .kick-select, .workshop-input, .map-filter { max-width: none; }
      .workshop-input, .map-filter { padding: 0.4rem 0.75rem; font-size: 16px; }
      h3 { margin: 0.9rem 0 0.4rem 0; font-size: 1rem; }
      .grid { grid-template-columns: repeat(auto-fill, minmax(140px, 1fr)); gap: 0.4rem; }
      button.map-button { padding: 0.55rem 0.4rem; font-size: 0.9rem; }
    }
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

    // Hostname, map, error text and player names all come from the game
    // server and may be attacker-controlled, so everything below builds DOM
    // nodes and assigns textContent/value. Never use innerHTML here.
    function makeEl(tag, className, text) {
      const el = document.createElement(tag);
      if (className) el.className = className;
      if (text !== undefined) el.textContent = String(text);
      return el;
    }

    function setStatusMessage(box, className, text) {
      box.textContent = '';
      box.appendChild(makeEl('div', className, text));
    }

    function statusItem(label, value) {
      const item = makeEl('div', 'status-item', label + ': ');
      item.appendChild(makeEl('span', '', value));
      return item;
    }

    const statusRequestSeq = {};

    async function fetchStatus(server) {
      const box = document.getElementById('status-' + server);
      if (!box) return;
      const seq = (statusRequestSeq[server] || 0) + 1;
      statusRequestSeq[server] = seq;

      let data;
      try {
        const res = await fetch('api/status?target=' + encodeURIComponent(server));
        data = await res.json();
      } catch (e) {
        if (seq === statusRequestSeq[server]) setStatusMessage(box, 'status-error', 'Error fetching status');
        return;
      }
      if (seq !== statusRequestSeq[server]) return; // a newer poll has already answered

      if (!data || !data.online) {
        setStatusMessage(box, 'status-error', 'OFFLINE or Unreachable (' + ((data && data.error) || 'Unknown') + ')');
        updateKickList(server, []);
        return;
      }

      const details = makeEl('div', 'status-details');
      details.appendChild(statusItem('Host', data.hostname || 'Unknown'));
      details.appendChild(statusItem('Map', data.map || 'Unknown'));
      details.appendChild(statusItem('Players', data.players || '0'));
      box.textContent = '';
      box.appendChild(details);
      updateKickList(server, data.player_list);
    }

    const KICK_PLACEHOLDER = '— Select player to kick —';

    // Rebuild the kick dropdown from the latest player list, keeping the
    // current choice if that same player (id and name) is still connected.
    function updateKickList(server, players) {
      const select = document.getElementById('kick-' + server);
      if (!select) return;
      // Don't yank the list out from under the user while it is open/focused;
      // the next poll will catch up once focus moves on.
      if (document.activeElement === select) return;

      const list = (Array.isArray(players) ? players : []).filter(function (p) {
        return p && Number.isInteger(p.id) && typeof p.name === 'string';
      });
      const signature = JSON.stringify(list.map(function (p) { return [p.id, p.name]; }));
      if (select.dataset.signature === signature) return;
      select.dataset.signature = signature;

      const previous = select.selectedIndex > 0 ? select.options[select.selectedIndex] : null;
      const prevId = previous ? previous.value : '';
      const prevName = previous ? previous.dataset.playerName : '';

      const placeholder = makeEl('option', '', KICK_PLACEHOLDER);
      placeholder.value = '';
      select.textContent = '';
      select.appendChild(placeholder);

      let keep = placeholder;
      list.forEach(function (p) {
        const opt = makeEl('option', '', (p.name || '(no name)') + ' (#' + p.id + ')');
        opt.value = String(p.id);
        opt.dataset.playerName = p.name;
        if (opt.value === prevId && p.name === prevName) keep = opt;
        select.appendChild(opt);
      });
      keep.selected = true;
      syncKickName(select);
    }

    // Mirror the chosen player's name into the hidden "name" field so the
    // server can refuse the kick if that id now belongs to someone else.
    function syncKickName(select) {
      const nameInput = select.form && select.form.querySelector('input[name="name"]');
      if (!nameInput) return;
      const opt = select.options[select.selectedIndex];
      nameInput.value = (opt && opt.value !== '') ? (opt.dataset.playerName || '') : '';
    }

    function confirmKick(form) {
      const select = form.querySelector('select[name="userid"]');
      if (!select || select.value === '') return false; // placeholder selected: nothing to do
      syncKickName(select);
      const name = form.querySelector('input[name="name"]').value;
      const target = form.querySelector('input[name="target"]').value;
      return window.confirm('Kick "' + name + '" (#' + select.value + ') from ' + target + '?');
    }

    // Screen Wake Lock: keep the screen on while this page is visible.
    // Browsers only expose it in secure contexts (HTTPS or localhost) and
    // release the lock whenever the tab is hidden, so re-acquire it on
    // visibilitychange for as long as the user wants it.
    let wakeLock = null;
    let wakeWanted = false;

    function renderWakeButton() {
      const btn = document.getElementById('wake-btn');
      if (!btn) return;
      if (!('wakeLock' in navigator)) {
        btn.disabled = true;
        btn.textContent = 'Keep awake: N/A';
        btn.title = window.isSecureContext ? 'Not supported by this browser' : 'Requires HTTPS';
        return;
      }
      const held = wakeLock !== null && !wakeLock.released;
      btn.classList.toggle('active', held);
      btn.setAttribute('aria-pressed', String(wakeWanted));
      btn.textContent = 'Keep awake: ' + (held ? 'On' : 'Off');
    }

    async function acquireWakeLock() {
      if (!('wakeLock' in navigator) || document.visibilityState !== 'visible') return;
      if (wakeLock && !wakeLock.released) return;
      try {
        const lock = await navigator.wakeLock.request('screen');
        if (!wakeWanted) {
          // Toggled off while the request was pending.
          await lock.release();
          return;
        }
        wakeLock = lock;
        wakeLock.addEventListener('release', renderWakeButton);
      } catch (e) {
        // e.g. battery saver or a browser that needs a user gesture first.
        wakeLock = null;
        console.warn('Wake lock request failed:', e);
      }
      renderWakeButton();
    }

    async function toggleWakeLock() {
      wakeWanted = !wakeWanted;
      try { localStorage.setItem('keepAwake', wakeWanted ? '1' : '0'); } catch (e) {}
      if (wakeWanted) {
        await acquireWakeLock();
      } else if (wakeLock) {
        const lock = wakeLock;
        wakeLock = null;
        await lock.release();
      }
      renderWakeButton();
    }

    document.addEventListener('visibilitychange', function () {
      if (wakeWanted && document.visibilityState === 'visible') acquireWakeLock();
    });

    // Client-side map filter: every whitespace-separated term must appear in
    // the map's title or ID (case-insensitive), so "de dust" finds de_dust2.
    function filterMaps(input) {
      const tab = input.closest('.tabcontent');
      if (!tab) return;
      const terms = input.value.toLowerCase().split(/\s+/).filter(Boolean);
      tab.querySelectorAll('.grid').forEach(function (grid) {
        const items = grid.querySelectorAll('form.map-item');
        let shown = 0;
        items.forEach(function (form) {
          const haystack = (form.dataset.search || '').toLowerCase();
          const match = terms.every(function (t) { return haystack.includes(t); });
          form.hidden = !match;
          if (match) shown++;
        });
        const empty = grid.querySelector('.filter-empty');
        if (empty) empty.hidden = items.length === 0 || shown > 0;
      });
    }

    function mapFilterKeydown(event, input) {
      if (event.key === 'Escape') {
        input.value = '';
        filterMaps(input);
      }
    }

    // "/" focuses the active tab's map filter unless the user is typing somewhere.
    document.addEventListener('keydown', function (e) {
      if (e.key !== '/' || e.ctrlKey || e.metaKey || e.altKey) return;
      const el = document.activeElement;
      if (el && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.tagName === 'SELECT' || el.isContentEditable)) return;
      const input = document.querySelector('.tabcontent.active .map-filter');
      if (input) {
        e.preventDefault();
        input.focus();
      }
    });

    // Workshop map sorting. Each mode reads a data-* attribute rendered from
    // Steam metadata; the stat it sorts by is shown under the map name.
    const mapNumberFormat = new Intl.NumberFormat(undefined, { notation: 'compact', maximumFractionDigits: 1 });
    function mapMonth(seconds) {
      return new Date(seconds * 1000).toLocaleDateString(undefined, { year: 'numeric', month: 'short' });
    }
    const MAP_SORTS = {
      order:   { key: 'order' },
      name:    { key: null },
      subs:    { key: 'subs', desc: true, meta: function (v) { return mapNumberFormat.format(v) + ' subs'; } },
      favs:    { key: 'favs', desc: true, meta: function (v) { return mapNumberFormat.format(v) + ' favs'; } },
      updated: { key: 'updated', desc: true, meta: function (v) { return 'Updated ' + mapMonth(v); } },
      created: { key: 'created', desc: true, meta: function (v) { return 'Added ' + mapMonth(v); } },
    };

    function sortMaps(mode) {
      const sort = Object.prototype.hasOwnProperty.call(MAP_SORTS, mode) ? mode : 'order';
      const spec = MAP_SORTS[sort];
      try { localStorage.setItem('mapSort', sort); } catch (e) {}
      document.querySelectorAll('.map-sort').forEach(function (select) { select.value = sort; });

      const num = function (form, key) { return Number(form.dataset[key]) || 0; };
      const title = function (form) { return form.querySelector('.map-title').textContent; };
      document.querySelectorAll('.workshop-grid').forEach(function (grid) {
        const items = Array.from(grid.querySelectorAll('form.map-item'));
        items.sort(function (a, b) {
          let d;
          if (sort === 'name') {
            d = title(a).localeCompare(title(b), undefined, { numeric: true, sensitivity: 'base' });
          } else {
            d = num(a, spec.key) - num(b, spec.key);
            if (spec.desc) d = -d;
          }
          return d || num(a, 'order') - num(b, 'order');
        });
        const empty = grid.querySelector('.filter-empty');
        items.forEach(function (form) {
          grid.insertBefore(form, empty);
          const meta = form.querySelector('.map-meta');
          const value = spec.key ? num(form, spec.key) : 0;
          meta.textContent = spec.meta && value ? spec.meta(value) : '';
          meta.hidden = !meta.textContent;
        });
      });
    }

    // The page reloads after "Set bots", so remember the last choice per server.
    function rememberBotCount(select) {
      try { localStorage.setItem('botCount:' + select.dataset.server, select.value); } catch (e) {}
    }

    function restoreBotCounts() {
      document.querySelectorAll('.bots-select').forEach(function (select) {
        let saved = null;
        try { saved = localStorage.getItem('botCount:' + select.dataset.server); } catch (e) {}
        if (saved !== null && Array.from(select.options).some(function (o) { return o.value === saved; })) {
          select.value = saved;
        }
      });
    }

    window.onload = function () {
      restoreBotCounts();
      let savedSort = 'order';
      try { savedSort = localStorage.getItem('mapSort') || 'order'; } catch (e) {}
      sortMaps(savedSort);
      // Some browsers restore field values on reload; re-apply any restored filter.
      document.querySelectorAll('.map-filter').forEach(function (input) { if (input.value) filterMaps(input); });

      try { wakeWanted = localStorage.getItem('keepAwake') === '1'; } catch (e) {}
      renderWakeButton();
      if (wakeWanted) acquireWakeLock();

      const last = localStorage.getItem('lastSelectedServer');
      const fallback = document.querySelector('.tabs button')?.id.replace('btn-', '') || '';
      if (last && document.getElementById(last)) { showTab(last); }
      else if (fallback) { showTab(fallback); }
    };
  </script>
</head>
<body>
<header class="topbar">
  <h1 class="visually-hidden">CS2 RCON</h1>
  <nav class="tabs" aria-label="Servers">
    {{range .Servers}}
    <button id="btn-{{.Name}}" onclick="showTab('{{.Name}}')">{{.Name}}</button>
    {{end}}
  </nav>
  <button id="wake-btn" class="bot-button" type="button" aria-pressed="false" onclick="toggleWakeLock()">Keep awake: Off</button>
</header>

{{range .Servers}}
{{$srv := .}}
<div id="{{$srv.Name}}" class="tabcontent">

  <div id="status-{{$srv.Name}}" class="status-box">
    <div class="status-loading">Loading server status...</div>
  </div>

  <div class="toolbar">
    <form method="POST" action="kick" class="kick-form toolbar-left" onsubmit="return confirmKick(this);">
      <input type="hidden" name="target" value="{{$srv.Name}}"/>
      <input type="hidden" name="name" value=""/>
      <select id="kick-{{$srv.Name}}" class="kick-select" name="userid" aria-label="Player to kick" onchange="syncKickName(this)">
        <option value="">— Select player to kick —</option>
      </select>
      <button class="bot-button danger" type="submit">Kick</button>
    </form>
    <form method="POST" action="bots" class="toolbar-right">
      <input type="hidden" name="target" value="{{$srv.Name}}"/>
      <label class="control-label">Bots
        <select class="bots-select" name="quota" data-server="{{$srv.Name}}" onchange="rememberBotCount(this)">
          {{range $n := botCounts}}<option value="{{$n}}"{{if eq $n 10}} selected{{end}}>{{$n}}</option>{{end}}
        </select>
      </label>
      <button class="bot-button" type="submit" name="action" value="quota">Set bots</button>
      <button class="bot-button danger" type="submit" name="action" value="kick" title="Kick all bots (bot_quota 0)">Kick bots</button>
    </form>
  </div>

  <div class="toolbar">
    <div class="toolbar-left">
      <input class="map-filter" type="search" placeholder="Filter maps…" title="Filter maps (press / to focus)" aria-label="Filter maps" autocomplete="off" spellcheck="false" oninput="filterMaps(this)" onkeydown="mapFilterKeydown(event, this)"/>
      <select class="map-sort" aria-label="Sort workshop maps" onchange="sortMaps(this.value)">
        <option value="order">Workshop order</option>
        <option value="name">Name A–Z</option>
        <option value="subs">Most subscribed</option>
        <option value="favs">Most favorited</option>
        <option value="updated">Recently updated</option>
        <option value="created">Newest</option>
      </select>
    </div>
    <form class="workshop-form toolbar-right" method="POST" action="rcon">
      <input type="hidden" name="target" value="{{$srv.Name}}"/>
      <input class="workshop-input" type="text" name="mapid" placeholder="Workshop ID or URL" aria-label="Workshop map ID or URL" required maxlength="256" autocomplete="off" spellcheck="false"/>
      <button class="bot-button" type="submit">Load</button>
    </form>
  </div>

  <h3>Official Maps</h3>
  <div class="grid">
    {{range $srv.OfficialMaps}}
    <form class="map-item" method="POST" action="rcon" data-search="{{.Title}} {{.ID}}">
      <input type="hidden" name="mapid" value="{{.ID}}"/>
      <input type="hidden" name="target" value="{{$srv.Name}}"/>
      <button class="map-button" type="submit">{{.Title}}</button>
    </form>
    {{else}}
    <p style="text-align:center; grid-column: 1 / -1;">No official maps found (or RCON failed).</p>
    {{end}}
    <p class="filter-empty" hidden>No official maps match the filter.</p>
  </div>

  <h3>Workshop Maps</h3>
  <div class="grid workshop-grid">
    {{range $srv.Maps}}
    <form class="map-item" method="POST" action="rcon" data-search="{{.Title}} {{.ID}}" data-order="{{.Order}}" data-subs="{{.Subscriptions}}" data-favs="{{.Favorited}}" data-created="{{.TimeCreated}}" data-updated="{{.TimeUpdated}}">
      <input type="hidden" name="mapid" value="{{.ID}}"/>
      <input type="hidden" name="target" value="{{$srv.Name}}"/>
      <button class="map-button" type="submit"><span class="map-title">{{.Title}}</span><small class="map-meta" hidden></small></button>
    </form>
    {{else}}
    <p style="text-align:center; grid-column: 1 / -1;">No maps found for this collection.</p>
    {{end}}
    <p class="filter-empty" hidden>No workshop maps match the filter.</p>
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
	installed := cache.installedMaps
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
		official := installed[name]
		serverViews = append(serverViews, ServerViewData{
			Name:         name,
			Maps:         maps,
			OfficialMaps: official,
		})
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
	if !readPostForm(w, r) {
		return
	}
	// Validate everything before dialing RCON.
	target, err := lookupTarget(r.PostFormValue("target"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cmd, err := parseMapID(r.PostFormValue("mapid"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	rconConn, err := rcon.Dial(target.Address, target.Password)
	if err != nil {
		log.Println("RCON dial error:", err)
		http.Error(w, "RCON connection failed", http.StatusInternalServerError)
		return
	}
	defer func() { _ = rconConn.Close() }()

	if _, err := rconConn.Execute(cmd); err != nil {
		log.Println("RCON exec error:", err)
		http.Error(w, "RCON execution failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, webPath, http.StatusSeeOther)
}

func botsHandler(w http.ResponseWriter, r *http.Request) {
	if !readPostForm(w, r) {
		return
	}
	// Validate everything before dialing RCON.
	target, err := lookupTarget(r.PostFormValue("target"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var cmds []string
	switch strings.TrimSpace(r.PostFormValue("action")) {
	case "kick":
		cmds = []string{"bot_quota 0"}
	case "quota":
		quota, err := parseQuota(r.PostFormValue("quota"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		cmds = []string{"bot_quota_mode fill", "bot_quota " + strconv.Itoa(quota)}
	default:
		http.Error(w, "invalid action: expected kick or quota", http.StatusBadRequest)
		return
	}

	conn, err := rcon.Dial(target.Address, target.Password)
	if err != nil {
		log.Printf("botsHandler dial error: %v", err)
		http.Error(w, "RCON connection failed", http.StatusInternalServerError)
		return
	}
	defer func() { _ = conn.Close() }()

	for _, c := range cmds {
		if _, err := conn.Execute(c); err != nil {
			log.Printf("botsHandler exec error: %v", err)
			http.Error(w, "RCON execution failed", http.StatusInternalServerError)
			return
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
	// Every response from this endpoint is JSON, including errors. The body
	// carries attacker-controlled player names, so never let it be sniffed.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	status := ServerStatus{Online: false, PlayerList: []Player{}}
	writeStatus := func(code int) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(status)
	}

	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		status.Error = "Method not allowed"
		writeStatus(http.StatusMethodNotAllowed)
		return
	}
	targetName := r.URL.Query().Get("target")
	target, ok := rconTargets[targetName]
	if !ok {
		status.Error = "Unknown server"
		writeStatus(http.StatusBadRequest)
		return
	}

	conn, err := rcon.Dial(target.Address, target.Password)
	if err != nil {
		status.Error = "Connection failed"
		writeStatus(http.StatusOK)
		return
	}
	defer func() { _ = conn.Close() }()

	resp, err := conn.Execute("status")
	if err != nil {
		status.Error = "Command failed"
		writeStatus(http.StatusOK)
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
	status.PlayerList = parseStatusPlayers(resp)

	writeStatus(http.StatusOK)
}

func apiMapsHandler(w http.ResponseWriter, r *http.Request) {
	cache.RLock()
	cols := cache.collections
	cache.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(cols)
}

func apiLoadMapHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Server string `json:"server"`
		MapID  string `json:"map_id"`
	}
	if !readJSONPost(w, r, &req) {
		return
	}
	// Validate everything before dialing RCON.
	target, err := lookupTarget(req.Server)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cmd, err := parseMapID(req.MapID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	conn, err := rcon.Dial(target.Address, target.Password)
	if err != nil {
		log.Printf("apiLoadMapHandler dial error: %v", err)
		http.Error(w, "RCON connection failed", http.StatusInternalServerError)
		return
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Execute(cmd); err != nil {
		log.Printf("apiLoadMapHandler exec error: %v", err)
		http.Error(w, "RCON execution failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprint(w, `{"status":"ok"}`)
}

// newRouter registers all routes under webPath and wraps them in cross-origin
// protection, so another website can't make a visitor's browser POST to us.
// Non-browser clients (curl, Home Assistant) send no Sec-Fetch-Site/Origin
// headers and are unaffected. trustedOrigins (e.g. "https://ha.example.com")
// are additionally allowed to make cross-origin POSTs.
func newRouter(trustedOrigins []string) (http.Handler, error) {
	mux := http.NewServeMux()
	mux.HandleFunc(webPath, indexHandler)
	mux.HandleFunc(webPath+"rcon", rconHandler)
	mux.HandleFunc(webPath+"bots", botsHandler)
	mux.HandleFunc(webPath+"kick", kickHandler)

	mux.HandleFunc(webPath+"api/maps", apiMapsHandler)
	mux.HandleFunc(webPath+"api/load", apiLoadMapHandler)
	mux.HandleFunc(webPath+"api/status", apiStatusHandler)

	cop := http.NewCrossOriginProtection()
	for _, origin := range trustedOrigins {
		if err := cop.AddTrustedOrigin(origin); err != nil {
			return nil, fmt.Errorf("invalid trusted origin %q: %v", origin, err)
		}
	}
	return cop.Handler(mux), nil
}

func main() {
	if len(rconTargets) == 0 {
		log.Fatal("No RCON targets configured.")
	}

	log.Printf("Starting CS2 Map RCON. Debug Mode: %v", debugMode)

	// For serving it as a subdirectory
	if os.Getenv("WEB_PATH") != "" {
		webPath = normalizeWebPath(os.Getenv("WEB_PATH"))
	}

	// Comma- or space-separated list of extra origins allowed to POST cross-site.
	trusted := strings.Fields(strings.ReplaceAll(os.Getenv("TRUSTED_ORIGINS"), ",", " "))
	router, err := newRouter(trusted)
	if err != nil {
		log.Fatal(err)
	}

	go updater()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Println("Listening on :" + port + webPath)
	log.Fatal(http.ListenAndServe(":"+port, router))
}
