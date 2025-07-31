package client

import (
	"encoding/json"
	"testing"

	"github.com/ovn-kubernetes/libovsdb/cache"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var apiTestSchema = []byte(`{
    "name": "OVN_Northbound",
    "version": "5.31.0",
    "cksum": "2352750632 28701",
    "tables": {
        "Logical_Switch": {
            "columns": {
                "name": {"type": "string"},
                "ports": {"type": {"key": {"type": "uuid",
                                           "refTable": "Logical_Switch_Port",
                                           "refType": "strong"},
                                   "min": 0,
                                   "max": "unlimited"}},
                "acls": {"type": {"key": {"type": "uuid",
                                          "refTable": "ACL",
                                          "refType": "strong"},
                                  "min": 0,
                                  "max": "unlimited"}},
                "qos_rules": {"type": {"key": {"type": "uuid",
                                          "refTable": "QoS",
                                          "refType": "strong"},
                                  "min": 0,
                                  "max": "unlimited"}},
                "load_balancer": {"type": {"key": {"type": "uuid",
                                                  "refTable": "Load_Balancer",
                                                  "refType": "weak"},
                                           "min": 0,
                                           "max": "unlimited"}},
                "dns_records": {"type": {"key": {"type": "uuid",
                                         "refTable": "DNS",
                                         "refType": "weak"},
                                  "min": 0,
                                  "max": "unlimited"}},
                "other_config": {
                    "type": {"key": "string", "value": "string",
                             "min": 0, "max": "unlimited"}},
                "external_ids": {
                    "type": {"key": "string", "value": "string",
                             "min": 0, "max": "unlimited"}},
               "forwarding_groups": {
                    "type": {"key": {"type": "uuid",
                                     "refTable": "Forwarding_Group",
                                     "refType": "strong"},
                                     "min": 0, "max": "unlimited"}}},
            "isRoot": true},
        "Logical_Switch_Port": {
            "columns": {
                "name": {"type": "string"},
                "type": {"type": "string"},
                "options": {
                     "type": {"key": "string",
                              "value": "string",
                              "min": 0,
                              "max": "unlimited"}},
                "parent_name": {"type": {"key": "string", "min": 0, "max": 1}},
                "tag_request": {
                     "type": {"key": {"type": "integer",
                                      "minInteger": 0,
                                      "maxInteger": 4095},
                              "min": 0, "max": 1}},
                "tag": {
                     "type": {"key": {"type": "integer",
                                      "minInteger": 1,
                                      "maxInteger": 4095},
                              "min": 0, "max": 1}},
                "addresses": {"type": {"key": "string",
                                       "min": 0,
                                       "max": "unlimited"}},
                "dynamic_addresses": {"type": {"key": "string",
                                       "min": 0,
                                       "max": 1}},
                "port_security": {"type": {"key": "string",
                                           "min": 0,
                                           "max": "unlimited"}},
                "up": {"type": {"key": "boolean", "min": 0, "max": 1}},
                "enabled": {"type": {"key": "boolean", "min": 0, "max": 1}},
                "dhcpv4_options": {"type": {"key": {"type": "uuid",
                                            "refTable": "DHCP_Options",
                                            "refType": "weak"},
                                 "min": 0,
                                 "max": 1}},
                "dhcpv6_options": {"type": {"key": {"type": "uuid",
                                            "refTable": "DHCP_Options",
                                            "refType": "weak"},
                                 "min": 0,
                                 "max": 1}},
                "ha_chassis_group": {
                    "type": {"key": {"type": "uuid",
                                     "refTable": "HA_Chassis_Group",
                                     "refType": "strong"},
                             "min": 0,
                             "max": 1}},
                "external_ids": {
                    "type": {"key": "string", "value": "string",
                             "min": 0, "max": "unlimited"}}},
            "indexes": [["name"]],
            "isRoot": false},
  "Bridge": {
     "columns": {
       "name": {
         "type": "string",
         "mutable": false},
       "datapath_type": {
         "type": "string"},
       "datapath_version": {
         "type": "string"},
       "datapath_id": {
         "type": {"key": "string", "min": 0, "max": 1},
         "ephemeral": true},
       "stp_enable": {
         "type": "boolean"},
       "rstp_enable": {
         "type": "boolean"},
       "mcast_snooping_enable": {
         "type": "boolean"},
       "ports": {
         "type": {"key": {"type": "uuid",
                          "refTable": "Port"},
                  "min": 0, "max": "unlimited"}},
       "mirrors": {
         "type": {"key": {"type": "uuid",
                          "refTable": "Mirror"},
                  "min": 0, "max": "unlimited"}},
       "netflow": {
         "type": {"key": {"type": "uuid",
                          "refTable": "NetFlow"},
                  "min": 0, "max": 1}},
       "sflow": {
         "type": {"key": {"type": "uuid",
                          "refTable": "sFlow"},
                  "min": 0, "max": 1}},
       "ipfix": {
         "type": {"key": {"type": "uuid",
                          "refTable": "IPFIX"},
                  "min": 0, "max": 1}},
       "controller": {
         "type": {"key": {"type": "uuid",
                          "refTable": "Controller"},
                  "min": 0, "max": "unlimited"}},
       "protocols": {
         "type": {"key": {"type": "string",
           "enum": ["set", ["OpenFlow10",
                            "OpenFlow11",
                            "OpenFlow12",
                            "OpenFlow13",
                            "OpenFlow14",
                            "OpenFlow15"]]},
           "min": 0, "max": "unlimited"}},
       "fail_mode": {
         "type": {"key": {"type": "string",
                          "enum": ["set", ["standalone", "secure"]]},
                  "min": 0, "max": 1}},
       "status": {
         "type": {"key": "string", "value": "string",
                  "min": 0, "max": "unlimited"},
         "ephemeral": true},
       "rstp_status": {
         "type": {"key": "string", "value": "string",
                  "min": 0, "max": "unlimited"},
         "ephemeral": true},
       "other_config": {
         "type": {"key": "string", "value": "string",
                  "min": 0, "max": "unlimited"}},
       "external_ids": {
         "type": {"key": "string", "value": "string",
                  "min": 0, "max": "unlimited"}},
       "flood_vlans": {
         "type": {"key": {"type": "integer",
                          "minInteger": 0,
                          "maxInteger": 4095},
                  "min": 0, "max": 4096}},
       "flow_tables": {
         "type": {"key": {"type": "integer",
                          "minInteger": 0,
                          "maxInteger": 254},
                  "value": {"type": "uuid",
                            "refTable": "Flow_Table"},
                  "min": 0, "max": "unlimited"}},
       "auto_attach": {
         "type": {"key": {"type": "uuid",
                          "refTable": "AutoAttach"},
                  "min": 0, "max": 1}}},
     "indexes": [["name"]]}
	}
    }`)

type testLogicalSwitch struct {
	UUID             string            `ovsdb:"_uuid"`
	Ports            []string          `ovsdb:"ports"`
	ExternalIDs      map[string]string `ovsdb:"external_ids"`
	Name             string            `ovsdb:"name"`
	QosRules         []string          `ovsdb:"qos_rules"`
	LoadBalancer     []string          `ovsdb:"load_balancer"`
	DNSRecords       []string          `ovsdb:"dns_records"`
	OtherConfig      map[string]string `ovsdb:"other_config"`
	ForwardingGroups []string          `ovsdb:"forwarding_groups"`
	Acls             []string          `ovsdb:"acls"`
}

// Table returns the table name. It's part of the Model interface
func (*testLogicalSwitch) Table() string {
	return "Logical_Switch"
}

// LogicalSwitchPort struct defines an object in Logical_Switch_Port table
type testLogicalSwitchPort struct {
	UUID             string            `ovsdb:"_uuid"`
	Up               *bool             `ovsdb:"up"`
	Dhcpv4Options    *string           `ovsdb:"dhcpv4_options"`
	Name             string            `ovsdb:"name"`
	DynamicAddresses *string           `ovsdb:"dynamic_addresses"`
	HaChassisGroup   *string           `ovsdb:"ha_chassis_group"`
	Options          map[string]string `ovsdb:"options"`
	Enabled          *bool             `ovsdb:"enabled"`
	Addresses        []string          `ovsdb:"addresses"`
	Dhcpv6Options    *string           `ovsdb:"dhcpv6_options"`
	TagRequest       *int              `ovsdb:"tag_request"`
	Tag              *int              `ovsdb:"tag"`
	PortSecurity     []string          `ovsdb:"port_security"`
	ExternalIDs      map[string]string `ovsdb:"external_ids"`
	Type             string            `ovsdb:"type"`
	ParentName       *string           `ovsdb:"parent_name"`
}

// Table returns the table name. It's part of the Model interface
func (*testLogicalSwitchPort) Table() string {
	return "Logical_Switch_Port"
}

// Bridge defines an object in Bridge table
type testBridge struct {
	UUID                string            `ovsdb:"_uuid"`
	AutoAttach          *string           `ovsdb:"auto_attach"`
	Controller          []string          `ovsdb:"controller"`
	DatapathID          *string           `ovsdb:"datapath_id"`
	DatapathType        string            `ovsdb:"datapath_type"`
	DatapathVersion     string            `ovsdb:"datapath_version"`
	ExternalIDs         map[string]string `ovsdb:"external_ids"`
	FailMode            *string           `ovsdb:"fail_mode"`
	FloodVLANs          []int             `ovsdb:"flood_vlans"`
	FlowTables          map[int]string    `ovsdb:"flow_tables"`
	IPFIX               *string           `ovsdb:"ipfix"`
	McastSnoopingEnable bool              `ovsdb:"mcast_snooping_enable"`
	Mirrors             []string          `ovsdb:"mirrors"`
	Name                string            `ovsdb:"name"`
	Netflow             *string           `ovsdb:"netflow"`
	OtherConfig         map[string]string `ovsdb:"other_config"`
	Ports               []string          `ovsdb:"ports"`
	Protocols           []string          `ovsdb:"protocols"`
	RSTPEnable          bool              `ovsdb:"rstp_enable"`
	RSTPStatus          map[string]string `ovsdb:"rstp_status"`
	Sflow               *string           `ovsdb:"sflow"`
	Status              map[string]string `ovsdb:"status"`
	STPEnable           bool              `ovsdb:"stp_enable"`
}

func apiTestCache(t testing.TB, data map[string]map[string]model.Model) *cache.TableCache {
	var schema ovsdb.DatabaseSchema
	err := json.Unmarshal(apiTestSchema, &schema)
	require.NoError(t, err)
	db, err := model.NewClientDBModel("OVN_Northbound", map[string]model.Model{
		"Logical_Switch":      &testLogicalSwitch{},
		"Logical_Switch_Port": &testLogicalSwitchPort{},
		"Bridge":              &testBridge{},
	})
	require.NoError(t, err)
	dbModel, errs := model.NewDatabaseModel(schema, db)
	assert.Empty(t, errs)
	cache, err := cache.NewTableCache(dbModel, data, nil)
	require.NoError(t, err)
	return cache
}
