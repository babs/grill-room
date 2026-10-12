package uivendor

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 004-AC19: one vendored copy serves every page, and a page's policy admits its import map only.
func TestHandlerAndCSP(t *testing.T) {
	for _, f := range []string{"preact.js", "preact-hooks.js", "signals-core.js", "signals.js", "htm.js", "htm-preact.js", "VERSIONS"} {
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, Prefix+f, nil))
		if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
			t.Errorf("%s: %d", f, rec.Code)
		}
	}
	m := `{"imports": {"preact": "/static/esm/preact.js"}}`
	sum := sha256.Sum256([]byte(m))
	csp := CSP([]byte(`<script type="importmap">`+m+`</script><script src="/a.js"></script>`), "'none'")
	if !strings.Contains(csp, "script-src 'self' 'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"';") || !strings.HasSuffix(csp, "form-action 'none'") {
		t.Fatalf("csp: %s", csp)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("a page without an import map passed")
		}
	}()
	CSP([]byte(`<script src="/a.js"></script>`), "'none'")
}
