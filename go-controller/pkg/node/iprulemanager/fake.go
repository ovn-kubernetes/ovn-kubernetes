// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package iprulemanager

import (
	"fmt"
	"time"
)

type FakeControllerWithError struct {
}

func (f *FakeControllerWithError) Run(_ <-chan struct{}, _ time.Duration) {
}
func (f *FakeControllerWithError) Add(_ IPRule) error {
	return nil
}
func (f *FakeControllerWithError) AddWithMetadata(_ IPRule, _ string) error {
	return nil
}
func (f *FakeControllerWithError) Delete(_ IPRule) error {
	return nil
}
func (f *FakeControllerWithError) DeleteWithMetadata(_ string) error {
	return fmt.Errorf("fake delete metadata error")
}
func (f *FakeControllerWithError) OwnPriority(_ int) error {
	return nil
}
func (f *FakeControllerWithError) AddNodeIPFwMarkRule(_ int) error {
	return nil
}
