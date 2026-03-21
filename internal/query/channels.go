package query

import (
	"net/url"
	"strings"
)

// Channel represents a traffic source category.
const (
	ChannelDirect  = "Direct"
	ChannelOrganic = "Organic Search"
	ChannelSocial  = "Social"
	ChannelReferral = "Referral"
	ChannelEmail   = "Email"
	ChannelPaid    = "Paid"
)

// ClassifyChannel determines the traffic channel from a referrer URL and
// UTM parameters. Follows Umami's approach of classifying at query time.
func ClassifyChannel(referrer, utmSource, utmMedium string) string {
	// UTM overrides take priority
	if utmMedium != "" {
		medium := strings.ToLower(utmMedium)
		switch {
		case medium == "cpc" || medium == "ppc" || medium == "paidsearch" ||
			medium == "paid" || medium == "display" || medium == "retargeting" ||
			medium == "banner" || medium == "cpm":
			return ChannelPaid
		case medium == "email" || medium == "e-mail":
			return ChannelEmail
		case medium == "social" || medium == "social-network" ||
			medium == "social-media" || medium == "sm":
			return ChannelSocial
		case medium == "organic":
			return ChannelOrganic
		case medium == "referral":
			return ChannelReferral
		}
	}

	if utmSource != "" {
		src := strings.ToLower(utmSource)
		if isSearchEngine(src) {
			return ChannelOrganic
		}
		if isSocialNetwork(src) {
			return ChannelSocial
		}
	}

	if referrer == "" {
		return ChannelDirect
	}

	host := referrerHost(referrer)
	if host == "" {
		return ChannelDirect
	}

	if isSearchEngine(host) {
		return ChannelOrganic
	}
	if isSocialNetwork(host) {
		return ChannelSocial
	}
	if isEmailProvider(host) {
		return ChannelEmail
	}

	return ChannelReferral
}

func referrerHost(ref string) string {
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	h := strings.ToLower(u.Hostname())
	// Strip www.
	h = strings.TrimPrefix(h, "www.")
	return h
}

func isSearchEngine(s string) bool {
	for _, engine := range searchEngines {
		if strings.Contains(s, engine) {
			return true
		}
	}
	return false
}

func isSocialNetwork(s string) bool {
	for _, social := range socialNetworks {
		if strings.Contains(s, social) {
			return true
		}
	}
	return false
}

func isEmailProvider(s string) bool {
	for _, email := range emailProviders {
		if strings.Contains(s, email) {
			return true
		}
	}
	return false
}

var searchEngines = []string{
	"google", "bing", "yahoo", "duckduckgo", "baidu", "yandex",
	"ecosia", "ask.com", "aol", "startpage", "qwant", "brave",
	"sogou", "naver", "daum", "seznam",
}

var socialNetworks = []string{
	"facebook", "twitter", "t.co", "x.com", "instagram", "linkedin",
	"pinterest", "reddit", "tiktok", "youtube", "snapchat",
	"whatsapp", "telegram", "mastodon", "threads.net",
	"discord", "tumblr", "vk.com", "weibo",
}

var emailProviders = []string{
	"mail.google", "outlook.live", "outlook.office",
	"mail.yahoo", "mail.proton",
}
