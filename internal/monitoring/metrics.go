// Package monitoring собирает метрики процесса без глобального registry.
package monitoring

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics хранит ограниченные наборы меток; идентификаторы остаются только в логах.
type Metrics struct {
	registry     *prometheus.Registry
	requests     *prometheus.CounterVec
	latency      *prometheus.HistogramVec
	queue        *prometheus.GaugeVec
	queuePoll    prometheus.Gauge
	queueUpdated prometheus.Gauge
	attempts     prometheus.Counter
	systemErrors prometheus.Counter
	active       prometheus.Gauge
	judging      prometheus.Histogram
	stages       *prometheus.HistogramVec
	verdicts     *prometheus.CounterVec
	cleanup      *prometheus.CounterVec
	payments     *prometheus.CounterVec
	limits       *prometheus.CounterVec
}

// New создаёт независимый registry с постоянной меткой сервиса.
func New(service string) *Metrics {
	registry := prometheus.NewRegistry()
	register := prometheus.WrapRegistererWith(prometheus.Labels{"service": service}, registry)
	m := &Metrics{
		registry: registry,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "autocheck_requests_total",
			Help: "Completed API requests.",
		}, []string{"transport", "method", "code"}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "autocheck_request_duration_seconds",
			Help:    "API request duration including streaming lifetime.",
			Buckets: prometheus.DefBuckets,
		}, []string{"transport", "method"}),
		queue: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "autocheck_judge_queue_depth",
			Help: "Nonterminal jobs in PostgreSQL, including delayed retries.",
		}, []string{"state"}),
		queuePoll: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "autocheck_judge_queue_poll_success",
			Help: "Whether the last queue poll succeeded.",
		}),
		queueUpdated: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "autocheck_judge_queue_updated_timestamp_seconds",
			Help: "Unix timestamp of last successful queue poll.",
		}),
		attempts: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "autocheck_judge_attempts_total",
			Help: "Claimed judging attempts, including retries.",
		}),
		systemErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "autocheck_judge_system_errors_total",
			Help: "System errors in judging attempts, including result persistence failures.",
		}),
		active: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "autocheck_judge_active",
			Help: "Currently executing judging attempts in this process.",
		}),
		judging: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "autocheck_judge_duration_seconds",
			Help:    "Wall duration per judging attempt, including IO and cleanup.",
			Buckets: []float64{.1, .5, 1, 2, 5, 10, 30, 60, 120, 300, 600},
		}),
		stages: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "autocheck_judge_stage_duration_seconds",
			Help:    "Wall duration of each sandbox compile or test execution.",
			Buckets: []float64{.01, .05, .1, .5, 1, 2, 5, 10, 30, 60},
		}, []string{"stage"}),
		verdicts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "autocheck_judge_verdicts_total",
			Help: "Committed terminal submission verdicts.",
		}, []string{"verdict"}),
		cleanup: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "autocheck_sandbox_cleanup_total",
			Help: "Workspace close and orphan sweep outcomes.",
		}, []string{"action", "result"}),
		payments: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "autocheck_billing_payments_total",
			Help: "Committed payment creations and successful settlements, excluding idempotent replays.",
		}, []string{"event"}),
		limits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "autocheck_billing_limit_rejections_total",
			Help: "Submission rejections with HTTP 429 equivalent.",
		}, []string{"limit"}),
	}
	register.MustRegister(
		m.requests, m.latency, m.queue, m.queuePoll, m.queueUpdated,
		m.attempts, m.systemErrors, m.active, m.judging, m.stages, m.verdicts,
		m.cleanup, m.payments, m.limits, collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	for _, state := range []string{"waiting", "running"} {
		m.queue.WithLabelValues(state).Set(0)
	}
	for _, event := range []string{"created", "succeeded"} {
		m.payments.WithLabelValues(event).Add(0)
	}
	for _, limit := range []string{"compilation", "test_runs"} {
		m.limits.WithLabelValues(limit).Add(0)
	}
	for _, verdict := range []string{"OK", "WA", "CE", "TL", "ML", "RE", "SE"} {
		m.verdicts.WithLabelValues(verdict).Add(0)
	}
	return m
}

// Request записывает завершённый запрос с именем метода или шаблоном маршрута.
func (m *Metrics) Request(transport, method, code string, elapsed time.Duration) {
	if m == nil {
		return
	}
	m.requests.WithLabelValues(transport, method, code).Inc()
	m.latency.WithLabelValues(transport, method).Observe(elapsed.Seconds())
}

// BeginAttempt учитывает активную попытку и возвращает функцию её завершения.
func (m *Metrics) BeginAttempt() func() {
	if m == nil {
		return func() {}
	}
	m.attempts.Inc()
	m.active.Inc()
	started := time.Now()
	return func() { m.active.Dec(); m.judging.Observe(time.Since(started).Seconds()) }
}

// SystemError учитывает сбой попытки до планирования повтора.
func (m *Metrics) SystemError() {
	if m != nil {
		m.systemErrors.Inc()
	}
}

// Verdict учитывает только зафиксированный конечный результат.
func (m *Metrics) Verdict(verdict string) {
	if m != nil {
		m.verdicts.WithLabelValues(verdict).Inc()
	}
}

// Stage измеряет отдельный запуск компилятора или решения.
func (m *Metrics) Stage(stage string, elapsed time.Duration) {
	if m != nil {
		m.stages.WithLabelValues(stage).Observe(elapsed.Seconds())
	}
}

// Cleanup учитывает как успешную уборку, так и отказ Docker.
func (m *Metrics) Cleanup(action string, err error) {
	if m == nil {
		return
	}
	result := "success"
	if err != nil {
		result = "error"
	}
	m.cleanup.WithLabelValues(action, result).Inc()
}

// Payment учитывает событие после commit, чтобы повтор запроса не завышал счётчик.
func (m *Metrics) Payment(event string) {
	if m != nil {
		m.payments.WithLabelValues(event).Inc()
	}
}

// Limit учитывает отказ сценария при исчерпании квоты.
func (m *Metrics) Limit(limit string) {
	if m != nil {
		m.limits.WithLabelValues(limit).Inc()
	}
}

// Queue обновляет снимок очереди; при ошибке сохраняет последнее значение и отмечает сбой.
func (m *Metrics) Queue(waiting, running int64, err error) {
	if err != nil {
		m.queuePoll.Set(0)
		return
	}
	m.queue.WithLabelValues("waiting").Set(float64(waiting))
	m.queue.WithLabelValues("running").Set(float64(running))
	m.queuePoll.Set(1)
	m.queueUpdated.SetToCurrentTime()
}
