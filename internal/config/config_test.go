package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeCORSOrigins(t *testing.T) {
	got, err := NormalizeCORSOrigins([]string{" https://APP.example.com ", "https://app.example.com", "capacitor://localhost", "https://localhost", ""})
	want := []string{"https://app.example.com", "capacitor://localhost", "https://localhost"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("origins = %v, %v; want %v", got, err, want)
	}
	for _, raw := range []string{"*", "null", "app.example.com", "https://*.example.com", "https://example.com/", "https://example.com/path", "https://example.com?", "https://example.com#", "https://user:pass@example.com", "https://example.com:0", "https://example.com:65536", "https://example.com:", "ftp://example.com", "capacitor://other", "capacitor://localhost:1234", "file://localhost", "https://"} {
		t.Run(raw, func(t *testing.T) {
			if _, err := NormalizeCORSOrigins([]string{raw}); err == nil {
				t.Fatalf("accepted invalid origin %q", raw)
			}
		})
	}
}

func TestLoadCORSOriginsByEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name, environment, origins string
		want                       []string
	}{
		{"development_defaults", "development", "", []string{"http://localhost:5173", "http://localhost:5174"}},
		{"production_empty", "production", "", []string{}},
		{"staging_empty", "staging", "", []string{}},
		{"explicit_replaces_defaults", "development", "https://app.example.com", []string{"https://app.example.com"}},
		{"explicit_native", "production", " capacitor://localhost, https://localhost,https://localhost ", []string{"capacitor://localhost", "https://localhost"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for key, value := range map[string]string{
				"APP_ENV": tc.environment, "CORS_ALLOWED_ORIGINS": tc.origins,
				"DATABASE_URL": "postgres://test:test@127.0.0.1/test", "REDIS_URL": "redis://127.0.0.1:6379/0",
				"PUBLIC_BASE_URL": "https://api.example.com", "EMAIL_CODE_HMAC_KEY": strings.Repeat("e", 32), "SHORT_CODE_HMAC_KEY": strings.Repeat("s", 32),
				"SMTP_PORT": "587", "SMTP_TLS_MODE": "starttls", "MINIO_USE_SSL": "false", "TRUSTED_PROXY_CIDRS": "",
			} {
				t.Setenv(key, value)
			}
			cfg, err := Load()
			if err != nil || !reflect.DeepEqual(cfg.CORSAllowedOrigins, tc.want) {
				t.Fatalf("Load origins = %v, %v; want %v", cfg.CORSAllowedOrigins, err, tc.want)
			}
			t.Setenv("CORS_ALLOWED_ORIGINS", "https://app.example.com/path")
			if _, err := Load(); err == nil {
				t.Fatal("Load accepted invalid CORS origin")
			}
		})
	}
}
