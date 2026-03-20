# Gateway Accelerated Interface Configuration

## Description

To provide hardware acceleration for traffic, both IN and OUT ports need to be a hardware 
accelerated netdevice backed by the Network Interface Card hardware itself.
In case of external traffic, when one such port is the external OVS bridge, which for example has the gateway IP, 
such traffic (like host networking traffic) would not be accelerated.
Using Switchdev VirtualFunction (VF) or SubFunction (SF) as a gateway interface allows to accelerate these too.


## How it works?

Instead of using the gateway interface as the external bridge itself, use a  switchdev VF or SF instead. 
This is depicted as following:

```
                         +----------+
                         |  br-ext  |
                   +--------+       |
                   | UPLINK |       |
                   +--------+       |  patch  +----------+
                         |          x---------x  br-int  |
    +--------+     +--------+       |  port   +----------+
    | NETDEV +-----+   REP  |       |
    +--------+     +--------+       |
                         +----------+
```

Where `UPLINK` is a port on an offloading capable network interface hardware, `NETDEV` is a switchdev function 
of this port and `REP` is a representor netdevice of the switchdev function. 
Node/Host IP assigned to `NETDEV` which make OVS to chose `REP` port for external flows instead of the bridge.


## How to use?

Gateway accelerated interface can be used in two steps:

a) Creating and configuring the device. 
See figure above.
An `UPLINK` device is connected to the OVS external bridge. 
An existing VF or SF `NETDEV` from the `UPLINK` is first selected as the the Gateway Interface. Its associated 
representor `REP` is plugged into the OVS external bridge (br-ext). The gateway IP is assigned to this interface 
instead of the OVS external bridge (br-ext). 

b) Specify `NETDEV` as a gateway interface explicitly via `OVN_GATEWAY_OPTS` environment variable for
  ovnkube-node container. Example:

```yaml
            - name: OVN_GATEWAY_OPTS
              value: "--gateway-accelerated-interface=<<NETDEV>>"
```

Note that this is mutually exclusive to the `--gateway-interface` flag for GATEWAY_OPTIONS.

c) Set the external-id on the bridge to detect the uplink device correctly. This is useful for instances where,
the name of the bridge (eg: br-ext) does not use the uplink device (eg: p0) in its name. The uplink can also 
be a bond device. 
```bash
ovs-vsctl br-set-external-id br-ext bridge-uplink p0
```
This gives more flexibility in detecting the uplink device in cases where the auto detection fails (like in case of 
bonded uplinks etc.)

## Verification

Openflow rules added to the external bridge will use this port as the IN/OUT port instead.

Example flows when pf0vf1 is the netdev and pf0vf1_r is the representor
```bash
 cookie=0xdeff105, duration=505314.637s, table=0, n_packets=0, n_bytes=0, priority=500,ip,in_port="pf0vf1_r",nw_dst=169.254.0.1 actions=ct(table=5,zone=64002,nat)
 cookie=0xdeff105, duration=505314.637s, table=0, n_packets=655, n_bytes=129843, priority=500,ip,in_port="pf0vf1_r",nw_dst=10.96.0.0/16 actions=ct(commit,table=2,zone=64001,nat(src=169.254.0.2))
 cookie=0xdeff105, duration=505314.637s, table=0, n_packets=359877855, n_bytes=531033264511, priority=205,udp,in_port=p0,dl_dst=42:0b:9a:f1:83:b2,tp_dst=6081 actions=output:"pf0vf1_r"
 cookie=0xdeff105, duration=505314.637s, table=0, n_packets=6252796, n_bytes=775727815, priority=200,udp,in_port="pf0vf1_r",tp_dst=6081 actions=output:p0
 cookie=0xdeff105, duration=505314.637s, table=0, n_packets=1867752, n_bytes=294547557, priority=100,ip,in_port="pf0vf1_r" actions=ct(commit,zone=64000,exec(load:0x2->NXM_NX_CT_MARK[])),output:p0
 cookie=0xdeff105, duration=505314.637s, table=0, n_packets=22, n_bytes=1320, priority=10,in_port=p0,dl_dst=42:0b:9a:f1:83:b2 actions=output:"patch-brp0_c-23",output:"pf0vf1_r"
 cookie=0xdeff105, duration=505314.637s, table=1, n_packets=1313364, n_bytes=669490616, priority=100,ct_state=+est+trk,ct_mark=0x2,ip actions=output:"pf0vf1_r"
 cookie=0xdeff105, duration=505314.637s, table=1, n_packets=0, n_bytes=0, priority=100,ct_state=+rel+trk,ct_mark=0x2,ip actions=output:"pf0vf1_r"
 cookie=0xdeff105, duration=505314.637s, table=1, n_packets=0, n_bytes=0, priority=13,udp,in_port=p0,tp_dst=3784 actions=output:"patch-brp0_c-23",output:"pf0vf1_r"
 cookie=0xdeff105, duration=505314.637s, table=1, n_packets=493602, n_bytes=48384748, priority=10,dl_dst=42:0b:9a:f1:83:b2 actions=output:"pf0vf1_r"
 cookie=0xdeff105, duration=505314.637s, table=3, n_packets=694, n_bytes=276779, actions=move:NXM_OF_ETH_DST[]->NXM_OF_ETH_SRC[],mod_dl_dst:42:0b:9a:f1:83:b2,output:"pf0vf1_r"


```