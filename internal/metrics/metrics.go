// Package metrics defines the Prometheus collectors of §16.
package metrics

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

type M struct {
	Reg              *prometheus.Registry
	JobsTotal        *prometheus.CounterVec
	JobDuration      *prometheus.HistogramVec
	QueueWait        *prometheus.HistogramVec
	TestcaseDuration *prometheus.HistogramVec
	GoJudgeRequests  *prometheus.CounterVec
	GoJudgeErrors    *prometheus.CounterVec
	EventsSpooled    prometheus.Gauge
	CacheBytes       prometheus.Gauge
	SlotHealthy      *prometheus.GaugeVec
	BusySlots        prometheus.Gauge
	EventsDropped    prometheus.Counter
	Alerts           *prometheus.CounterVec
}

func New() *M {
	m := &M{
		Reg: prometheus.NewRegistry(),
		JobsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "judge_jobs_total", Help: "Jobs finished, by lane, kind and verdict.",
		}, []string{"lane", "kind", "verdict"}),
		JobDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "judge_job_duration_seconds", Help: "Job duration from start to done.",
			Buckets: []float64{0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300},
		}, []string{"lane", "kind"}),
		QueueWait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "judge_queue_wait_seconds", Help: "Time from created_at to started.",
			Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300, 600},
		}, []string{"lane"}),
		TestcaseDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "judge_testcase_duration_seconds", Help: "Wall time per testcase including check.",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10},
		}, []string{"language"}),
		GoJudgeRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "judge_gojudge_requests_total", Help: "go-judge command results by status.",
		}, []string{"status"}),
		GoJudgeErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "judge_gojudge_errors_total", Help: "go-judge errors by kind (transport, internal, file).",
		}, []string{"kind"}),
		EventsSpooled: prometheus.NewGauge(prometheus.GaugeOpts{Name: "judge_events_spooled", Help: "Events waiting in the spool."}),
		CacheBytes:    prometheus.NewGauge(prometheus.GaugeOpts{Name: "judge_cache_bytes", Help: "Testdata cache size."}),
		SlotHealthy: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "judge_worker_slot_healthy", Help: "1 if the slot passed its last health check.",
		}, []string{"slot"}),
		BusySlots:     prometheus.NewGauge(prometheus.GaugeOpts{Name: "judge_worker_busy_slots", Help: "Slots running a job."}),
		EventsDropped: prometheus.NewCounter(prometheus.CounterOpts{Name: "judge_events_dropped_total", Help: "Events rejected by the API with a 4xx (bug)."}),
		Alerts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "judge_worker_alerts_total", Help: "Conditions that need an operator (unknown status, poison, bad package…).",
		}, []string{"reason"}),
	}
	m.Reg.MustRegister(m.JobsTotal, m.JobDuration, m.QueueWait, m.TestcaseDuration, m.GoJudgeRequests,
		m.GoJudgeErrors, m.EventsSpooled, m.CacheBytes, m.SlotHealthy, m.BusySlots, m.EventsDropped, m.Alerts,
		prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	return m
}

func (m *M) Slot(i int) prometheus.Gauge { return m.SlotHealthy.WithLabelValues(strconv.Itoa(i)) }

func (m *M) Alert(reason string) { m.Alerts.WithLabelValues(reason).Inc() }
