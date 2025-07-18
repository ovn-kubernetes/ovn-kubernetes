package config

import "fmt"

type ValidationErrorType string

const (
	ErrCIDRNotProperlyFormatted         ValidationErrorType = "CIDRNotProperlyFormatted"
	ErrInvalidCIDRAddress               ValidationErrorType = "InvalidCIDRAddress"
	ErrHostSubnetMask                   ValidationErrorType = "HostSubnetMask"
	ErrInvalidIPv4HostSubnet            ValidationErrorType = "InvalidIPv4HostSubnet"
	ErrSubnetOverlap                    ValidationErrorType = "SubnetOverlap"
	ErrExcludedSubnetNotContained       ValidationErrorType = "ExcludedSubnetNotContained"
	ErrReservedSubnetNotContained       ValidationErrorType = "ReservedSubnetNotContained"
	ErrInfrastructureSubnetNotContained ValidationErrorType = "InfrastructureSubnetNotContained"
	ErrTopologyConfigMismatch           ValidationErrorType = "TopologyConfigMismatch"
	ErrIPAMLifecycleNotSupported        ValidationErrorType = "IPAMLifecycleNotSupported"
	ErrSubnetsRequired                  ValidationErrorType = "SubnetsRequired"
	ErrSubnetsMustBeUnset               ValidationErrorType = "SubnetsMustBeUnset"
)

type ValidationError struct {
	Type    ValidationErrorType
	Message string
}

func (e *ValidationError) Error() string {
	return e.Message
}

// CIDR Validation Errors
func NewCIDRNotProperlyFormattedError(cidr string) *ValidationError {
	return &ValidationError{
		Type:    ErrCIDRNotProperlyFormatted,
		Message: fmt.Sprintf("CIDR %q not properly formatted", cidr),
	}
}

func NewInvalidCIDRAddressError() *ValidationError {
	return &ValidationError{
		Type:    ErrInvalidCIDRAddress,
		Message: "invalid CIDR address",
	}
}

// Subnet Validation Errors
func NewHostSubnetMaskError(hostSubnetLength, clusterSubnetLength int) *ValidationError {
	return &ValidationError{
		Type: ErrHostSubnetMask,
		Message: fmt.Sprintf("cannot use a host subnet length mask shorter than or equal to the cluster subnet mask. "+
			"host subnet length: %d, cluster subnet length: %d", hostSubnetLength, clusterSubnetLength),
	}
}

func NewInvalidIPv4HostSubnetError() *ValidationError {
	return &ValidationError{
		Type:    ErrInvalidIPv4HostSubnet,
		Message: "invalid host subnet, IPv4 subnet must be < 32",
	}
}

func NewSubnetOverlapError(a, b ConfigSubnet) *ValidationError {
	return &ValidationError{
		Type: ErrSubnetOverlap,
		Message: fmt.Sprintf("%s %q overlaps %s %q",
			a.SubnetType, a.Subnet.String(),
			b.SubnetType, b.Subnet.String()),
	}
}

func NewExcludedSubnetNotContainedError(excludeSubnet interface{}) *ValidationError {
	return &ValidationError{
		Type:    ErrExcludedSubnetNotContained,
		Message: fmt.Sprintf("the provided network subnets do not contain excluded subnets %v", excludeSubnet),
	}
}

func NewReservedSubnetNotContainedError(reservedSubnet interface{}) *ValidationError {
	return &ValidationError{
		Type:    ErrReservedSubnetNotContained,
		Message: fmt.Sprintf("the provided network subnets do not contain reserved subnets %v", reservedSubnet),
	}
}

func NewInfrastructureSubnetNotContainedError(infrastructureSubnet interface{}) *ValidationError {
	return &ValidationError{
		Type:    ErrInfrastructureSubnetNotContained,
		Message: fmt.Sprintf("the provided network subnets do not contain infrastructure subnets %v", infrastructureSubnet),
	}
}

// Topology Validation Errors
func NewTopologyConfigMismatchError(topology string) *ValidationError {
	return &ValidationError{
		Type:    ErrTopologyConfigMismatch,
		Message: fmt.Sprintf("topology %[1]s is specified but %[1]s config is nil", topology),
	}
}

// IPAM Validation Errors
func NewIPAMLifecycleNotSupportedError() *ValidationError {
	return &ValidationError{
		Type:    ErrIPAMLifecycleNotSupported,
		Message: "lifecycle Persistent is only supported when ipam.mode is Enabled",
	}
}

func NewSubnetsRequiredError() *ValidationError {
	return &ValidationError{
		Type:    ErrSubnetsRequired,
		Message: "subnets is required with ipam.mode is Enabled or unset",
	}
}

func NewSubnetsMustBeUnsetError() *ValidationError {
	return &ValidationError{
		Type:    ErrSubnetsMustBeUnset,
		Message: "subnets must be unset when ipam.mode is Disabled",
	}
}
