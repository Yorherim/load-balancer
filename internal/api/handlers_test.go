// Package api_test содержит тесты для пакета api.
package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"load-balancer/internal/api"
	"load-balancer/internal/config"
	"load-balancer/internal/response"
	"load-balancer/internal/storage"

	_ "modernc.org/sqlite" // Импорт драйвера
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

	// Создаем тестовый сервер, ИМИТИРУЯ StripPrefix из main.go
	server := httptest.NewServer(http.StripPrefix("/clients", apiHandler))
	defer server.Close()

	clientID := "api-client-1"
	initialLimit := config.ClientRateConfig{Rate: 10, Capacity: 100} // Используем ClientRateConfig

	// 1. Create Client
	createReqBody, _ := json.Marshal(api.ClientLimitRequest{
		ClientID: clientID,
		Limit:    initialLimit,
	})
	resp, err := http.Post(server.URL+"/clients/", "application/json", bytes.NewBuffer(createReqBody))
	require.NoError(t, err)
	assert.Equal(t, http.StatusCreated, resp.StatusCode, "Create status code")
	// Проверяем тело ответа
	var createResp api.ClientLimitResponse
	err = json.NewDecoder(resp.Body).Decode(&createResp)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, clientID, createResp.ClientID)
	assert.Equal(t, initialLimit.Rate, createResp.Limit.Rate)         // Проверяем вложенное поле
	assert.Equal(t, initialLimit.Capacity, createResp.Limit.Capacity) // Проверяем вложенное поле

	// Попытка создать дубликат
	resp, err = http.Post(server.URL+"/clients/", "application/json", bytes.NewBuffer(createReqBody))
	require.NoError(t, err)
	assert.Equal(t, http.StatusConflict, resp.StatusCode, "Create duplicate status code")
	// Проверяем тело ошибки
	var errResp response.ErrorResponse
	err = json.NewDecoder(resp.Body).Decode(&errResp)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Contains(t, errResp.Error, "уже существует")

	// 2. Get Client
	resp, err = http.Get(server.URL + "/clients/" + clientID)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "Get status code")
	var getResp api.ClientLimitResponse
	err = json.NewDecoder(resp.Body).Decode(&getResp)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, clientID, getResp.ClientID)
	assert.Equal(t, initialLimit.Rate, getResp.Limit.Rate)         // Проверяем вложенное поле
	assert.Equal(t, initialLimit.Capacity, getResp.Limit.Capacity) // Проверяем вложенное поле

	// Get Non-existent Client
	resp, err = http.Get(server.URL + "/clients/non-existent")
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "Get non-existent status code")
	resp.Body.Close()

	// 3. Update Client
	updatedLimit := config.ClientRateConfig{Rate: 20, Capacity: 200} // Используем ClientRateConfig
	updateReqBody, _ := json.Marshal(api.ClientLimitRequest{
		// ClientID в теле опционален для PUT, если он есть в пути
		Limit: updatedLimit,
	})
	req, _ := http.NewRequest(http.MethodPut, server.URL+"/clients/"+clientID, bytes.NewBuffer(updateReqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "Update status code")
	// Проверяем тело ответа
	var updateResp api.ClientLimitResponse
	err = json.NewDecoder(resp.Body).Decode(&updateResp)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, clientID, updateResp.ClientID)
	assert.Equal(t, updatedLimit.Rate, updateResp.Limit.Rate)         // Проверяем вложенное поле
	assert.Equal(t, updatedLimit.Capacity, updateResp.Limit.Capacity) // Проверяем вложенное поле

	// Проверяем, что GET возвращает обновленные данные
	resp, err = http.Get(server.URL + "/clients/" + clientID)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "Get after update status code")
	err = json.NewDecoder(resp.Body).Decode(&getResp)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, updatedLimit.Rate, getResp.Limit.Rate)         // Проверяем вложенное поле
	assert.Equal(t, updatedLimit.Capacity, getResp.Limit.Capacity) // Проверяем вложенное поле

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
		Limit:    config.ClientRateConfig{Rate: -1, Capacity: 0},
	})
	resp, err = http.Post(server.URL+"/clients/", "application/json", bytes.NewBuffer(createReqBody))
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()

	// 4. Update - несовпадение ID в пути и теле
	updateReqBody, _ := json.Marshal(api.ClientLimitRequest{
		ClientID: "other-id",
		Limit:    config.ClientRateConfig{Rate: 1, Capacity: 1},
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

// logErrorHandler - обертка для логгирования ошибок сервера во время тестов
func logErrorHandler(t *testing.T, handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lw := &loggingResponseWriter{ResponseWriter: w}
		handler.ServeHTTP(lw, r)
		if lw.statusCode >= 400 {
			t.Logf("[Server Error] %s %s -> %d", r.Method, r.URL.Path, lw.statusCode)
		}
	})
}

type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (lrw *loggingResponseWriter) WriteHeader(code int) {
	lrw.statusCode = code
	lrw.ResponseWriter.WriteHeader(code)
}

// Дополнительные тесты можно добавить для проверки одновременного доступа,
// но для этого потребуется более сложная настройка.
