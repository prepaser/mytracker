package providerurl

import "testing"

func TestNormalize(t *testing.T) {
	for raw, want := range map[string]string{
		"HTTPS://EXAMPLE.COM:443/api/":        "https://example.com/api",
		"https://example.com:08443/api":       "https://example.com:8443/api",
		"https://example.com./api/%7e":        "https://example.com/api/~",
		"https://example.com/api/%2f":         "https://example.com/api/%2F",
		"https://example%2ecom/api":           "https://example.com/api",
		"https://example.com/a/%2e%2e/api":    "https://example.com/api",
		"https://example.com/api/../api":      "https://example.com/api",
		"https://example.com/api//../api":     "https://example.com/api/api",
		"https://[2001:0db8:0:0:0:0:0:1]/api": "https://[2001:db8::1]/api",
		"https://[::ffff:192.0.2.1]/api":      "https://192.0.2.1/api",
		"https://example.com/api#unused":      "https://example.com/api",
	} {
		got, err := Normalize(raw)
		if err != nil || got != want {
			t.Fatalf("Normalize(%q)=%q err=%v want %q", raw, got, err, want)
		}
	}
}

func TestNormalizeRejectsQueryAndCredentials(t *testing.T) {
	for _, raw := range []string{
		"https://example.com/api?token=secret",
		"https://user:pass@example.com/api",
		"ftp://example.com/api",
	} {
		if _, err := Normalize(raw); err == nil {
			t.Fatalf("Normalize(%q) accepted", raw)
		}
	}
}

func TestNormalizeLoopbackHTTP(t *testing.T) {
	for raw, want := range map[string]string{
		"http://LOCALHOST:80/api/":          "http://localhost/api",
		"http://127.0.0.1:3000/api/":        "http://127.0.0.1:3000/api",
		"http://[::1]:80/api/":              "http://[::1]/api",
		"http://[::ffff:127.0.0.1]:80/api/": "http://127.0.0.1/api",
		"https://example.com/api/":          "https://example.com/api",
	} {
		got, err := NormalizeLoopbackHTTP(raw)
		if err != nil || got != want {
			t.Fatalf("NormalizeLoopbackHTTP(%q)=%q err=%v want %q", raw, got, err, want)
		}
	}
	if _, err := NormalizeLoopbackHTTP("http://example.com/api"); err == nil {
		t.Fatal("remote HTTP provider accepted")
	}
}
