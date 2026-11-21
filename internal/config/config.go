package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Config 收敛所有进程配置；业务代码不直接读取环境变量。
type Config struct {
	Environment           string
	HTTPAddr              string
	PublicBaseURL         string
	DatabaseURL           string
	RedisURL              string
	InitialAdminEmail     string
	InitialAdminPassword  string
	EmailCodeHMACKey      string
	ShortCodeHMACKey      string
	SMTPHost              string
	SMTPPort              int
	SMTPUsername          string
	SMTPPassword          string
	SMTPFrom              string
	SMTPTLSMode           string
	MinIOInternalEndpoint string
	MinIOPublicEndpoint   string
	MinIOAccessKey        string
	MinIOSecretKey        string
	MinIOUseSSL           bool
	CORSAllowedOrigins    []string
	TrustedProxyCIDRs     []string
}

// Load 解析配置并在启动前拦截缺失或不合法的基础依赖。
func Load() (Config, error) {
	// 1. 解析基础地址和布尔/数字类型字段。
	cfg := Config{
		Environment:           getEnv("APP_ENV", "development"),
		HTTPAddr:              getEnv("HTTP_ADDR", ":8080"),
		PublicBaseURL:         strings.TrimRight(getEnv("PUBLIC_BASE_URL", ""), "/"),
		DatabaseURL:           strings.TrimSpace(os.Getenv("DATABASE_URL")),
		RedisURL:              strings.TrimSpace(os.Getenv("REDIS_URL")),
		InitialAdminEmail:     strings.ToLower(strings.TrimSpace(os.Getenv("INITIAL_ADMIN_EMAIL"))),
		InitialAdminPassword:  os.Getenv("INITIAL_ADMIN_PASSWORD"),
		EmailCodeHMACKey:      os.Getenv("EMAIL_CODE_HMAC_KEY"),
		ShortCodeHMACKey:      os.Getenv("SHORT_CODE_HMAC_KEY"),
		SMTPHost:              strings.TrimSpace(os.Getenv("SMTP_HOST")),
		SMTPUsername:          os.Getenv("SMTP_USERNAME"),
		SMTPPassword:          os.Getenv("SMTP_PASSWORD"),
		SMTPFrom:              strings.TrimSpace(os.Getenv("SMTP_FROM")),
		SMTPTLSMode:           strings.ToLower(getEnv("SMTP_TLS_MODE", "starttls")),
		MinIOInternalEndpoint: strings.TrimSpace(os.Getenv("MINIO_INTERNAL_ENDPOINT")),
		MinIOPublicEndpoint:   strings.TrimRight(strings.TrimSpace(os.Getenv("MINIO_PUBLIC_ENDPOINT")), "/"),
		MinIOAccessKey:        os.Getenv("MINIO_ACCESS_KEY"),
		MinIOSecretKey:        os.Getenv("MINIO_SECRET_KEY"),
		CORSAllowedOrigins:    splitCSV(os.Getenv("CORS_ALLOWED_ORIGINS")),
		TrustedProxyCIDRs:     splitCSV(os.Getenv("TRUSTED_PROXY_CIDRS")),
	}

	var err error
	if len(cfg.CORSAllowedOrigins) == 0 && cfg.Environment == "development" {
		cfg.CORSAllowedOrigins = []string{"http://localhost:5173", "http://localhost:5174"}
	}
	if cfg.CORSAllowedOrigins, err = NormalizeCORSOrigins(cfg.CORSAllowedOrigins); err != nil {
		return Config{}, err
	}
	if cfg.SMTPPort, err = getInt("SMTP_PORT", 587); err != nil {
		return Config{}, err
	}
	if cfg.MinIOUseSSL, err = getBool("MINIO_USE_SSL", false); err != nil {
		return Config{}, err
	}

	// 2. 验证服务启动必需项，MinIO/SMTP 可在对应功能启用前单独配置。
	for key, value := range map[string]string{
		"DATABASE_URL":        cfg.DatabaseURL,
		"REDIS_URL":           cfg.RedisURL,
		"PUBLIC_BASE_URL":     cfg.PublicBaseURL,
		"EMAIL_CODE_HMAC_KEY": cfg.EmailCodeHMACKey,
		"SHORT_CODE_HMAC_KEY": cfg.ShortCodeHMACKey,
	} {
		if value == "" {
			return Config{}, fmt.Errorf("缺少必需环境变量 %s", key)
		}
	}
	if err := validateURL("DATABASE_URL", cfg.DatabaseURL); err != nil {
		return Config{}, err
	}
	if err := validateURL("REDIS_URL", cfg.RedisURL); err != nil {
		return Config{}, err
	}
	if err := validateURL("PUBLIC_BASE_URL", cfg.PublicBaseURL); err != nil {
		return Config{}, err
	}
	if cfg.SMTPPort < 1 || cfg.SMTPPort > 65535 {
		return Config{}, fmt.Errorf("SMTP_PORT 超出有效范围")
	}
	if cfg.SMTPTLSMode != "starttls" && cfg.SMTPTLSMode != "implicit" && cfg.SMTPTLSMode != "none" {
		return Config{}, fmt.Errorf("SMTP_TLS_MODE 只能是 starttls、implicit 或 none")
	}
	if len(cfg.EmailCodeHMACKey) < 32 || len(cfg.ShortCodeHMACKey) < 32 {
		return Config{}, fmt.Errorf("EMAIL_CODE_HMAC_KEY 和 SHORT_CODE_HMAC_KEY 至少需要 32 个字符")
	}
	if cfg.Environment == "production" && !strings.HasPrefix(cfg.PublicBaseURL, "https://") {
		return Config{}, fmt.Errorf("生产环境 PUBLIC_BASE_URL 必须使用 HTTPS")
	}
	for _, cidr := range cfg.TrustedProxyCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return Config{}, fmt.Errorf("TRUSTED_PROXY_CIDRS 包含无效网段 %q", cidr)
		}
	}
	return cfg, nil
}

func getEnv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func getInt(key string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("环境变量 %s 必须是整数", key)
	}
	return parsed, nil
}

func getBool(key string, fallback bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("环境变量 %s 必须是布尔值", key)
	}
	return parsed, nil
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// NormalizeCORSOrigins 仅接受完整、无路径的显式来源；空列表表示拒绝跨域。
func NormalizeCORSOrigins(origins []string) ([]string, error) {
	result := make([]string, 0, len(origins))
	seen := make(map[string]bool, len(origins))
	for _, raw := range origins {
		origin := strings.TrimSpace(raw)
		if origin == "" {
			continue
		}
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.ContainsAny(origin, "*?#\\") {
			return nil, fmt.Errorf("CORS_ALLOWED_ORIGINS 包含无效来源 %q", origin)
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" && origin != "capacitor://localhost" {
			return nil, fmt.Errorf("CORS_ALLOWED_ORIGINS 包含不支持的来源 %q", origin)
		}
		if port := parsed.Port(); port != "" {
			value, err := strconv.Atoi(port)
			if err != nil || value < 1 || value > 65535 {
				return nil, fmt.Errorf("CORS_ALLOWED_ORIGINS 包含无效端口 %q", origin)
			}
		} else if strings.HasSuffix(parsed.Host, ":") {
			return nil, fmt.Errorf("CORS_ALLOWED_ORIGINS 包含无效端口 %q", origin)
		}
		origin = strings.ToLower(origin)
		if !seen[origin] {
			result = append(result, origin)
			seen[origin] = true
		}
	}
	return result, nil
}

func validateURL(name, raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("环境变量 %s 必须是完整 URL", name)
	}
	return nil
}
