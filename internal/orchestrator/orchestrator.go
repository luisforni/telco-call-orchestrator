package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	appkafka "telco-call-orchestrator/internal/kafka"
	"telco-call-orchestrator/internal/models"
)

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
	cfg      Config
	activeMu sync.RWMutex
	active   map[string]*models.Call
	rdb      *redis.Client
	producer *appkafka.Producer
	log      *zap.Logger
}

func New(cfg Config, producer *appkafka.Producer, rdb *redis.Client, log *zap.Logger) *Orchestrator {
	return &Orchestrator{
		cfg:      cfg,
		active:   make(map[string]*models.Call),
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
	o.activeMu.Unlock()
	if !ok {
		return nil
	}
	if err := call.Transition(models.StateConnected); err != nil {
		o.log.Warn("transition error", zap.Error(err))
	}
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
	}
	o.activeMu.Unlock()

	if !ok {
		return nil
	}

	_ = call.Transition(models.StateEnded)
	call.HangupCause = ce.HangupCause

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
