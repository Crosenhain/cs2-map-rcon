package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// --- Input limits ---

const (
	maxRequestBodyBytes = 4 << 10 // POST bodies (form or JSON) are tiny; 4 KiB is plenty
	maxMapNameLen       = 64      // official map names are far shorter
	maxMapIDInputLen    = 256     // raw mapid input (ID, URL or name); keep in sync with the template's maxlength
	maxWorkshopIDDigits = 20      // a uint64 has at most 20 digits
	maxBotQuota         = 64      // CS2 max players
)

var (
	// Map names start alphanumeric so they can never look like a flag (e.g. "-5").
	reValidMapName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)
	reAllDigits    = regexp.MustCompile(`^[0-9]+$`)
)

// Workshop URL hosts and paths we accept, e.g.
// https://steamcommunity.com/sharedfiles/filedetails/?id=123456789
var (
	workshopHosts = map[string]bool{
		"steamcommunity.com":     true,
		"www.steamcommunity.com": true,
	}
	workshopPaths = map[string]bool{
		"/sharedfiles/filedetails/": true,
		"/sharedfiles/filedetails":  true,
		"/workshop/filedetails/":    true,
		"/workshop/filedetails":     true,
	}
)

var errInvalidMapID = errors.New("invalid map ID: expected a Workshop ID, Steam Workshop URL or official map name")

// --- Field validation ---

// isValidMapName reports whether name looks like a plain map name (e.g. de_dust2).
func isValidMapName(name string) bool {
	return len(name) <= maxMapNameLen && reValidMapName.MatchString(name)
}

// lookupTarget returns the configured RCON target with the given name.
func lookupTarget(name string) (TargetServer, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return TargetServer{}, errors.New("missing target")
	}
	target, ok := rconTargets[name]
	if !ok {
		return TargetServer{}, errors.New("unknown RCON target")
	}
	return target, nil
}

// parseMapID validates a map selection and returns the RCON command to load it.
// Accepted: a numeric Workshop ID, a Steam Workshop URL, or an official map name.
func parseMapID(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	switch {
	case s == "":
		return "", errors.New("missing map ID")
	case len(s) > maxMapIDInputLen:
		return "", errInvalidMapID
	case reAllDigits.MatchString(s):
		// Numeric input is always a Workshop ID, never a map name.
		id, err := parseWorkshopID(s)
		if err != nil {
			return "", err
		}
		return "host_workshop_map " + id, nil
	case isValidMapName(s):
		return "map " + s, nil
	}
	id, err := workshopIDFromURL(s)
	if err != nil {
		return "", err
	}
	return "host_workshop_map " + id, nil
}

// parseWorkshopID validates a numeric Workshop ID and returns it in canonical form.
func parseWorkshopID(s string) (string, error) {
	if len(s) > maxWorkshopIDDigits || !reAllDigits.MatchString(s) {
		return "", errors.New("invalid Workshop ID")
	}
	id, err := strconv.ParseUint(s, 10, 64)
	if err != nil || id == 0 {
		return "", errors.New("invalid Workshop ID")
	}
	return strconv.FormatUint(id, 10), nil
}

// workshopIDFromURL extracts the Workshop ID from a Steam Workshop item URL.
func workshopIDFromURL(s string) (string, error) {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil ||
		!workshopHosts[strings.ToLower(u.Host)] || !workshopPaths[u.Path] {
		return "", errInvalidMapID
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(q["id"]) != 1 {
		return "", errors.New("invalid Workshop URL: expected a single ?id=<number>")
	}
	return parseWorkshopID(q.Get("id"))
}

// parseQuota validates a bot quota (0..maxBotQuota).
func parseQuota(raw string) (int, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, errors.New("missing quota")
	}
	// Digits only (no sign); the length cap keeps Atoi well within range.
	if len(s) <= 3 && reAllDigits.MatchString(s) {
		if n, err := strconv.Atoi(s); err == nil && n <= maxBotQuota {
			return n, nil
		}
	}
	return 0, fmt.Errorf("invalid quota: must be a number from 0 to %d", maxBotQuota)
}

// --- Request parsing ---

// requirePost rejects non-POST requests and caps the request body size.
// On failure it writes the error response and returns false.
func requirePost(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	return true
}

// readPostForm enforces POST and a body size limit, then parses the form body.
// Handlers must read values via r.PostFormValue so URL query params are ignored.
// On failure it writes the error response and returns false.
func readPostForm(w http.ResponseWriter, r *http.Request) bool {
	if !requirePost(w, r) {
		return false
	}
	var err error
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct == "multipart/form-data" {
		// Still accept multipart (e.g. curl -F); it also calls ParseForm.
		err = r.ParseMultipartForm(maxRequestBodyBytes)
	} else {
		err = r.ParseForm()
	}
	if err != nil {
		writeBodyError(w, err, "Invalid form data")
		return false
	}
	return true
}

// readJSONPost enforces POST and a body size limit, then decodes a single JSON
// object into dst. Unknown fields are ignored (external clients may send extras),
// but trailing data after the object is rejected.
// On failure it writes the error response and returns false.
func readJSONPost(w http.ResponseWriter, r *http.Request, dst any) bool {
	if !requirePost(w, r) {
		return false
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		writeBodyError(w, err, "Invalid JSON body")
		return false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeBodyError(w, err, "Invalid JSON body: unexpected data after object")
		return false
	}
	return true
}

// writeBodyError maps a body read/parse error to 413 or 400.
func writeBodyError(w http.ResponseWriter, err error, msg string) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	http.Error(w, msg, http.StatusBadRequest)
}
