package api

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"go.uber.org/zap"

	"telco-call-orchestrator/internal/models"
	"telco-call-orchestrator/internal/orchestrator"
)

const maxBodyBytes = 1 << 20 // 1 MiB

// callIDRegexp restricts call IDs to safe alphanumeric/hyphen/underscore strings
// of 1–128 characters.
var callIDRegexp = regexp.MustCompile(`^[a-zA-Z0-9_\-]{1,128}$`)

// Handler exposes the orchestrator over HTTP REST.
type Handler struct {
	orch *orchestrator.Orchestrator
	log  *zap.Logger
}

// NewHandler constructs a new REST handler.
func NewHandler(orch *orchestrator.Orchestrator, log *zap.Logger) *Handler {
	return &Handler{orch: orch, log: log}
}

// Register mounts all orchestrator endpoints on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/orchestrator/calls", h.handleCalls)
	mux.HandleFunc("/orchestrator/calls/", h.handleCallByID)
}

// handleCalls handles POST /orchestrator/calls.
func (h *Handler) handleCalls(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req orchestrator.StartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if !callIDRegexp.MatchString(req.CallID) {
		h.writeError(w, http.StatusBadRequest, "call_id must be 1-128 alphanumeric/hyphen/underscore characters")
		return
	}

	call, err := h.orch.StartCall(r.Context(), req)
	if err != nil {
		h.log.Error("start call failed", zap.Error(err))
		h.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	h.writeJSON(w, http.StatusCreated, call)
}

// handleCallByID routes sub-paths under /orchestrator/calls/{id}.
func (h *Handler) handleCallByID(w http.ResponseWriter, r *http.Request) {
	// Path examples:
	//   /orchestrator/calls/{id}
	//   /orchestrator/calls/{id}/transition
	//   /orchestrator/calls/{id}/history
	//   /orchestrator/calls/{id}/cancel
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/orchestrator/calls/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "call_id required", http.StatusBadRequest)
		return
	}

	callID := parts[0]
	if !callIDRegexp.MatchString(callID) {
		h.writeError(w, http.StatusBadRequest, "invalid call_id format")
		return
	}
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}

	switch {
	case sub == "" && r.Method == http.MethodGet:
		h.getCall(w, r, callID)
	case sub == "transition" && r.Method == http.MethodPost:
		h.transitionCall(w, r, callID)
	case sub == "history" && r.Method == http.MethodGet:
		h.getHistory(w, r, callID)
	case sub == "cancel" && r.Method == http.MethodPost:
		h.cancelCall(w, r, callID)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// getCall handles GET /orchestrator/calls/{call_id}.
func (h *Handler) getCall(w http.ResponseWriter, r *http.Request, callID string) {
	call, err := h.orch.GetCall(r.Context(), callID)
	if err != nil {
		if err == orchestrator.ErrCallNotFound {
			h.writeError(w, http.StatusNotFound, "call not found")
			return
		}
		h.log.Error("get call failed", zap.String("call_id", callID), zap.Error(err))
		h.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.writeJSON(w, http.StatusOK, call)
}

// transitionCall handles POST /orchestrator/calls/{call_id}/transition.
func (h *Handler) transitionCall(w http.ResponseWriter, r *http.Request, callID string) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req struct {
		State models.CallState `json:"state"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.State == "" {
		h.writeError(w, http.StatusBadRequest, "state is required")
		return
	}

	call, err := h.orch.TransitionCall(r.Context(), callID, req.State)
	if err != nil {
		if err == orchestrator.ErrCallNotFound {
			h.writeError(w, http.StatusNotFound, "call not found")
			return
		}
		h.writeError(w, http.StatusConflict, err.Error())
		return
	}
	h.writeJSON(w, http.StatusOK, call)
}

// getHistory handles GET /orchestrator/calls/{call_id}/history.
func (h *Handler) getHistory(w http.ResponseWriter, r *http.Request, callID string) {
	history, err := h.orch.GetHistory(r.Context(), callID)
	if err != nil {
		if err == orchestrator.ErrCallNotFound {
			h.writeError(w, http.StatusNotFound, "call not found")
			return
		}
		h.log.Error("get history failed", zap.String("call_id", callID), zap.Error(err))
		h.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.writeJSON(w, http.StatusOK, history)
}

// cancelCall handles POST /orchestrator/calls/{call_id}/cancel.
func (h *Handler) cancelCall(w http.ResponseWriter, r *http.Request, callID string) {
	call, err := h.orch.CancelCall(r.Context(), callID)
	if err != nil {
		if err == orchestrator.ErrCallNotFound {
			h.writeError(w, http.StatusNotFound, "call not found")
			return
		}
		h.log.Error("cancel call failed", zap.String("call_id", callID), zap.Error(err))
		h.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.writeJSON(w, http.StatusOK, call)
}

func (h *Handler) writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.log.Error("write json response failed", zap.Error(err))
	}
}

func (h *Handler) writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
