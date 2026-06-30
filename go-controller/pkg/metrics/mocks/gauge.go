// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package mocks

import (
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
