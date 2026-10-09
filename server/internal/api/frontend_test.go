package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cordova/server/internal/inference"
)

func TestFrontendPreservesAPIAndRestrictsFiles(t *testing.T) {
	root := t.TempDir()
	build := filepath.Join(root, "build")
	if err := os.MkdirAll(filepath.Join(build, "assets"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		filepath.Join(build, "index.html"):       "<html>Cordova panel</html>",
		filepath.Join(build, "login.html"):       "<html>Login route</html>",
		filepath.Join(build, "dashboard.html"):   "<html>Dashboard route</html>",
		filepath.Join(build, "assets", "app.js"): "/* public asset */",
		filepath.Join(root, "secret.txt"):        "private sentinel",
	} {
		if err := os.WriteFile(name, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	h, err := WithFrontend(handler(t, inference.Mock{}), build)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		method, route string
		code          int
	}{
		{"GET", "/", 200}, {"GET", "/login", 200}, {"GET", "/dashboard", 200}, {"GET", "/assets/app.js", 200},
		{"GET", "/assets/", 404}, {"GET", "/%2e%2e/secret.txt", 404},
		{"GET", "/.local/api-key.txt", 404}, {"POST", "/", 405},
		{"GET", "/healthz", 200}, {"GET", "/v1/models", 401},
		{"GET", "/api/session", 404}, {"POST", "/api/auth/login", 404},
	} {
		w := call(h, test.method, test.route, "", "")
		if w.Code != test.code || strings.Contains(w.Body.String(), "private sentinel") {
			t.Fatalf("%s %s: got %d", test.method, test.route, w.Code)
		}
	}
	if w := call(h, "GET", "/v1/models", "", testKey); w.Code != 200 || !strings.Contains(w.Body.String(), ModelID) {
		t.Fatal("frontend wrapper broke authenticated API")
	}
	for _, route := range []string{"/api/session", "/api/auth/login"} {
		method := "GET"
		if route == "/api/auth/login" {
			method = "POST"
		}
		w := call(h, method, route, "{}", "")
		if w.Header().Get("X-Request-ID") == "" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("frontend wrapper did not pass %s through to the API", route)
		}
	}
}

func TestFrontendRequiresBuiltIndex(t *testing.T) {
	if _, err := WithFrontend(handler(t, inference.Mock{}), t.TempDir()); err == nil {
		t.Fatal("missing frontend build must not expose an arbitrary directory")
	}
}
