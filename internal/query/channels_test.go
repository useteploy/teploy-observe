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
