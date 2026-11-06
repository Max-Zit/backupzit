package server_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestOpenAPIMatchesRoutes checks that openapi.json documents exactly the
// /api/v1 routes the console registers, and that the console serves it.
func TestOpenAPIMatchesRoutes(t *testing.T) {
	raw, err := os.ReadFile("openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		OpenAPI string                                `json:"openapi"`
		Paths   map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || !strings.HasPrefix(doc.OpenAPI, "3.") {
		t.Fatalf("openapi.json: %v %q", err, doc.OpenAPI)
	}
	var inSpec []string
	for p, ops := range doc.Paths {
		for m := range ops {
			inSpec = append(inSpec, strings.ToUpper(m)+" "+p)
		}
	}
	files, _ := filepath.Glob("*.go")
	re := regexp.MustCompile(`HandleFunc\("((?:GET|POST|PUT|DELETE) /api/v1/[^"]*)"`)
	loop := regexp.MustCompile(`"(POST /api/v1/jobs/\{id\}/)"\+x\.path`)
	seen := map[string]bool{}
	var inCode []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			if !seen[m[1]] && !strings.HasSuffix(m[1], "/") {
				seen[m[1]] = true
				inCode = append(inCode, m[1])
			}
		}
		if loop.Match(b) { // enable/disable are registered in a loop
			inCode = append(inCode, "POST /api/v1/jobs/{id}/enable", "POST /api/v1/jobs/{id}/disable")
		}
	}
	sort.Strings(inSpec)
	sort.Strings(inCode)
	if strings.Join(inSpec, "\n") != strings.Join(inCode, "\n") {
		t.Errorf("routes and openapi.json differ\ncode:\n%s\nspec:\n%s", strings.Join(inCode, "\n"), strings.Join(inSpec, "\n"))
	}

	e := setup(t)
	code, body := apiCall(t, e, "", "GET", "/api/v1/openapi.json")
	if code != 200 || !strings.Contains(body, `"x-backupzit-version"`) || !strings.Contains(body, `"/api/v1/runs/{id}/restore"`) {
		t.Errorf("served spec: %d %.200s", code, body)
	}
}
