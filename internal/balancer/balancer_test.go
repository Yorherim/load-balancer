// Package balancer_test содержит тесты для пакета balancer.
package balancer_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"load-balancer/internal/balancer" // Импортируем тестируемый пакет
	"load-balancer/internal/config"   // Добавляем импорт config
	"load-balancer/internal/ratelimiter"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock" // Добавляем mock
	"github.com/stretchr/testify/require"
)

// MockRateLimitStore - мок для интерфейса ratelimiter.StoreConfigInterface
// Скопировано из ratelimiter_test, но без методов StateStore.
type MockRateLimitStore struct {
	mock.Mock
}

// NewMockRateLimitStore создает новый экземпляр MockRateLimitStore
func NewMockRateLimitStore() *MockRateLimitStore {
	return &MockRateLimitStore{}
}

// --- Реализация методов интерфейса ratelimiter.StoreConfigInterface ---

func (m *MockRateLimitStore) GetClientLimitConfig(clientID string) (rate, capacity float64, found bool, err error) {
	args := m.Called(clientID)
	// Проверяем количество возвращаемых значений, чтобы избежать паники
	if len(args) < 4 {
		panic(fmt.Sprintf("MockRateLimitStore: GetClientLimitConfig called for %s, but not enough return values configured (%d)", clientID, len(args)))
	}
	return args.Get(0).(float64), args.Get(1).(float64), args.Bool(2), args.Error(3)
}

func (m *MockRateLimitStore) CreateClientLimit(clientID string, limit config.ClientRateConfig) error {
	args := m.Called(clientID, limit)
	return args.Error(0)
}

func (m *MockRateLimitStore) UpdateClientLimit(clientID string, limit config.ClientRateConfig) error {
	args := m.Called(clientID, limit)
	return args.Error(0)
}

func (m *MockRateLimitStore) DeleteClientLimit(clientID string) error {
	args := m.Called(clientID)
	return args.Error(0)
}

func (m *MockRateLimitStore) SupportsStatePersistence() bool {
	// В тестах balancer нам не нужно сохранение состояния
	return false
}

// setupTestBalancer создает тестовый бэкенд-сервер и экземпляр Balancer для тестов.
func setupTestBalancer(b *testing.B) (*balancer.Balancer, *httptest.Server) {
	b.Helper()

	// Создаем простой тестовый HTTP-сервер, имитирующий бэкенд.
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "Hello from backend")
	}))

	// Создаем мок-хранилище для Rate Limiter.
	mockStore := NewMockRateLimitStore() // Используем локальный мок
	// Настроим мок: для любого клиента возвращаем 'не найдено', чтобы использовались дефолты RL
	mockStore.On("GetClientLimitConfig", mock.Anything).Return(0.0, 0.0, false, nil)

	// Создаем фиктивный конфиг Rate Limiter
	rlCfg := &config.RateLimiterConfig{
		Enabled:          true,
		DefaultRate:      1000, // Высокие лимиты для бенчмарка
		DefaultCapacity:  1000,
		IdentifierHeader: "X-Client-ID",
	}
	// Создаем Rate Limiter, используя новую сигнатуру
	rl, errRl := ratelimiter.New(rlCfg, mockStore)
	if errRl != nil {
		b.Fatalf("Ошибка создания тестового Rate Limiter: %v", errRl)
	}

	// Создаем Balancer с URL нашего тестового бэкенда.
	// Добавляем пустой конфиг Health Check и алгоритм по умолчанию.
	hcConfig := config.HealthCheckConfig{Enabled: false}
	lbAlgorithm := "round_robin"
	lb, err := balancer.New([]string{backendServer.URL}, rl, hcConfig, lbAlgorithm)
	if err != nil {
		b.Fatalf("Ошибка создания тестового балансировщика: %v", err)
	}

	// Добавляем Cleanup для закрытия тестового сервера после тестов.
	b.Cleanup(func() {
		backendServer.Close()
		if rl != nil {
			rl.Stop() // Останавливаем RateLimiter, если он был запущен с Ticker
		}
		if lb != nil {
			lb.StopHealthChecks() // Также останавливаем Health Checks, если они были запущены
		}
	})

	return lb, backendServer
}

// BenchmarkServeHTTP измеряет производительность основного обработчика балансировщика.
func BenchmarkServeHTTP(b *testing.B) {
	lb, _ := setupTestBalancer(b)

	// Сбрасываем таймер, чтобы не учитывать время на setup.
	b.ResetTimer()

	// Запускаем бенчмарк параллельно.
	b.RunParallel(func(pb *testing.PB) {
		// Каждая горутина будет выполнять этот код в цикле.
		for pb.Next() {
			// Создаем тестовый запрос.
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			// Добавляем заголовок для идентификации (не обязательно для этого бенчмарка).
			req.Header.Set("X-Client-ID", "bench-client")
			// Создаем рекордер для записи ответа.
			w := httptest.NewRecorder()

			// Вызываем тестируемый обработчик.
			lb.ServeHTTP(w, req)

			// Проверяем базовый результат (не обязательно для бенчмарка, но полезно).
			if w.Code != http.StatusOK {
				respBody, _ := io.ReadAll(w.Body)
				b.Errorf("Ожидался статус 200 OK, получено %d. Body: %s", w.Code, string(respBody))
				// Не используем Fatalf в RunParallel, чтобы не остановить другие горутины.
			}
		}
	})
}

// --- Unit тесты ---

func TestNewBalancer_InvalidAlgorithm(t *testing.T) {
	backendUrls := []string{"http://localhost:1234"}
	// Используем конструктор New с nil store для RL
	rlCfg := &config.RateLimiterConfig{Enabled: false} // RL выключен
	rl, errRl := ratelimiter.New(rlCfg, nil)
	require.NoError(t, errRl, "Ошибка создания выключенного Rate Limiter")

	hcConfig := config.HealthCheckConfig{Enabled: false}
	invalidAlgo := "least_connections"

	// Логгер теперь выводит Warning и использует дефолтный алгоритм
	// Проверяем, что ошибки нет и балансировщик создан
	lb, err := balancer.New(backendUrls, rl, hcConfig, invalidAlgo)
	require.NoError(t, err, "Не должно быть ошибки для невалидного алгоритма")
	require.NotNil(t, lb, "Балансировщик должен быть создан")
	// TODO: Проверить, что используется round_robin (сложно без экспорта поля)
}

func TestNewBalancer_NoBackends(t *testing.T) {
	backendUrls := []string{} // Пустой срез
	rlCfg := &config.RateLimiterConfig{Enabled: false}
	rl, errRl := ratelimiter.New(rlCfg, nil)
	require.NoError(t, errRl)
	hcConfig := config.HealthCheckConfig{Enabled: false}

	lb, err := balancer.New(backendUrls, rl, hcConfig, "round_robin")
	assert.Error(t, err, "Должна быть ошибка при отсутствии бэкендов")
	assert.Nil(t, lb, "Балансировщик должен быть nil при ошибке")
	assert.Contains(t, err.Error(), "не указаны бэкенд-серверы")
}

func TestNewBalancer_InvalidBackendURL(t *testing.T) {
	backendUrls := []string{"http://localhost:1234", "invalid-url"} // Невалидный URL
	rlCfg := &config.RateLimiterConfig{Enabled: false}
	rl, errRl := ratelimiter.New(rlCfg, nil)
	require.NoError(t, errRl)
	hcConfig := config.HealthCheckConfig{Enabled: false}

	lb, err := balancer.New(backendUrls, rl, hcConfig, "round_robin")

	// 1. Проверяем, что ошибка НЕ nil
	assert.Error(t, err, "Должна быть ошибка при невалидном URL бэкенда")

	// 2. Если ошибка есть, проверяем ее содержимое и что lb равен nil
	if err != nil {
		assert.Nil(t, lb, "Балансировщик должен быть nil при ошибке")
		assert.Contains(t, err.Error(), "должен быть абсолютным", "Текст ошибки не содержит ожидаемую подстроку")
		assert.Contains(t, err.Error(), "invalid-url", "Текст ошибки не содержит невалидный URL")
	} else {
		t.Errorf("Ожидалась ошибка парсинга URL, но получено nil")
	}
}
