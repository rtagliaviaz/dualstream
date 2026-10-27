package main

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Counters. Cumulative since process start. Layer replacement does not
// reset them: from Prometheus' perspective, "total packets on layer h"
// is a meaningful monotonic number across broadcaster restarts.
var (
	metricLayerPackets = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "dualstream",
		Subsystem: "layer",
		Name:      "packets_total",
		Help:      "Total RTP packets received on each layer.",
	}, []string{"rid"})

	metricLayerBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "dualstream",
		Subsystem: "layer",
		Name:      "bytes_total",
		Help:      "Total RTP payload bytes received on each layer.",
	}, []string{"rid"})

	metricSwitches = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "dualstream",
		Subsystem: "viewer",
		Name:      "switches_total",
		Help:      "Total layer switches, labeled by source and target rid.",
	}, []string{"from", "to"})

	metricAudioPackets = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "dualstream",
		Name:      "audio_packets_forwarded_total",
		Help:      "Total audio RTP packets read from the broadcaster.",
	})

	metricResets = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "dualstream",
		Name:      "resets_total",
		Help:      "Number of POST /reset calls served.",
	})
)

// layerCollector emits per-layer gauges (bitrate, viewers) at scrape
// time by reading the SFU's current state. This avoids stale values
// after a layer is killed or replaced, without any bookkeeping.
type layerCollector struct {
	sfu         *SFUServer
	bitrateDesc *prometheus.Desc
	viewersDesc *prometheus.Desc
}

func newLayerCollector(s *SFUServer) *layerCollector {
	return &layerCollector{
		sfu: s,
		bitrateDesc: prometheus.NewDesc(
			"dualstream_layer_bitrate_bps",
			"Measured bitrate of each layer, in bits per second.",
			[]string{"rid"}, nil,
		),
		viewersDesc: prometheus.NewDesc(
			"dualstream_layer_viewers",
			"Number of viewers currently subscribed to each layer.",
			[]string{"rid"}, nil,
		),
	}
}

func (c *layerCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.bitrateDesc
	ch <- c.viewersDesc
}

func (c *layerCollector) Collect(ch chan<- prometheus.Metric) {
	// Snapshot the map under s.mu, then read each layer under its own
	// lock. Same lock order as handleLayers: s.mu → layer.mu.
	c.sfu.mu.RLock()
	layers := make(map[string]*layerState, len(c.sfu.layers))
	for rid, ls := range c.sfu.layers {
		layers[rid] = ls
	}
	c.sfu.mu.RUnlock()

	for rid, ls := range layers {
		ls.mu.RLock()
		br := ls.measuredBitrate
		vc := len(ls.viewers)
		ls.mu.RUnlock()

		ch <- prometheus.MustNewConstMetric(
			c.bitrateDesc, prometheus.GaugeValue, float64(br), rid)
		ch <- prometheus.MustNewConstMetric(
			c.viewersDesc, prometheus.GaugeValue, float64(vc), rid)
	}
}

// registerMetrics wires every metric into the default registry. Called
// once from main() right after creating the SFU, so the GaugeFuncs and
// the layerCollector can capture the server pointer.
func registerMetrics(s *SFUServer) {
	prometheus.MustRegister(
		metricLayerPackets,
		metricLayerBytes,
		metricSwitches,
		metricAudioPackets,
		metricResets,
	)
	prometheus.MustRegister(newLayerCollector(s))

	prometheus.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace: "dualstream",
		Name:      "broadcaster_up",
		Help:      "1 if a broadcaster is connected, 0 otherwise.",
	}, func() float64 {
		s.mu.RLock()
		defer s.mu.RUnlock()
		if s.broadcaster != nil {
			return 1
		}
		return 0
	}))

	prometheus.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace: "dualstream",
		Name:      "audio_up",
		Help:      "1 if an audio source is active, 0 otherwise.",
	}, func() float64 {
		s.mu.RLock()
		defer s.mu.RUnlock()
		if s.audio != nil {
			return 1
		}
		return 0
	}))

	prometheus.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace: "dualstream",
		Name:      "viewers_total",
		Help:      "Total number of viewers across all layers.",
	}, func() float64 {
		s.mu.RLock()
		defer s.mu.RUnlock()
		total := 0
		for _, ls := range s.layers {
			ls.mu.RLock()
			total += len(ls.viewers)
			ls.mu.RUnlock()
		}
		return float64(total)
	}))

	prometheus.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace: "dualstream",
		Subsystem: "hls",
		Name:      "variants_available",
		Help:      "Number of HLS variants currently available on disk.",
	}, func() float64 {
		return float64(len(s.hls.Snapshot().Variants))
	}))
}
