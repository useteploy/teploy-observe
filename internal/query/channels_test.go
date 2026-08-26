package query

import "testing"

func TestClassifyChannel(t *testing.T) {
	tests := []struct {
		referrer  string
		utmSource string
		utmMedium string
		want      string
	}{
		// Direct
		{"", "", "", ChannelDirect},

		// Organic search
		{"https://www.google.com/search?q=test", "", "", ChannelOrganic},
		{"https://www.bing.com/search?q=test", "", "", ChannelOrganic},
		{"https://duckduckgo.com/?q=test", "", "", ChannelOrganic},
		{"https://search.yahoo.com/search?p=test", "", "", ChannelOrganic},

		// Social
		{"https://www.facebook.com/somepost", "", "", ChannelSocial},
		{"https://t.co/abc123", "", "", ChannelSocial},
		{"https://www.reddit.com/r/golang", "", "", ChannelSocial},
		{"https://www.linkedin.com/feed", "", "", ChannelSocial},
		{"https://x.com/user/status/123", "", "", ChannelSocial},

		// Referral
		{"https://someblog.com/article", "", "", ChannelReferral},
		{"https://news.ycombinator.com/item?id=123", "", "", ChannelReferral},

		// UTM overrides
		{"https://google.com", "", "cpc", ChannelPaid},
		{"https://google.com", "", "email", ChannelEmail},
		{"", "newsletter", "email", ChannelEmail},
		{"", "google", "cpc", ChannelPaid},
		{"", "facebook", "social", ChannelSocial},

		// UTM source detection
		{"", "google", "", ChannelOrganic},
		{"", "facebook", "", ChannelSocial},
	}

	for _, tt := range tests {
		got := ClassifyChannel(tt.referrer, tt.utmSource, tt.utmMedium)
		if got != tt.want {
			t.Errorf("ClassifyChannel(%q, %q, %q) = %q, want %q",
				tt.referrer, tt.utmSource, tt.utmMedium, got, tt.want)
		}
	}
}

// AI assistants are their own channel. Before this existed they all fell
// through to "Referral", and the Google-hosted ones were worse than that:
// gemini.google.com matched the "google" entry and was reported as Organic
// Search, so AI traffic was not merely uncategorised, some of it was
// attributed to the wrong acquisition channel entirely.
func TestClassifyChannel_AIAssistants(t *testing.T) {
	for _, ref := range []string{
		"https://chatgpt.com/",
		"https://chat.openai.com/c/abc",
		"https://claude.ai/chat/123",
		"https://www.perplexity.ai/search?q=teploy",
		"https://grok.com/chat",
		"https://x.ai/",
		"https://gemini.google.com/app",
		"https://copilot.microsoft.com/",
		"https://chat.deepseek.com/",
		"https://meta.ai/",
	} {
		if got := ClassifyChannel(ref, "", ""); got != ChannelAI {
			t.Errorf("ClassifyChannel(%q) = %q, want %q", ref, got, ChannelAI)
		}
	}
	// utm_source carries the same names when the referrer header is stripped.
	if got := ClassifyChannel("", "chatgpt.com", ""); got != ChannelAI {
		t.Errorf("utm_source=chatgpt.com = %q, want %q", got, ChannelAI)
	}
}

// Google search itself must not be swallowed by the AI catalogue.
func TestClassifyChannel_GoogleSearchIsStillOrganic(t *testing.T) {
	for _, ref := range []string{"https://www.google.com/", "https://google.co.uk/", "https://news.google.com/"} {
		if got := ClassifyChannel(ref, "", ""); got != ChannelOrganic {
			t.Errorf("ClassifyChannel(%q) = %q, want %q", ref, got, ChannelOrganic)
		}
	}
}

// Matching used to be strings.Contains, so any host that merely CONTAINED a
// catalogue entry was mis-classified — silently, because a wrong channel is
// indistinguishable from a right one in the dashboard.
func TestClassifyChannel_SubstringsDoNotMatch(t *testing.T) {
	cases := []struct{ ref, want string }{
		{"https://sandbox.company/blog", ChannelReferral}, // contains "x.com"
		{"https://kaolin.io/post", ChannelReferral},       // contains "aol"
		{"https://bravery.example.org/", ChannelReferral}, // contains "brave"
		{"https://redditchbeer.co.uk/", ChannelReferral},  // contains "reddit"
		{"https://notaskcomedy.net/", ChannelReferral},    // contains "ask.com"
	}
	for _, c := range cases {
		if got := ClassifyChannel(c.ref, "", ""); got != c.want {
			t.Errorf("ClassifyChannel(%q) = %q, want %q", c.ref, got, c.want)
		}
	}
}

// The real entries still match, including subdomains and other TLDs.
func TestClassifyChannel_LegitimateHostsStillMatch(t *testing.T) {
	cases := []struct{ ref, want string }{
		{"https://www.facebook.com/", ChannelSocial},
		{"https://m.facebook.com/story", ChannelSocial},
		{"https://t.co/abc", ChannelSocial},
		{"https://x.com/someone", ChannelSocial},
		{"https://duckduckgo.com/?q=x", ChannelOrganic},
		// Regression: webmail lives on a search brand's domain. Before the
		// channels were ordered email-first, every Gmail referral was
		// reported as Organic Search.
		{"https://mail.google.com/mail/u/0", ChannelEmail},
		{"https://mail.yahoo.com/d/folders/1", ChannelEmail},
		{"https://outlook.live.com/mail/", ChannelEmail},
		{"", ChannelDirect},
	}
	for _, c := range cases {
		if got := ClassifyChannel(c.ref, "", ""); got != c.want {
			t.Errorf("ClassifyChannel(%q) = %q, want %q", c.ref, got, c.want)
		}
	}
}
