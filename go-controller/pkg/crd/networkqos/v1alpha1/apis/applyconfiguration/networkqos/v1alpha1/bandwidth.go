/*


Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
// Code generated by applyconfiguration-gen. DO NOT EDIT.

package v1alpha1

// BandwidthApplyConfiguration represents a declarative configuration of the Bandwidth type for use
// with apply.
type BandwidthApplyConfiguration struct {
	Rate  *uint32 `json:"rate,omitempty"`
	Burst *uint32 `json:"burst,omitempty"`
}

// BandwidthApplyConfiguration constructs a declarative configuration of the Bandwidth type for use with
// apply.
func Bandwidth() *BandwidthApplyConfiguration {
	return &BandwidthApplyConfiguration{}
}

// WithRate sets the Rate field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the Rate field is set to the value of the last call.
func (b *BandwidthApplyConfiguration) WithRate(value uint32) *BandwidthApplyConfiguration {
	b.Rate = &value
	return b
}

// WithBurst sets the Burst field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the Burst field is set to the value of the last call.
func (b *BandwidthApplyConfiguration) WithBurst(value uint32) *BandwidthApplyConfiguration {
	b.Burst = &value
	return b
}
