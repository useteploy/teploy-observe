package ingest

import "testing"

func TestParseUA_Chrome(t *testing.T) {
	ua := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.6099.109 Safari/537.36"
	r := ParseUA(ua)
	if r.Browser != "Chrome" {
		t.Errorf("browser = %q, want Chrome", r.Browser)
	}
	if r.BrowserVersion != "120.0" {
		t.Errorf("version = %q, want 120.0", r.BrowserVersion)
	}
	if r.OS != "Windows" {
		t.Errorf("os = %q, want Windows", r.OS)
	}
	if r.OSVersion != "10" {
		t.Errorf("os_version = %q, want 10", r.OSVersion)
	}
	if r.Device != "desktop" {
		t.Errorf("device = %q, want desktop", r.Device)
	}
}

func TestParseUA_Safari_iOS(t *testing.T) {
	ua := "Mozilla/5.0 (iPhone; CPU iPhone OS 17_2 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.2 Mobile/15E148 Safari/604.1"
	r := ParseUA(ua)
	if r.Browser != "Safari" {
		t.Errorf("browser = %q, want Safari", r.Browser)
	}
	if r.OS != "iOS" {
		t.Errorf("os = %q, want iOS", r.OS)
	}
	if r.Device != "mobile" {
		t.Errorf("device = %q, want mobile", r.Device)
	}
}

func TestParseUA_Firefox_Linux(t *testing.T) {
	ua := "Mozilla/5.0 (X11; Linux x86_64; rv:121.0) Gecko/20100101 Firefox/121.0"
	r := ParseUA(ua)
	if r.Browser != "Firefox" {
		t.Errorf("browser = %q, want Firefox", r.Browser)
	}
	if r.OS != "Linux" {
		t.Errorf("os = %q, want Linux", r.OS)
	}
	if r.Device != "desktop" {
		t.Errorf("device = %q, want desktop", r.Device)
	}
}

func TestParseUA_Edge(t *testing.T) {
	ua := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36 Edg/120.0.2210.91"
	r := ParseUA(ua)
	if r.Browser != "Edge" {
		t.Errorf("browser = %q, want Edge", r.Browser)
	}
}

func TestParseUA_Android_Mobile(t *testing.T) {
	ua := "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.6099.43 Mobile Safari/537.36"
	r := ParseUA(ua)
	if r.OS != "Android" {
		t.Errorf("os = %q, want Android", r.OS)
	}
	if r.Device != "mobile" {
		t.Errorf("device = %q, want mobile", r.Device)
	}
}

func TestParseUA_Android_Tablet(t *testing.T) {
	ua := "Mozilla/5.0 (Linux; Android 14; SM-X200) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.6099.43 Safari/537.36"
	r := ParseUA(ua)
	if r.OS != "Android" {
		t.Errorf("os = %q, want Android", r.OS)
	}
	if r.Device != "tablet" {
		t.Errorf("device = %q, want tablet", r.Device)
	}
}

func TestParseUA_MacOS_Safari(t *testing.T) {
	ua := "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_2) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.2 Safari/605.1.15"
	r := ParseUA(ua)
	if r.Browser != "Safari" {
		t.Errorf("browser = %q, want Safari", r.Browser)
	}
	if r.OS != "macOS" {
		t.Errorf("os = %q, want macOS", r.OS)
	}
	if r.OSVersion != "14.2" {
		t.Errorf("os_version = %q, want 14.2", r.OSVersion)
	}
}

func TestParseUA_Bot(t *testing.T) {
	ua := "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)"
	r := ParseUA(ua)
	if r.Browser != "Bot" {
		t.Errorf("browser = %q, want Bot", r.Browser)
	}
}

func TestParseUA_Empty(t *testing.T) {
	r := ParseUA("")
	if r.Browser != "" || r.OS != "" || r.Device != "" {
		t.Errorf("empty UA should produce empty result, got %+v", r)
	}
}

func TestIsBot(t *testing.T) {
	bots := []string{
		"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
		"Mozilla/5.0 (compatible; bingbot/2.0; +http://www.bing.com/bingbot.htm)",
		"facebookexternalhit/1.1",
		"Twitterbot/1.0",
		"python-requests/2.28.0",
		"curl/7.88.0",
		"Go-http-client/1.1",
		"Mozilla/5.0 (compatible; AhrefsBot/7.0; +http://ahrefs.com/robot/)",
		"",
	}
	for _, ua := range bots {
		if !IsBot(ua) {
			t.Errorf("expected bot for UA %q", ua)
		}
	}
}

func TestIsBot_NotBot(t *testing.T) {
	humans := []string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.6099.109 Safari/537.36",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_2 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.2 Mobile/15E148 Safari/604.1",
		"Mozilla/5.0 (X11; Linux x86_64; rv:121.0) Gecko/20100101 Firefox/121.0",
	}
	for _, ua := range humans {
		if IsBot(ua) {
			t.Errorf("should not be bot: %q", ua)
		}
	}
}
