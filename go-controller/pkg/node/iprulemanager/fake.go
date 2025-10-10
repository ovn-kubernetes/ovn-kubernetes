package iprulemanager

import (
	"fmt"
	"time"

	"github.com/vishvananda/netlink"
)

type FakeControllerWithError struct {
}

func (f *FakeControllerWithError) Run(_ <-chan struct{}, _ time.Duration) {
}
func (f *FakeControllerWithError) Add(_ netlink.Rule) error {
	return nil
}
func (f *FakeControllerWithError) AddWithMetadata(_ netlink.Rule, _ string) error {
	return nil
}
func (f *FakeControllerWithError) Delete(_ netlink.Rule) error {
	return nil
}
func (f *FakeControllerWithError) DeleteWithMetadata(_ string) error {
	return fmt.Errorf("fake delete metadata error")
}
func (f *FakeControllerWithError) OwnPriority(_ int) error {
	return nil
}
