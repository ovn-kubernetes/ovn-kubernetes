module github.com/ovn-org/ovn-kubernetes/go-controller

go 1.24.0

toolchain go1.24.5

require (
	github.com/Microsoft/hcsshim v0.9.6
	github.com/alexflint/go-filemutex v1.2.0
	github.com/asaskevich/govalidator v0.0.0-20210307081110-f21760c49a8d
	github.com/bhendo/go-powershell v0.0.0-20190719160123-219e7fb4e41e
	github.com/cenkalti/backoff/v4 v4.3.0
	github.com/containernetworking/cni v1.2.3
	github.com/containernetworking/plugins v1.2.0
	github.com/coreos/go-iptables v0.6.0
	github.com/fsnotify/fsnotify v1.9.0
	github.com/gaissmai/cidrtree v0.1.4
	github.com/go-logr/logr v1.4.3
	github.com/go-logr/stdr v1.2.2
	github.com/godbus/dbus/v5 v5.1.0
	github.com/google/go-cmp v0.7.0
	github.com/google/gopacket v1.1.19
	github.com/google/uuid v1.6.0
	github.com/gorilla/mux v1.8.0
	github.com/k8snetworkplumbingwg/govdpa v0.1.5-0.20230926073613-07c1031aea47
	github.com/k8snetworkplumbingwg/ipamclaims v0.5.1-alpha
	github.com/k8snetworkplumbingwg/multi-networkpolicy v1.0.1
	github.com/k8snetworkplumbingwg/network-attachment-definition-client v1.7.7
	github.com/k8snetworkplumbingwg/sriovnet v1.2.1-0.20250818105516-24ab680f94f3
	github.com/mdlayher/arp v0.0.0-20220512170110-6706a2966875
	github.com/mdlayher/ndp v1.0.1
	github.com/mdlayher/socket v0.2.1
	github.com/metallb/frr-k8s v0.0.21
	github.com/miekg/dns v1.1.31
	github.com/mitchellh/copystructure v1.2.0
	github.com/moby/sys/userns v0.1.0
	github.com/onsi/ginkgo/v2 v2.22.0
	github.com/onsi/gomega v1.36.1
	github.com/openshift/api v0.0.0-20251020135558-286504b695bc
	github.com/openshift/client-go v0.0.0-20251015124057-db0dee36e235
	github.com/ovn-kubernetes/libovsdb v0.8.1
	github.com/prometheus/client_golang v1.22.0
	github.com/prometheus/client_model v0.6.2
	github.com/safchain/ethtool v0.3.1-0.20231027162144-83e5e0097c91
	github.com/spf13/afero v1.14.0
	github.com/stretchr/testify v1.10.0
	github.com/urfave/cli/v2 v2.27.2
	github.com/vishvananda/netlink v1.3.1
	golang.org/x/exp v0.0.0-20240719175910-8a7402abbf56
	golang.org/x/net v0.46.0
	golang.org/x/sync v0.17.0
	golang.org/x/sys v0.37.0
	golang.org/x/time v0.11.0
	google.golang.org/grpc v1.72.1
	google.golang.org/grpc/security/advancedtls v0.0.0-20240425232638-1e8b9b7fc655
	google.golang.org/protobuf v1.36.10
	gopkg.in/fsnotify/fsnotify.v1 v1.4.7
	gopkg.in/gcfg.v1 v1.2.3
	gopkg.in/k8snetworkplumbingwg/multus-cni.v4 v4.0.2
	gopkg.in/natefinch/lumberjack.v2 v2.2.1
	k8s.io/api v0.34.1
	k8s.io/apimachinery v0.34.1
	k8s.io/client-go v0.34.1
	k8s.io/component-helpers v0.34.1
	k8s.io/klog/v2 v2.130.1
	k8s.io/kubelet v0.34.1
	k8s.io/kubernetes v1.34.1
	k8s.io/utils v0.0.0-20251002143259-bc988d571ff4
	kubevirt.io/api v1.0.0-alpha.0
	sigs.k8s.io/controller-runtime v0.22.1
	sigs.k8s.io/knftables v0.0.19
	sigs.k8s.io/network-policy-api v0.1.5
	sigs.k8s.io/structured-merge-diff/v6 v6.3.0
	sigs.k8s.io/yaml v1.6.0
)

require (
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/blang/semver/v4 v4.0.0 // indirect
	github.com/cenkalti/hub v1.0.2 // indirect
	github.com/cenkalti/rpc2 v1.0.4 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/containerd/cgroups v1.1.0 // indirect
	github.com/cpuguy83/go-md2man/v2 v2.0.6 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/emicklei/go-restful/v3 v3.12.2 // indirect
	github.com/evanphx/json-patch/v5 v5.9.11 // indirect
	github.com/fxamacker/cbor/v2 v2.9.0 // indirect
	github.com/gabriel-vasile/mimetype v1.4.8 // indirect
	github.com/go-jose/go-jose/v4 v4.0.4 // indirect
	github.com/go-openapi/jsonpointer v0.21.1 // indirect
	github.com/go-openapi/jsonreference v0.21.0 // indirect
	github.com/go-openapi/swag v0.23.1 // indirect
	github.com/go-playground/locales v0.14.1 // indirect
	github.com/go-playground/universal-translator v0.18.1 // indirect
	github.com/go-playground/validator/v10 v10.26.0 // indirect
	github.com/go-task/slim-sprig/v3 v3.0.0 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/golang/groupcache v0.0.0-20210331224755-41bb18bfe9da // indirect
	github.com/google/btree v1.1.3 // indirect
	github.com/google/gnostic-models v0.7.0 // indirect
	github.com/google/pprof v0.0.0-20241029153458-d1b30febd7db // indirect
	github.com/gorilla/websocket v1.5.4-0.20250319132907-e064f32e3674 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/josharian/intern v1.0.0 // indirect
	github.com/josharian/native v1.0.0 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/juju/errors v0.0.0-20200330140219-3fe23663418f // indirect
	github.com/juju/testing v0.0.0-20200706033705-4c23f9c453cd // indirect
	github.com/leodido/go-urn v1.4.0 // indirect
	github.com/mailru/easyjson v0.9.0 // indirect
	github.com/mdlayher/ethernet v0.0.0-20220221185849-529eae5b6118 // indirect
	github.com/mdlayher/packet v1.0.0 // indirect
	github.com/mitchellh/reflectwalk v1.0.2 // indirect
	github.com/moby/spdystream v0.5.0 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.3-0.20250322232337-35a7c28c31ee // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/mxk/go-flowrate v0.0.0-20140419014527-cca7078d478f // indirect
	github.com/openshift/custom-resource-status v1.1.2 // indirect
	github.com/pborman/uuid v1.2.0 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/prometheus/common v0.63.0 // indirect
	github.com/prometheus/procfs v0.16.1 // indirect
	github.com/russross/blackfriday/v2 v2.1.0 // indirect
	github.com/sirupsen/logrus v1.9.3 // indirect
	github.com/spf13/cobra v1.9.1 // indirect
	github.com/spf13/pflag v1.0.6 // indirect
	github.com/spiffe/go-spiffe/v2 v2.5.0 // indirect
	github.com/stretchr/objx v0.5.2 // indirect
	github.com/vishvananda/netns v0.0.5 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	github.com/xrash/smetrics v0.0.0-20240312152122-5f08fbb34913 // indirect
	github.com/zeebo/errs v1.4.0 // indirect
	go.opencensus.io v0.24.0 // indirect
	go.opentelemetry.io/otel v1.35.0 // indirect
	go.opentelemetry.io/otel/trace v1.35.0 // indirect
	go.yaml.in/yaml/v2 v2.4.2 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/crypto v0.43.0 // indirect
	golang.org/x/oauth2 v0.29.0 // indirect
	golang.org/x/term v0.36.0 // indirect
	golang.org/x/text v0.30.0 // indirect
	golang.org/x/tools v0.38.0 // indirect
	gomodules.xyz/jsonpatch/v2 v2.5.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251014184007-4626949a642f // indirect
	gopkg.in/evanphx/json-patch.v4 v4.12.0 // indirect
	gopkg.in/inf.v0 v0.9.1 // indirect
	gopkg.in/warnings.v0 v0.1.2 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	k8s.io/apiextensions-apiserver v0.34.1 // indirect
	k8s.io/apiserver v0.34.1 // indirect
	k8s.io/component-base v0.34.1 // indirect
	k8s.io/controller-manager v0.34.1 // indirect
	k8s.io/kube-openapi v0.0.0-20250910181357-589584f1c912 // indirect
	kubevirt.io/containerized-data-importer-api v1.55.0 // indirect
	kubevirt.io/controller-lifecycle-operator-sdk/api v0.0.0-20220329064328-f3cc58c6ed90 // indirect
	sigs.k8s.io/json v0.0.0-20250730193827-2d320260d730 // indirect
	sigs.k8s.io/randfill v1.0.0 // indirect
)

replace (
	github.com/Microsoft/hcsshim => github.com/Microsoft/hcsshim v0.8.20
	github.com/coreos/go-iptables => github.com/trozet/go-iptables v0.0.0-20240328221912-077e672b3808
	github.com/gogo/protobuf => github.com/gogo/protobuf v1.3.2
	github.com/j-keck/arping => github.com/JacobTanenbaum/arping v0.0.0-20240209152419-3987db83bd51
	// TEMPORARY: Use fork with VLAN filtering/VNI support until merged upstream
	// Adds: BridgeVlanAdd, BridgeVniAdd, BridgeVlanAddTunnelInfo, Vxlan.VniFilter
	// TODO: Remove once https://github.com/vishvananda/netlink/pull/1159 is merged
	github.com/vishvananda/netlink => github.com/kyrtapz/netlink v0.0.0-20260127093224-f8e95bf26303
)
