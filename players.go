package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gorcon/rcon"
)

// Player is a human player parsed from the CS2 RCON `status` output.
type Player struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

const (
	// kickMaxUserIDDigits / kickMaxUserID bound the user id accepted by the kick
	// handler. CS2 user ids fit in 16 bits and 65535 is the "[NoChan]"
	// placeholder row, which is never a kickable player.
	kickMaxUserIDDigits = 5
	kickMaxUserID       = 65534

	// statusPlaceholderID is the id CS2 prints for the "[NoChan]" pseudo row.
	statusPlaceholderID = 65535
)

// parseStatusPlayers extracts the human players from CS2 `status` output.
//
// The relevant part of the output looks like this:
//
//	---------players--------
//	  id     time ping loss      state   rate adr name
//	65535 [NoChan]    0    0 challenging      0unknown ''
//	    1      BOT    0    0     active      0 'Rezan'
//	    2    03:12   24    0     active 786432 203.0.113.7:27005 'Some Player'
//	#end
//
// Bot rows and the 65535 "[NoChan]" placeholder are skipped. Columns before
// the name never contain a single quote, so the name is everything between
// the first ' on the line (the one following the address column) and the
// final ' on the line; this keeps names containing spaces or quotes intact.
// Lines that do not look like a player row are ignored. The result is never
// nil so it always encodes as a JSON array.
func parseStatusPlayers(status string) []Player {
	players := []Player{}
	inPlayers := false

	for _, raw := range strings.Split(status, "\n") {
		line := strings.TrimSpace(raw)

		if !inPlayers {
			if strings.HasPrefix(line, "---") && strings.Contains(line, "players") {
				inPlayers = true
			}
			continue
		}

		if strings.HasPrefix(line, "#end") || strings.HasPrefix(line, "---") {
			break
		}

		if p, ok := parseStatusPlayerLine(line); ok {
			players = append(players, p)
		}
	}

	return players
}

// parseStatusPlayerLine parses a single (already trimmed) row of the players
// section, returning ok=false for headers, bots, placeholders and anything
// malformed.
func parseStatusPlayerLine(line string) (Player, bool) {
	first := strings.IndexByte(line, '\'')
	last := strings.LastIndexByte(line, '\'')
	if first < 0 || last <= first || last != len(line)-1 {
		return Player{}, false
	}

	// id, time, ping, loss, state, rate and adr. The rate and adr columns can
	// run together (e.g. "0unknown" on the placeholder row), so accept 6 or 7.
	cols := strings.Fields(line[:first])
	if len(cols) < 6 || len(cols) > 7 {
		return Player{}, false
	}
	if cols[1] == "BOT" || cols[1] == "[NoChan]" {
		return Player{}, false
	}
	// Humans have a connection time such as "03:12" or "1:03:12" and numeric
	// ping/loss columns; anything else is not a player row.
	if !statusIsDigits(cols[0]) || !statusIsConnTime(cols[1]) || !statusIsDigits(cols[2]) || !statusIsDigits(cols[3]) {
		return Player{}, false
	}

	id, err := strconv.Atoi(cols[0])
	if err != nil || id >= statusPlaceholderID {
		return Player{}, false
	}

	// Keep names valid UTF-8 so they survive the JSON round trip through the
	// browser unchanged and still match on the kick name check.
	name := strings.ToValidUTF8(line[first+1:last], string(utf8.RuneError))
	return Player{ID: id, Name: name}, true
}

// statusIsDigits reports whether s is non-empty and only ASCII digits.
func statusIsDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// statusIsConnTime reports whether s looks like a status connection time,
// i.e. digit groups separated by colons ("03:12", "1:03:12").
func statusIsConnTime(s string) bool {
	parts := strings.Split(s, ":")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if !statusIsDigits(part) {
			return false
		}
	}
	return true
}

// parseKickUserID validates the user id submitted by the kick form: ASCII
// digits only, at most kickMaxUserIDDigits long and no larger than
// kickMaxUserID.
func parseKickUserID(s string) (int, error) {
	if len(s) > kickMaxUserIDDigits || !statusIsDigits(s) {
		return 0, errors.New("user id must be a number")
	}
	id, err := strconv.Atoi(s)
	if err != nil || id > kickMaxUserID {
		return 0, errors.New("user id out of range")
	}
	return id, nil
}

// kickHandler kicks a single human player by the user id shown in `status`.
//
// Before kicking it re-reads `status` and only proceeds if that id is still on
// the server and, when a name was submitted, still belongs to a player with
// that name. User ids get reused, so this stops a stale page from kicking
// whoever has inherited the id since.
func kickHandler(w http.ResponseWriter, r *http.Request) {
	if !readPostForm(w, r) {
		return
	}

	target, err := lookupTarget(r.PostFormValue("target"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	userIDStr := strings.TrimSpace(r.PostFormValue("userid"))
	if userIDStr == "" {
		// The "Select player to kick" placeholder was submitted: nothing to do.
		http.Redirect(w, r, webPath, http.StatusSeeOther)
		return
	}
	userID, err := parseKickUserID(userIDStr)
	if err != nil {
		http.Error(w, "Invalid player id: "+err.Error(), http.StatusBadRequest)
		return
	}
	expectedName := r.PostFormValue("name")

	conn, err := rcon.Dial(target.Address, target.Password)
	if err != nil {
		log.Printf("kickHandler dial error for %s: %v", target.Name, err)
		http.Error(w, "RCON connection failed", http.StatusInternalServerError)
		return
	}
	defer func() { _ = conn.Close() }()

	status, err := conn.Execute("status")
	if err != nil {
		log.Printf("kickHandler status error for %s: %v", target.Name, err)
		http.Error(w, "RCON status command failed", http.StatusInternalServerError)
		return
	}

	var player Player
	found := false
	for _, p := range parseStatusPlayers(status) {
		if p.ID == userID {
			player, found = p, true
			break
		}
	}
	if !found {
		http.Error(w, fmt.Sprintf("Player id %d is no longer on the server. Reload the page and try again.", userID), http.StatusConflict)
		return
	}
	if expectedName != "" && player.Name != expectedName {
		http.Error(w, fmt.Sprintf("Player id %d now belongs to a different player. Reload the page and try again.", userID), http.StatusConflict)
		return
	}

	log.Printf("Kicking player from %s: id=%d name=%q", target.Name, player.ID, player.Name)
	if _, err := conn.Execute(fmt.Sprintf("kickid %d", player.ID)); err != nil {
		log.Printf("kickHandler kickid error for %s: %v", target.Name, err)
		http.Error(w, "RCON kick command failed", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, webPath, http.StatusSeeOther)
}
