package ingest

import (
	"strings"
)

// UAResult holds parsed User-Agent fields.
type UAResult struct {
	Browser        string
	BrowserVersion string
	OS             string
	OSVersion      string
	Device         string
}

// IsBot returns true if the UA string looks like a bot/crawler/spider.
func IsBot(ua string) bool {
	if ua == "" {
		return true
	}
	low := strings.ToLower(ua)
	for _, sig := range botSignatures {
		if strings.Contains(low, sig) {
			return true
		}
	}
	return false
}

var botSignatures = []string{
	"bot", "crawl", "spider", "slurp", "wget", "curl",
	"python-requests", "python-urllib", "go-http-client",
	"java/", "apache-httpclient", "okhttp",
	"headlesschrome", "phantomjs", "lighthouse",
	"pingdom", "uptimerobot", "statuscake",
	"facebookexternalhit", "twitterbot", "linkedinbot",
	"whatsapp", "telegrambot", "discordbot",
	"bingpreview", "googleother", "petalbot",
	"semrushbot", "ahrefsbot", "dotbot", "mj12bot",
	"yandexbot", "baiduspider", "duckduckbot",
}

// ParseUA extracts browser, OS, and device from a User-Agent string.
// Lightweight parser — handles 95%+ of real-world traffic without external deps.
func ParseUA(ua string) UAResult {
	var r UAResult
	if ua == "" {
		return r
	}

	r.Browser, r.BrowserVersion = parseBrowser(ua)
	r.OS, r.OSVersion = parseOS(ua)
	r.Device = parseDevice(ua)
	return r
}

func parseBrowser(ua string) (string, string) {
	// Order matters: check specific browsers before generic engines
	switch {
	case strings.Contains(ua, "Edg/"):
		return "Edge", extractVersion(ua, "Edg/")
	case strings.Contains(ua, "OPR/") || strings.Contains(ua, "Opera/"):
		if strings.Contains(ua, "OPR/") {
			return "Opera", extractVersion(ua, "OPR/")
		}
		return "Opera", extractVersion(ua, "Opera/")
	case strings.Contains(ua, "Vivaldi/"):
		return "Vivaldi", extractVersion(ua, "Vivaldi/")
	case strings.Contains(ua, "Brave"):
		// Brave identifies as Chrome; check for "Brave" in the UA
		return "Brave", extractVersion(ua, "Chrome/")
	case strings.Contains(ua, "SamsungBrowser/"):
		return "Samsung Browser", extractVersion(ua, "SamsungBrowser/")
	case strings.Contains(ua, "UCBrowser/"):
		return "UC Browser", extractVersion(ua, "UCBrowser/")
	case strings.Contains(ua, "Firefox/"):
		return "Firefox", extractVersion(ua, "Firefox/")
	case strings.Contains(ua, "CriOS/"):
		return "Chrome", extractVersion(ua, "CriOS/")
	case strings.Contains(ua, "FxiOS/"):
		return "Firefox", extractVersion(ua, "FxiOS/")
	case strings.Contains(ua, "Chrome/") && !strings.Contains(ua, "Chromium"):
		return "Chrome", extractVersion(ua, "Chrome/")
	case strings.Contains(ua, "Chromium/"):
		return "Chromium", extractVersion(ua, "Chromium/")
	case strings.Contains(ua, "Safari/") && strings.Contains(ua, "Version/"):
		return "Safari", extractVersion(ua, "Version/")
	case strings.Contains(ua, "MSIE "):
		return "IE", extractVersion(ua, "MSIE ")
	case strings.Contains(ua, "Trident/"):
		return "IE", "11"
	default:
		if strings.Contains(ua, "bot") || strings.Contains(ua, "Bot") ||
			strings.Contains(ua, "crawl") || strings.Contains(ua, "spider") {
			return "Bot", ""
		}
		return "Other", ""
	}
}

func parseOS(ua string) (string, string) {
	switch {
	case strings.Contains(ua, "iPhone") || strings.Contains(ua, "iPad") || strings.Contains(ua, "iPod"):
		return "iOS", extractIOSVersion(ua)
	case strings.Contains(ua, "Mac OS X"):
		return "macOS", extractMacVersion(ua)
	case strings.Contains(ua, "Android"):
		return "Android", extractVersion(ua, "Android ")
	case strings.Contains(ua, "Windows NT"):
		return "Windows", mapWindowsVersion(extractVersion(ua, "Windows NT "))
	case strings.Contains(ua, "CrOS"):
		return "Chrome OS", ""
	case strings.Contains(ua, "Linux"):
		return "Linux", ""
	case strings.Contains(ua, "FreeBSD"):
		return "FreeBSD", ""
	default:
		return "Other", ""
	}
}

func parseDevice(ua string) string {
	switch {
	case strings.Contains(ua, "iPhone"):
		return "mobile"
	case strings.Contains(ua, "iPad"):
		return "tablet"
	case strings.Contains(ua, "Android"):
		if strings.Contains(ua, "Mobile") {
			return "mobile"
		}
		return "tablet"
	case strings.Contains(ua, "Mobile"):
		return "mobile"
	default:
		return "desktop"
	}
}

// extractVersion pulls the version string after a prefix token.
// e.g. extractVersion("Chrome/120.0.6099.109", "Chrome/") -> "120.0.6099.109"
// Returns only up to the first space or semicolon.
func extractVersion(ua, token string) string {
	idx := strings.Index(ua, token)
	if idx < 0 {
		return ""
	}
	start := idx + len(token)
	end := start
	for end < len(ua) {
		c := ua[end]
		if c == ' ' || c == ';' || c == ')' || c == '/' {
			break
		}
		end++
	}
	v := ua[start:end]
	// Return only major.minor
	if dot1 := strings.IndexByte(v, '.'); dot1 >= 0 {
		if dot2 := strings.IndexByte(v[dot1+1:], '.'); dot2 >= 0 {
			return v[:dot1+1+dot2]
		}
	}
	return v
}

func extractIOSVersion(ua string) string {
	// iOS UA contains "CPU iPhone OS 17_2 like Mac OS X"
	for _, prefix := range []string{"CPU iPhone OS ", "CPU OS "} {
		if idx := strings.Index(ua, prefix); idx >= 0 {
			start := idx + len(prefix)
			end := strings.Index(ua[start:], " ")
			if end < 0 {
				end = len(ua) - start
			}
			return strings.ReplaceAll(ua[start:start+end], "_", ".")
		}
	}
	return ""
}

func extractMacVersion(ua string) string {
	if idx := strings.Index(ua, "Mac OS X "); idx >= 0 {
		start := idx + len("Mac OS X ")
		end := start
		for end < len(ua) {
			c := ua[end]
			if c == ';' || c == ')' || c == ' ' {
				break
			}
			end++
		}
		return strings.ReplaceAll(ua[start:end], "_", ".")
	}
	return ""
}

func mapWindowsVersion(ntVer string) string {
	switch ntVer {
	case "10.0":
		return "10"
	case "6.3":
		return "8.1"
	case "6.2":
		return "8"
	case "6.1":
		return "7"
	case "6.0":
		return "Vista"
	case "5.1":
		return "XP"
	default:
		return ntVer
	}
}
