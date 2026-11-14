package check

import (
	"regexp"
	"strings"

	"github.com/ClearedStack/shakedown/internal/js"
)

// Names that reach the network once the page has loaded.
var networkNames = map[string]bool{"fetch": true, "XMLHttpRequest": true, "WebSocket": true, "EventSource": true,
	"RTCPeerConnection": true, "WebTransport": true, "importScripts": true}

var cookieNames = map[string]bool{"cookie": true, "cookieStore": true}

var storageNames = map[string]bool{"localStorage": true, "sessionStorage": true, "indexedDB": true}

var remoteStringRE = regexp.MustCompile(`^(?i:https?:|wss?:|ftp:)?//`)

type scriptFacts struct {
	storageFiles []string // scripts that touch browser storage
}

// scanScripts reads every script in the bundle, whether or not a page uses
// it. The scan is of names and strings as written: it does not follow code
// that builds a name at run time. The policy check is what stops that.
func (c *checker) scanScripts(b *bundle) scriptFacts {
	var facts scriptFacts
	for _, p := range b.withExt(".js") {
		if c.done() {
			break
		}
		prog, err := js.Parse(p, string(b.data[p]))
		if err != nil {
			c.add(wholeBundle, "BUNDLE-SCRIPT", EParse, p, "the script could not be parsed")
			continue
		}
		storage := false
		see := func(s string) {
			switch {
			case networkNames[s]:
				c.add(Offline, "OFF-NETWORK-API", EParse, p, "uses "+s+", which reaches the network after the page has loaded")
			case s == "sendBeacon":
				c.add(Tracking, "TRK-BEACON", EParse, p, "uses sendBeacon")
			case cookieNames[s]:
				c.add(Tracking, "TRK-COOKIE", EParse, p, "reads or writes cookies")
			case storageNames[s]:
				storage = true
			}
		}
		for _, id := range prog.Identifiers {
			see(id)
		}
		for _, s := range prog.Strings {
			see(s)
			if remoteStringRE.MatchString(strings.TrimSpace(s)) {
				c.add(Offline, "OFF-REMOTE-STRING", EParse, p, "contains an address outside the bundle")
			}
		}
		if storage {
			facts.storageFiles = append(facts.storageFiles, p)
		}
	}
	return facts
}

var (
	cssCommentRE = regexp.MustCompile(`(?s)/\*.*?\*/`)
	cssURLRE     = regexp.MustCompile(`(?i)url\(\s*(?:"([^"]*)"|'([^']*)'|([^)'"]*))\s*\)`)
	cssImportRE  = regexp.MustCompile(`(?i)@import\s+(?:"([^"]*)"|'([^']*)')`)
)

func (c *checker) scanStyles(b *bundle) {
	for _, p := range b.withExt(".css") {
		if c.done() {
			break
		}
		css := cssCommentRE.ReplaceAllString(string(b.data[p]), " ")
		if strings.Contains(css, `\`) {
			c.add(Offline, "OFF-UNFAMILIAR-REFERENCE", ENeedsSubtool, p, "no rule covers escapes in a style sheet")
			continue
		}
		for _, re := range []*regexp.Regexp{cssURLRE, cssImportRE} {
			for _, m := range re.FindAllStringSubmatch(css, -1) {
				c.reportRef(b, p, strings.Join(m[1:], ""), false)
			}
		}
	}
}

func (c *checker) checkTracking(entry *page, entryPath string, facts scriptFacts) {
	if len(facts.storageFiles) == 0 || entry == nil {
		return
	}
	disclosed := false
	for _, m := range entry.find(markStorage) {
		disclosed = disclosed || m.text != ""
	}
	if !disclosed {
		c.add(Tracking, "TRK-STORAGE-DISCLOSURE", EParse, entryPath, "a script uses browser storage and the page does not disclose it")
	}
	control := false
	for _, m := range entry.find(markClear) {
		control = control || m.tag == "button"
	}
	if !control {
		c.add(Tracking, "TRK-STORAGE-CLEAR", EParse, entryPath, "a script uses browser storage and the page has no clear-data button")
	}
}
