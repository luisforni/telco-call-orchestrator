package models

import (
	"fmt"
	"time"
)

type CallState string

const (
	StateIdle        CallState = "IDLE"
	StateRinging     CallState = "RINGING"
	StateConnected   CallState = "CONNECTED"
	StateOnHold      CallState = "ON_HOLD"
	StateRecording   CallState = "RECORDING"
	StateTransferring CallState = "TRANSFERRING"
	StateEnded       CallState = "ENDED"
)

var ValidTransitions = map[CallState][]CallState{
	StateIdle:        {StateRinging},
	StateRinging:     {StateConnected, StateEnded},
	StateConnected:   {StateOnHold, StateRecording, StateTransferring, StateEnded},
	StateOnHold:      {StateConnected, StateEnded},
	StateRecording:   {StateConnected, StateEnded},
	StateTransferring: {StateConnected, StateEnded},
	StateEnded:       {},
}

type Call struct {
	ID           string     `json:"id"`
	CallerID     string     `json:"caller_id"`
	Destination  string     `json:"destination"`
	Direction    string     `json:"direction"`
	State        CallState  `json:"state"`
	AgentID      string     `json:"agent_id,omitempty"`
	QueueID      string     `json:"queue_id,omitempty"`
	StartTime    time.Time  `json:"start_time"`
	AnswerTime   *time.Time `json:"answer_time,omitempty"`
	EndTime      *time.Time `json:"end_time,omitempty"`
	HangupCause  string     `json:"hangup_cause,omitempty"`
	RecordingKey string     `json:"recording_key,omitempty"`
}

func (c *Call) Transition(to CallState) error {
	allowed := ValidTransitions[c.State]
	for _, s := range allowed {
		if s == to {
			c.State = to
			if to == StateConnected && c.AnswerTime == nil {
				now := time.Now()
				c.AnswerTime = &now
			}
			if to == StateEnded {
				now := time.Now()
				c.EndTime = &now
			}
			return nil
		}
	}
	return fmt.Errorf("invalid transition %s -> %s", c.State, to)
}
