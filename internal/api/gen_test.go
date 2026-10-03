package api

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// oapi-codegen gives a response type over a pure union its own MarshalJSON, but not a response type over a struct that
// mixes a union with extra properties: such a response silently drops the union's fields. Each one must be written
// by a hand-made response type in internal/server; list it here once that exists.
var handled = map[string]string{
	"GetMyListing200JSONResponse": "server.listingDetailResponse",
}

func TestUnionResponsesKeepTheirFields(t *testing.T) {
	src, err := os.ReadFile("api.gen.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	union := regexp.MustCompile(`(?m)^\s+union\s+json\.RawMessage$`)
	for _, m := range regexp.MustCompile(`(?m)^type (\w+) struct \{$`).FindAllStringSubmatchIndex(s, -1) {
		name := s[m[2]:m[3]]
		body := s[m[1] : m[1]+strings.Index(s[m[1]:], "\n}\n")]
		fields := 0
		for _, l := range strings.Split(body, "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "//") {
				fields++
			}
		}
		if !union.MatchString(body) || fields < 2 {
			continue
		}
		for _, r := range regexp.MustCompile(`(?m)^type (\w+JSONResponse) `+name+`$`).FindAllStringSubmatch(s, -1) {
			if !strings.Contains(s, "func (t "+r[1]+") MarshalJSON") && handled[r[1]] == "" {
				t.Errorf("%s is a %s (union + fields) without MarshalJSON: write a response type in internal/server and list it in handled", r[1], name)
			}
		}
	}
}
