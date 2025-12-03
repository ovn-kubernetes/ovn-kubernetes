package mocks

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	io_prometheus_client "github.com/prometheus/client_model/go"
)

type GaugeMock struct {
	value float64
	mutex sync.Mutex
}

func NewGaugeMock() *GaugeMock {
	return &GaugeMock{}
}

func (h *GaugeMock) Desc() *prometheus.Desc {
	panic("unimplemented")
}

func (h *GaugeMock) Write(*io_prometheus_client.Metric) error {
	panic("unimplemented")
}

func (h *GaugeMock) Describe(chan<- *prometheus.Desc) {
}

func (h *GaugeMock) Collect(chan<- prometheus.Metric) {
	panic("unimplemented")
}

func (h *GaugeMock) Observe(value float64) {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	h.value = value
}

func (gm *GaugeMock) Set(value float64) {
	gm.mutex.Lock()
	defer gm.mutex.Unlock()
	gm.value = value
}

func (gm *GaugeMock) Inc() {
	gm.mutex.Lock()
	defer gm.mutex.Unlock()
	gm.value++
}

func (gm *GaugeMock) Dec() {
	gm.mutex.Lock()
	defer gm.mutex.Unlock()
	gm.value--
}

func (gm *GaugeMock) Add(value float64) {
	gm.mutex.Lock()
	defer gm.mutex.Unlock()
	gm.value += value
}

func (gm *GaugeMock) Sub(value float64) {
	gm.mutex.Lock()
	defer gm.mutex.Unlock()
	gm.value -= value
}

func (gm *GaugeMock) SetToCurrentTime() {
	gm.mutex.Lock()
	defer gm.mutex.Unlock()
	gm.value = float64(time.Now().UnixMilli())
}

func (gm *GaugeMock) GetValue() float64 {
	gm.mutex.Lock()
	defer gm.mutex.Unlock()
	return gm.value
}

type GaugeVecMock struct {
	gaugesByLabels map[string]*GaugeMock
	mutex          *sync.Mutex
}

func NewGaugeVecMock() *GaugeVecMock {
	return &GaugeVecMock{gaugesByLabels: make(map[string]*GaugeMock),
		mutex: &sync.Mutex{}}
}

func (GaugeVecMock) GetMetricWithLabelValues(_ ...string) (prometheus.Gauge, error) {
	panic("unimplemented")
}

func (GaugeVecMock) GetMetricWith(_ prometheus.Labels) (prometheus.Gauge, error) {
	panic("unimplemented")
}

func (v *GaugeVecMock) WithLabelValues(lvs ...string) prometheus.Gauge {
	v.mutex.Lock()
	defer v.mutex.Unlock()
	key := strings.Join(lvs, "-")
	if g, exists := v.gaugesByLabels[key]; exists {
		return g
	}
	newGauge := NewGaugeMock()
	v.gaugesByLabels[key] = newGauge
	return newGauge
}

func (GaugeVecMock) With(_ prometheus.Labels) prometheus.Gauge {
	panic("unimplemented")
}

func (GaugeVecMock) MustCurryWith(_ prometheus.Labels) *prometheus.GaugeVec {
	panic("unimplemented")
}

func (GaugeVecMock) DeleteLabelValues(_ ...string) bool {
	panic("unimplemented")
}

func (GaugeVecMock) Delete(_ prometheus.Labels) bool {
	panic("unimplemented")
}

func (GaugeVecMock) DeletePartialMatch(_ prometheus.Labels) int {
	panic("unimplemented")
}

func (GaugeVecMock) Describe(chan<- *prometheus.Desc) {
}

func (GaugeVecMock) Collect(chan<- prometheus.Metric) {
	panic("unimplemented")
}

func (GaugeVecMock) Reset() {
	panic("unimplemented")
}

func (v *GaugeVecMock) GetValue(lvs ...string) (float64, error) {
	v.mutex.Lock()
	defer v.mutex.Unlock()
	key := strings.Join(lvs, "-")
	if g, exists := v.gaugesByLabels[key]; exists {
		return g.GetValue(), nil
	}
	return 0, fmt.Errorf("no gauge metric found for label value selector %v", lvs)
}
