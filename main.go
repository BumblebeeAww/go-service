package main

import (
	"context"
	"encoding/json"
	"log"
	"math"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

type Metric struct {
	Timestamp time.Time `json:"timestamp"`
	CPU       float64   `json:"cpu"`
	RPS       float64   `json:"rps"`
}

type AppState struct {
	redisClient *redis.Client
	mu          sync.Mutex
	windowSize  int
	// Prometheus Metrics
	requestCounter  prometheus.Counter
	anomalyCounter  prometheus.Counter
	cpuGauge        prometheus.Gauge
	rpsGauge        prometheus.Gauge
	rollingAvgGauge prometheus.Gauge
}

var appState *AppState

func main() {
	redisAddr := getEnv("REDIS_ADDR", "redis-master.default.svc.cluster.local:6379")
	redisPassword := getEnv("REDIS_PASSWORD", "")

	log.Printf("Connecting to Redis at: %s", redisAddr)

	rdb := redis.NewClient(&redis.Options{
		Addr:     redisAddr,
		Password: redisPassword,
		DB:       0,
	})

	ctx := context.Background()
	var redisConnected bool
	for i := 0; i < 5; i++ {
		_, err := rdb.Ping(ctx).Result()
		if err == nil {
			redisConnected = true
			log.Printf("Redis connection successful")
			break
		}
		log.Printf("Redis connection attempt %d failed: %v", i+1, err)
		time.Sleep(2 * time.Second)
	}

	if !redisConnected {
		log.Printf("Warning: Could not establish Redis connection after retries")
	}

	requestCounter := promauto.NewCounter(prometheus.CounterOpts{
		Name: "go_service_requests_total",
		Help: "The total number of processed requests",
	})

	anomalyCounter := promauto.NewCounter(prometheus.CounterOpts{
		Name: "go_service_anomalies_total",
		Help: "The total number of detected anomalies",
	})

	cpuGauge := promauto.NewGauge(prometheus.GaugeOpts{
		Name: "go_service_cpu_percent",
		Help: "Current CPU usage percentage",
	})

	rpsGauge := promauto.NewGauge(prometheus.GaugeOpts{
		Name: "go_service_rps_current",
		Help: "Current RPS value",
	})

	rollingAvgGauge := promauto.NewGauge(prometheus.GaugeOpts{
		Name: "go_service_rps_rolling_avg",
		Help: "Rolling average of RPS values",
	})

	appState = &AppState{
		redisClient:     rdb,
		windowSize:      50,
		requestCounter:  requestCounter,
		anomalyCounter:  anomalyCounter,
		cpuGauge:        cpuGauge,
		rpsGauge:        rpsGauge,
		rollingAvgGauge: rollingAvgGauge,
	}

	// HTTP Handlers
	http.HandleFunc("/", rootHandler)
	http.HandleFunc("/metrics", handleMetrics)
	http.HandleFunc("/analyze", handleAnalyze)
	http.HandleFunc("/count", countHandler)
	http.HandleFunc("/health", healthHandler)

	port := getEnv("PORT", "8080")
	log.Printf("Server starting on port %s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

func rootHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte("Go Streaming Analytics Service\n\n"))
	w.Write([]byte("Available endpoints:\n"))
	w.Write([]byte("POST /analyze - Submit metrics for analysis\n"))
	w.Write([]byte("GET  /metrics - Prometheus metrics\n"))
	w.Write([]byte("GET  /count   - Get request count\n"))
	w.Write([]byte("GET  /health  - Health check\n"))
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := context.Background()
	_, err := appState.redisClient.Ping(ctx).Result()
	redisStatus := "healthy"
	if err != nil {
		redisStatus = "unhealthy"
	}

	response := map[string]string{
		"status":    "healthy",
		"redis":     redisStatus,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func countHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := context.Background()
	count, err := appState.redisClient.Get(ctx, "request_count").Int()
	if err != nil {
		if err == redis.Nil {
			err := appState.redisClient.Set(ctx, "request_count", 0, 0).Err()
			if err != nil {
				log.Printf("Redis SET error: %v", err)
				http.Error(w, "Error initializing count", http.StatusInternalServerError)
				return
			}
			count = 0
		} else {
			log.Printf("Redis GET error: %v", err)
			http.Error(w, "Error retrieving count", http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	response := map[string]int{"count": count}
	json.NewEncoder(w).Encode(response)
}

func getEnv(key, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	return value
}

func handleMetrics(w http.ResponseWriter, r *http.Request) {
	promhttp.Handler().ServeHTTP(w, r)
}

func handleAnalyze(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := context.Background()
	newCount, err := appState.redisClient.Incr(ctx, "request_count").Result()
	if err != nil {
		log.Printf("Redis INCR error: %v", err)
		http.Error(w, "Error incrementing counter", http.StatusInternalServerError)
		return
	}
	log.Printf("Redis counter incremented to: %d", newCount)

	appState.requestCounter.Inc()

	var metric Metric
	err = json.NewDecoder(r.Body).Decode(&metric)
	if err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	appState.cpuGauge.Set(metric.CPU)
	appState.rpsGauge.Set(metric.RPS)

	go func(m Metric) {
		ctx := context.Background()

		key := "metrics"
		jsonData, _ := json.Marshal(m)
		err := appState.redisClient.RPush(ctx, key, jsonData).Err()
		if err != nil {
			log.Printf("Redis RPush error: %v", err)
			return
		}

		err = appState.redisClient.LTrim(ctx, key, -int64(appState.windowSize), -1).Err()
		if err != nil {
			log.Printf("Redis LTrim error: %v", err)
		}

		window, err := appState.redisClient.LRange(ctx, key, 0, -1).Result()
		if err != nil {
			log.Printf("Redis LRange error: %v", err)
			return
		}

		var rpsValues, cpuValues []float64
		for _, item := range window {
			var met Metric
			json.Unmarshal([]byte(item), &met)
			rpsValues = append(rpsValues, met.RPS)
			cpuValues = append(cpuValues, met.CPU)
		}

		// Calculate Rolling Average (RPS)
		rollingAvg := calculateAverage(rpsValues)
		appState.rollingAvgGauge.Set(rollingAvg)

		// Calculate Z-Score for current RPS value (anomaly detection)
		if len(rpsValues) >= 2 { // Need at least 2 values for std deviation
			currentValue := m.RPS
			mean := calculateAverage(rpsValues)
			stdDev := calculateStandardDeviation(rpsValues, mean)

			if stdDev != 0 {
				zScore := (currentValue - mean) / stdDev
				if math.Abs(zScore) > 2.0 {
					log.Printf("ANOMALY DETECTED! RPS: %.2f, Z-Score: %.2f, Mean: %.2f, StdDev: %.2f",
						currentValue, zScore, mean, stdDev)
					appState.anomalyCounter.Inc()
				}
			}
		}

		log.Printf("Processed metric: Timestamp=%v, RPS=%.2f, CPU=%.2f, RollingAvgRPS=%.2f",
			m.Timestamp.Format("15:04:05"), m.RPS, m.CPU, rollingAvg)
	}(metric)

	w.WriteHeader(http.StatusAccepted)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "accepted",
		"message": "Metric accepted for processing",
	})
}

func calculateAverage(values []float64) float64 {
	if len(values) == 0 {
		return 0.0
	}
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

func calculateStandardDeviation(values []float64, mean float64) float64 {
	if len(values) <= 1 {
		return 0.0
	}
	sum := 0.0
	for _, v := range values {
		sum += math.Pow(v-mean, 2)
	}
	variance := sum / float64(len(values)-1)
	return math.Sqrt(variance)
}
