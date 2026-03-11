package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	appkafka "telco-call-orchestrator/internal/kafka"
	"telco-call-orchestrator/internal/models"
)

// ErrCallNotFound is returned when a call_id does not exist.
var ErrCallNotFound = errors.New("call not found")

// StartRequest is the payload for starting a new orchestrated call.
type StartRequest struct {
	CallID      string `json:"call_id"`
	CallerID    string `json:"caller_id"`
	Destination string `json:"destination"`
	Direction   string `json:"direction"`
}

// HistoryEntry records a single state-change event on a call.
type HistoryEntry struct {
	State     models.CallState `json:"state"`
	Timestamp time.Time        `json:"timestamp"`
}

type Config struct {
	KafkaBrokers       string
	TopicCallEvents    string
	TopicOrchEvents    string
	TopicRoutingReq    string
	TopicRecordingCtrl string
	RedisAddr          string
	ConsumerGroup      string
}

type callEvent struct {
	EventType   string `json:"event_type"`
	CallID      string `json:"call_id"`
	CallerID    string `json:"caller_id"`
	Destination string `json:"destination"`
	Direction   string `json:"direction"`
	HangupCause string `json:"hangup_cause,omitempty"`
}

type Orchestrator struct {
	cfg       Config
	activeMu  sync.RWMutex
	active    map[string]*models.Call
	historyMu sync.RWMutex
	history   map[string][]HistoryEntry
	rdb       *redis.Client
	producer  *appkafka.Producer
	log       *zap.Logger
}

func New(cfg Config, producer *appkafka.Producer, rdb *redis.Client, log *zap.Logger) *Orchestrator {
	return &Orchestrator{
		cfg:      cfg,
		active:   make(map[string]*models.Call),
		history:  make(map[string][]HistoryEntry),
		rdb:      rdb,
		producer: producer,
		log:      log,
	}
}

func (o *Orchestrator) ActiveCallCount() int {
	o.activeMu.RLock()
	defer o.activeMu.RUnlock()
	return len(o.active)
}

func (o *Orchestrator) HandleEvent(ctx context.Context, evt appkafka.RawEvent) error {
	var ce callEvent
	if err := json.Unmarshal(evt.Value, &ce); err != nil {
		return fmt.Errorf("unmarshal call event: %w", err)
	}

	switch ce.EventType {
	case "CHANNEL_CREATE":
		return o.handleCreate(ctx, ce)
	case "CHANNEL_ANSWER":
		return o.handleAnswer(ctx, ce)
	case "CHANNEL_DESTROY":
		return o.handleDestroy(ctx, ce)
	default:
		o.log.Debug("unknown event type", zap.String("type", ce.EventType))
	}
	return nil
}

func (o *Orchestrator) handleCreate(ctx context.Context, ce callEvent) error {
	call := &models.Call{
		ID:          ce.CallID,
		CallerID:    ce.CallerID,
		Destination: ce.Destination,
		Direction:   ce.Direction,
		State:       models.StateRinging,
		StartTime:   time.Now(),
	}

	o.activeMu.Lock()
	o.active[call.ID] = call
	o.activeMu.Unlock()

	o.appendHistory(call.ID, call.State)

	data, _ := json.Marshal(call)
	o.rdb.Set(ctx, "call:"+call.ID, data, 24*time.Hour)

	routingReq := map[string]interface{}{
		"call_id":   call.ID,
		"caller_id": call.CallerID,
		"direction": call.Direction,
		"queue_id":  "general",
	}
	if err := o.producer.Publish(o.cfg.TopicRoutingReq, call.ID, routingReq); err != nil {
		o.log.Error("publish routing request", zap.Error(err))
	}

	o.log.Info("call.created", zap.String("id", call.ID), zap.String("from", call.CallerID))
	return nil
}

func (o *Orchestrator) handleAnswer(ctx context.Context, ce callEvent) error {
	o.activeMu.Lock()
	call, ok := o.active[ce.CallID]
	if ok {
		if err := call.Transition(models.StateConnected); err != nil {
			o.log.Warn("transition error", zap.Error(err))
			ok = false // don't record history on bad transition
		}
	}
	o.activeMu.Unlock()
	if !ok {
		return nil
	}
	o.appendHistory(call.ID, call.State)

	data, _ := json.Marshal(call)
	o.rdb.Set(ctx, "call:"+call.ID, data, 24*time.Hour)

	recCtrl := map[string]interface{}{"call_id": call.ID, "action": "start"}
	o.producer.Publish(o.cfg.TopicRecordingCtrl, call.ID, recCtrl)

	o.log.Info("call.answered", zap.String("id", call.ID))
	return nil
}

func (o *Orchestrator) handleDestroy(ctx context.Context, ce callEvent) error {
	o.activeMu.Lock()
	call, ok := o.active[ce.CallID]
	if ok {
		delete(o.active, ce.CallID)
		_ = call.Transition(models.StateEnded)
		call.HangupCause = ce.HangupCause
	}
	o.activeMu.Unlock()

	if !ok {
		return nil
	}

	o.appendHistory(call.ID, call.State)

	data, _ := json.Marshal(call)
	o.rdb.Set(ctx, "call:"+call.ID, data, 24*time.Hour)

	recCtrl := map[string]interface{}{"call_id": call.ID, "action": "stop"}
	o.producer.Publish(o.cfg.TopicRecordingCtrl, call.ID, recCtrl)

	o.log.Info("call.ended",
		zap.String("id", call.ID),
		zap.String("cause", ce.HangupCause),
	)
	return nil
}

// appendHistory records a state transition in the in-memory history log.
func (o *Orchestrator) appendHistory(callID string, state models.CallState) {
	o.historyMu.Lock()
	defer o.historyMu.Unlock()
	o.history[callID] = append(o.history[callID], HistoryEntry{
		State:     state,
		Timestamp: time.Now(),
	})
}

// StartCall creates a new orchestrated call via the REST API.
func (o *Orchestrator) StartCall(ctx context.Context, req StartRequest) (*models.Call, error) {
	if req.CallID == "" {
		return nil, fmt.Errorf("call_id is required")
	}
	if len(req.CallID) > 128 {
		return nil, fmt.Errorf("call_id exceeds maximum length of 128 characters")
	}

	o.activeMu.Lock()
	if _, exists := o.active[req.CallID]; exists {
		o.activeMu.Unlock()
		return nil, fmt.Errorf("call %s already exists", req.CallID)
	}
	call := &models.Call{
		ID:          req.CallID,
		CallerID:    req.CallerID,
		Destination: req.Destination,
		Direction:   req.Direction,
		State:       models.StateRinging,
		StartTime:   time.Now(),
	}
	o.active[call.ID] = call
	o.activeMu.Unlock()

	o.appendHistory(call.ID, call.State)

	data, _ := json.Marshal(call)
	o.rdb.Set(ctx, "call:"+call.ID, data, 24*time.Hour)

	o.producer.Publish(o.cfg.TopicOrchEvents, call.ID, map[string]interface{}{
		"event_type": "call_started",
		"call_id":    call.ID,
		"caller_id":  call.CallerID,
	})

	o.log.Info("call.started via REST", zap.String("id", call.ID))
	return call, nil
}

// GetCall retrieves the current state of a call by ID.
// It first checks the in-memory active map, then falls back to Redis.
func (o *Orchestrator) GetCall(ctx context.Context, callID string) (*models.Call, error) {
	o.activeMu.RLock()
	call, ok := o.active[callID]
	o.activeMu.RUnlock()
	if ok {
		return call, nil
	}

	// Fall back to Redis for ended calls.
	data, err := o.rdb.Get(ctx, "call:"+callID).Bytes()
	if err != nil {
		if err == redis.Nil {
			return nil, ErrCallNotFound
		}
		return nil, fmt.Errorf("redis get: %w", err)
	}
	var c models.Call
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("unmarshal call: %w", err)
	}
	return &c, nil
}

// TransitionCall applies a manual state transition to an active call.
func (o *Orchestrator) TransitionCall(ctx context.Context, callID string, to models.CallState) (*models.Call, error) {
	o.activeMu.Lock()
	call, ok := o.active[callID]
	if ok {
		if err := call.Transition(to); err != nil {
			o.activeMu.Unlock()
			return nil, err
		}
	}
	o.activeMu.Unlock()
	if !ok {
		return nil, ErrCallNotFound
	}

	o.appendHistory(call.ID, call.State)

	data, _ := json.Marshal(call)
	o.rdb.Set(ctx, "call:"+call.ID, data, 24*time.Hour)

	o.producer.Publish(o.cfg.TopicOrchEvents, call.ID, map[string]interface{}{
		"event_type": "state_changed",
		"call_id":    call.ID,
		"state":      call.State,
	})

	o.log.Info("call.transition", zap.String("id", call.ID), zap.String("state", string(call.State)))
	return call, nil
}

// GetHistory returns the state-transition history for a call.
func (o *Orchestrator) GetHistory(ctx context.Context, callID string) ([]HistoryEntry, error) {
	o.historyMu.RLock()
	entries, ok := o.history[callID]
	o.historyMu.RUnlock()
	if !ok {
		// Verify the call existed at all (active or in Redis).
		if _, err := o.GetCall(ctx, callID); err != nil {
			return nil, err
		}
		return []HistoryEntry{}, nil
	}
	// Return a copy so callers can't mutate internal state.
	result := make([]HistoryEntry, len(entries))
	copy(result, entries)
	return result, nil
}

// CancelCall transitions an active call to the ENDED state.
func (o *Orchestrator) CancelCall(ctx context.Context, callID string) (*models.Call, error) {
	o.activeMu.Lock()
	call, ok := o.active[callID]
	if ok {
		delete(o.active, callID)
		_ = call.Transition(models.StateEnded)
		call.HangupCause = "CANCELLED"
	}
	o.activeMu.Unlock()
	if !ok {
		return nil, ErrCallNotFound
	}

	o.appendHistory(call.ID, call.State)

	data, _ := json.Marshal(call)
	o.rdb.Set(ctx, "call:"+call.ID, data, 24*time.Hour)

	o.producer.Publish(o.cfg.TopicOrchEvents, call.ID, map[string]interface{}{
		"event_type": "call_ended",
		"call_id":    call.ID,
		"cause":      "CANCELLED",
	})

	o.log.Info("call.cancelled", zap.String("id", call.ID))
	return call, nil
}
