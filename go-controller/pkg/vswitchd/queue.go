// Code generated by "libovsdb.modelgen"
// DO NOT EDIT.

package vswitchd

import "github.com/ovn-kubernetes/libovsdb/model"

const QueueTable = "Queue"

// Queue defines an object in Queue table
type Queue struct {
	UUID        string            `ovsdb:"_uuid"`
	DSCP        *int              `ovsdb:"dscp"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
	OtherConfig map[string]string `ovsdb:"other_config"`
}

func (a *Queue) GetUUID() string {
	return a.UUID
}

func (a *Queue) GetDSCP() *int {
	return a.DSCP
}

func copyQueueDSCP(a *int) *int {
	if a == nil {
		return nil
	}
	b := *a
	return &b
}

func equalQueueDSCP(a, b *int) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == b {
		return true
	}
	return *a == *b
}

func (a *Queue) GetExternalIDs() map[string]string {
	return a.ExternalIDs
}

func copyQueueExternalIDs(a map[string]string) map[string]string {
	if a == nil {
		return nil
	}
	b := make(map[string]string, len(a))
	for k, v := range a {
		b[k] = v
	}
	return b
}

func equalQueueExternalIDs(a, b map[string]string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || v != w {
			return false
		}
	}
	return true
}

func (a *Queue) GetOtherConfig() map[string]string {
	return a.OtherConfig
}

func copyQueueOtherConfig(a map[string]string) map[string]string {
	if a == nil {
		return nil
	}
	b := make(map[string]string, len(a))
	for k, v := range a {
		b[k] = v
	}
	return b
}

func equalQueueOtherConfig(a, b map[string]string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || v != w {
			return false
		}
	}
	return true
}

func (a *Queue) DeepCopyInto(b *Queue) {
	*b = *a
	b.DSCP = copyQueueDSCP(a.DSCP)
	b.ExternalIDs = copyQueueExternalIDs(a.ExternalIDs)
	b.OtherConfig = copyQueueOtherConfig(a.OtherConfig)
}

func (a *Queue) DeepCopy() *Queue {
	b := new(Queue)
	a.DeepCopyInto(b)
	return b
}

func (a *Queue) CloneModelInto(b model.Model) {
	c := b.(*Queue)
	a.DeepCopyInto(c)
}

func (a *Queue) CloneModel() model.Model {
	return a.DeepCopy()
}

func (a *Queue) Equals(b *Queue) bool {
	return a.UUID == b.UUID &&
		equalQueueDSCP(a.DSCP, b.DSCP) &&
		equalQueueExternalIDs(a.ExternalIDs, b.ExternalIDs) &&
		equalQueueOtherConfig(a.OtherConfig, b.OtherConfig)
}

func (a *Queue) EqualsModel(b model.Model) bool {
	c := b.(*Queue)
	return a.Equals(c)
}

var _ model.CloneableModel = &Queue{}
var _ model.ComparableModel = &Queue{}
