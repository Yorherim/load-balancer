package balancer_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"load-balancer/internal/balancer"
	"load-balancer/internal/config"
	"load-balancer/internal/ratelimiter"
	"load-balancer/internal/response"
)

// --- Управляемый обработчик для Health Checks ---

type healthAwareHandler struct {
	id           int
	healthPath   string
	isHealthy    bool
	mux          sync.RWMutex
	requestCount int // Счетчик запросов к этому бэкенду
}

func newHealthAwareHandler(id int, healthPath string) *healthAwareHandler {
	return &healthAwareHandler{
		id:         id,
		healthPath: healthPath,
		isHealthy:  true, // По умолчанию здоров
	}
}

func (h *healthAwareHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.RLock()
	healthy := h.isHealthy
	path := h.healthPath
	requestNum := h.requestCount + 1 // Локальная копия для записи в ответ
	h.mux.RUnlock()

	if r.URL.Path == path {
		// Это запрос Health Check
		if healthy {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, "OK")
		} else {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintln(w, "Not Healthy")
		}
		return
	}

	// Это обычный запрос
	h.mux.Lock()
	h.requestCount = requestNum // Увеличиваем счетчик
	h.mux.Unlock()
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Backend %d Request %d OK\n", h.id, requestNum)
}

// setHealth устанавливает состояние здоровья обработчика.
func (h *healthAwareHandler) setHealth(healthy bool) {
	h.mux.Lock()
	h.isHealthy = healthy
	h.mux.Unlock()
}

// --- Обновленная настройка теста ---

// testSetup содержит настройки и компоненты для интеграционного теста.
type testSetup struct {
	balancer          *balancer.Balancer
	backends          []*httptest.Server
	backendHandlers   []*healthAwareHandler // Добавляем хэндлеры для управления
	rateLimiter       *ratelimiter.RateLimiter
	mockStore         *MockRateLimitStore // Используем мок, определенный в balancer_test.go
	backendURLs       []string
	clientIDHeader    string
	healthCheckConfig config.HealthCheckConfig // Добавляем конфиг HC
	lbAlgorithm       string                   // Добавляем алгоритм
}

// setupIntegrationTest создает окружение для интеграционного теста.
// numBackends - количество тестовых бэкендов для запуска.
// rlEnabled - включить ли Rate Limiter.
// rlRate, rlCapacity - дефолтные лимиты Rate Limiter.
// hcEnabled - включить ли Health Check.
// hcInterval - интервал между проверками Health Check.
// hcTimeout - таймаут ожидания ответа от Health Check.
// hcPath - путь для Health Check.
// lbAlgorithm - алгоритм балансировки нагрузки
func setupIntegrationTest(t *testing.T,
	numBackends int,
	rlEnabled bool, rlRate, rlCapacity float64,
	hcEnabled bool, hcInterval, hcTimeout time.Duration, hcPath string,
	lbAlgorithm string, // Новый параметр алгоритма
) *testSetup {
	t.Helper()

	// Устанавливаем дефолтный алгоритм, если не указан (для старых тестов)
	if lbAlgorithm == "" {
		lbAlgorithm = "round_robin"
	}

	clientIDHeader := "X-Test-Client-ID"

	setup := &testSetup{
		backends:        make([]*httptest.Server, numBackends),
		backendHandlers: make([]*healthAwareHandler, numBackends),
		backendURLs:     make([]string, numBackends),
		clientIDHeader:  clientIDHeader, // Используем переменную
		healthCheckConfig: config.HealthCheckConfig{
			Enabled:  hcEnabled,
			Interval: hcInterval,
			Timeout:  hcTimeout,
			Path:     hcPath,
		},
		lbAlgorithm: lbAlgorithm,
	}

	// Запускаем тестовые бэкенды с управляемыми обработчиками
	for i := 0; i < numBackends; i++ {
		handler := newHealthAwareHandler(i, setup.healthCheckConfig.Path)
		server := httptest.NewServer(handler)
		setup.backends[i] = server
		setup.backendHandlers[i] = handler
		setup.backendURLs[i] = server.URL
	}

	// Настраиваем Rate Limiter
	setup.mockStore = NewMockRateLimitStore() // Используем локальный конструктор
	// Если RL включен, настроим мок, чтобы он возвращал дефолтные конфиги при запросе
	if rlEnabled {
		// Используем mock.Anything, т.к. clientID может быть разным
		setup.mockStore.On("GetClientLimitConfig", mock.Anything).Return(rlRate, rlCapacity, false, nil)
	}

	// Создаем фиктивный конфиг для RateLimiter
	rlConfig := config.RateLimiterConfig{
		Enabled:          rlEnabled,
		DefaultRate:      rlRate,
		DefaultCapacity:  rlCapacity,
		IdentifierHeader: clientIDHeader,
		// DatabasePath не нужен, так как используем mockStore
	}
	// Используем сигнатуру New с интерфейсом StoreConfigInterface
	var err error
	// Передаем конфиг и mock store
	setup.rateLimiter, err = ratelimiter.New(&rlConfig, setup.mockStore)
	require.NoError(t, err, "Ошибка создания Rate Limiter")

	// Создаем балансировщик
	setup.balancer, err = balancer.New(setup.backendURLs, setup.rateLimiter, setup.healthCheckConfig, setup.lbAlgorithm)
	require.NoError(t, err, "Ошибка создания балансировщика")

	// Очистка после теста
	t.Cleanup(func() {
		setup.balancer.StopHealthChecks()
		for _, srv := range setup.backends {
			srv.Close()
		}
		if setup.rateLimiter != nil { // Проверяем на nil перед вызовом Stop
			setup.rateLimiter.Stop()
		}
	})

	return setup
}

// sendRequest отправляет запрос на балансировщик и возвращает тело ответа и статус код.
func sendRequest(t *testing.T, handler http.Handler, clientID string, clientHeader string) (string, int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if clientID != "" {
		req.Header.Set(clientHeader, clientID)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	respBody, err := io.ReadAll(w.Body)
	require.NoError(t, err)
	return string(respBody), w.Code
}

// TestIntegration_RoundRobin проверяет корректность Round Robin распределения.
func TestIntegration_RoundRobin(t *testing.T) {
	numBackends := 3
	ts := setupIntegrationTest(t, numBackends,
		false, 10, 10, // RL выключен
		false, 0, 0, "", // HC
		"round_robin", // LB Algorithm
	)

	expectedPattern := ""
	for i := 0; i < numBackends; i++ {
		// Ожидаем "Backend X Request Y OK\n"
		expectedPattern += fmt.Sprintf("Backend %d Request %d OK\n", i, 1) // Первый запрос к каждому
	}
	for i := 0; i < numBackends; i++ {
		expectedPattern += fmt.Sprintf("Backend %d Request %d OK\n", i, 2) // Второй запрос к каждому
	}

	responses := ""
	for i := 0; i < numBackends*2; i++ {
		body, code := sendRequest(t, ts.balancer, "rr-client", ts.clientIDHeader)
		require.Equal(t, http.StatusOK, code, "Ожидался статус 200 OK")
		responses += body
	}

	assert.Equal(t, expectedPattern, responses, "Запросы не прошли по Round Robin")
}

// TestIntegration_RateLimiting проверяет работу Rate Limiter.
func TestIntegration_RateLimiting(t *testing.T) {
	rate := 1.0
	capacity := 2.0
	ts := setupIntegrationTest(t, 1,
		true, rate, capacity, // RL включен
		false, 0, 0, "", // HC
		"round_robin", // LB Algorithm
	)

	clientID := "rl-client"

	// Настроим мок для этого конкретного теста (setup уже настроил его на возврат дефолтов)
	// Здесь ничего дополнительно настраивать не нужно, setup уже настроил .Return

	// 1. Первые два запроса должны пройти
	body, code := sendRequest(t, ts.balancer, clientID, ts.clientIDHeader)
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "Backend 0 Request 1 OK")

	body, code = sendRequest(t, ts.balancer, clientID, ts.clientIDHeader)
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "Backend 0 Request 2 OK")

	// 2. Третий запрос должен быть заблокирован
	body, code = sendRequest(t, ts.balancer, clientID, ts.clientIDHeader)
	require.Equal(t, http.StatusTooManyRequests, code, "Ожидался статус 429")
	// --- Проверка JSON ---
	var errResp3 response.ErrorResponse
	err := json.Unmarshal([]byte(body), &errResp3)
	require.NoError(t, err, "Не удалось распарсить JSON ошибки 429: %s", body)
	assert.Equal(t, http.StatusTooManyRequests, errResp3.Code, "Incorrect error code in body 3")
	assert.Contains(t, errResp3.Message, "Rate limit exceeded", "Incorrect error message in body 3")
	// --------------------

	// 3. Ждем > 1 сек, чтобы токены пополнились (тикер работает)
	// Увеличим паузу для большей надежности
	time.Sleep(2100 * time.Millisecond)

	// 4. Четвертый запрос должен пройти
	body, code = sendRequest(t, ts.balancer, clientID, ts.clientIDHeader)
	require.Equal(t, http.StatusOK, code, "Ожидался статус 200 OK после паузы")
	// Бэкенд теперь получил 3-й запрос
	assert.Contains(t, body, "Backend 0 Request 3 OK")

	// Проверим, что GetClientLimitConfig вызывался
	ts.mockStore.AssertCalled(t, "GetClientLimitConfig", clientID)
}

// TestIntegration_UnhealthyBackend проверяет пропуск нерабочего бэкенда (с использованием ErrorHandler прокси).
func TestIntegration_UnhealthyBackend(t *testing.T) {
	numBackends := 2
	ts := setupIntegrationTest(t, numBackends,
		false, 10, 10, // RL
		false, 0, 0, "", // HC
		"round_robin", // LB Algorithm
	)

	// Останавливаем первый бэкенд (индекс 0)
	t.Logf("Остановка бэкенда #0: %s", ts.backendURLs[0])
	ts.backends[0].Close() // Физически останавливаем сервер

	// Делаем несколько запросов
	firstResponse502 := false
	healthyBackendHitCount := 0
	for i := 0; i < numBackends*3; i++ {
		body, code := sendRequest(t, ts.balancer, "unhealthy-test", ts.clientIDHeader)

		if code == http.StatusBadGateway {
			// Первый запрос к упавшему бэкенду должен вернуть 502 (обработано ErrorHandler)
			t.Logf("Получен ожидаемый 502 Bad Gateway при попытке доступа к бэкенду #0")
			assert.False(t, firstResponse502, "502 Bad Gateway получен более одного раза")
			// Проверяем тело ответа ErrorHandler'а - должен быть JSON
			var errResp response.ErrorResponse
			err := json.Unmarshal([]byte(body), &errResp)
			require.NoError(t, err, "Не удалось распарсить JSON ошибки 502: %s", body)
			assert.Equal(t, http.StatusBadGateway, errResp.Code, "Incorrect code in Bad Gateway body")
			assert.Contains(t, errResp.Message, "Bad Gateway from Custom Handler", "Incorrect message in Bad Gateway body")
			firstResponse502 = true
			continue // Пропускаем проверку тела
		}

		// Все остальные запросы должны идти на бэкенд #1 и возвращать 200 OK
		require.Equal(t, http.StatusOK, code, "Ожидался статус 200 OK от рабочего бэкенда")
		assert.Contains(t, body, "Backend 1", "Запрос ушел не на рабочий бэкенд #1")
		healthyBackendHitCount++
	}

	assert.True(t, firstResponse502, "Не был получен 502 Bad Gateway при запросе к упавшему бэкенду")
	// Проверяем, что рабочий бэкенд получил запросы (все, кроме первого)
	assert.Equal(t, numBackends*3-1, healthyBackendHitCount, "Рабочий бэкенд не получил ожидаемое количество запросов")
}

// TestIntegration_AllBackendsUnhealthy проверяет возврат 503 (с использованием ErrorHandler прокси).
func TestIntegration_AllBackendsUnhealthy(t *testing.T) {
	numBackends := 2
	ts := setupIntegrationTest(t, numBackends,
		false, 10, 10, // RL
		false, 0, 0, "", // HC
		"round_robin", // LB Algorithm
	)

	// Останавливаем все бэкенды
	for i := range ts.backends {
		t.Logf("Остановка бэкенда #%d: %s", i, ts.backendURLs[i])
		ts.backends[i].Close()
	}

	// Первые запросы (по числу бэкендов) должны вернуть 502, помечая бэкенды как нерабочие
	for i := 0; i < numBackends; i++ {
		_, code := sendRequest(t, ts.balancer, fmt.Sprintf("unhealthy-all-%d", i), ts.clientIDHeader)
		assert.Equal(t, http.StatusBadGateway, code, "Ожидался 502 при первом запросе к упавшему бэкенду #%d", i)
	}

	// Следующий запрос должен вернуть 503 Service Unavailable
	t.Log("Отправка запроса, когда все бэкенды должны быть помечены как нерабочие")
	body, code := sendRequest(t, ts.balancer, "unhealthy-all-final", ts.clientIDHeader)
	require.Equal(t, http.StatusServiceUnavailable, code, "Ожидался статус 503, когда все бэкенды недоступны")
	// --- Проверка JSON ---
	var errResp response.ErrorResponse
	err := json.Unmarshal([]byte(body), &errResp)
	require.NoError(t, err, "Не удалось распарсить JSON ошибки 503: %s", body)
	assert.Equal(t, http.StatusServiceUnavailable, errResp.Code, "Incorrect code in 503 error body")
	assert.Contains(t, errResp.Message, "All backend servers are unavailable", "Incorrect message in 503 error body")
}

// TestIntegration_HealthChecks проверяет работу Health Checks.
func TestIntegration_HealthChecks(t *testing.T) {
	numBackends := 2
	healthPath := "/healthz"
	hcInterval := 150 * time.Millisecond
	hcTimeout := 50 * time.Millisecond
	ts := setupIntegrationTest(t, numBackends,
		false, 10, 10, // RL
		true, hcInterval, hcTimeout, healthPath, // HC
		"round_robin", // LB Algorithm
	)

	// Даем время на запуск HC и первоначальную проверку
	time.Sleep(hcInterval * 2)

	// 1. Проверяем начальное состояние: все бэкенды должны быть живы
	for i, backend := range ts.balancer.GetBackends() { // Предполагаем наличие метода GetBackends()
		assert.True(t, backend.IsAlive(), "Бэкенд #%d должен быть жив изначально", i)
	}

	// 2. Симулируем падение бэкенда #0
	backendToFailIndex := 0
	t.Logf("Симуляция сбоя Health Check для бэкенда #%d (%s)", backendToFailIndex, ts.backendURLs[backendToFailIndex])
	ts.backendHandlers[backendToFailIndex].setHealth(false)

	// 3. Ждем, пока Health Check обнаружит падение
	// Используем assert.Eventually для ожидания изменения статуса
	require.Eventually(t, func() bool {
		// Получаем актуальный статус бэкенда из балансировщика
		backends := ts.balancer.GetBackends()
		if len(backends) <= backendToFailIndex {
			return false // На случай, если бэкенды еще не полностью инициализированы
		}
		return !backends[backendToFailIndex].IsAlive()
	}, hcInterval*4, hcInterval/2, "Health Check не пометил бэкенд #%d как нерабочий", backendToFailIndex)

	t.Logf("Бэкенд #%d помечен как нерабочий Health Check'ом", backendToFailIndex)

	// 4. Отправляем запросы - они должны идти только на бэкенд #1
	for i := 0; i < 5; i++ {
		body, code := sendRequest(t, ts.balancer, "hc-fail-test", ts.clientIDHeader)
		// Ожидаем 200 OK, так как рабочий бэкенд #1 должен взять нагрузку
		require.Equal(t, http.StatusOK, code, "Ожидался статус 200 OK от рабочего бэкенда #1")
		// Проверяем, что ответ именно от бэкенда #1
		assert.Contains(t, body, "Backend 1", "Запрос ушел не на рабочий бэкенд #1")
	}

	// 5. Симулируем восстановление бэкенда #0
	t.Logf("Симуляция восстановления Health Check для бэкенда #%d", backendToFailIndex)
	ts.backendHandlers[backendToFailIndex].setHealth(true)

	// 6. Ждем, пока Health Check обнаружит восстановление
	require.Eventually(t, func() bool {
		backends := ts.balancer.GetBackends()
		if len(backends) <= backendToFailIndex {
			return false
		}
		return backends[backendToFailIndex].IsAlive()
	}, hcInterval*4, hcInterval/2, "Health Check не пометил бэкенд #%d как рабочий после восстановления", backendToFailIndex)

	t.Logf("Бэкенд #%d помечен как рабочий Health Check'ом", backendToFailIndex)

	// 7. Отправляем запросы - они должны снова распределяться по Round Robin
	responses := map[int]int{0: 0, 1: 0} // Счетчик ответов от каждого бэкенда
	for i := 0; i < numBackends*3; i++ {
		body, code := sendRequest(t, ts.balancer, "hc-recover-test", ts.clientIDHeader)
		require.Equal(t, http.StatusOK, code, "Ожидался статус 200 OK после восстановления")

		// Определяем, какой бэкенд ответил, и инкрементируем счетчик
		respondedBackendID := -1
		if strings.Contains(body, "Backend 0") {
			respondedBackendID = 0
		} else if strings.Contains(body, "Backend 1") {
			respondedBackendID = 1
		}
		// Используем require, чтобы тест упал, если ответ не от ожидаемого бэкенда
		require.NotEqual(t, -1, respondedBackendID, "Неожиданный ответ бэкенда: %s", body)
		responses[respondedBackendID]++
	}

	// Проверяем, что оба бэкенда получили запросы после восстановления
	assert.Greater(t, responses[0], 0, "Бэкенд #0 не получил запросов после восстановления")
	assert.Greater(t, responses[1], 0, "Бэкенд #1 не получил запросов после восстановления")
	// Проверяем примерное равенство для Round Robin
	assert.InDelta(t, responses[0], responses[1], 1, "Распределение запросов неравномерно после восстановления (ожидалось %d +/- 1)", numBackends*3/numBackends)
}

// --- Новый тест для Random алгоритма ---

func TestIntegration_Random(t *testing.T) {
	numBackends := 3
	ts := setupIntegrationTest(t, numBackends,
		false, 100, 100, // RL (выключен или с большим лимитом)
		false, 0, 0, "", // HC выключен
		"random", // LB Algorithm
	)

	numRequests := numBackends * 10 // Достаточное количество запросов для проверки
	responses := make(map[int]int)  // responses[backendID] = count
	for i := 0; i < numBackends; i++ {
		responses[i] = 0
	}

	t.Logf("Отправка %d запросов с использованием Random алгоритма...", numRequests)
	for i := 0; i < numRequests; i++ {
		body, code := sendRequest(t, ts.balancer, fmt.Sprintf("random-client-%d", i), ts.clientIDHeader)
		require.Equal(t, http.StatusOK, code, "Ожидался статус 200 OK")

		respondedBackendID := -1
		for backendID := 0; backendID < numBackends; backendID++ {
			if strings.Contains(body, fmt.Sprintf("Backend %d", backendID)) {
				respondedBackendID = backendID
				break
			}
		}
		require.NotEqual(t, -1, respondedBackendID, "Неожиданный ответ бэкенда: %s", body)
		responses[respondedBackendID]++
	}

	// Проверяем, что все бэкенды получили хотя бы один запрос
	// При случайном распределении нет гарантии равномерности, особенно на малых числах,
	// но каждый должен иметь шанс быть выбранным.
	for backendID := 0; backendID < numBackends; backendID++ {
		assert.Greater(t, responses[backendID], 0, "Бэкенд #%d не получил ни одного запроса при Random алгоритме", backendID)
		t.Logf("Бэкенд #%d получил %d запросов", backendID, responses[backendID])
	}
	// Можно добавить проверку, что сумма всех ответов равна numRequests
	totalResponses := 0
	for _, count := range responses {
		totalResponses += count
	}
	assert.Equal(t, numRequests, totalResponses, "Общее количество ответов не совпадает с количеством запросов")
}

// TestIntegration_Random_AllBackendsUnhealthy проверяет ответ 503 для Random алгоритма.
func TestIntegration_Random_AllBackendsUnhealthy(t *testing.T) {
	numBackends := 2
	ts := setupIntegrationTest(t, numBackends,
		false, 10, 10, // RL
		false, 0, 0, "", // HC
		"random", // !!! LB Algorithm
	)

	// Останавливаем все бэкенды
	for i := range ts.backends {
		t.Logf("Остановка бэкенда #%d: %s", i, ts.backendURLs[i])
		ts.backends[i].Close()
	}

	// Отправляем запросы, чтобы ErrorHandler пометил бэкенды как нерабочие
	// С Random алгоритмом нет гарантии, что мы попадем на все бэкенды за N запросов,
	// но ErrorHandler срабатывает при *любой* ошибке проксирования.
	// Важно, чтобы в итоге *все* были помечены как нерабочие. Попробуем сделать N*2 запросов.
	for i := 0; i < numBackends*2; i++ {
		_, code := sendRequest(t, ts.balancer, fmt.Sprintf("random-unhealthy-%d", i), ts.clientIDHeader)
		// Ожидаем 502, пока балансировщик пытается проксировать
		if code != http.StatusBadGateway {
			// Если получили не 502, возможно, балансировщик уже вернул 503, что тоже приемлемо на этом этапе.
			assert.Equal(t, http.StatusServiceUnavailable, code, "Ожидался 502 или 503 на этапе обнаружения нерабочих бэкендов")
		}
		// Дадим небольшую паузу, чтобы состояние Alive успело обновиться (хотя мьютекс должен справляться)
		time.Sleep(10 * time.Millisecond)
	}

	// Проверяем, что все бэкенды действительно помечены как нерабочие
	require.Eventually(t, func() bool {
		for _, be := range ts.balancer.GetBackends() {
			if be.IsAlive() {
				return false // Хотя бы один еще жив
			}
		}
		return true // Все мертвы
	}, time.Second, 50*time.Millisecond, "Не все бэкенды помечены как нерабочие")

	// Следующий запрос должен точно вернуть 503 Service Unavailable
	t.Log("Отправка запроса, когда все бэкенды (random) должны быть нерабочие")
	body, code := sendRequest(t, ts.balancer, "random-unhealthy-final", ts.clientIDHeader)
	require.Equal(t, http.StatusServiceUnavailable, code, "Ожидался статус 503, когда все бэкенды недоступны (random)")
	// --- Проверка JSON ---
	var errResp response.ErrorResponse
	err := json.Unmarshal([]byte(body), &errResp)
	require.NoError(t, err, "Не удалось распарсить JSON ошибки 503 (random): %s", body)
	assert.Equal(t, http.StatusServiceUnavailable, errResp.Code, "Incorrect code in 503 error body")
	assert.Contains(t, errResp.Message, "All backend servers are unavailable", "Incorrect message in 503 error body")
	// --------------------
}
