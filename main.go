package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	appapi "telco-call-orchestrator/internal/api"
	appgrpc "telco-call-orchestrator/internal/grpc"
	appkafka "telco-call-orchestrator/internal/kafka"
	"telco-call-orchestrator/internal/orchestrator"
)

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	log, _ := zap.NewProduction()
	defer log.Sync()

	cfg := orchestrator.Config{
		KafkaBrokers:       getEnv("KAFKA_BROKERS", "kafka:9092"),
		TopicCallEvents:    getEnv("TOPIC_CALL_EVENTS", "call-events"),
		TopicOrchEvents:    getEnv("TOPIC_ORCH_EVENTS", "orchestrator-events"),
		TopicRoutingReq:    getEnv("TOPIC_ROUTING_REQ", "routing-requests"),
		TopicRecordingCtrl: getEnv("TOPIC_RECORDING_CTRL", "recording-control"),
		RedisAddr:          getEnv("REDIS_ADDR", "redis:6379"),
		ConsumerGroup:      getEnv("CONSUMER_GROUP", "call-orchestrator"),
	}

	
	rdb := redis.NewClient(&redis.Options{
		Addr:         cfg.RedisAddr,
		Password:     getEnv("REDIS_PASSWORD", ""),
		DB:           0,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
		PoolSize:     20,
	})

	
	producer, err := appkafka.NewProducer(cfg.KafkaBrokers, log)
	if err != nil {
		log.Fatal("kafka producer", zap.Error(err))
	}
	defer producer.Close()

	
	orch := orchestrator.New(cfg, producer, rdb, log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	
	consumer, err := appkafka.NewConsumer(
		cfg.KafkaBrokers,
		cfg.ConsumerGroup,
		[]string{cfg.TopicCallEvents},
		orch.HandleEvent,
		log,
	)
	if err != nil {
		log.Fatal("kafka consumer", zap.Error(err))
	}
	go consumer.Start(ctx)

	
	grpcSrv := appgrpc.NewServer(50051, log)
	go func() {
		if err := grpcSrv.Start(ctx); err != nil {
			log.Error("grpc server error", zap.Error(err))
		}
	}()

	
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"status":"ok","active_calls":%d}`, orch.ActiveCallCount())
	})
	apiHandler := appapi.NewHandler(orch, log)
	apiHandler.Register(mux)
	httpSrv := &http.Server{Addr: ":8080", Handler: mux}
	go httpSrv.ListenAndServe()

	log.Info("call orchestrator started")
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Info("shutting down")
	cancel()
	httpSrv.Shutdown(context.Background())
}
