package api

import (
	"net/http"
	"os"
	"strings"
)

// LocaleConfigPath is the file the installer writes the operator's
// chosen language to during firstboot. The web GUI fetches this
// before the user has authenticated so the login screen renders in
// the same language the installer ran in.
//
// One-line plain-text file containing only the language code
// (e.g. "nl"). Anything else falls back to "en".
const LocaleConfigPath = "/etc/smoothnas/locale"

// LocaleHandler serves GET /api/locale unauthenticated. The login
// screen needs the operator's installer-chosen language before it
// can authenticate.
type LocaleHandler struct {
	configPath string
}

func NewLocaleHandler() *LocaleHandler {
	return &LocaleHandler{configPath: LocaleConfigPath}
}

func (h *LocaleHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonMethodNotAllowed(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	lang := readSystemLocale(h.configPath)
	if lang == "" {
		w.Write([]byte(`{"language":""}`))
		return
	}
	w.Write([]byte(`{"language":"` + lang + `"}`))
}

// readSystemLocale reads the installer-written locale file. It
// returns "" if the file is missing, unreadable, or contains
// anything that doesn't look like a 2- or 5-letter language tag
// (e.g. "en", "nl", "en-US"). The strict whitelist keeps a
// corrupt/attacker-controlled file from sneaking JSON-injectable
// strings into the response.
func readSystemLocale(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lang := strings.TrimSpace(string(data))
	if lang == "" {
		return ""
	}
	// Only allow lowercase letters and one optional hyphen +
	// region: 'en', 'nl', 'en-US', 'en-us'. Anything else is
	// treated as missing.
	if !validLocaleTag(lang) {
		return ""
	}
	return lang
}

func validLocaleTag(s string) bool {
	if len(s) < 2 || len(s) > 5 {
		return false
	}
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z':
			// ok
		case c >= 'A' && c <= 'Z':
			// ok (regions)
		case c == '-':
			if i == 0 || i == len(s)-1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
