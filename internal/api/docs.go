package api

import (
	_ "embed"
	"fmt"
	"net/http"
)

//go:embed openapi.yaml
var openAPISpec []byte

func (s *Server) handleOpenAPISpec(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write(openAPISpec)
}

// Swagger UI is loaded from a CDN, pinned to an exact version and guarded by a
// subresource-integrity hash. It used to ask for `swagger-ui-dist@5` — a floating major
// on a third-party CDN with no integrity attribute — which means whatever that URL
// returned, today or in two years, ran on this origin with this origin's privileges. A
// floating tag is a standing invitation: the bytes can change without anything here
// changing, and a compromised or mis-published package is executing code on the same
// origin that serves the API. See docs/AUDIT-2.md L1.
//
// The hashes are sha384 over the exact files, cross-checked against both unpkg and
// jsdelivr so they are the canonical npm artifacts rather than one CDN's rewrite. If the
// browser gets anything else, it refuses to run it and /docs renders unstyled rather
// than running someone else's script.
//
// Bumping the version means recomputing both hashes; they are not optional decoration:
//
//	V=5.32.14
//	for f in swagger-ui.css swagger-ui-bundle.js; do
//	  curl -sSL "https://unpkg.com/swagger-ui-dist@$V/$f" |
//	    openssl dgst -sha384 -binary | openssl base64 -A; echo "  $f"
//	done
//
// crossorigin="anonymous" is required for SRI to be enforced on a cross-origin fetch —
// without it the browser cannot read the response to check it, and silently skips the
// check rather than failing.
const (
	swaggerUIVersion   = "5.32.14"
	swaggerUICSSHash   = "sha384-fgyWYkUAamzuI8mJFu/xpRP0JWCJRwkwUwsYDoOYVHUJ8NQE5cENn8ib3ppwFFSX"
	swaggerUIJSHash    = "sha384-Dt83RhU85ZmX7werw9uTFCzmauXUoSyx3pdzTQMABtsnFmooJy4Vz9/ACh7n5m1A"
	swaggerUIURLPrefix = "https://unpkg.com/swagger-ui-dist@"
)

// handleDocs serves a minimal Swagger-UI page that loads the embedded spec. The raw
// spec is always available at /openapi.yaml for offline Swift code generation, and that
// is the copy to depend on — this page is a convenience over a third-party CDN.
func (s *Server) handleDocs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	base := swaggerUIURLPrefix + swaggerUIVersion
	_, _ = fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8"/>
  <meta name="viewport" content="width=device-width, initial-scale=1"/>
  <title>SoccerKit API — Docs</title>
  <link rel="stylesheet" href="%[1]s/swagger-ui.css"
        integrity="%[2]s" crossorigin="anonymous"/>
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="%[1]s/swagger-ui-bundle.js"
          integrity="%[3]s" crossorigin="anonymous"></script>
  <script>
    window.ui = SwaggerUIBundle({ url: '/openapi.yaml', dom_id: '#swagger-ui' });
  </script>
</body>
</html>`, base, swaggerUICSSHash, swaggerUIJSHash)
}
