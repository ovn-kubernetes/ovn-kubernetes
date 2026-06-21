# Configure OpenvSwitch Bridge

Configure OpenvSwitch Bridge

##  Netplan (Debian/Ubuntu)
Here is example netplan config which configures OpenvSwitch bridge
```
network:
  version: 2
  ethernets:
    enp0s3:
      dhcp4: no
  bridges:
    brext:
      dhcp4: no
      addresses:
      - 192.168.122.11/24
      gateway4: 192.168.122.1
      nameservers:
        addresses:
        - 8.8.8.8
      interfaces:
      - enp0s3
      openvswitch: {}
```
Run `netplan try` or `netplan apply` to apply the config
