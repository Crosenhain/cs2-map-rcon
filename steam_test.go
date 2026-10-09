package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeSteam serves canned GetCollectionDetails / GetPublishedFileDetails replies.
func fakeSteam(t *testing.T, collection, details string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ISteamRemoteStorage/GetCollectionDetails/v1/":
			_, _ = w.Write([]byte(collection))
		case "/ISteamRemoteStorage/GetPublishedFileDetails/v1/":
			_, _ = w.Write([]byte(details))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := steamAPIBase
	steamAPIBase = srv.URL
	t.Cleanup(func() { steamAPIBase = old })
}

func TestFetchCollectionUsesSortOrder(t *testing.T) {
	fakeSteam(t, `{"response":{"collectiondetails":[{"children":[
		{"publishedfileid":"30","sortorder":2},
		{"publishedfileid":"10","sortorder":0},
		{"publishedfileid":"20","sortorder":1}]}]}}`, "")

	ids, err := fetchCollection("1")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ids, ","); got != "10,20,30" {
		t.Fatalf("ids = %s, want 10,20,30", got)
	}
}

func TestFetchDetailsMetadataAndOrder(t *testing.T) {
	// Steam returns the items out of request order and one without a title.
	fakeSteam(t, "", `{"response":{"publishedfiledetails":[
		{"publishedfileid":"20","title":"Bank","subscriptions":14757,"favorited":235,"time_created":1699005876,"time_updated":1759434029},
		{"publishedfileid":"10","title":"Agency","subscriptions":40088,"favorited":1383,"time_created":1727616194,"time_updated":1757240271},
		{"publishedfileid":"30","result":9}]}}`)

	maps, err := fetchDetails([]string{"10", "20", "30"})
	if err != nil {
		t.Fatal(err)
	}
	want := []MapInfo{
		{ID: "10", Title: "Agency", Order: 0, Subscriptions: 40088, Favorited: 1383, TimeCreated: 1727616194, TimeUpdated: 1757240271},
		{ID: "20", Title: "Bank", Order: 1, Subscriptions: 14757, Favorited: 235, TimeCreated: 1699005876, TimeUpdated: 1759434029},
		{ID: "30", Title: "[No Title] (30)", Order: 2},
	}
	if len(maps) != len(want) {
		t.Fatalf("got %d maps, want %d: %+v", len(maps), len(want), maps)
	}
	for i := range want {
		if maps[i] != want[i] {
			t.Errorf("maps[%d] = %+v, want %+v", i, maps[i], want[i])
		}
	}
}

func TestIndexWorkshopSortData(t *testing.T) {
	setTestTargets(t, map[string]TargetServer{"alpha": {Name: "alpha", CollectionID: "c1"}})
	cache.Lock()
	oldCollections := cache.collections
	cache.collections = map[string][]MapInfo{"c1": {{ID: "10", Title: "Agency", Order: 3, Subscriptions: 40088, Favorited: 1383, TimeCreated: 1727616194, TimeUpdated: 1757240271}}}
	cache.Unlock()
	t.Cleanup(func() {
		cache.Lock()
		cache.collections = oldCollections
		cache.Unlock()
	})

	rec := httptest.NewRecorder()
	indexHandler(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	page := rec.Body.String()
	for _, want := range []string{
		`<select class="map-sort" aria-label="Sort workshop maps"`,
		`<div class="grid workshop-grid">`,
		`data-order="3" data-subs="40088" data-favs="1383" data-created="1727616194" data-updated="1757240271"`,
		`<span class="map-title">Agency</span><small class="map-meta" hidden></small>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("index page missing %q", want)
		}
	}
}
