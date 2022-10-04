package libovsdbops

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

// GetACLName returns the ACL name if it has one otherwise returns
// an empty string.
func GetACLName(acl *nbdb.ACL) string {
	if acl.Name != nil {
		return *acl.Name
	}
	return ""
}

// isEquivalentACL if it has same uuid, or if it has same name
// and external ids, or if it has same priority, direction, match
// and action.
func isEquivalentACL(existing *nbdb.ACL, searched *nbdb.ACL) bool {
	if searched.UUID != "" && existing.UUID == searched.UUID {
		return true
	}

	return existing.Priority == searched.Priority &&
		existing.Direction == searched.Direction &&
		existing.Match == searched.Match &&
		existing.Action == searched.Action &&
		reflect.DeepEqual(existing.ExternalIDs, searched.ExternalIDs)
}

type aclPredicate func(*nbdb.ACL) bool

// FindACLsWithPredicate looks up ACLs from the cache based on a given predicate
func FindACLsWithPredicate(nbClient libovsdbclient.Client, p aclPredicate) ([]*nbdb.ACL, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	acls := []*nbdb.ACL{}
	err := nbClient.WhereCache(p).List(ctx, &acls)
	return acls, err
}

// BuildACL builds an ACL with empty optional properties unset
func BuildACL(name string, direction nbdb.ACLDirection, priority int, match string, action nbdb.ACLAction, meter string,
	severity nbdb.ACLSeverity, log bool, externalIds map[string]string, options map[string]string) *nbdb.ACL {
	name = fmt.Sprintf("%.63s", name)

	var realName *string
	var realMeter *string
	var realSeverity *string
	if len(name) != 0 {
		realName = &name
	}
	if len(meter) != 0 {
		realMeter = &meter
	}
	if len(severity) != 0 {
		realSeverity = &severity
	}
	acl := &nbdb.ACL{
		Name:        realName,
		Direction:   direction,
		Match:       match,
		Action:      action,
		Priority:    priority,
		Severity:    realSeverity,
		Log:         log,
		Meter:       realMeter,
		ExternalIDs: externalIds,
		Options:     options,
	}

	return acl
}

func SetACLLogging(acl *nbdb.ACL, severity nbdb.ACLSeverity, log bool) {
	var realSeverity *string
	if len(severity) != 0 {
		realSeverity = &severity
	}
	acl.Severity = realSeverity
	acl.Log = log
}

// CreateOrUpdateACLsOps creates or updates the provided ACLs returning the
// corresponding ops
func CreateOrUpdateACLsOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, acls ...*nbdb.ACL) ([]libovsdb.Operation, error) {
	opModels := make([]operationModel, 0, len(acls))
	for i := range acls {
		// can't use i in the predicate, for loop replaces it in-memory
		acl := acls[i]
		opModel := operationModel{
			Model:          acl,
			ModelPredicate: func(item *nbdb.ACL) bool { return isEquivalentACL(item, acl) },
			OnModelUpdates: onModelUpdatesAllNonDefault(),
			ErrNotFound:    false,
			BulkOp:         false,
		}
		opModels = append(opModels, opModel)
	}

	modelClient := newModelClient(nbClient)
	return modelClient.CreateOrUpdateOps(ops, opModels...)
}

// CreateOrUpdateACLs creates or updates the provided ACLs
func CreateOrUpdateACLs(nbClient libovsdbclient.Client, acls ...*nbdb.ACL) error {
	ops, err := CreateOrUpdateACLsOps(nbClient, nil, acls...)
	if err != nil {
		return err
	}

	_, err = TransactAndCheckAndSetUUIDs(nbClient, acls, ops)
	return err
}

// UpdateACLsLoggingOps updates the log and severity on the provided ACLs and
// returns the corresponding ops
func UpdateACLsLoggingOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, acls ...*nbdb.ACL) ([]libovsdb.Operation, error) {
	opModels := make([]operationModel, 0, len(acls))
	for i := range acls {
		// can't use i in the predicate, for loop replaces it in-memory
		acl := acls[i]
		opModel := operationModel{
			Model:          acl,
			ModelPredicate: func(item *nbdb.ACL) bool { return isEquivalentACL(item, acl) },
			OnModelUpdates: []interface{}{&acl.Severity, &acl.Log},
			ErrNotFound:    true,
			BulkOp:         false,
		}
		opModels = append(opModels, opModel)
	}

	modelClient := newModelClient(nbClient)
	return modelClient.CreateOrUpdateOps(ops, opModels...)
}

// DeleteACLsOps deletes the provided ACLs and returns the corresponding ops
// portGroupNames and switchPred reminds to delete ACL references for port groups or switches
// in case port group or switch is not completely deleted
func DeleteACLsOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation,
	portGroupNames []string, switchPred switchPredicate, acls ...*nbdb.ACL) ([]libovsdb.Operation, error) {
	var err error
	for _, portGroupName := range portGroupNames {
		ops, err = DeleteACLsFromPortGroupOps(nbClient, ops, portGroupName, acls...)
		if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
			return ops, fmt.Errorf("deleting ACLs from port group %s failed: %v", portGroupName, err)
		}
	}
	if switchPred != nil {
		ops, err = RemoveACLsFromLogicalSwitchesWithPredicateOps(nbClient, ops, switchPred, acls...)
		if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
			return ops, fmt.Errorf("deleting ACLs from switch with predicate failed: %v", err)
		}
	}
	opModels := make([]operationModel, 0, len(acls))
	for i := range acls {
		// can't use i in the predicate, for loop replaces it in-memory
		acl := acls[i]
		opModel := operationModel{
			Model:          acl,
			ModelPredicate: func(item *nbdb.ACL) bool { return isEquivalentACL(item, acl) },
			ErrNotFound:    false,
			BulkOp:         true,
		}
		opModels = append(opModels, opModel)
	}

	modelClient := newModelClient(nbClient)
	return modelClient.DeleteOps(ops, opModels...)
}

// DeleteACLs deletes the provided ACLs
// portGroupNames and switchPred reminds to delete ACL references for port groups or switches
// in case port group or switch is not completely deleted
func DeleteACLs(nbClient libovsdbclient.Client, portGroupNames []string, switchPred switchPredicate, acls ...*nbdb.ACL) error {
	ops, err := DeleteACLsOps(nbClient, nil, portGroupNames, switchPred, acls...)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(nbClient, ops)
	return err
}
