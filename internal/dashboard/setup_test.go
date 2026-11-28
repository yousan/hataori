package dashboard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/desktopsetup"
	"github.com/yousan/hataori/internal/nativehttp"
)

func TestRestrictedSetupHasNoNormalOwnerOperations(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "dashboard-setup-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	setup, err := desktopsetup.Open(root, nil, nil, func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer setup.Close()
	handler := SetupHandler(nativehttp.Config{Root: root, Instance: "setup-fixture", Version: "fixture-version"}, setup)
	for _, target := range []string{ThreadsPath, EventsPath, TerminalPath, AttachPath, OperatePath, SendPath, "/signin", "/api/session", "/api/passkeys", "/api/../setup"} {
		request := httptest.NewRequest("GET", target, nil)
		request.Host = "instance"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("setup exposed %s: %d", target, response.Code)
		}
	}
	for _, header := range []string{"Authorization", "Forwarded", "X-Forwarded-For"} {
		request := httptest.NewRequest("GET", NativeIdentityPath, nil)
		request.Host = "instance"
		request.Header[header] = []string{""}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("accepted %s", header)
		}
	}
	request := httptest.NewRequest("GET", "/", nil)
	request.Host = "instance"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Security-Policy") != contentSecurityPolicy || !strings.Contains(response.Body.String(), "script") {
		t.Fatal("setup did not serve the existing page under its CSP")
	}
	request = httptest.NewRequest("POST", "/api/setup", strings.NewReader(`{}`))
	request.Host = "instance"
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatal("missing-origin setup mutation accepted")
	}
}
