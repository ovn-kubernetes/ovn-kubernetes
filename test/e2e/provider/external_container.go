package provider

import (
	"errors"
	"fmt"
	"strings"
)

type ExternalContainer struct {
	Name    string
	Image   string
	Network Network
	Args    []string
	ExtPort uint16
	ipv4    string
	ipv6    string
}

func (ec ExternalContainer) GetName() string {
	return ec.Name
}

func (ec ExternalContainer) GetIPv4() string {
	return ec.ipv4
}

func (ec ExternalContainer) GetIPv6() string {
	return ec.ipv6
}

func (ec ExternalContainer) GetPort() string {
	if ec.ExtPort == 0 {
		panic("port isn't defined")
	}
	return fmt.Sprintf("%d", ec.ExtPort)
}

func (ec ExternalContainer) IsIPv4() bool {
	return ec.ipv4 != ""
}

func (ec ExternalContainer) IsIPv6() bool {
	return ec.ipv6 != ""
}

func (ec ExternalContainer) String() string {
	str := fmt.Sprintf("Name: %q, Image: %q, Network: %q, Command: %q", ec.Name, ec.Image, ec.Network, strings.Join(ec.Args, " "))
	if ec.IsIPv4() {
		str = fmt.Sprintf("%s, IPv4 address: %q", str, ec.GetIPv4())
	}
	if ec.IsIPv6() {
		str = fmt.Sprintf("%s, IPv6 address: %s", str, ec.GetIPv6())
	}
	return str
}

func (ec ExternalContainer) isValidPreCreateContainer() (bool, error) {
	var errs []error
	if ec.Name == "" {
		errs = append(errs, errors.New("name is not set"))
	}
	if ec.Image == "" {
		errs = append(errs, errors.New("image is not set"))
	}
	if ec.Network.String() == "" {
		errs = append(errs, errors.New("network is not set"))
	}
	if ec.ExtPort == 0 {
		errs = append(errs, errors.New("port is not set"))
	}
	if len(errs) == 0 {
		return true, nil
	}
	if doesNetworkExist(ec.Network.Name) {
		errs = append(errs, errors.New("network does not exist"))
	}
	return false, condenseErrors(errs)
}

func (ec ExternalContainer) isValidPostCreate() (bool, error) {
	var errs []error
	if ec.ipv4 == "" && ec.ipv6 == "" {
		errs = append(errs, errors.New("provider did not populate an IPv4 or an IPv6 address"))
	}
	if len(errs) == 0 {
		return true, nil
	}
	return false, condenseErrors(errs)
}

func (ec ExternalContainer) isValidPreDelete() (bool, error) {
	if ec.ipv4 == "" && ec.ipv6 == "" {
		return false, fmt.Errorf("IPv4 or IPv6 must be set")
	}
	return true, nil
}

func condenseErrors(errs []error) error {
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	}
	err := errs[0]
	for _, e := range errs[1:] {
		err = errors.Join(err, e)
	}
	return err
}
