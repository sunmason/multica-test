// Package metrics is a minimal, dependency-free metrics registry exposing
// the Prometheus text format (E2: run counts by status, durations, queue
// depth and wait times).
package metrics

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Counter is a monotonically increasing counter.
type Counter struct {
	name, help string
	mu         sync.Mutex
	values     map[string]float64 // label value -> count
}

// Inc adds 1 under the given label value.
func (c *Counter) Inc(label string) { c.Add(label, 1) }

// Add adds n under the given label value.
func (c *Counter) Add(label string, n float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values[label] += n
}

// Gauge is a value that can go up and down.
type Gauge struct {
	name, help string
	mu         sync.Mutex
	values     map[string]float64
}

// Set replaces the gauge value under the given label value.
func (g *Gauge) Set(label string, v float64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.values[label] = v
}

// Histogram tracks observations in fixed buckets (cumulative, Prom-style).
type Histogram struct {
	name, help string
	mu         sync.Mutex
	buckets    []float64 // upper bounds, inclusive; +Inf implicit
	counts     map[string][]uint64
	sum        map[string]float64
	total      map[string]uint64
}

// Observe records one value under the given label value.
func (h *Histogram) Observe(label string, v float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	counts := h.counts[label]
	if counts == nil {
		counts = make([]uint64, len(h.buckets))
		h.counts[label] = counts
	}
	for i, ub := range h.buckets {
		if v <= ub {
			counts[i]++
		}
	}
	h.sum[label] += v
	h.total[label]++
}

type labeled interface {
	write(b *strings.Builder)
}

// Registry owns all metric objects and renders the exposition format.
type Registry struct {
	mu       sync.Mutex
	objects  []labeled
	runs     *Counter
	rejects  *Counter
	duration *Histogram
	queueGet *Histogram
	qDepth   *Gauge
	running  *Gauge
}

// New builds the registry with the sandbox's metric set.
func New() *Registry {
	r := &Registry{}
	r.runs = newCounter("gosandbox_runs_total", "Completed runs by terminal status.")
	r.rejects = newCounter("gosandbox_rejections_total", "Submissions rejected at admission by reason.")
	r.duration = newHistogram("gosandbox_run_duration_seconds", "Slot-held duration (compile+run) per run.", []float64{0.5, 1, 2, 5, 10, 20, 30, 60})
	r.queueGet = newHistogram("gosandbox_queue_wait_seconds", "Time spent waiting in queue.", []float64{0.1, 0.5, 1, 5, 10, 30, 60})
	r.qDepth = newGauge("gosandbox_queue_depth", "Current queue depth.")
	r.running = newGauge("gosandbox_running", "Currently executing runs.")
	r.objects = []labeled{r.runs, r.rejects, r.duration, r.queueGet, r.qDepth, r.running}
	return r
}

// Runs returns the run-status counter.
func (r *Registry) Runs() *Counter { return r.runs }

// Rejects returns the rejection counter.
func (r *Registry) Rejects() *Counter { return r.rejects }

// Duration returns the run-duration histogram.
func (r *Registry) Duration() *Histogram { return r.duration }

// QueueWait returns the queue-wait histogram.
func (r *Registry) QueueWait() *Histogram { return r.queueGet }

// QueueDepth returns the queue-depth gauge.
func (r *Registry) QueueDepth() *Gauge { return r.qDepth }

// Running returns the running-count gauge.
func (r *Registry) Running() *Gauge { return r.running }

// Render produces the Prometheus text exposition of every metric.
func (r *Registry) Render() string {
	var b strings.Builder
	for _, o := range r.objects {
		o.write(&b)
	}
	return b.String()
}

func newCounter(name, help string) *Counter {
	return &Counter{name: name, help: help, values: map[string]float64{}}
}

func (c *Counter) write(b *strings.Builder) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s counter\n", c.name, c.help, c.name)
	for _, label := range sortedKeys(c.values) {
		fmt.Fprintf(b, "%s{status=%q} %g\n", c.name, label, c.values[label])
	}
	if len(c.values) == 0 {
		fmt.Fprintf(b, "%s 0\n", c.name)
	}
}

func newGauge(name, help string) *Gauge {
	return &Gauge{name: name, help: help, values: map[string]float64{}}
}

func (g *Gauge) write(b *strings.Builder) {
	g.mu.Lock()
	defer g.mu.Unlock()
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n", g.name, g.help, g.name)
	for _, label := range sortedKeys(g.values) {
		fmt.Fprintf(b, "%s{%s} %g\n", g.name, gaugeLabel(label), g.values[label])
	}
}

// gaugeLabel maps the semantic series ("" = current value) to a name.
func gaugeLabel(label string) string {
	if label == "" {
		label = "current"
	}
	return `state="` + label + `"`
}

func newHistogram(name, help string, buckets []float64) *Histogram {
	return &Histogram{
		name: name, help: help, buckets: buckets,
		counts: map[string][]uint64{}, sum: map[string]float64{}, total: map[string]uint64{},
	}
}

func (h *Histogram) write(b *strings.Builder) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s histogram\n", h.name, h.help, h.name)
	for _, label := range sortedKeys(h.total) {
		for i, ub := range h.buckets {
			fmt.Fprintf(b, "%s_bucket{status=%q,le=%g} %d\n", h.name, label, ub, h.counts[label][i])
		}
		fmt.Fprintf(b, "%s_bucket{status=%q,le=\"+Inf\"} %d\n", h.name, label, h.total[label])
		fmt.Fprintf(b, "%s_sum{status=%q} %g\n", h.name, label, h.sum[label])
		fmt.Fprintf(b, "%s_count{status=%q} %d\n", h.name, label, h.total[label])
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
