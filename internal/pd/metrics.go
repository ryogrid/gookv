package pd

import (
	"github.com/prometheus/client_golang/prometheus"
)

// defaultDurationBuckets for pd package.
// Defined separately to avoid circular dependency with internal/server.
var defaultDurationBuckets = []float64{
	0.0001, 0.0002, 0.0004, 0.0008, 0.0016, 0.0032, 0.0064, 0.0128,
	0.0256, 0.0512, 0.1024, 0.2048, 0.4096, 0.8192, 1.6384, 3.2768,
	6.5536, 13.1072, 26.2144, 52.4288,
}

var pdTsoHandleDuration = prometheus.NewHistogram(
	prometheus.HistogramOpts{
		Namespace: "gookv",
		Subsystem: "pd",
		Name:      "tso_handle_duration_seconds",
		Help:      "Duration of TSO allocation.",
		Buckets:   defaultDurationBuckets,
	},
)

var pdTsoTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "gookv",
		Subsystem: "pd",
		Name:      "tso_total",
		Help:      "Total TSO requests handled.",
	},
)

var pdRegionHeartbeatDuration = prometheus.NewHistogram(
	prometheus.HistogramOpts{
		Namespace: "gookv",
		Subsystem: "pd",
		Name:      "region_heartbeat_duration_seconds",
		Help:      "Duration of region heartbeat processing.",
		Buckets:   defaultDurationBuckets,
	},
)

var pdRegionHeartbeatTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "gookv",
		Subsystem: "pd",
		Name:      "region_heartbeat_total",
		Help:      "Total region heartbeats received.",
	},
)

var pdStoreHeartbeatTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "gookv",
		Subsystem: "pd",
		Name:      "store_heartbeat_total",
		Help:      "Total store heartbeats received.",
	},
)

var pdRegionCount = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "gookv",
		Subsystem: "pd",
		Name:      "region_count",
		Help:      "Total regions in cluster.",
	},
)

var pdStoreCount = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "gookv",
		Subsystem: "pd",
		Name:      "store_count",
		Help:      "Total stores in cluster.",
	},
)

var pdScheduleCommandTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "gookv",
		Subsystem: "pd",
		Name:      "schedule_command_total",
		Help:      "Scheduler commands issued.",
	},
	[]string{"type"},
)

func init() {
	prometheus.MustRegister(pdTsoHandleDuration)
	prometheus.MustRegister(pdTsoTotal)
	prometheus.MustRegister(pdRegionHeartbeatDuration)
	prometheus.MustRegister(pdRegionHeartbeatTotal)
	prometheus.MustRegister(pdStoreHeartbeatTotal)
	prometheus.MustRegister(pdRegionCount)
	prometheus.MustRegister(pdStoreCount)
	prometheus.MustRegister(pdScheduleCommandTotal)
}
