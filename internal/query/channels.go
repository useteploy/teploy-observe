package query

import (
	"net/url"
	"strings"
)

// Channel represents a traffic source category.
const (
	ChannelDirect   = "Direct"
	ChannelOrganic  = "Organic Search"
	ChannelSocial   = "Social"
	ChannelReferral = "Referral"
	ChannelEmail    = "Email"
	ChannelPaid     = "Paid"
	// ChannelAI is traffic arriving from an AI assistant's web UI. It is its
	// own channel rather than a kind of Referral because the acquisition story
	// is different: nobody linked to you, a model cited you.
	ChannelAI = "AI Assistant"
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
		if isAIAssistant(src) {
			return ChannelAI
		}
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

	// Order is significant, most specific first. Webmail lives on the search
	// engines' own domains — mail.google.com and mail.yahoo.com both carry a
	// search brand — so checking search first reported every Gmail referral as
	// Organic Search. It did so before this catalogue was host-aware too; the
	// substring match hid it rather than caused it.
	if isAIAssistant(host) {
		return ChannelAI
	}
	if isEmailProvider(host) {
		return ChannelEmail
	}
	if isSearchEngine(host) {
		return ChannelOrganic
	}
	if isSocialNetwork(host) {
		return ChannelSocial
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

// hostMatches reports whether a host belongs to a catalogue entry.
//
// It replaces a plain strings.Contains, which mis-classified any host that
// merely contained an entry as a substring: "sandbox.company" contains "x.com"
// and read as Social, "kaolin.io" contains "aol" and read as Organic Search.
// Both are silent — a wrong channel looks exactly like a right one.
//
// A dotted entry ("x.com", "mail.google.com") matches the host itself or any
// subdomain of it. A bare entry ("google", "facebook") is a brand that owns
// many TLDs, so it matches a whole DNS LABEL — google.co.uk and news.google.com
// match, kaolin.io does not.
func hostMatches(host, entry string) bool {
	if host == "" || entry == "" {
		return false
	}
	if strings.Contains(entry, ".") {
		return host == entry || strings.HasSuffix(host, "."+entry)
	}
	for _, label := range strings.Split(host, ".") {
		if label == entry {
			return true
		}
	}
	return false
}

func matchesAny(s string, catalogue []string) bool {
	for _, entry := range catalogue {
		if hostMatches(s, entry) {
			return true
		}
	}
	return false
}

func isAIAssistant(s string) bool { return matchesAny(s, aiAssistants) }

func isSearchEngine(s string) bool { return matchesAny(s, searchEngines) }

func isSocialNetwork(s string) bool { return matchesAny(s, socialNetworks) }

func isEmailProvider(s string) bool { return matchesAny(s, emailProviders) }

// aiAssistants is checked BEFORE searchEngines: Google's assistants live on
// google.com subdomains and would otherwise be reported as organic search.
// Only the assistant surfaces are listed — bing.com/chat cannot be told apart
// from Bing search by referrer alone, so it is deliberately absent.
var aiAssistants = []string{
	"chatgpt.com", "chat.openai.com", "openai.com",
	"claude.ai", "anthropic.com",
	"perplexity.ai",
	"grok.com", "x.ai",
	"gemini.google.com", "bard.google.com", "aistudio.google.com",
	"copilot.microsoft.com",
	"chat.deepseek.com", "deepseek.com",
	"chat.mistral.ai", "le-chat.mistral.ai",
	"meta.ai", "poe.com", "you.com", "phind.com", "kimi.com",
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

// Full hostnames: these are matched as domain-or-subdomain, so a bare
// "mail.google" would no longer match mail.google.com.
var emailProviders = []string{
	"mail.google.com", "outlook.live.com", "outlook.office.com",
	"outlook.office365.com", "mail.yahoo.com",
	"mail.proton.me", "mail.protonmail.com",
}
