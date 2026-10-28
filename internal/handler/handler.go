package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xotapy-main/courseGo/internal/model"
	"github.com/xotapy-main/courseGo/internal/repository"
	"github.com/xotapy-main/courseGo/internal/txmanager"
)

type ProblemDetails struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail"`
	Instance string `json:"instance"`
	Code     string `json:"code"`
}

type PointDTO struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

type CreateTripRequest struct {
	UserID     uuid.UUID `json:"user_id"`
	DriverID   uuid.UUID `json:"driver_id"`
	StartPoint PointDTO  `json:"start_point"`
	EndPoint   PointDTO  `json:"end_point"`
	Price      int64     `json:"price"`
}

type TripResponse struct {
	ID             uuid.UUID `json:"id"`
	UserID         uuid.UUID `json:"user_id"`
	DriverID       uuid.UUID `json:"driver_id"`
	StartPoint     PointDTO  `json:"start_point"`
	EndPoint       PointDTO  `json:"end_point"`
	Price          int64     `json:"price"`
	Status         string    `json:"status"`
	StartedAt      string    `json:"started_at"`
	FinishedAt     *string   `json:"finished_at"`
	LastPositionAt *string   `json:"last_position_at"`
}

type Handler struct {
	tripRepo    *repository.TripRepository
	historyRepo *repository.HistoryRepository
	txManager   txmanager.TxManager
	pool        *pgxpool.Pool
}

func New(
	tripRepo *repository.TripRepository,
	historyRepo *repository.HistoryRepository,
	txManager txmanager.TxManager,
	pool *pgxpool.Pool,
) *Handler {
	return &Handler{
		tripRepo:    tripRepo,
		historyRepo: historyRepo,
		txManager:   txManager,
		pool:        pool,
	}
}

func (h *Handler) writeProblem(w http.ResponseWriter, r *http.Request, status int, code, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ProblemDetails{
		Type:     fmt.Sprintf("https://tripgo.example/problems/%s", code),
		Title:    title,
		Status:   status,
		Detail:   detail,
		Instance: r.URL.Path,
		Code:     code,
	})
}

func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (h *Handler) Ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := h.pool.Ping(ctx); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"unavailable"}`))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (h *Handler) CreateTrip(w http.ResponseWriter, r *http.Request) {
	var req CreateTripRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		h.writeProblem(w, r, http.StatusBadRequest, "invalid_request", "Invalid request", err.Error())
		return
	}

	if req.UserID == uuid.Nil || req.DriverID == uuid.Nil || req.Price < 0 {
		h.writeProblem(w, r, http.StatusBadRequest, "invalid_request", "Invalid request", "Validation failed")
		return
	}

	now := time.Now().UTC()
	trip := &model.Trip{
		ID:         uuid.New(),
		UserID:     req.UserID,
		DriverID:   req.DriverID,
		StartPoint: model.Point{Latitude: req.StartPoint.Latitude, Longitude: req.StartPoint.Longitude},
		EndPoint:   model.Point{Latitude: req.EndPoint.Latitude, Longitude: req.EndPoint.Longitude},
		Price:      req.Price,
		Status:     "active",
		StartedAt:  now,
	}

	err := h.txManager.Do(r.Context(), func(ctx context.Context) error {
		if err := h.tripRepo.Create(ctx, trip); err != nil {
			return err
		}

		history := &model.TripStatusHistory{
			TripID:    trip.ID,
			Status:    trip.Status,
			ChangedAt: now,
		}
		return h.historyRepo.Insert(ctx, history)
	})

	if err != nil {
		if errors.Is(err, model.ErrDriverBusy) {
			h.writeProblem(w, r, http.StatusConflict, "driver_busy", "Driver busy", "Driver already has an active trip")
			return
		}
		h.writeProblem(w, r, http.StatusInternalServerError, "internal_error", "Internal error", "Internal server error")
		return
	}

	w.Header().Set("Location", fmt.Sprintf("/api/v1/trips/%s", trip.ID))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(toTripResponse(trip))
}

func (h *Handler) GetTrip(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "tripId")
	tripID, err := uuid.Parse(idStr)
	if err != nil {
		h.writeProblem(w, r, http.StatusBadRequest, "invalid_request", "Invalid request", "Invalid UUID")
		return
	}

	trip, err := h.tripRepo.GetByID(r.Context(), tripID)
	if err != nil {
		if errors.Is(err, model.ErrTripNotFound) {
			h.writeProblem(w, r, http.StatusNotFound, "trip_not_found", "Trip not found", "Trip not found")
			return
		}
		h.writeProblem(w, r, http.StatusInternalServerError, "internal_error", "Internal error", "Internal server error")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(toTripResponse(trip))
}

func (h *Handler) FinishTrip(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "tripId")
	tripID, err := uuid.Parse(idStr)
	if err != nil {
		h.writeProblem(w, r, http.StatusBadRequest, "invalid_request", "Invalid request", "Invalid UUID")
		return
	}

	now := time.Now().UTC()
	var finishedTrip *model.Trip

	err = h.txManager.Do(r.Context(), func(ctx context.Context) error {
		var err error
		finishedTrip, err = h.tripRepo.Finish(ctx, tripID, now)
		if err != nil {
			return err
		}

		history := &model.TripStatusHistory{
			TripID:    tripID,
			Status:    "completed",
			ChangedAt: now,
		}
		return h.historyRepo.Insert(ctx, history)
	})

	if err != nil {
		switch {
		case errors.Is(err, model.ErrTripNotFound):
			h.writeProblem(w, r, http.StatusNotFound, "trip_not_found", "Trip not found", "Trip not found")
		case errors.Is(err, model.ErrTripCompleted):
			h.writeProblem(w, r, http.StatusConflict, "trip_completed", "Trip completed", "Trip is already completed")
		default:
			h.writeProblem(w, r, http.StatusInternalServerError, "internal_error", "Internal error", "Internal server error")
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(toTripResponse(finishedTrip))
}

func toTripResponse(t *model.Trip) TripResponse {
	resp := TripResponse{
		ID:       t.ID,
		UserID:   t.UserID,
		DriverID: t.DriverID,
		StartPoint: PointDTO{
			Latitude:  t.StartPoint.Latitude,
			Longitude: t.StartPoint.Longitude,
		},
		EndPoint: PointDTO{
			Latitude:  t.EndPoint.Latitude,
			Longitude: t.EndPoint.Longitude,
		},
		Price:     t.Price,
		Status:    t.Status,
		StartedAt: t.StartedAt.Format(time.RFC3339),
	}
	if t.FinishedAt != nil {
		f := t.FinishedAt.Format(time.RFC3339)
		resp.FinishedAt = &f
	}
	if t.LastPositionAt != nil {
		lp := t.LastPositionAt.Format(time.RFC3339)
		resp.LastPositionAt = &lp
	}
	return resp
}
