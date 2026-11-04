package main

import "testing"

func navEntry(nav map[string]any, key string) (string, bool) {
	for _, a := range nav["apps"].([]map[string]string) {
		if a["key"] == key {
			return a["url"], true
		}
	}
	return "", false
}

// Teploy Cloud joins the switcher once its URL is configured, like the
// self-hosted siblings.
func TestTeployNavCloud(t *testing.T) {
	if _, ok := navEntry(teployNav("observe"), "cloud"); ok {
		t.Fatal("cloud listed without TEPLOY_NAV_CLOUD_URL")
	}
	t.Setenv("TEPLOY_NAV_CLOUD_URL", "https://cloud.teploy.com")
	if url, ok := navEntry(teployNav("observe"), "cloud"); !ok || url != "https://cloud.teploy.com" {
		t.Fatalf("cloud entry = %q %v", url, ok)
	}
	if url, ok := navEntry(teployNav("observe"), "observe"); !ok || url != "" {
		t.Fatalf("current product entry = %q %v, want present without a link", url, ok)
	}
}
