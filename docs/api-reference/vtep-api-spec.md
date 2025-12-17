# API Reference

## Packages
- [k8s.ovn.org/v1](#k8sovnorgv1)


## k8s.ovn.org/v1

Package v1 contains API Schema definitions for the network v1 API group

### Resource Types
- [VTEP](#vtep)
- [VTEPList](#vteplist)



#### CIDR

_Underlying type:_ _string_

CIDR represents a CIDR notation IP range.

_Validation:_
- MaxLength: 43

_Appears in:_
- [DualStackCIDRs](#dualstackcidrs)



#### DualStackCIDRs

_Underlying type:_ _[CIDR](#cidr)_

DualStackCIDRs is a list of CIDRs that supports dual-stack (IPv4 and IPv6).

_Validation:_
- MaxItems: 2
- MaxLength: 43
- MinItems: 1

_Appears in:_
- [VTEPSpec](#vtepspec)



#### VTEP



VTEP defines VTEP (VXLAN Tunnel Endpoint) IP configuration for EVPN.



_Appears in:_
- [VTEPList](#vteplist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `k8s.ovn.org/v1` | | |
| `kind` _string_ | `VTEP` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[VTEPSpec](#vtepspec)_ | Spec defines the desired VTEP configuration. |  | Required: \{\} <br /> |
| `status` _[VTEPStatus](#vtepstatus)_ | Status contains the observed state of the VTEP. |  |  |


#### VTEPList



VTEPList contains a list of VTEP.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `k8s.ovn.org/v1` | | |
| `kind` _string_ | `VTEPList` | | |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[VTEP](#vtep) array_ |  |  |  |


#### VTEPMode

_Underlying type:_ _string_

VTEPMode defines the mode of VTEP IP allocation.

_Validation:_
- Enum: [Managed Unmanaged]

_Appears in:_
- [VTEPSpec](#vtepspec)

| Field | Description |
| --- | --- |
| `Managed` | VTEPModeManaged means OVN-Kubernetes allocates and assigns VTEP IPs per node automatically.<br /> |
| `Unmanaged` | VTEPModeUnmanaged means an external provider handles IP assignment;<br />OVN-Kubernetes discovers existing IPs on nodes.<br /> |


#### VTEPSpec



VTEPSpec defines the desired state of VTEP.



_Appears in:_
- [VTEP](#vtep)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `cidrs` _[DualStackCIDRs](#dualstackcidrs)_ | CIDRs is the list of IP ranges from which VTEP IPs are allocated.<br />Dual-stack clusters may set 2 CIDRs (one for each IP family), otherwise only 1 CIDR is allowed.<br />The format should match standard CIDR notation (for example, "100.64.0.0/24" or "fd00::/64"). |  | MaxItems: 2 <br />MaxLength: 43 <br />MinItems: 1 <br />Required: \{\} <br /> |
| `mode` _[VTEPMode](#vtepmode)_ | Mode specifies how VTEP IPs are managed.<br />"Managed" means OVN-Kubernetes allocates and assigns VTEP IPs per node automatically.<br />"Unmanaged" means an external provider handles IP assignment; OVN-Kubernetes discovers existing IPs on nodes.<br />Defaults to "Managed". | Managed | Enum: [Managed Unmanaged] <br /> |


#### VTEPStatus



VTEPStatus contains the observed state of the VTEP.



_Appears in:_
- [VTEP](#vtep)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#condition-v1-meta) array_ | Conditions slice of condition objects indicating details about VTEP status. |  |  |


