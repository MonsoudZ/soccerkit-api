package api_test

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"
)

// openAPIDoc is the part of the spec this test reads: the operations, and enough
// structure to walk every $ref.
type openAPIDoc struct {
	Paths      map[string]map[string]yaml.Node `yaml:"paths"`
	Components struct {
		Schemas    map[string]yaml.Node `yaml:"schemas"`
		Responses  map[string]yaml.Node `yaml:"responses"`
		Parameters map[string]yaml.Node `yaml:"parameters"`
	} `yaml:"components"`
}

// httpMethods are the keys under a path that describe an operation. A path item may also
// carry `parameters`, `summary` and friends, which are not operations.
var httpMethods = map[string]bool{
	"get": true, "put": true, "post": true, "delete": true,
	"options": true, "head": true, "patch": true, "trace": true,
}

// undocumentedRoutes are mounted deliberately without a spec entry. They are the two
// endpoints that serve the documentation itself: describing them in the document they
// serve adds nothing a client generator can use.
var undocumentedRoutes = map[string]bool{
	"GET /openapi.yaml": true,
	"GET /docs":         true,
}

// TestOpenAPISpecMatchesTheRouter is the only thing keeping the spec honest.
//
// openapi.yaml is hand-written, not generated from the handlers, and it is what the iOS
// client's types are generated from. Nothing connected the two: a route added without a
// spec entry, or a spec entry left behind by a deleted route, compiled and passed every
// test, and surfaced later as a Swift client missing a method or sending a field the
// server rejects. Both directions are checked here, because both drift.
//
// It reads the spec over HTTP rather than off disk, so it also proves the copy embedded
// in the binary is the copy that gets served.
func TestOpenAPISpecMatchesTheRouter(t *testing.T) {
	res := do(t, http.MethodGet, "/openapi.yaml", "", nil)
	if res.status != http.StatusOK {
		t.Fatalf("GET /openapi.yaml: %d", res.status)
	}
	var doc openAPIDoc
	if err := yaml.Unmarshal(res.raw, &doc); err != nil {
		t.Fatalf("the served spec is not valid YAML: %v", err)
	}

	inSpec := map[string]bool{}
	for path, item := range doc.Paths {
		for method := range item {
			if httpMethods[method] {
				inSpec[strings.ToUpper(method)+" "+path] = true
			}
		}
	}

	router, ok := testRouter.(chi.Routes)
	if !ok {
		t.Fatalf("the router is %T, which cannot be walked", testRouter)
	}
	inRouter := map[string]bool{}
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		// chi spells a sub-router's index route with a trailing slash ("/api/v1/teams/")
		// where the spec names the collection ("/api/v1/teams"). Same endpoint.
		if len(route) > 1 {
			route = strings.TrimSuffix(route, "/")
		}
		op := method + " " + route
		if !undocumentedRoutes[op] {
			inRouter[op] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk routes: %v", err)
	}

	if missing := difference(inRouter, inSpec); len(missing) > 0 {
		t.Errorf("mounted but absent from openapi.yaml — the generated client will not "+
			"have these:\n  %s", strings.Join(missing, "\n  "))
	}
	if stale := difference(inSpec, inRouter); len(stale) > 0 {
		t.Errorf("described in openapi.yaml but not mounted — the generated client will "+
			"call these and get 404:\n  %s", strings.Join(stale, "\n  "))
	}
}

// TestOpenAPISpecRefsResolve catches the other way a hand-written spec breaks: a $ref
// naming a component that was renamed or never added. A generator given one of these
// fails on a document that otherwise parses cleanly.
func TestOpenAPISpecRefsResolve(t *testing.T) {
	res := do(t, http.MethodGet, "/openapi.yaml", "", nil)
	if res.status != http.StatusOK {
		t.Fatalf("GET /openapi.yaml: %d", res.status)
	}
	var doc openAPIDoc
	if err := yaml.Unmarshal(res.raw, &doc); err != nil {
		t.Fatalf("the served spec is not valid YAML: %v", err)
	}
	defined := map[string]bool{}
	for name := range doc.Components.Schemas {
		defined["#/components/schemas/"+name] = true
	}
	for name := range doc.Components.Responses {
		defined["#/components/responses/"+name] = true
	}
	for name := range doc.Components.Parameters {
		defined["#/components/parameters/"+name] = true
	}

	// Deduplicated: a renamed schema is referenced from every operation that returns
	// it, and listing it once per call site buries the one name that has to be fixed.
	seen := map[string]bool{}
	var broken []string
	for _, ref := range refsIn(string(res.raw)) {
		if !defined[ref] && !seen[ref] {
			seen[ref] = true
			broken = append(broken, ref)
		}
	}
	if len(broken) > 0 {
		sort.Strings(broken)
		t.Errorf("$ref targets that do not exist:\n  %s", strings.Join(broken, "\n  "))
	}
}

// refsIn pulls every local $ref target out of the raw document. The spec writes them as
// `{ $ref: "#/components/..." }`, so a scan is enough and avoids walking every node.
func refsIn(spec string) []string {
	var out []string
	for _, line := range strings.Split(spec, "\n") {
		idx := strings.Index(line, "$ref:")
		if idx < 0 {
			continue
		}
		rest := strings.TrimSpace(line[idx+len("$ref:"):])
		rest = strings.TrimPrefix(rest, `"`)
		if end := strings.IndexAny(rest, `" }`); end >= 0 {
			rest = rest[:end]
		}
		if strings.HasPrefix(rest, "#/") {
			out = append(out, rest)
		}
	}
	return out
}

// difference returns the keys of a that are not in b, sorted for a stable message.
func difference(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
