package ratelimiter_test

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"load-balancer/internal/config"
	"load-balancer/internal/ratelimiter"
	"load-balancer/internal/storage"
)

var (
	_ ratelimiter.StoreConfigInterface = (*MockStore)(nil)
	_ ratelimiter.StateStore           = (*MockStore)(nil)
)

// MockStore - мок для интерфейсов ratelimiter.StoreConfigInterface и ratelimiter.StateStore
type MockStore struct {
	mock.Mock
	// Добавляем поле, чтобы имитировать возможность быть *storage.DB
	isDB bool
	// Добавляем поле для хранения ожидаемых вызовов GetClientSavedState
	expectedSavedState map[string]struct {
		tokens     float64
		lastRefill time.Time
		found      bool
		err        error
	}
	// Добавляем поле для перехвата вызовов BatchUpdateClientState
	capturedBatchUpdate      map[string]storage.ClientState
	expectedBatchUpdateError error
}

// NewMockStore создает новый экземпляр MockStore
func NewMockStore() *MockStore {
	return &MockStore{
		expectedSavedState: make(map[string]struct {
			tokens     float64
			lastRefill time.Time
			found      bool
			err        error
		}),
		capturedBatchUpdate: nil, // Инициализируется при ожидании
	}
}

// --- Реализация методов интерфейса ratelimiter.StoreConfigInterface ---

func (m *MockStore) GetClientLimitConfig(clientID string) (rate, capacity float64, found bool, err error) {
	args := m.Called(clientID)
	return args.Get(0).(float64), args.Get(1).(float64), args.Bool(2), args.Error(3)
}

func (m *MockStore) CreateClientLimit(clientID string, limit config.ClientRateConfig) error {
	args := m.Called(clientID, limit)
	return args.Error(0)
}

func (m *MockStore) UpdateClientLimit(clientID string, limit config.ClientRateConfig) error {
	args := m.Called(clientID, limit)
	return args.Error(0)
}

func (m *MockStore) DeleteClientLimit(clientID string) error {
	args := m.Called(clientID)
	return args.Error(0)
}

func (m *MockStore) SupportsStatePersistence() bool {
	// Возвращаем значение флага isDB, чтобы контролировать поведение в тестах
	return m.isDB
}

// --- Методы для имитации *storage.DB --- (не часть интерфейса)

// AsDB делает мок "совместимым" с *storage.DB для type assertion
func (m *MockStore) AsDB() *MockStore {
	m.isDB = true
	return m
}

// GetClientSavedState имитирует метод *storage.DB
// Использует карту expectedSavedState для возврата значений
func (m *MockStore) GetClientSavedState(clientID string) (tokens float64, lastRefill time.Time, found bool, err error) {
	if !m.isDB {
		panic("GetClientSavedState called on MockStore not configured to support state (isDB=false)")
	}
	expected, ok := m.expectedSavedState[clientID]
	if !ok {
		// Возвращаем "не найдено" по умолчанию, если не задано ожидание
		return 0, time.Time{}, false, nil
	}
	return expected.tokens, expected.lastRefill, expected.found, expected.err
}

// ExpectGetClientSavedState задает ожидаемое возвращаемое значение для GetClientSavedState
func (m *MockStore) ExpectGetClientSavedState(clientID string, tokens float64, lastRefill time.Time, found bool, err error) {
	m.expectedSavedState[clientID] = struct {
		tokens     float64
		lastRefill time.Time
		found      bool
		err        error
	}{tokens, lastRefill, found, err}
}

// BatchUpdateClientState имитирует метод *storage.DB
// Сохраняет переданные данные в capturedBatchUpdate
func (m *MockStore) BatchUpdateClientState(states map[string]storage.ClientState) error {
	if !m.isDB {
		panic("BatchUpdateClientState called on MockStore not configured to support state (isDB=false)")
	}
	m.capturedBatchUpdate = states // Сохраняем для проверки
	return m.expectedBatchUpdateError
}

// ExpectBatchUpdate задает ожидаемую ошибку для BatchUpdateClientState и инициализирует карту для захвата
func (m *MockStore) ExpectBatchUpdate(err error) {
	m.expectedBatchUpdateError = err
	// Сбрасываем захваченные данные перед новым ожиданием
	m.capturedBatchUpdate = make(map[string]storage.ClientState)
}

// AssertBatchUpdateCalledWith проверяет, что BatchUpdateClientState был вызван с ожидаемыми данными
func (m *MockStore) AssertBatchUpdateCalledWith(t *testing.T, expected map[string]storage.ClientState) {
	require.NotNil(t, m.capturedBatchUpdate, "BatchUpdateClientState was not called")
	assert.Equal(t, expected, m.capturedBatchUpdate, "BatchUpdateClientState called with unexpected data")
}

// AssertBatchUpdateNotCalled проверяет, что BatchUpdateClientState не вызывался
func (m *MockStore) AssertBatchUpdateNotCalled(t *testing.T) {
	assert.Nil(t, m.capturedBatchUpdate, "BatchUpdateClientState should not have been called")
}

// TestNewRateLimiter проверяет конструктор RateLimiter.
func TestNewRateLimiter(t *testing.T) {
	mockStore := NewMockStore() // Используем старое имя

	// Тест 1: RL выключен
	cfgDisabled := &config.RateLimiterConfig{Enabled: false}
	rlDisabled, errDisabled := ratelimiter.New(cfgDisabled, mockStore)
	require.NoError(t, errDisabled)
	assert.False(t, rlDisabled.IsEnabled(), "RL должен быть выключен")
	assert.True(t, rlDisabled.Allow("any_client"), "Выключенный RL должен разрешать все")

	// Тест 2: RL включен
	cfgEnabled := &config.RateLimiterConfig{
		Enabled:          true,
		DefaultRate:      10,
		DefaultCapacity:  20,
		IdentifierHeader: "X-Test-ID",
	}
	// Для RL включенного не нужно настраивать мок, т.к. Allow не вызывается
	rlEnabled, errEnabled := ratelimiter.New(cfgEnabled, mockStore)
	require.NoError(t, errEnabled)
	assert.True(t, rlEnabled.IsEnabled(), "RL должен быть включен")
	rlEnabled.Stop() // Останавливаем тикер

	// Тест 3: RL включен, но store = nil
	rlNoStore, errNoStore := ratelimiter.New(cfgEnabled, nil)
	require.NoError(t, errNoStore) // Ошибки быть не должно, только warning
	assert.True(t, rlNoStore.IsEnabled())
	assert.True(t, rlNoStore.Allow("client1"), "RL без store должен работать с дефолтными лимитами")
	rlNoStore.Stop()
}

// TestRateLimiter_GetClientID проверяет получение ID клиента.
func TestRateLimiter_GetClientID(t *testing.T) {
	cfgHeader := &config.RateLimiterConfig{Enabled: true, IdentifierHeader: "X-Real-ID"}
	cfgIP := &config.RateLimiterConfig{Enabled: true, IdentifierHeader: ""} // Без заголовка, используем IP

	rlHeader, _ := ratelimiter.New(cfgHeader, nil)
	rlIP, _ := ratelimiter.New(cfgIP, nil)
	defer rlHeader.Stop()
	defer rlIP.Stop()

	// Тест 1: Есть заголовок
	reqHeader := httptest.NewRequest("GET", "/", nil)
	reqHeader.Header.Set("X-Real-ID", "user123")
	reqHeader.RemoteAddr = "192.0.2.1:12345"
	assert.Equal(t, "user123", rlHeader.GetClientID(reqHeader), "Должен быть ID из заголовка")

	// Тест 2: Заголовок настроен, но пуст в запросе -> fallback на IP
	reqHeaderEmpty := httptest.NewRequest("GET", "/", nil)
	reqHeaderEmpty.RemoteAddr = "192.0.2.1:12345"
	assert.Equal(t, "192.0.2.1", rlHeader.GetClientID(reqHeaderEmpty), "Должен быть IP, если заголовок пуст")

	// Тест 3: Используем IP (RemoteAddr)
	reqIP := httptest.NewRequest("GET", "/", nil)
	reqIP.RemoteAddr = "192.0.2.2:54321"
	assert.Equal(t, "192.0.2.2", rlIP.GetClientID(reqIP), "Должен быть IP из RemoteAddr")

	// Тест 4: Используем X-Forwarded-For
	reqXFF := httptest.NewRequest("GET", "/", nil)
	reqXFF.Header.Set("X-Forwarded-For", "10.0.0.1, 192.0.2.3")
	reqXFF.RemoteAddr = "172.16.0.1:8080"
	assert.Equal(t, "10.0.0.1", rlIP.GetClientID(reqXFF), "Должен быть первый IP из XFF")

	// Тест 5: X-Forwarded-For с пробелами и невалидными записями
	reqXFFSpaced := httptest.NewRequest("GET", "/", nil)
	reqXFFSpaced.Header.Set("X-Forwarded-For", " garbage , 10.0.0.2 , unknown")
	reqXFFSpaced.RemoteAddr = "172.16.0.1:8080"
	assert.Equal(t, "10.0.0.2", rlIP.GetClientID(reqXFFSpaced), "Должен быть валидный IP из XFF после очистки")

	// Тест 6: Только заголовок настроен, XFF есть, но заголовок приоритетнее
	reqHeaderWithXFF := httptest.NewRequest("GET", "/", nil)
	reqHeaderWithXFF.Header.Set("X-Real-ID", "user456")
	reqHeaderWithXFF.Header.Set("X-Forwarded-For", "10.0.0.3")
	reqHeaderWithXFF.RemoteAddr = "192.0.2.4:11111"
	assert.Equal(t, "user456", rlHeader.GetClientID(reqHeaderWithXFF), "Заголовок должен быть приоритетнее XFF")

	// Тест 7: Невалидный RemoteAddr
	reqInvalidAddr := httptest.NewRequest("GET", "/", nil)
	reqInvalidAddr.RemoteAddr = "invalid-address"
	assert.Equal(t, "invalid-address", rlIP.GetClientID(reqInvalidAddr), "Должен возвращаться RemoteAddr как есть при ошибке парсинга")
}

// TestRateLimiter_LoadState проверяет загрузку состояния из store (*storage.DB)
func TestRateLimiter_LoadState(t *testing.T) {
	mockStore := NewMockStore().AsDB() // Используем старое имя
	clientID := "load-state-client"

	// Конфиг, который вернет GetClientLimitConfig
	configRate := 10.0
	configCapacity := 50.0
	mockStore.On("GetClientLimitConfig", clientID).Return(configRate, configCapacity, true, nil).Once()

	// Состояние, которое вернет GetClientSavedState
	savedTokens := 5.0
	savedLastRefill := time.Now().Add(-10 * time.Second) // 10 секунд назад
	mockStore.ExpectGetClientSavedState(clientID, savedTokens, savedLastRefill, true, nil)

	cfg := &config.RateLimiterConfig{Enabled: true, DefaultRate: 1, DefaultCapacity: 1}
	rl, err := ratelimiter.New(cfg, mockStore)
	require.NoError(t, err)
	defer rl.Stop()

	// Вызываем Allow, чтобы триггернуть getOrCreateBucket и загрузку состояния
	// Ожидаемое состояние после загрузки и refill:
	// Прошло ~10 секунд.
	// Добавлено токенов: ~10 * configRate = 100.
	// Новые токены: min(configCapacity=50, savedTokens=5 + 100) = 50.
	// Ожидаем, что будет 50 токенов, и можно будет сделать запрос.
	assert.True(t, rl.Allow(clientID), "Первый Allow должен быть разрешен после загрузки и refill")

	// Проверяем, что моки были вызваны
	mockStore.AssertExpectations(t)
	// Дополнительно можно проверить внутреннее состояние корзины, но это не очень надежно.
}

// TestRateLimiter_LoadState_NotFound проверяет случай, когда конфиг найден, а состояние нет.
func TestRateLimiter_LoadState_NotFound(t *testing.T) {
	mockStore := NewMockStore().AsDB() // Используем старое имя
	clientID := "load-state-notfound"

	configRate := 10.0
	configCapacity := 50.0
	mockStore.On("GetClientLimitConfig", clientID).Return(configRate, configCapacity, true, nil).Once()

	// Ожидаем, что состояние НЕ будет найдено
	mockStore.ExpectGetClientSavedState(clientID, 0, time.Time{}, false, nil)

	cfg := &config.RateLimiterConfig{Enabled: true, DefaultRate: 1, DefaultCapacity: 1}
	rl, err := ratelimiter.New(cfg, mockStore)
	require.NoError(t, err)
	defer rl.Stop()

	// Ожидаем, что будет использовано начальное состояние (токены = capacity)
	// и refill ничего не сделает (т.к. lastRefill = now).
	// Должно быть 50 токенов.
	assert.True(t, rl.Allow(clientID), "Allow должен быть разрешен с начальным состоянием (токены=capacity)")

	mockStore.AssertExpectations(t)
}

// TestRateLimiter_LoadState_NotDB проверяет, что состояние не загружается, если store не *storage.DB.
func TestRateLimiter_LoadState_NotDB(t *testing.T) {
	mockStore := NewMockStore() // Используем старое имя
	clientID := "load-state-notdb"

	configRate := 10.0
	configCapacity := 50.0
	mockStore.On("GetClientLimitConfig", clientID).Return(configRate, configCapacity, true, nil).Once()

	// GetClientSavedState не должен вызываться

	cfg := &config.RateLimiterConfig{Enabled: true, DefaultRate: 1, DefaultCapacity: 1}
	rl, err := ratelimiter.New(cfg, mockStore)
	require.NoError(t, err)
	defer rl.Stop()

	// Ожидаем, что будет использовано начальное состояние (токены = capacity).
	assert.True(t, rl.Allow(clientID), "Allow должен быть разрешен с начальным состоянием (токены=capacity)")

	mockStore.AssertExpectations(t)
}

// TestRateLimiter_SaveState проверяет сохранение состояния в store (*storage.DB)
func TestRateLimiter_SaveState(t *testing.T) {
	mockStore := NewMockStore().AsDB() // Имитируем *storage.DB
	client1 := "save-client-1"
	client2 := "save-client-2"

	rate1, capacity1 := 1.0, 1.0
	rate2, capacity2 := 2.0, 2.0

	mockStore.On("GetClientLimitConfig", client1).Return(rate1, capacity1, true, nil)
	mockStore.On("GetClientLimitConfig", client2).Return(rate2, capacity2, true, nil)

	// Состояния не будут найдены при загрузке
	mockStore.ExpectGetClientSavedState(client1, 0, time.Time{}, false, nil)
	mockStore.ExpectGetClientSavedState(client2, 0, time.Time{}, false, nil)

	cfg := &config.RateLimiterConfig{Enabled: true}
	rl, err := ratelimiter.New(cfg, mockStore)
	require.NoError(t, err)
	// Не останавливаем тикер сразу, даем ему поработать

	// Выполняем запросы, чтобы изменить состояние
	require.True(t, rl.Allow(client1)) // client1: 1 -> 0 tokens
	require.True(t, rl.Allow(client2)) // client2: 2 -> 1 tokens
	require.True(t, rl.Allow(client2)) // client2: 1 -> 0 tokens

	// Ждем немного, чтобы lastRefill обновился и токены немного накопились
	// Увеличиваем паузу, чтобы тикер гарантированно сработал
	time.Sleep(1100 * time.Millisecond)
	// client1: 0 + ~1.1*1 = ~1.1 -> min(1, 1.1) = 1
	// client2: 0 + ~1.1*2 = ~2.2 -> min(2, 2.2) = 2

	rl.Stop() // Останавливаем тикер перед сохранением

	// Ожидаем вызов BatchUpdateClientState
	mockStore.ExpectBatchUpdate(nil) // Ожидаем успешное сохранение

	err = rl.SaveState()
	require.NoError(t, err, "SaveState failed")

	// Проверяем, что BatchUpdateClientState был вызван с правильными данными
	// Получаем фактически сохраненные данные из мока.
	captured := mockStore.capturedBatchUpdate
	require.NotNil(t, captured, "BatchUpdate не был вызван")
	require.Len(t, captured, 2, "Должно быть сохранено 2 клиента")

	state1, ok1 := captured[client1]
	require.True(t, ok1, "Состояние client1 должно быть сохранено")
	// Ожидаем ~1 токен
	assert.InDelta(t, 1.0, state1.Tokens, 0.1, "Client1 tokens mismatch")
	assert.False(t, state1.LastRefill.IsZero(), "Client1 lastRefill не должно быть нулевым")

	state2, ok2 := captured[client2]
	require.True(t, ok2, "Состояние client2 должно быть сохранено")
	// Ожидаем ~2 токена
	assert.InDelta(t, 2.0, state2.Tokens, 0.1, "Client2 tokens mismatch")
	assert.False(t, state2.LastRefill.IsZero(), "Client2 lastRefill не должно быть нулевым")

	// Убедимся, что GetClientLimitConfig вызывался для обоих клиентов
	mockStore.AssertExpectations(t)
}

// TestRateLimiter_SaveState_NotDB проверяет, что состояние не сохраняется, если store не *storage.DB
func TestRateLimiter_SaveState_NotDB(t *testing.T) {
	mockStore := NewMockStore() // Используем старое имя
	clientID := "save-notdb"

	mockStore.On("GetClientLimitConfig", clientID).Return(1.0, 1.0, true, nil).Once()

	cfg := &config.RateLimiterConfig{Enabled: true}
	rl, err := ratelimiter.New(cfg, mockStore)
	require.NoError(t, err)
	defer rl.Stop()

	// Вызываем Allow, чтобы создать корзину
	require.True(t, rl.Allow(clientID))

	// BatchUpdateState не должен вызываться
	err = rl.SaveState()
	require.NoError(t, err, "SaveState should not return error for non-DB store")

	// Проверяем, что BatchUpdateClientState НЕ был вызван
	mockStore.AssertBatchUpdateNotCalled(t)
	mockStore.AssertExpectations(t)
}
