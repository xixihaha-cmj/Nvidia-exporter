package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	namespace = "nvidia"
)

// Exporter 在后台周期性采集 NVML，并把结果缓存起来。
//
// /metrics 只读缓存、不做任何阻塞的 NVML 调用，所以：
//   - 抓取永远很快返回，不会把 Prometheus 拖到超时；
//   - 驱动/NVML 卡死时，靠 nvidia_collect_* 系列指标暴露出来，
//     而不是表现为「指标不更新但 up 还是 1」。
type Exporter struct {
	// 采集自身的健康度（本次改造新增）
	up               prometheus.Gauge
	collectSuccess   prometheus.Gauge
	collectStuck     prometheus.Gauge
	collectTimestamp prometheus.Gauge
	collectDuration  prometheus.Gauge
	collectTimeouts  prometheus.Counter
	collectErrors    *prometheus.CounterVec
	nvmlReinitTotal  *prometheus.CounterVec

	// GPU 指标
	info                  *prometheus.GaugeVec
	deviceCount           prometheus.Gauge
	deviceExpected        prometheus.Gauge
	deviceInfo            *prometheus.GaugeVec
	gpuHealth             *prometheus.GaugeVec
	gpuErrors             *prometheus.CounterVec
	temperatures          *prometheus.GaugeVec
	powerUsage            *prometheus.GaugeVec
	powerUsageAverage     *prometheus.GaugeVec
	fanSpeed              *prometheus.GaugeVec
	memoryTotal           *prometheus.GaugeVec
	memoryUsed            *prometheus.GaugeVec
	utilizationMemory     *prometheus.GaugeVec
	utilizationGPU        *prometheus.GaugeVec
	utilizationGPUAverage *prometheus.GaugeVec

	interval   time.Duration
	stuckAfter time.Duration
	expected   float64

	nvmlReinitInterval time.Duration
	lastNVMLReinit     time.Time

	mu          sync.RWMutex
	snapshot    *Metrics
	lastSuccess time.Time
	collecting  bool
}

func main() {
	var (
		listenAddress = flag.String("web.listen-address", ":9401", "Address to listen on for web interface and telemetry.")
		metricsPath   = flag.String("web.telemetry-path", "/metrics", "Path under which to expose metrics.")
		interval      = flag.Duration("collect-interval", 5*time.Second, "How often NVML metrics are collected in the background.")
		stuckAfter    = flag.Duration("collect-stuck-after", 20*time.Second, "Mark a collection as stuck when it exceeds this duration.")
		expectedDev   = flag.Int("expected-devices", 0, "Expected number of GPUs on this host (0 disables nvidia_device_expected).")
		expectedFile  = flag.String("expected-devices-file", "", "File containing the expected number of GPUs (takes precedence over -expected-devices).")
		nvmlReinit    = flag.Duration("nvml-reinit-interval", 5*time.Minute, "Re-initialize NVML periodically so hot-reset/hot-added GPUs are picked up (0 disables).")
	)
	flag.Parse()

	exporter := NewExporter(*interval, *stuckAfter, expectedDeviceCount(*expectedDev, *expectedFile), *nvmlReinit)
	prometheus.MustRegister(exporter)
	exporter.Run()

	http.Handle(*metricsPath, promhttp.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
             <head><title>NVML Exporter</title></head>
             <body>
             <h1>NVML Exporter</h1>
             <p><a href='` + *metricsPath + `'>Metrics</a></p>
             </body>
             </html>`))
	})
	fmt.Println("Starting HTTP server on", *listenAddress)
	log.Fatal(http.ListenAndServe(*listenAddress, nil))
}

// expectedDeviceCount 读取期望卡数：优先文件，其次命令行参数，都没有就是 0（不校验）。
func expectedDeviceCount(flagValue int, file string) float64 {
	if file != "" {
		data, err := ioutil.ReadFile(file)
		if err != nil {
			log.Printf("cannot read expected devices file %q: %s", file, err)
		} else {
			text := strings.TrimSpace(string(data))
			if n, err := strconv.Atoi(text); err == nil && n >= 0 {
				return float64(n)
			}
			log.Printf("invalid expected devices file %q: %q", file, text)
		}
	}
	if flagValue > 0 {
		return float64(flagValue)
	}
	return 0
}

func NewExporter(interval, stuckAfter time.Duration, expected float64, nvmlReinitInterval time.Duration) *Exporter {
	e := &Exporter{
		interval:   interval,
		stuckAfter: stuckAfter,
		expected:   expected,

		nvmlReinitInterval: nvmlReinitInterval,

		up: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "up",
			Help:      "NVML Metric Collection Operational",
		}),
		collectSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "collect_success",
			Help:      "1 if the last background NVML collection succeeded, 0 otherwise",
		}),
		collectStuck: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "collect_stuck",
			Help:      "1 if the current NVML collection exceeded -collect-stuck-after (driver likely wedged)",
		}),
		collectTimestamp: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "collect_timestamp_seconds",
			Help:      "Unix timestamp of the last successful NVML collection",
		}),
		collectDuration: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "collect_duration_seconds",
			Help:      "Duration of the last NVML collection",
		}),
		collectTimeouts: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "collect_timeout_total",
			Help:      "Number of collections that exceeded -collect-stuck-after",
		}),
		collectErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "collect_errors_total",
			Help:      "Number of failed collections by reason",
		}, []string{"reason"}),
		nvmlReinitTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "nvml_reinit_total",
			Help:      "Periodic NVML re-initializations by result (picks up hot-reset/hot-added GPUs)",
		}, []string{"result"}),

		info: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "driver_info",
			Help:      "NVML Info",
		}, []string{"version"}),
		deviceCount: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "device_count",
			Help:      "Count of found nvidia devices",
		}),
		deviceExpected: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "device_expected",
			Help:      "Expected count of nvidia devices on this host (0 = not configured)",
		}),
		deviceInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "info",
			Help:      "Info as reported by the device",
		}, []string{"index", "minor", "uuid", "name"}),
		gpuHealth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "gpu_health",
			Help:      "1 if every NVML call for this device succeeded, 0 if the device is enumerable but unusable",
		}, []string{"index", "minor", "uuid", "name"}),
		gpuErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "gpu_error_total",
			Help:      "Number of failed NVML calls per device and call name",
		}, []string{"uuid", "call"}),
	}
	e.deviceExpected.Set(expected)

	// 每张卡的数值指标统一带上 gpu_uuid，避免 minor 号漂移后告警指错卡。
	e.temperatures = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "temperatures",
		Help:      "Temperature as reported by the device",
	}, []string{"minor", "gpu_uuid"})
	e.powerUsage = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "power_usage",
		Help:      "Power usage as reported by the device",
	}, []string{"minor", "gpu_uuid"})
	e.powerUsageAverage = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "power_usage_average",
		Help:      "Power usage as reported by the device averaged over 10s",
	}, []string{"minor", "gpu_uuid"})
	e.fanSpeed = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "fanspeed",
		Help:      "Fan speed as reported by the device",
	}, []string{"minor", "gpu_uuid"})
	e.memoryTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "memory_total",
		Help:      "Total memory as reported by the device",
	}, []string{"minor", "gpu_uuid"})
	e.memoryUsed = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "memory_used",
		Help:      "Used memory as reported by the device",
	}, []string{"minor", "gpu_uuid"})
	e.utilizationMemory = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "utilization_memory",
		Help:      "Memory Utilization as reported by the device",
	}, []string{"minor", "gpu_uuid"})
	e.utilizationGPU = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "utilization_gpu",
		Help:      "GPU utilization as reported by the device",
	}, []string{"minor", "gpu_uuid"})
	e.utilizationGPUAverage = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "utilization_gpu_average",
		Help:      "GPU utilization as reported by the device averaged over 10s",
	}, []string{"minor", "gpu_uuid"})

	return e
}

// Run 启动后台采集。注意：采集不在 HTTP 处理里做。
func (e *Exporter) Run() {
	go func() {
		e.collectOnce()
		ticker := time.NewTicker(e.interval)
		defer ticker.Stop()
		for range ticker.C {
			e.collectOnce()
		}
	}()
}

type collectResult struct {
	metrics  *Metrics
	err      error
	duration float64
}

// collectOnce 触发一次采集。同一时刻只允许一次在跑（防止卡死时不断堆 goroutine）。
func (e *Exporter) collectOnce() {
	e.mu.Lock()
	if e.collecting {
		e.mu.Unlock()
		return
	}
	e.collecting = true
	e.mu.Unlock()

	// Periodic NVML re-init: a hot-reset/hot-added GPU is only enumerated after Initialize again.
	// Kept out of the HTTP path so /metrics never blocks.
	if e.nvmlReinitInterval > 0 && time.Since(e.lastNVMLReinit) >= e.nvmlReinitInterval {
		e.lastNVMLReinit = time.Now()
		if err := nvmlReinit(); err != nil {
			e.nvmlReinitTotal.WithLabelValues("error").Inc()
			log.Printf("periodic NVML re-init failed: %s", err)
		} else {
			e.nvmlReinitTotal.WithLabelValues("ok").Inc()
		}
	}

	ch := make(chan collectResult, 1)
	go func() {
		start := time.Now()
		m, err := collectMetrics()
		ch <- collectResult{metrics: m, err: err, duration: time.Since(start).Seconds()}
	}()

	go func() {
		select {
		case r := <-ch:
			e.finish(r, false)
		case <-time.After(e.stuckAfter):
			// NVML/驱动卡住了。cgo 调用没法 cancel，这里先把状态暴露给 Prometheus，
			// 等它最终返回（或者永远不返回）再收尾。
			e.up.Set(0)
			e.collectSuccess.Set(0)
			e.collectStuck.Set(1)
			e.collectTimeouts.Inc()
			log.Printf("collection is taking longer than %s, marking as stuck", e.stuckAfter)
			r := <-ch
			e.finish(r, true)
		}
	}()
}

func (e *Exporter) finish(r collectResult, wasStuck bool) {
	e.collectDuration.Set(r.duration)
	e.collectStuck.Set(0)

	e.mu.Lock()
	e.collecting = false
	if r.err != nil {
		e.mu.Unlock()
		e.up.Set(0)
		e.collectSuccess.Set(0)
		e.collectErrors.WithLabelValues(classifyError(r.err)).Inc()
		log.Printf("Failed to collect metrics: %s", r.err)
		return
	}
	e.snapshot = r.metrics
	e.lastSuccess = time.Now()
	e.mu.Unlock()

	e.up.Set(1)
	e.collectSuccess.Set(1)

	for _, d := range r.metrics.Devices {
		for _, call := range d.FailedCalls {
			e.gpuErrors.WithLabelValues(d.UUID, call).Inc()
		}
	}
	if wasStuck {
		log.Printf("collection recovered after the stuck threshold (%s devices)", strconv.Itoa(len(r.metrics.Devices)))
	}
}

// classifyError 把错误归类成有限的 reason 取值，方便做告警和面板。
func classifyError(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "could not load NVML library"):
		return "nvml_library"
	case strings.Contains(msg, "Not Supported"):
		return "not_supported"
	case strings.Contains(msg, "Insufficient Size"):
		return "insufficient_size"
	default:
		return "nvml"
	}
}

// Collect 只读缓存，永远不做阻塞调用。
func (e *Exporter) Collect(metrics chan<- prometheus.Metric) {
	e.mu.RLock()
	snapshot := e.snapshot
	lastSuccess := e.lastSuccess
	e.mu.RUnlock()

	if !lastSuccess.IsZero() {
		e.collectTimestamp.Set(float64(lastSuccess.Unix()))
	}

	if snapshot == nil {
		e.up.Set(0)
	} else {
		// Reset 之后再写入：这样掉卡/换卡后旧序列会自动消失，
		// Prometheus 侧就能看到 nvidia_device_count 掉下来。
		e.info.Reset()
		e.deviceInfo.Reset()
		e.gpuHealth.Reset()
		e.fanSpeed.Reset()
		e.memoryTotal.Reset()
		e.memoryUsed.Reset()
		e.powerUsage.Reset()
		e.powerUsageAverage.Reset()
		e.temperatures.Reset()
		e.utilizationGPU.Reset()
		e.utilizationGPUAverage.Reset()
		e.utilizationMemory.Reset()

		e.info.WithLabelValues(snapshot.Version).Set(1)
		e.deviceCount.Set(float64(len(snapshot.Devices)))

		for i := range snapshot.Devices {
			d := snapshot.Devices[i]
			e.deviceInfo.WithLabelValues(d.Index, d.MinorNumber, d.UUID, d.Name).Set(1)
			e.gpuHealth.WithLabelValues(d.Index, d.MinorNumber, d.UUID, d.Name).Set(d.Health)
			e.fanSpeed.WithLabelValues(d.MinorNumber, d.UUID).Set(d.FanSpeed)
			e.memoryTotal.WithLabelValues(d.MinorNumber, d.UUID).Set(d.MemoryTotal)
			e.memoryUsed.WithLabelValues(d.MinorNumber, d.UUID).Set(d.MemoryUsed)
			e.powerUsage.WithLabelValues(d.MinorNumber, d.UUID).Set(d.PowerUsage)
			e.powerUsageAverage.WithLabelValues(d.MinorNumber, d.UUID).Set(d.PowerUsageAverage)
			e.temperatures.WithLabelValues(d.MinorNumber, d.UUID).Set(d.Temperature)
			e.utilizationGPU.WithLabelValues(d.MinorNumber, d.UUID).Set(d.UtilizationGPU)
			e.utilizationGPUAverage.WithLabelValues(d.MinorNumber, d.UUID).Set(d.UtilizationGPUAverage)
			e.utilizationMemory.WithLabelValues(d.MinorNumber, d.UUID).Set(d.UtilizationMemory)

		}
	}

	e.up.Collect(metrics)
	e.collectSuccess.Collect(metrics)
	e.collectStuck.Collect(metrics)
	e.collectTimestamp.Collect(metrics)
	e.collectDuration.Collect(metrics)
	e.collectTimeouts.Collect(metrics)
	e.collectErrors.Collect(metrics)
	e.nvmlReinitTotal.Collect(metrics)
	e.deviceCount.Collect(metrics)
	e.deviceExpected.Collect(metrics)
	e.deviceInfo.Collect(metrics)
	e.gpuHealth.Collect(metrics)
	e.gpuErrors.Collect(metrics)
	e.fanSpeed.Collect(metrics)
	e.info.Collect(metrics)
	e.memoryTotal.Collect(metrics)
	e.memoryUsed.Collect(metrics)
	e.powerUsage.Collect(metrics)
	e.powerUsageAverage.Collect(metrics)
	e.temperatures.Collect(metrics)
	e.utilizationGPU.Collect(metrics)
	e.utilizationGPUAverage.Collect(metrics)
	e.utilizationMemory.Collect(metrics)
}

func (e *Exporter) Describe(descs chan<- *prometheus.Desc) {
	e.up.Describe(descs)
	e.collectSuccess.Describe(descs)
	e.collectStuck.Describe(descs)
	e.collectTimestamp.Describe(descs)
	e.collectDuration.Describe(descs)
	e.collectTimeouts.Describe(descs)
	e.collectErrors.Describe(descs)
	e.nvmlReinitTotal.Describe(descs)
	e.deviceCount.Describe(descs)
	e.deviceExpected.Describe(descs)
	e.deviceInfo.Describe(descs)
	e.gpuHealth.Describe(descs)
	e.gpuErrors.Describe(descs)
	e.fanSpeed.Describe(descs)
	e.info.Describe(descs)
	e.memoryTotal.Describe(descs)
	e.memoryUsed.Describe(descs)
	e.powerUsage.Describe(descs)
	e.powerUsageAverage.Describe(descs)
	e.temperatures.Describe(descs)
	e.utilizationGPU.Describe(descs)
	e.utilizationGPUAverage.Describe(descs)
	e.utilizationMemory.Describe(descs)
}
