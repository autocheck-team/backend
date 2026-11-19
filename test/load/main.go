// Package main измеряет очередь проверок через тот же REST и watch, что использует клиент.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"slices"
	"sync"
	"syscall"
	"time"

	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
	"github.com/autocheck-team/backend/test/integration/driver"
)

type measurements struct {
	Submissions       int            `json:"submissions"`
	Concurrency       int            `json:"client_concurrency"`
	Workers           int            `json:"workers"`
	WorkerConcurrency int            `json:"worker_concurrency"`
	Completed         int            `json:"completed"`
	Errors            int            `json:"errors"`
	Seconds           float64        `json:"elapsed_seconds"`
	PerMinute         float64        `json:"submissions_per_minute"`
	P50               float64        `json:"p50_seconds"`
	P95               float64        `json:"p95_seconds"`
	P99               float64        `json:"p99_seconds"`
	Latencies         []float64      `json:"latencies_seconds"`
	Failures          map[string]int `json:"failures"`
}

func execute(parent context.Context, count, concurrency, workers, workerConcurrency int) (measurements, error) {
	ctx, cancel := context.WithTimeout(parent, 15*time.Minute)
	defer cancel()
	base, keycloak, database := os.Getenv("E2E_API_URL"), os.Getenv("E2E_KEYCLOAK_URL"), os.Getenv("E2E_DATABASE_URL")
	if base == "" || keycloak == "" || database == "" {
		return measurements{}, errors.New("E2E_API_URL, E2E_KEYCLOAK_URL and E2E_DATABASE_URL are required")
	}
	token, err := driver.Login(ctx, keycloak+"/realms/autocheck", "author@autocheck.local", "author")
	if err != nil {
		return measurements{}, err
	}
	author := driver.New(base, token)
	student, userID, err := driver.Student(ctx, keycloak, base)
	if err != nil {
		return measurements{}, err
	}
	if err := driver.Pro(ctx, database, userID); err != nil {
		return measurements{}, err
	}
	problem, err := author.Problem(ctx, []string{"python3.14"})
	if err != nil {
		return measurements{}, err
	}
	submit := func() (time.Duration, error) {
		start := time.Now()
		sub, err := student.Submit(ctx, problem, "python3.14", `print(sum(map(int,input().split())))`)
		if err != nil {
			return 0, err
		}
		result, err := student.Watch(ctx, sub.GetId())
		if err != nil {
			return 0, err
		}
		if result.GetVerdict() != v1.Verdict_VERDICT_OK {
			return 0, fmt.Errorf("unexpected verdict %s", result.GetVerdict())
		}
		return time.Since(start), nil
	}
	// Один прогрев за пределами измерения исключает загрузку образа и холодный запуск API.
	if _, err := submit(); err != nil {
		return measurements{}, fmt.Errorf("warmup: %w", err)
	}
	result := measurements{
		Submissions: count, Concurrency: concurrency, Workers: workers, WorkerConcurrency: workerConcurrency,
		Latencies: make([]float64, 0, count), Failures: make(map[string]int),
	}
	jobs := make(chan struct{}, count)
	for range count {
		jobs <- struct{}{}
	}
	close(jobs)
	var mutex sync.Mutex
	var group sync.WaitGroup
	start := time.Now()
	for range min(concurrency, count) {
		group.Go(func() {
			for range jobs {
				duration, err := submit()
				mutex.Lock()
				if err != nil {
					result.Errors++
					result.Failures[err.Error()]++
				} else {
					result.Completed++
					result.Latencies = append(result.Latencies, duration.Seconds())
				}
				mutex.Unlock()
			}
		})
	}
	group.Wait()
	result.Seconds = time.Since(start).Seconds()
	result.PerMinute = float64(result.Completed) / result.Seconds * 60
	slices.Sort(result.Latencies)
	percentile := func(fraction float64) float64 {
		if len(result.Latencies) == 0 {
			return 0
		}
		return result.Latencies[int(math.Ceil(float64(len(result.Latencies))*fraction))-1]
	}
	result.P50, result.P95, result.P99 = percentile(.50), percentile(.95), percentile(.99)
	return result, nil
}

func main() {
	count := flag.Int("n", 40, "число измеряемых посылок после одного прогрева")
	concurrency := flag.Int("concurrency", 8, "число одновременно ожидаемых посылок")
	workers := flag.Int("workers", 1, "фактические реплики worker из Compose")
	workerConcurrency := flag.Int("worker-concurrency", 1, "WORKER_CONCURRENCY каждой реплики")
	flag.Parse()
	if *count < 1 || *concurrency < 1 || *workers < 1 || *workerConcurrency < 1 {
		fmt.Fprintln(os.Stderr, "all parameters must be positive")
		os.Exit(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	result, err := execute(ctx, *count, *concurrency, *workers, *workerConcurrency)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, "unable to write measurements")
		os.Exit(1)
	}
	if result.Errors > 0 {
		os.Exit(1)
	}
}
