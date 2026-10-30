package api

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestBuildExplorerContainsValidation(t *testing.T) {
	setupDevMode(t)
	pinned := "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:" + strings.Repeat("a", 64)
	many := url.Values{"pr": {"1"}}
	for i := 0; i <= prSearchMaxImages; i++ {
		many.Add("image", pinned)
	}
	cases := map[string]string{
		"missing pr and commit": "",
		"both pr and commit":    "pr=1&commit=" + strings.Repeat("a", 40),
		"non-numeric pr":        "pr=abc",
		"hash prefix":           "pr=%23123",
		"zero":                  "pr=0",
		"negative":              "pr=-5",
		"huge":                  "pr=123456789012",
		"short commit":          "commit=abc12",
		"non-hex commit":        "commit=abcdefg",
		"long commit":           "commit=" + strings.Repeat("a", 41),
		"repo without owner":    "pr=1&repo=odh-dashboard",
		"repo with bad chars":   "pr=1&repo=" + url.QueryEscape("opendatahub-io/odh dashboard"),
		"repo with three parts": "pr=1&repo=a/b/c",
		"path traversal repo":   "pr=1&repo=" + url.QueryEscape("opendatahub-io/odh-dashboard/../x"),
		"tag-only image":        "pr=1&image=" + url.QueryEscape("quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6"),
		"foreign registry":      "pr=1&image=" + url.QueryEscape("docker.io/evil/fbc@sha256:"+strings.Repeat("a", 64)),
		"other quay repo":       "pr=1&image=" + url.QueryEscape("quay.io/rhoai/odh-dashboard-rhel9@sha256:"+strings.Repeat("a", 64)),
		"too many images":       many.Encode(),
	}
	for name, query := range cases {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/api/build-explorer/contains?"+query, nil)
			r.Header.Set("X-Forwarded-Access-Token", "tok")
			HandleBuildExplorerContains(w, r)
			if w.Code != 400 {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			var body struct {
				ErrorCode string `json:"errorCode"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if body.ErrorCode != "validation" {
				t.Errorf("errorCode %q", body.ErrorCode)
			}
		})
	}
}

func TestBuildExplorerContainsIsAReadRoute(t *testing.T) {
	for _, r := range routes {
		if strings.HasSuffix(r.Pattern, "/api/build-explorer/contains") {
			if r.Pattern != "GET /api/build-explorer/contains" {
				t.Errorf("pattern %q", r.Pattern)
			}
			return
		}
	}
	t.Error("GET /api/build-explorer/contains is not registered")
}
