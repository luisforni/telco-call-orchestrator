# Telco Call Orchestrator

A high-performance microservice that manages call flows, state transitions, and coordinates between telco services (CTI, SIP Gateway, Routing, Recording) using Kafka for event-driven communication.

## Features

- **State machine** for call lifecycle management (`RINGING → CONNECTED → ON_HOLD / RECORDING / TRANSFERRING → ENDED`)
- **REST API** for starting, inspecting, transitioning, and cancelling calls
- **Kafka consumer** that reacts to low-level telephony events (`CHANNEL_CREATE`, `CHANNEL_ANSWER`, `CHANNEL_DESTROY`)
- **Kafka producer** that emits orchestration events and control messages to downstream services
- **Redis** for durable call-state persistence (survives restarts)
- **gRPC server** with health-check reflection (port `50051`)
- **Prometheus metrics** at `/metrics`
- **Health endpoint** at `/healthz`

## API Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/orchestrator/calls` | Start a new call orchestration |
| `GET` | `/orchestrator/calls/{call_id}` | Get call state and metadata |
| `POST` | `/orchestrator/calls/{call_id}/transition` | Trigger a manual state transition |
| `GET` | `/orchestrator/calls/{call_id}/history` | Get state-change history |
| `POST` | `/orchestrator/calls/{call_id}/cancel` | Cancel and end an active call |
| `GET` | `/healthz` | Service health (returns active call count) |
| `GET` | `/metrics` | Prometheus metrics |

### Start a call

```bash
curl -X POST http://localhost:8080/orchestrator/calls \
  -H 'Content-Type: application/json' \
  -d '{
    "call_id":     "call-abc-123",
    "caller_id":   "+1-555-0100",
    "destination": "+1-555-0200",
    "direction":   "inbound"
  }'
```

### Get call status

```bash
curl http://localhost:8080/orchestrator/calls/call-abc-123
```

### Trigger a state transition

```bash
curl -X POST http://localhost:8080/orchestrator/calls/call-abc-123/transition \
  -H 'Content-Type: application/json' \
  -d '{"state": "ON_HOLD"}'
```

Valid states: `IDLE`, `RINGING`, `CONNECTED`, `ON_HOLD`, `RECORDING`, `TRANSFERRING`, `ENDED`

### Get call history

```bash
curl http://localhost:8080/orchestrator/calls/call-abc-123/history
```

### Cancel a call

```bash
curl -X POST http://localhost:8080/orchestrator/calls/call-abc-123/cancel
```

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `KAFKA_BROKERS` | `kafka:9092` | Comma-separated list of Kafka brokers |
| `TOPIC_CALL_EVENTS` | `call-events` | Topic consumed for telephony events |
| `TOPIC_ORCH_EVENTS` | `orchestrator-events` | Topic for orchestration events |
| `TOPIC_ROUTING_REQ` | `routing-requests` | Topic for routing requests |
| `TOPIC_RECORDING_CTRL` | `recording-control` | Topic for recording control commands |
| `REDIS_ADDR` | `redis:6379` | Redis server address |
| `REDIS_PASSWORD` | _(empty)_ | Redis password (optional) |
| `CONSUMER_GROUP` | `call-orchestrator` | Kafka consumer group ID |

## Kafka Events

### Consumed (from `call-events`)

| `event_type` | Trigger |
|---|---|
| `CHANNEL_CREATE` | New call channel opened — moves call to `RINGING` |
| `CHANNEL_ANSWER` | Call answered — moves call to `CONNECTED`, starts recording |
| `CHANNEL_DESTROY` | Call terminated — moves call to `ENDED`, stops recording |

### Produced (to `orchestrator-events`)

| `event_type` | When |
|---|---|
| `call_started` | Call created via REST API |
| `state_changed` | Manual transition via REST API |
| `call_ended` | Call cancelled via REST API |

## Running

### With Docker Compose (recommended)

```bash
docker compose up orchestrator
```

### Local (requires running Kafka + Redis)

```bash
go build -o orchestrator .
KAFKA_BROKERS=localhost:9092 REDIS_ADDR=localhost:6379 ./orchestrator
```

### Docker

```bash
docker build -t telco-call-orchestrator .
docker run -p 8080:8080 -p 50051:50051 \
  -e KAFKA_BROKERS=kafka:9092 \
  -e REDIS_ADDR=redis:6379 \
  telco-call-orchestrator
```

## Integration Points

| Service | Address | Purpose |
|---------|---------|---------|
| Kafka | `kafka:9092` | Event bus |
| Redis | `redis:6379` | Call state persistence |
| CTI Service | `http://cti-service:8006` | Telephony integration |
| SIP Gateway | `http://sip-gateway:8002` | SIP signalling |
| Routing Service | `http://routing-service:8007` | Call routing decisions |
| Recording Service | via Kafka `recording-control` | Call recording control |

## Architecture

```
[SIP Gateway / CTI]
        │ CHANNEL_CREATE / CHANNEL_ANSWER / CHANNEL_DESTROY
        ▼
  [Kafka: call-events]
        │
        ▼
[Call Orchestrator]──► [Kafka: routing-requests]──► [Routing Service]
        │
        ├──► [Kafka: recording-control]──► [Recording Service]
        │
        └──► [Kafka: orchestrator-events]──► downstream consumers
        │
      Redis (call state)
        │
      REST API (:8080) + gRPC (:50051)
```
