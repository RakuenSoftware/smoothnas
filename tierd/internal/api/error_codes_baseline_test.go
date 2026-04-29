package api

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// uncodedBaseline is the maximum permitted number of error
// response sites in this package that still emit a literal
// English message without a stable error code. New code must
// either reuse one of the canonical helpers (jsonNotFound,
// jsonAuthRequired, jsonMethodNotAllowed, jsonInvalidRequestBody)
// or call jsonErrorCoded(w, msg, status, code) directly. Adding a
// new uncoded site grows the count and fails this test.
//
// Phase 6 of the i18n proposal walks this baseline down toward
// zero. To convert a remaining site, swap the literal-string call
// for jsonErrorCoded with a stable code, add a matching
// `error.<code>` entry to tierd-ui/src/i18n/locales/en.ts and
// nl.ts, and decrement the constant below by however many sites
// you converted.
//
// What's counted:
//   - jsonError(w, "<literal>", status)            uncoded message
//   - http.Error(w, `{"error":"..."}`, status)     legacy plain-text
//
// What's NOT counted (these are intentionally exempt):
//   - jsonError(w, err.Error(), ...)               passthrough
//   - jsonError(w, "prefix: "+err.Error(), ...)    passthrough w/ prefix
//   - jsonErrorCoded(...)                          already coded
//   - jsonNotFound / jsonAuthRequired / etc.       canonical helpers
const uncodedBaseline = 0

// jsonErrLiteralPattern matches jsonError(w, "<literal>", ...) but
// not jsonError(w, err.Error(), ...) or jsonError(w, "prefix: "+err.Error(), ...).
// The trailing comma after the closing quote rules out string concatenation
// (`"..." + err.Error()`), since those have a `+` before the comma.
var jsonErrLiteralPattern = regexp.MustCompile(`\bjsonError\(w, "[^"]*",`)

// httpErrorPattern matches every legacy http.Error site. Any
// remaining http.Error in this package is a candidate for
// migration to jsonErrorCoded — http.Error sets Content-Type to
// text/plain which doesn't match the rest of the API.
var httpErrorPattern = regexp.MustCompile(`\bhttp\.Error\(w`)

func TestUncodedErrorBaseline(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}

	count := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		// helpers.go owns the canonical helpers and reuses the
		// literal strings in their bodies, so excluding it avoids
		// double-counting the helper definitions themselves.
		if file == "helpers.go" {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		count += len(jsonErrLiteralPattern.FindAll(src, -1))
		count += len(httpErrorPattern.FindAll(src, -1))
	}

	if count > uncodedBaseline {
		t.Fatalf("uncoded error sites grew: have %d, baseline %d.\n"+
			"New error responses must use jsonErrorCoded(w, msg, status, code) or one\n"+
			"of the canonical helpers (jsonNotFound, jsonAuthRequired, etc.) so the\n"+
			"frontend can localise them. See helpers.go for the helper set and\n"+
			"docs/proposals/done/smoothnas-i18n-en-nl.md for the convention.",
			count, uncodedBaseline)
	}
	if count < uncodedBaseline {
		t.Logf("uncoded error sites dropped to %d (baseline still %d). "+
			"Lower uncodedBaseline in this file to lock in the gain.",
			count, uncodedBaseline)
	}
}
