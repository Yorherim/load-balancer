// Package api содержит обработчики для CRUD операций с лимитами клиентов.
package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"load-balancer/internal/config" // Используем config.ClientRateConfig
	"load-balancer/internal/response"
	"load-balancer/internal/storage"
)

// ClientLimitRequest структура для тела запроса при создании/обновлении лимита.
// Использует тип ClientRateConfig из пакета config.
type ClientLimitRequest struct {
	ClientID string                  `json:"client_id"`
	Limit    config.ClientRateConfig `json:"limit"` // Используем переименованный тип
}

// ClientLimitResponse структура для ответа при получении лимита.
// Также использует ClientRateConfig.
type ClientLimitResponse struct {
	ClientID string                  `json:"client_id"`
	Limit    config.ClientRateConfig `json:"limit"` // Используем переименованный тип
}

// APIHandler обрабатывает HTTP-запросы к API.
type APIHandler struct {
	Store *storage.DB // Используем storage.DB напрямую
}

// NewAPIHandler создает новый экземпляр APIHandler.
func NewAPIHandler(store *storage.DB) *APIHandler {
	if store == nil {
		log.Println("[API] Warning: Хранилище (Store) не предоставлено APIHandler. CRUD операции не будут работать.")
	}
	return &APIHandler{Store: store}
}

// ServeHTTP является основным маршрутизатором для /clients API.
// Он определяет метод и наличие clientID в пути *после* StripPrefix.
func (h *APIHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Проверяем, что store доступен.
	if h.Store == nil {
		response.RespondWithError(w, http.StatusServiceUnavailable, "Хранилище лимитов недоступно") // Используем response.
		return
	}

	// r.URL.Path здесь уже *после* применения StripPrefix("/clients", ...)
	// Если исходный путь был /clients или /clients/, то r.URL.Path будет "" или "/"
	// Если исходный путь был /clients/{id} или /clients/{id}/, то r.URL.Path будет "/{id}" или "/{id}/"
	pathPart := strings.TrimPrefix(r.URL.Path, "/") // Убираем ведущий слэш, если есть
	pathPart = strings.TrimSuffix(pathPart, "/")    // Убираем завершающий слэш, если есть

	log.Printf("[API] Debug: Path after StripPrefix and Trim: '%s' (Original r.URL.Path: '%s')", pathPart, r.URL.Path)

	if pathPart == "" { // Обработка запросов к коллекции (/clients или /clients/)
		switch r.Method {
		case http.MethodPost:
			h.createClient(w, r)
		case http.MethodGet:
			// TODO: Реализовать GET /clients для получения списка всех клиентов?
			response.RespondWithError(w, http.StatusNotImplemented, "Получение списка всех клиентов не реализовано") // Используем response.
		default:
			response.RespondWithError(w, http.StatusMethodNotAllowed, fmt.Sprintf("Метод %s не поддерживается для /clients", r.Method)) // Используем response.
		}
		return // Завершаем обработку
	}

	// Обработка путей для конкретного клиента (pathPart теперь это {id})
	clientID := pathPart
	switch r.Method {
	case http.MethodGet:
		h.getClient(w, r, clientID)
	case http.MethodPut:
		h.updateClient(w, r, clientID)
	case http.MethodDelete:
		h.deleteClient(w, r, clientID)
	default:
		response.RespondWithError(w, http.StatusMethodNotAllowed, fmt.Sprintf("Метод %s не поддерживается для /clients/{id}", r.Method)) // Используем response.
	}
}

// createClient обрабатывает POST /clients
func (h *APIHandler) createClient(w http.ResponseWriter, r *http.Request) {
	var req ClientLimitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.RespondWithError(w, http.StatusBadRequest, fmt.Sprintf("Ошибка парсинга JSON: %v", err))
		return
	}

	if req.ClientID == "" {
		response.RespondWithError(w, http.StatusBadRequest, "Поле client_id обязательно")
		return
	}
	if req.Limit.Rate <= 0 || req.Limit.Capacity <= 0 {
		response.RespondWithError(w, http.StatusBadRequest, "Значения rate и capacity должны быть положительными")
		return
	}

	err := h.Store.CreateClientLimit(req.ClientID, req.Limit) // Передаем req.Limit (типа ClientRateConfig)
	if err != nil {
		// Проверяем, является ли ошибка конфликтом (уже существует)
		if strings.Contains(err.Error(), "уже существует") { // TODO: Более надежная проверка ошибки
			response.RespondWithError(w, http.StatusConflict, err.Error())
		} else {
			response.RespondWithError(w, http.StatusInternalServerError, fmt.Sprintf("Ошибка создания лимита в БД: %v", err))
		}
		return
	}

	response.RespondWithJSON(w, http.StatusCreated, req) // Возвращаем созданный объект
}

// getClient обрабатывает GET /clients/{clientID}
func (h *APIHandler) getClient(w http.ResponseWriter, r *http.Request, clientID string) {
	// Используем новый GetClientLimitConfig, т.к. нам нужны только rate и capacity для ответа
	rate, capacity, found, err := h.Store.GetClientLimitConfig(clientID)
	if err != nil {
		response.RespondWithError(w, http.StatusInternalServerError, fmt.Sprintf("Ошибка получения лимита из БД: %v", err))
		return
	}
	if !found {
		response.RespondWithError(w, http.StatusNotFound, fmt.Sprintf("Клиент с ID '%s' не найден", clientID))
		return
	}

	resp := ClientLimitResponse{
		ClientID: clientID,
		Limit: config.ClientRateConfig{
			Rate:     rate,
			Capacity: capacity,
		},
	}
	response.RespondWithJSON(w, http.StatusOK, resp)
}

// updateClient обрабатывает PUT /clients/{clientID}
func (h *APIHandler) updateClient(w http.ResponseWriter, r *http.Request, clientID string) {
	var req ClientLimitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.RespondWithError(w, http.StatusBadRequest, fmt.Sprintf("Ошибка парсинга JSON: %v", err))
		return
	}

	// Проверяем, что client_id в теле совпадает с путем (или отсутствует в теле)
	if req.ClientID != "" && req.ClientID != clientID {
		response.RespondWithError(w, http.StatusBadRequest, "client_id в теле запроса не совпадает с ID в пути")
		return
	}
	if req.Limit.Rate <= 0 || req.Limit.Capacity <= 0 {
		response.RespondWithError(w, http.StatusBadRequest, "Значения rate и capacity должны быть положительными")
		return
	}

	err := h.Store.UpdateClientLimit(clientID, req.Limit) // Передаем req.Limit (типа ClientRateConfig)
	if err != nil {
		if strings.Contains(err.Error(), "не найден для обновления") { // TODO: Более надежная проверка ошибки
			response.RespondWithError(w, http.StatusNotFound, err.Error())
		} else {
			response.RespondWithError(w, http.StatusInternalServerError, fmt.Sprintf("Ошибка обновления лимита в БД: %v", err))
		}
		return
	}

	// Формируем ответ с обновленными данными
	resp := ClientLimitResponse{
		ClientID: clientID,
		Limit:    req.Limit,
	}
	response.RespondWithJSON(w, http.StatusOK, resp)
}

// deleteClient обрабатывает DELETE /clients/{clientID}
func (h *APIHandler) deleteClient(w http.ResponseWriter, r *http.Request, clientID string) {
	err := h.Store.DeleteClientLimit(clientID)
	if err != nil {
		if strings.Contains(err.Error(), "не найден") {
			response.RespondWithError(w, http.StatusNotFound, err.Error()) // Используем response.
		} else {
			response.RespondWithError(w, http.StatusInternalServerError, fmt.Sprintf("Ошибка БД: %v", err)) // Используем response.
		}
		return
	}

	// Успешное удаление - часто возвращают 204 No Content без тела
	w.WriteHeader(http.StatusNoContent)
}
