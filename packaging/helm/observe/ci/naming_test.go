package ci

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func TestRenderedNamesAreUniqueAtBoundaries(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm unavailable")
	}
	for _, c := range []struct{ release, fullname, name string }{
		{"observe-test", "observe", ""},
		{"observe-test", strings.Repeat("a", 63), ""},
		{"observe-test", strings.Repeat("b", 55) + "-nucleus", ""},
		{"observe-test", strings.Repeat("c", 53) + "-c", ""},
		{strings.Repeat("r", 53), "", strings.Repeat("n", 63)},
		{strings.Repeat("z", 53), "", "z"},
	} {
		t.Run(c.release+"/"+c.fullname+"/"+c.name, func(t *testing.T) {
			out, err := exec.Command("helm", "template", c.release, "..", "-f", "values-ci.yaml", "--set", "fullnameOverride="+c.fullname, "--set", "nameOverride="+c.name, "--set", "service.port=8080").CombinedOutput()
			if err != nil {
				t.Fatalf("render: %v: %s", err, out)
			}
			seen := map[string]bool{}
			namePattern := regexp.MustCompile(`(?m)^  name: ([a-z0-9-]+)$`)
			kindPattern := regexp.MustCompile(`(?m)^kind: (\w+)$`)
			dnsPattern := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
			services := map[string]bool{}
			for _, doc := range strings.Split(string(out), "\n---") {
				names := namePattern.FindStringSubmatch(doc)
				kinds := kindPattern.FindStringSubmatch(doc)
				if len(names) == 0 || len(kinds) == 0 {
					continue
				}
				n, k := names[1], kinds[1]
				if len(n) > 63 || !dnsPattern.MatchString(n) {
					t.Fatalf("invalid %s name %q", k, n)
				}
				key := k + "/" + n
				if seen[key] {
					t.Fatalf("duplicate resource %s", key)
				}
				seen[key] = true
				if k == "Service" {
					services[n] = true
				}
			}
			if len(services) != 3 {
				t.Fatalf("services=%v", services)
			}
			for _, doc := range strings.Split(string(out), "\n---") {
				if !strings.Contains(doc, "kind: StatefulSet") {
					continue
				}
				servicePattern := regexp.MustCompile(`(?m)^  serviceName: ([a-z0-9-]+)$`)
				ref := servicePattern.FindStringSubmatch(doc)
				if len(ref) == 0 || !services[ref[1]] {
					t.Fatalf("invalid StatefulSet service reference: %v", ref)
				}
			}
		})
	}
}
