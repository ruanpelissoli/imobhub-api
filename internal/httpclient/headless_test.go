package httpclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestFetchHeadlessRejectsEmptyURL(t *testing.T) {
	// URL vazia precisa falhar antes de subir o browser: um Chrome levantado à
	// toa custa segundos e um processo órfão em caso de erro.
	start := time.Now()
	html, err := FetchHeadless(context.Background(), "   ", "imobhub-bot")
	if err == nil {
		t.Fatalf("FetchHeadless(\"\") error = nil, want error")
	}
	if html != "" {
		t.Errorf("FetchHeadless(\"\") html = %q, want empty", html)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("elapsed = %v, want near zero (browser should not start)", elapsed)
	}
	if !strings.Contains(err.Error(), "httpclient:") {
		t.Errorf("error = %q, want it to name the package", err)
	}
}

func TestFetchHeadlessRespectsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := FetchHeadless(ctx, "https://example.com", "imobhub-bot"); err == nil {
		t.Fatal("FetchHeadless() error = nil, want error for canceled context")
	}
}

func TestHeadlessFlagsCoverContainerRequirements(t *testing.T) {
	// Faltar qualquer uma destas flags troca um erro de rede por uma falha na
	// subida do Chrome dentro de container — sintoma bem mais difícil de ligar
	// à causa. O teste é a documentação executável dessa exigência.
	want := []string{"no-sandbox", "disable-setuid-sandbox", "disable-dev-shm-usage", "headless", "disable-gpu"}

	got := make(map[string]any, len(headlessFlags))
	for _, f := range headlessFlags {
		got[f.Name] = f.Value
	}

	for _, name := range want {
		value, ok := got[name]
		if !ok {
			t.Errorf("headlessFlags is missing %q", name)
			continue
		}
		if enabled, isBool := value.(bool); !isBool || !enabled {
			t.Errorf("headlessFlags[%q] = %v, want true", name, value)
		}
	}
}

func TestIdleBudgetLeavesRoomForCapture(t *testing.T) {
	t.Run("sem prazo", func(t *testing.T) {
		if _, bounded := idleBudget(context.Background()); bounded {
			t.Error("idleBudget() bounded = true, want false for a context without deadline")
		}
	})

	t.Run("prazo cheio", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), HeadlessTimeout)
		defer cancel()

		budget, bounded := idleBudget(ctx)
		if !bounded {
			t.Fatal("idleBudget() bounded = false, want true")
		}
		// A espera precisa terminar antes do prazo para sobrar tempo de captura.
		if budget > HeadlessTimeout-captureReserve {
			t.Errorf("budget = %v, want at most %v", budget, HeadlessTimeout-captureReserve)
		}
		if budget <= 0 {
			t.Errorf("budget = %v, want positive", budget)
		}
	})

	t.Run("prazo quase esgotado", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), captureReserve/2)
		defer cancel()

		budget, bounded := idleBudget(ctx)
		if !bounded {
			t.Fatal("idleBudget() bounded = false, want true")
		}
		// Orçamento não positivo sinaliza "capture já, não espere mais".
		if budget > 0 {
			t.Errorf("budget = %v, want non-positive", budget)
		}
	})
}

// TestFetchHeadlessRendersJavaScript é o único teste que sobe um browser de
// verdade. Ele é pulado quando não há Chrome/Chromium disponível — a máquina de
// CI pode não ter, e o restante do pacote não deve ficar refém disso.
func TestFetchHeadlessRendersJavaScript(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping browser test in short mode")
	}
	if browserPath() == "" {
		t.Skip("skipping: no Chrome/Chromium found in PATH")
	}

	var gotUserAgent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUserAgent = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// O conteúdo só existe depois que o script roda: se FetchHeadless
		// devolvesse o HTML inicial, o marcador não apareceria.
		_, _ = w.Write([]byte(`<html><body><div id="app"></div>
<script>document.getElementById('app').textContent = 'preco-renderizado';</script>
</body></html>`))
	}))
	defer server.Close()

	const userAgent = "imobhub-bot/1.0 (+https://imobhub.com.br)"
	html, err := FetchHeadless(context.Background(), server.URL, userAgent)
	if err != nil {
		t.Fatalf("FetchHeadless() error = %v, want nil", err)
	}
	if !strings.Contains(html, "preco-renderizado") {
		t.Errorf("html does not contain the JavaScript-rendered content: %q", html)
	}
	if gotUserAgent != userAgent {
		t.Errorf("User-Agent = %q, want %q", gotUserAgent, userAgent)
	}
}

// browserPath repete a busca que o chromedp faz para decidir se há browser
// instalado. O chromedp não expõe essa função, e sem ela o teste de integração
// falharia (em vez de pular) em máquinas sem Chrome.
func browserPath() string {
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		candidates = []string{
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		}
	case "windows":
		candidates = []string{
			"chrome",
			`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files\Google\Chrome\Application\chrome.exe`,
			filepath.Join(os.Getenv("USERPROFILE"), `AppData\Local\Google\Chrome\Application\chrome.exe`),
			filepath.Join(os.Getenv("USERPROFILE"), `AppData\Local\Chromium\Application\chrome.exe`),
		}
	default:
		candidates = []string{
			"headless_shell", "headless-shell", "chromium", "chromium-browser",
			"google-chrome", "google-chrome-stable",
		}
	}

	for _, candidate := range candidates {
		if path, err := exec.LookPath(candidate); err == nil {
			return path
		}
	}
	return ""
}
