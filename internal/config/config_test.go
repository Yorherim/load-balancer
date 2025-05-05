package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"load-balancer/internal/config"
)

// TestLoadConfig_Success проверяет успешную загрузку валидного конфига.
func TestLoadConfig_Success(t *testing.T) {
	yamlContent := `
port: "8081"
backend_servers:
  - "http://backend1:9000"
  - "http://backend2:9001"
load_balancing_algorithm: "random"
health_check:
  enabled: true
  interval: "15s"
  timeout: "3s"
  path: "/healthz"
rate_limiter:
  enabled: true
  default_rate: 100
  default_capacity: 200
  identifier_header: "X-My-Client-ID"
  database_path: "/data/limits.db"
`
	// Создаем временный файл
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "config.yaml")
	err := os.WriteFile(tmpFile, []byte(yamlContent), 0o644)
	require.NoError(t, err, "Не удалось создать временный файл конфига")

	// Загружаем конфиг
	cfg, err := config.LoadConfig(tmpFile)
	require.NoError(t, err, "LoadConfig вернул ошибку для валидного файла")
	require.NotNil(t, cfg, "LoadConfig вернул nil cfg для валидного файла")

	// Проверяем основные поля
	assert.Equal(t, "8081", cfg.Port)
	assert.Equal(t, []string{"http://backend1:9000", "http://backend2:9001"}, cfg.BackendServers)
	assert.Equal(t, "random", cfg.LoadBalancingAlgorithm)

	// Проверяем HealthCheck
	assert.True(t, cfg.HealthCheck.Enabled)
	assert.Equal(t, "15s", cfg.HealthCheck.IntervalStr)
	assert.Equal(t, 15*time.Second, cfg.HealthCheck.Interval)
	assert.Equal(t, "3s", cfg.HealthCheck.TimeoutStr)
	assert.Equal(t, 3*time.Second, cfg.HealthCheck.Timeout)
	assert.Equal(t, "/healthz", cfg.HealthCheck.Path)

	// Проверяем RateLimiter
	assert.True(t, cfg.RateLimiter.Enabled)
	assert.Equal(t, 100.0, cfg.RateLimiter.DefaultRate)
	assert.Equal(t, 200.0, cfg.RateLimiter.DefaultCapacity)
	assert.Equal(t, "X-My-Client-ID", cfg.RateLimiter.IdentifierHeader)
	assert.Equal(t, "/data/limits.db", cfg.RateLimiter.DatabasePath)
}

// TestLoadConfig_FileNotFound проверяет ошибку, если файл не найден.
func TestLoadConfig_FileNotFound(t *testing.T) {
	_, err := config.LoadConfig("non_existent_file.yaml")
	require.Error(t, err, "LoadConfig не вернул ошибку для несуществующего файла")
	// Проверяем на конкретную ошибку отсутствия файла
	assert.True(t, os.IsNotExist(err), "Ожидалась ошибка os.ErrNotExist")
}

// TestLoadConfig_InvalidYAML проверяет ошибку при невалидном формате YAML.
func TestLoadConfig_InvalidYAML(t *testing.T) {
	yamlContent := `
port: 8080:
backend_servers:
  - http://backend1
`
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "invalid.yaml")
	err := os.WriteFile(tmpFile, []byte(yamlContent), 0o644)
	require.NoError(t, err)

	_, err = config.LoadConfig(tmpFile)
	require.Error(t, err, "LoadConfig не вернул ошибку для невалидного YAML")
	// Не проверяем точный текст ошибки YAML парсера, он может меняться
	assert.ErrorContains(t, err, "yaml:", "Текст ошибки не похож на ошибку YAML парсера")
}

// TestLoadConfig_Defaults проверяет установку значений по умолчанию.
func TestLoadConfig_Defaults(t *testing.T) {
	// Минимально необходимый конфиг
	yamlContent := `
port: "9000"
backend_servers:
  - "http://onlyone:80"
`
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "defaults.yaml")
	err := os.WriteFile(tmpFile, []byte(yamlContent), 0o644)
	require.NoError(t, err)

	cfg, err := config.LoadConfig(tmpFile)
	require.NoError(t, err)
	require.NotNil(t, cfg)

	// Проверяем значения по умолчанию
	assert.Equal(t, "round_robin", cfg.LoadBalancingAlgorithm)
	assert.False(t, cfg.HealthCheck.Enabled)
	// Значения Interval/Timeout не должны парситься, если Enabled=false
	assert.Zero(t, cfg.HealthCheck.Interval)
	assert.Zero(t, cfg.HealthCheck.Timeout)
	assert.Equal(t, "", cfg.HealthCheck.Path)
	assert.False(t, cfg.RateLimiter.Enabled)
	assert.Equal(t, 1.0, cfg.RateLimiter.DefaultRate)
	assert.Equal(t, 1.0, cfg.RateLimiter.DefaultCapacity)
	assert.Equal(t, "", cfg.RateLimiter.IdentifierHeader)
	assert.Equal(t, "./rate_limits.db", cfg.RateLimiter.DatabasePath)
}

// TestLoadConfig_InvalidDuration проверяет ошибку при невалидном формате времени.
func TestLoadConfig_InvalidDuration(t *testing.T) {
	yamlContent := `
port: "8080"
backend_servers: ["http://b1"]
health_check:
  enabled: true
  interval: "неверно" # Невалидное значение
`
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "invalid_duration.yaml")
	err := os.WriteFile(tmpFile, []byte(yamlContent), 0o644)
	require.NoError(t, err)

	_, err = config.LoadConfig(tmpFile)
	require.Error(t, err, "LoadConfig не вернул ошибку для невалидного interval")
	assert.ErrorContains(t, err, "неверный формат интервала HealthCheck", "Текст ошибки не содержит ожидаемую подстроку")
}

// TestLoadConfig_InvalidAlgorithm проверяет ошибку при невалидном алгоритме.
func TestLoadConfig_InvalidAlgorithm(t *testing.T) {
	yamlContent := `
port: "8080"
backend_servers: ["http://b1"]
load_balancing_algorithm: "least_latency"
`
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "invalid_algo.yaml")
	err := os.WriteFile(tmpFile, []byte(yamlContent), 0o644)
	require.NoError(t, err)

	_, err = config.LoadConfig(tmpFile)
	require.Error(t, err, "LoadConfig не вернул ошибку для невалидного алгоритма")
	assert.ErrorContains(t, err, "неподдерживаемый load_balancing_algorithm")
}
