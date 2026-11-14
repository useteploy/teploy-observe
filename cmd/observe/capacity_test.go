package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// /healthz is unauthenticated on both listeners, including the public ingest
// one, so the capacity block must not name filesystem paths: the data
// directory's absolute path tells an outsider how the host is laid out.
func TestCapacityViewDoesNotDiscloseDataPaths(t *testing.T) {
	dataDir := t.TempDir()
	engineDir := t.TempDir()
	missing := dataDir + "/does-not-exist"
	t.Setenv("OBSERVE_NUCLEUS_DATA_DIR", engineDir)

	for _, dir := range []string{dataDir, missing} {
		body, err := json.Marshal(capacityView(dir, nil))
		if err != nil {
			t.Fatal(err)
		}
		for _, leaked := range []string{dir, engineDir, `"path"`} {
			if strings.Contains(string(body), leaked) {
				t.Errorf("capacity block for %s contains %q: %s", dir, leaked, body)
			}
		}
	}

	var block struct {
		Disk       map[string]any `json:"disk"`
		EngineDisk map[string]any `json:"engine_disk"`
	}
	body, _ := json.Marshal(capacityView(dataDir, nil))
	if err := json.Unmarshal(body, &block); err != nil {
		t.Fatal(err)
	}
	for name, d := range map[string]map[string]any{"disk": block.Disk, "engine_disk": block.EngineDisk} {
		if d["ok"] != true {
			t.Errorf("%s: want ok=true for an existing directory, got %v", name, d)
		}
		for _, k := range []string{"free_bytes", "total_bytes", "used_pct"} {
			if _, ok := d[k]; !ok {
				t.Errorf("%s: missing %s: %v", name, k, d)
			}
		}
	}
}
