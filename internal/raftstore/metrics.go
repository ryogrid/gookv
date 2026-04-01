package raftstore

import (
	"github.com/prometheus/client_golang/prometheus"
)

// defaultDurationBuckets for raftstore package.
// Defined separately from internal/server to avoid circular dependency.
var defaultDurationBuckets = []float64{
	0.0001, 0.0002, 0.0004, 0.0008, 0.0016, 0.0032, 0.0064, 0.0128,
	0.0256, 0.0512, 0.1024, 0.2048, 0.4096, 0.8192, 1.6384, 3.2768,
	6.5536, 13.1072, 26.2144, 52.4288,
}

var HandleReadyDuration = prometheus.NewHistogram(
	prometheus.HistogramOpts{
		Namespace: "gookv",
		Subsystem: "raftstore",
		Name:      "handle_ready_duration_seconds",
		Help:      "Duration of Raft Ready processing.",
		Buckets:   defaultDurationBuckets,
	},
)

var ApplyDuration = prometheus.NewHistogram(
	prometheus.HistogramOpts{
		Namespace: "gookv",
		Subsystem: "raftstore",
		Name:      "apply_duration_seconds",
		Help:      "Duration of entry apply (both sync and async paths).",
		Buckets:   defaultDurationBuckets,
	},
)

var LogPersistDuration = prometheus.NewHistogram(
	prometheus.HistogramOpts{
		Namespace: "gookv",
		Subsystem: "raftstore",
		Name:      "log_persist_duration_seconds",
		Help:      "Duration of Raft log persistence (SaveReady/BuildWriteTask).",
		Buckets:   defaultDurationBuckets,
	},
)

var ProposalTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "gookv",
		Subsystem: "raftstore",
		Name:      "proposal_total",
		Help:      "Total number of normal write proposals.",
	},
)

var ReadIndexTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "gookv",
		Subsystem: "raftstore",
		Name:      "read_index_total",
		Help:      "Total number of ReadIndex requests.",
	},
)

var AdminCmdTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "gookv",
		Subsystem: "raftstore",
		Name:      "admin_cmd_total",
		Help:      "Total number of admin commands.",
	},
	[]string{"type"},
)

var SendMessageTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "gookv",
		Subsystem: "raftstore",
		Name:      "send_message_total",
		Help:      "Total Raft messages sent.",
	},
)

var LeaderCount = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "gookv",
		Subsystem: "raftstore",
		Name:      "leader_count",
		Help:      "Number of regions where this peer is leader.",
	},
)

var RegionCount = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "gookv",
		Subsystem: "raftstore",
		Name:      "region_count",
		Help:      "Total number of regions on this store.",
	},
)

var SnapshotSendTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "gookv",
		Subsystem: "raftstore",
		Name:      "snapshot_send_total",
		Help:      "Total snapshots sent.",
	},
)

func init() {
	prometheus.MustRegister(HandleReadyDuration)
	prometheus.MustRegister(ApplyDuration)
	prometheus.MustRegister(LogPersistDuration)
	prometheus.MustRegister(ProposalTotal)
	prometheus.MustRegister(ReadIndexTotal)
	prometheus.MustRegister(AdminCmdTotal)
	prometheus.MustRegister(SendMessageTotal)
	prometheus.MustRegister(LeaderCount)
	prometheus.MustRegister(RegionCount)
	prometheus.MustRegister(SnapshotSendTotal)
}
