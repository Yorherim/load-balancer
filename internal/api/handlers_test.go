package api_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"load-balancer/internal/api"
	"load-balancer/internal/config"
	"load-balancer/internal/storage"

	_ "modernc.org/sqlite"
)

// setupTestAPI создает тестовый APIHandler с временной БД.
func setupTestAPI(t *testing.T) (*api.APIHandler, func()) {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_api_limits.db")
	db, err := storage.NewSQLiteDB(dbPath)
	require.NoError(t, err, "Ошибка создания тестовой БД для API")
	require.NotNil(t, db, "DB не должен быть nil")

	apiHandler := api.NewAPIHandler(db)

	cleanup := func() {
		err := db.Close()
		assert.NoError(t, err, "Ошибка закрытия тестовой БД API")
	}
	return apiHandler, cleanup
}

// TestAPI_CRUD_Operations проверяет полный CRUD цикл через API.
func TestAPI_CRUD_Operations(t *testing.T) {
	apiHandler, cleanup := setupTestAPI(t)
	defer cleanup()

	server := httptest.NewServer(http.StripPrefix("/clients", apiHandler))
	defer server.Close()

	clientID := "api-client-1"
	initialLimit := config.ClientRateConfig{Rate: 10, Capacity: 100} // Используем ClientRateConfig

	createReqBody, _ := json.Marshal(map[string]interface{}{
		"client_id":    clientID,
		"rate_per_sec": initialLimit.Rate,
		"capacity":     initialLimit.Capacity,
	})
	resp, err := http.Post(server.URL+"/clients/", "application/json", bytes.NewBuffer(createReqBody))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode, "Create status code")
	// Проверяем тело ответа (ожидаем плоскую структуру)
	var createResp api.ClientLimitResponse
	err = json.NewDecoder(resp.Body).Decode(&createResp)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, clientID, createResp.ClientID)
	assert.Equal(t, initialLimit.Rate, createResp.Rate)         // Проверяем поле Rate
	assert.Equal(t, initialLimit.Capacity, createResp.Capacity) // Проверяем поле Capacity

	// 2. Попытка создать дубликат
	resp, err = http.Post(server.URL+"/clients/", "application/json", bytes.NewBuffer(createReqBody))
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, resp.StatusCode, "Create duplicate status code")
	// Проверяем новое сообщение об ошибке
	bodyBytes, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close() // Не забываем закрыть тело
	require.Contains(t, string(bodyBytes), fmt.Sprintf("Клиент с ID '%s' уже существует", clientID), "Create duplicate error message")

	// 3. Get Client
	resp, err = http.Get(server.URL + "/clients/" + clientID)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "Get status code")
	var getResp api.ClientLimitResponse
	err = json.NewDecoder(resp.Body).Decode(&getResp)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, clientID, getResp.ClientID)
	assert.Equal(t, initialLimit.Rate, getResp.Rate)         // Проверяем поле Rate
	assert.Equal(t, initialLimit.Capacity, getResp.Capacity) // Проверяем поле Capacity

	// Get Non-existent Client
	resp, err = http.Get(server.URL + "/clients/non-existent")
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "Get non-existent status code")
	resp.Body.Close()

	// 4. Update Client (используем плоский JSON)
	updatedLimit := config.ClientRateConfig{Rate: 20, Capacity: 200}
	updateReqBody, _ := json.Marshal(map[string]interface{}{
		// "client_id": clientID, // client_id можно не указывать в теле PUT
		"rate_per_sec": updatedLimit.Rate,
		"capacity":     updatedLimit.Capacity,
	})
	req, _ := http.NewRequest(http.MethodPut, server.URL+"/clients/"+clientID, bytes.NewBuffer(updateReqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "Update status code")
	// Проверяем тело ответа (ожидаем плоскую структуру)
	var updateResp api.ClientLimitResponse
	err = json.NewDecoder(resp.Body).Decode(&updateResp)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, clientID, updateResp.ClientID)
	assert.Equal(t, updatedLimit.Rate, updateResp.Rate)         // Проверяем поле Rate
	assert.Equal(t, updatedLimit.Capacity, updateResp.Capacity) // Проверяем поле Capacity

	// 7. Получить обновленный лимит (проверка)
	resp, err = http.Get(server.URL + "/clients/" + clientID)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "Get after update status code")
	err = json.NewDecoder(resp.Body).Decode(&getResp)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, updatedLimit.Rate, getResp.Rate)         // Проверяем поле Rate
	assert.Equal(t, updatedLimit.Capacity, getResp.Capacity) // Проверяем поле Capacity

	// Update Non-existent Client
	req, _ = http.NewRequest(http.MethodPut, server.URL+"/clients/non-existent", bytes.NewBuffer(updateReqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "Update non-existent status code")
	resp.Body.Close()

	// 4. Delete Client
	req, _ = http.NewRequest(http.MethodDelete, server.URL+"/clients/"+clientID, nil)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNoContent, resp.StatusCode, "Delete status code")
	resp.Body.Close() // Закрываем тело, даже если оно пустое

	// Проверяем, что GET теперь возвращает 404
	resp, err = http.Get(server.URL + "/clients/" + clientID)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "Get after delete status code")
	resp.Body.Close()

	// Delete Non-existent Client
	req, _ = http.NewRequest(http.MethodDelete, server.URL+"/clients/non-existent", nil)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "Delete non-existent status code")
	resp.Body.Close()
}

// TestAPI_BadRequest проверяет обработку некорректных запросов.
func TestAPI_BadRequest(t *testing.T) {
	apiHandler, cleanup := setupTestAPI(t)
	defer cleanup()

	// Создаем тестовый сервер, ИМИТИРУЯ StripPrefix из main.go
	server := httptest.NewServer(http.StripPrefix("/clients", apiHandler))
	defer server.Close()

	// clientID := "api-client-1" // Убираем неиспользуемую переменную

	// 1. Create - некорректный JSON
	// Добавляем слэш в конце URL для POST
	resp, err := http.Post(server.URL+"/clients/", "application/json", bytes.NewBufferString("{"))
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()

	// 2. Create - отсутствует client_id
	createReqBody, _ := json.Marshal(map[string]interface{}{
		"limit": map[string]float64{"rate": 1, "capacity": 1},
	})
	resp, err = http.Post(server.URL+"/clients/", "application/json", bytes.NewBuffer(createReqBody))
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()

	// 3. Create - некорректные значения rate/capacity
	createReqBody, _ = json.Marshal(api.ClientLimitRequest{
		ClientID: "bad-values",
		Rate:     -1,
		Capacity: 0,
	})
	resp, err = http.Post(server.URL+"/clients/", "application/json", bytes.NewBuffer(createReqBody))
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()

	// 4. Update - несовпадение ID в пути и теле
	updateReqBody, _ := json.Marshal(map[string]interface{}{
		"client_id":    "other-id",
		"rate_per_sec": 1.0,
		"capacity":     1.0,
	})
	req, _ := http.NewRequest(http.MethodPut, server.URL+"/clients/some-id", bytes.NewBuffer(updateReqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()

	// 5. Неподдерживаемый метод
	req, _ = http.NewRequest(http.MethodPatch, server.URL+"/clients/some-id", nil)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	resp.Body.Close()
}

type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (lrw *loggingResponseWriter) WriteHeader(code int) {
	lrw.statusCode = code
	lrw.ResponseWriter.WriteHeader(code)
}

// --- Mocks and Helpers ---

// mockStore реализует ClientLimitStore для тестов
type mockStore struct {
	getClientLimitConfigFunc func(clientID string) (rate, capacity float64, found bool, err error)
	createClientLimitFunc    func(clientID string, limit config.ClientRateConfig) error
	updateClientLimitFunc    func(clientID string, limit config.ClientRateConfig) error
	deleteClientLimitFunc    func(clientID string) error
}

func (m *mockStore) GetClientLimitConfig(clientID string) (rate, capacity float64, found bool, err error) {
	if m.getClientLimitConfigFunc != nil {
		return m.getClientLimitConfigFunc(clientID)
	}
	// Дефолтная реализация (не найдено)
	return 0, 0, false, nil
}

func (m *mockStore) CreateClientLimit(clientID string, limit config.ClientRateConfig) error {
	if m.createClientLimitFunc != nil {
		return m.createClientLimitFunc(clientID, limit)
	}
	// Дефолтная реализация (успех)
	return nil
}

func (m *mockStore) UpdateClientLimit(clientID string, limit config.ClientRateConfig) error {
	if m.updateClientLimitFunc != nil {
		return m.updateClientLimitFunc(clientID, limit)
	}
	// Дефолтная реализация (успех)
	return nil
}

func (m *mockStore) DeleteClientLimit(clientID string) error {
	if m.deleteClientLimitFunc != nil {
		return m.deleteClientLimitFunc(clientID)
	}
	// Дефолтная реализация (успех)
	return nil
}

// TestAPIHandler_NewNilStore проверяет создание хендлера с nil store
func TestAPIHandler_NewNilStore(t *testing.T) {
	// Ожидаем, что не будет паники и вернется хендлер
	// Лог предупреждения мы проверить не можем стандартными средствами,
	// но сам факт вызова покрывает код.
	h := api.NewAPIHandler(nil)
	if h == nil {
		t.Error("NewAPIHandler(nil) должен возвращать не-nil хендлер")
	}
	if h.Store != nil {
		t.Error("h.Store должен быть nil, если передан nil store")
	}
}

// TestAPIHandler_ServeHTTP_NilStore проверяет ответ при nil store
func TestAPIHandler_ServeHTTP_NilStore(t *testing.T) {
	h := api.NewAPIHandler(nil) // Создаем хендлер с nil store
	req := httptest.NewRequest(http.MethodGet, "/clients/some-id", nil)
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)

	assertStatusCode(t, rr, http.StatusServiceUnavailable)
	assertErrorResponseContains(t, rr, http.StatusServiceUnavailable, "Хранилище лимитов недоступно")
}

// TestAPIHandler_ServeHTTP_MethodNotAllowed проверяет ответ для некорректного метода
func TestAPIHandler_ServeHTTP_MethodNotAllowed(t *testing.T) {
	store := &mockStore{}
	h := api.NewAPIHandler(store)

	tests := []struct {
		method string
		target string
	}{
		{http.MethodDelete, "/clients"},
		{http.MethodPut, "/clients"},
		{http.MethodPost, "/clients/some-id"},
	}

	for _, tc := range tests {
		t.Run(fmt.Sprintf("%s_%s", tc.method, tc.target), func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.target, nil)
			rr := httptest.NewRecorder()

			mux := http.NewServeMux()
			mux.Handle("/clients/", http.StripPrefix("/clients", h))
			mux.Handle("/clients", http.StripPrefix("/clients", h))

			mux.ServeHTTP(rr, req)
			assertStatusCode(t, rr, http.StatusMethodNotAllowed)
		})
	}
}

// TestAPIHandler_ServeHTTP_GetClientsNotImplemented проверяет ответ для GET /clients
func TestAPIHandler_ServeHTTP_GetClientsNotImplemented(t *testing.T) {
	store := &mockStore{}
	h := api.NewAPIHandler(store)
	req := httptest.NewRequest(http.MethodGet, "/clients", nil)
	rr := httptest.NewRecorder()

	mux := http.NewServeMux()
	mux.Handle("/clients/", http.StripPrefix("/clients", h))
	mux.Handle("/clients", http.StripPrefix("/clients", h))
	mux.ServeHTTP(rr, req)

	assertStatusCode(t, rr, http.StatusNotImplemented)
	assertErrorResponseContains(t, rr, http.StatusNotImplemented, "Получение списка всех клиентов не реализовано")
}

// TestAPIHandler_CreateClient_StoreError проверяет 500 при ошибке создания в store
func TestAPIHandler_CreateClient_StoreError(t *testing.T) {
	expectedError := errors.New("db is on fire")
	store := &mockStore{
		createClientLimitFunc: func(clientID string, limit config.ClientRateConfig) error {
			return expectedError // Возвращаем НЕ ошибку конфликта
		},
	}
	h := api.NewAPIHandler(store)
	body := `{"client_id":"test-client","rate_per_sec":10,"capacity":100}`
	req := httptest.NewRequest(http.MethodPost, "/clients", strings.NewReader(body))
	rr := httptest.NewRecorder()

	mux := http.NewServeMux()
	mux.Handle("/clients/", http.StripPrefix("/clients", h))
	mux.Handle("/clients", http.StripPrefix("/clients", h))
	mux.ServeHTTP(rr, req)

	assertStatusCode(t, rr, http.StatusInternalServerError)
	// Проверяем новое общее сообщение об ошибке
	assertErrorResponseContains(t, rr, http.StatusInternalServerError, "Внутренняя ошибка сервера при создании клиента")
}

// TestAPIHandler_GetClient_StoreError проверяет 500 при ошибке получения из store
func TestAPIHandler_GetClient_StoreError(t *testing.T) {
	expectedError := errors.New("cannot reach db")
	store := &mockStore{
		getClientLimitConfigFunc: func(clientID string) (rate, capacity float64, found bool, err error) {
			return 0, 0, false, expectedError
		},
	}
	h := api.NewAPIHandler(store)
	req := httptest.NewRequest(http.MethodGet, "/clients/some-id", nil)
	rr := httptest.NewRecorder()

	// Используем ServeMux
	mux := http.NewServeMux()
	mux.Handle("/clients/", http.StripPrefix("/clients", h))
	mux.ServeHTTP(rr, req)

	assertStatusCode(t, rr, http.StatusInternalServerError)
	assertErrorResponseContains(t, rr, http.StatusInternalServerError, "Ошибка получения лимита из БД")
}

// TestAPIHandler_UpdateClient_StoreError проверяет 500 при ошибке обновления в store
func TestAPIHandler_UpdateClient_StoreError(t *testing.T) {
	expectedError := errors.New("failed to update")
	store := &mockStore{
		updateClientLimitFunc: func(clientID string, limit config.ClientRateConfig) error {
			return expectedError
		},
	}
	h := api.NewAPIHandler(store)
	body := `{"rate_per_sec":20,"capacity":200}`
	req := httptest.NewRequest(http.MethodPut, "/clients/test-client", strings.NewReader(body))
	rr := httptest.NewRecorder()

	mux := http.NewServeMux()
	mux.Handle("/clients/", http.StripPrefix("/clients", h))
	mux.ServeHTTP(rr, req)

	assertStatusCode(t, rr, http.StatusInternalServerError)
	// Проверяем новое общее сообщение об ошибке
	assertErrorResponseContains(t, rr, http.StatusInternalServerError, "Внутренняя ошибка сервера при обновлении клиента")
}

// TestAPIHandler_UpdateClient_BadRequest проверяет 400 при некорректном запросе обновления
func TestAPIHandler_UpdateClient_BadRequest(t *testing.T) {
	store := &mockStore{} // Store не будет вызван
	h := api.NewAPIHandler(store)
	badBody := `{"client_id":"test-client","rate_per_sec":10,"capacity":100` // Незакрытая скобка
	req := httptest.NewRequest(http.MethodPut, "/clients/test-client", strings.NewReader(badBody))
	rr := httptest.NewRecorder()

	// Используем ServeMux
	mux := http.NewServeMux()
	mux.Handle("/clients/", http.StripPrefix("/clients", h))
	mux.ServeHTTP(rr, req)

	assertStatusCode(t, rr, http.StatusBadRequest)
	assertErrorResponseContains(t, rr, http.StatusBadRequest, "Ошибка парсинга JSON")
}

// TestAPIHandler_DeleteClient_StoreError проверяет 500 при ошибке удаления в store
func TestAPIHandler_DeleteClient_StoreError(t *testing.T) {
	expectedError := errors.New("cannot delete")
	store := &mockStore{
		deleteClientLimitFunc: func(clientID string) error {
			return expectedError // Возвращаем НЕ ошибку "не найден"
		},
	}
	h := api.NewAPIHandler(store)
	req := httptest.NewRequest(http.MethodDelete, "/clients/some-id", nil)
	rr := httptest.NewRecorder()

	mux := http.NewServeMux()
	mux.Handle("/clients/", http.StripPrefix("/clients", h))
	mux.ServeHTTP(rr, req)

	assertStatusCode(t, rr, http.StatusInternalServerError)
	// Проверяем новое общее сообщение об ошибке
	assertErrorResponseContains(t, rr, http.StatusInternalServerError, "Внутренняя ошибка сервера при удалении клиента")
}

// --- Вспомогательные функции для ассертов (если их еще нет) ---

func assertStatusCode(t *testing.T, rr *httptest.ResponseRecorder, expectedStatus int) {
	t.Helper()
	if status := rr.Code; status != expectedStatus {
		t.Errorf("handler returned wrong status code: got %v want %v",
			status, expectedStatus)
	}
}

// errorResponse используется для парсинга нового формата ошибок
type errorResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func assertErrorResponseContains(t *testing.T, rr *httptest.ResponseRecorder, expectedCode int, expectedSubstring string) {
	t.Helper()
	var respBody errorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &respBody); err != nil {
		if !strings.Contains(rr.Body.String(), expectedSubstring) {
			t.Errorf("handler response body does not contain expected error substring: got %q want substring %q", rr.Body.String(), expectedSubstring)
		}
		return
	}
	if respBody.Code != expectedCode {
		t.Errorf("handler returned wrong error code: got %d want %d", respBody.Code, expectedCode)
	}
	if !strings.Contains(respBody.Message, expectedSubstring) {
		t.Errorf("handler returned error message does not contain expected substring: got %q want substring %q",
			respBody.Message, expectedSubstring)
	}
}
