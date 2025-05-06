package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"load-balancer/internal/config"
	"load-balancer/internal/response"
	"load-balancer/internal/storage"
)

type ClientLimitStore interface {
	GetClientLimitConfig(clientID string) (rate, capacity float64, found bool, err error)
	CreateClientLimit(clientID string, limit config.ClientRateConfig) error
	UpdateClientLimit(clientID string, limit config.ClientRateConfig) error
	DeleteClientLimit(clientID string) error
}

// ClientLimitRequest структура для тела запроса при создании/обновлении лимита.
type ClientLimitRequest struct {
	ClientID string  `json:"client_id"`
	Rate     float64 `json:"rate_per_sec"`
	Capacity float64 `json:"capacity"`
}

// ClientLimitResponse структура для ответа при получении/создании/обновлении лимита.
type ClientLimitResponse struct {
	ClientID string  `json:"client_id"`
	Rate     float64 `json:"rate_per_sec"`
	Capacity float64 `json:"capacity"`
}

// APIHandler обрабатывает HTTP-запросы к API.
type APIHandler struct {
	Store ClientLimitStore
}

func NewAPIHandler(store ClientLimitStore) *APIHandler {
	if store == nil {
		log.Println("[API] Warning: Хранилище (Store) не предоставлено APIHandler. CRUD операции не будут работать.")
	}
	return &APIHandler{Store: store}
}

func (h *APIHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		response.RespondWithError(w, http.StatusServiceUnavailable, "Хранилище лимитов недоступно")
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
			response.RespondWithError(w, http.StatusNotImplemented, "Получение списка всех клиентов не реализовано")
		default:
			response.RespondWithError(w, http.StatusMethodNotAllowed, fmt.Sprintf("Метод %s не поддерживается для /clients", r.Method))
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
		response.RespondWithError(w, http.StatusMethodNotAllowed, fmt.Sprintf("Метод %s не поддерживается для /clients/{id}", r.Method))
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
	// Используем req.Rate и req.Capacity напрямую
	if req.Rate <= 0 || req.Capacity <= 0 {
		response.RespondWithError(w, http.StatusBadRequest, "Значения rate и capacity должны быть положительными")
		return
	}

	// Создаем структуру ClientRateConfig для передачи в Store
	limitConfig := config.ClientRateConfig{
		Rate:     req.Rate,
		Capacity: req.Capacity,
	}

	err := h.Store.CreateClientLimit(req.ClientID, limitConfig)
	if err != nil {
		// Используем errors.Is для проверки конкретной ошибки из хранилища
		if errors.Is(err, storage.ErrClientAlreadyExists) {
			// Возвращаем осмысленный HTTP статус и сообщение
			response.RespondWithError(w, http.StatusConflict, fmt.Sprintf("Клиент с ID '%s' уже существует", req.ClientID))
		} else {
			// Логируем оригинальную ошибку для отладки
			log.Printf("[API] Ошибка при создании клиента '%s': %v", req.ClientID, err)
			response.RespondWithError(w, http.StatusInternalServerError, "Внутренняя ошибка сервера при создании клиента")
		}
		return
	}

	// Возвращаем созданный объект (используем ClientLimitResponse для ответа)
	resp := ClientLimitResponse{
		ClientID: req.ClientID,
		Rate:     req.Rate,
		Capacity: req.Capacity,
	}
	response.RespondWithJSON(w, http.StatusCreated, resp)
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
		Rate:     rate,
		Capacity: capacity,
	}
	response.RespondWithJSON(w, http.StatusOK, resp)
}

// updateClient обрабатывает PUT /clients/{clientID}
func (h *APIHandler) updateClient(w http.ResponseWriter, r *http.Request, clientID string) {
	var req ClientLimitRequest // Ожидаем плоскую структуру
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.RespondWithError(w, http.StatusBadRequest, fmt.Sprintf("Ошибка парсинга JSON: %v", err))
		return
	}

	// Проверяем, что client_id в теле совпадает с путем (или отсутствует в теле)
	if req.ClientID != "" && req.ClientID != clientID {
		response.RespondWithError(w, http.StatusBadRequest, "client_id в теле запроса не совпадает с ID в пути")
		return
	}
	// Используем req.Rate и req.Capacity напрямую
	if req.Rate <= 0 || req.Capacity <= 0 {
		response.RespondWithError(w, http.StatusBadRequest, "Значения rate и capacity должны быть положительными")
		return
	}

	limitConfig := config.ClientRateConfig{
		Rate:     req.Rate,
		Capacity: req.Capacity,
	}

	err := h.Store.UpdateClientLimit(clientID, limitConfig)
	if err != nil {
		// Используем errors.Is для проверки
		if errors.Is(err, storage.ErrClientNotFound) {
			response.RespondWithError(w, http.StatusNotFound, fmt.Sprintf("Клиент с ID '%s' не найден для обновления", clientID))
		} else {
			log.Printf("[API] Ошибка при обновлении клиента '%s': %v", clientID, err)
			response.RespondWithError(w, http.StatusInternalServerError, "Внутренняя ошибка сервера при обновлении клиента")
		}
		return
	}

	resp := ClientLimitResponse{
		ClientID: clientID,
		Rate:     req.Rate,
		Capacity: req.Capacity,
	}
	response.RespondWithJSON(w, http.StatusOK, resp)
}

// deleteClient обрабатывает DELETE /clients/{clientID}
func (h *APIHandler) deleteClient(w http.ResponseWriter, r *http.Request, clientID string) {
	err := h.Store.DeleteClientLimit(clientID)
	if err != nil {
		// Используем errors.Is для проверки
		if errors.Is(err, storage.ErrClientNotFound) {
			response.RespondWithError(w, http.StatusNotFound, fmt.Sprintf("Клиент с ID '%s' не найден для удаления", clientID))
		} else {
			log.Printf("[API] Ошибка при удалении клиента '%s': %v", clientID, err)
			response.RespondWithError(w, http.StatusInternalServerError, "Внутренняя ошибка сервера при удалении клиента")
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
