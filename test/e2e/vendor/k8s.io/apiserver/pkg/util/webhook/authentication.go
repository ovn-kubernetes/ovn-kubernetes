/*
Copyright 2017 The Kubernetes Authors.

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

package webhook

import (
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"

	corev1 "k8s.io/api/core/v1"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apiserver/pkg/features"
	egressselector "k8s.io/apiserver/pkg/server/egressselector"
	"k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	tracing "k8s.io/component-base/tracing"
)

// AuthenticationInfoResolverWrapper can be used to inject Dial function to the
// rest.Config generated by the resolver.
type AuthenticationInfoResolverWrapper func(AuthenticationInfoResolver) AuthenticationInfoResolver

// NewDefaultAuthenticationInfoResolverWrapper builds a default authn resolver wrapper
func NewDefaultAuthenticationInfoResolverWrapper(
	proxyTransport *http.Transport,
	egressSelector *egressselector.EgressSelector,
	kubeapiserverClientConfig *rest.Config,
	tp trace.TracerProvider) AuthenticationInfoResolverWrapper {

	webhookAuthResolverWrapper := func(delegate AuthenticationInfoResolver) AuthenticationInfoResolver {
		return &AuthenticationInfoResolverDelegator{
			ClientConfigForFunc: func(hostPort string) (*rest.Config, error) {
				if hostPort == "kubernetes.default.svc:443" {
					return kubeapiserverClientConfig, nil
				}
				ret, err := delegate.ClientConfigFor(hostPort)
				if err != nil {
					return nil, err
				}
				if feature.DefaultFeatureGate.Enabled(features.APIServerTracing) {
					ret.Wrap(tracing.WrapperFor(tp))
				}

				if egressSelector != nil {
					networkContext := egressselector.ControlPlane.AsNetworkContext()
					var egressDialer utilnet.DialFunc
					egressDialer, err = egressSelector.Lookup(networkContext)

					if err != nil {
						return nil, err
					}

					ret.Dial = egressDialer
				}
				return ret, nil
			},
			ClientConfigForServiceFunc: func(serviceName, serviceNamespace string, servicePort int) (*rest.Config, error) {
				if serviceName == "kubernetes" && serviceNamespace == corev1.NamespaceDefault && servicePort == 443 {
					return kubeapiserverClientConfig, nil
				}
				ret, err := delegate.ClientConfigForService(serviceName, serviceNamespace, servicePort)
				if err != nil {
					return nil, err
				}
				if feature.DefaultFeatureGate.Enabled(features.APIServerTracing) {
					ret.Wrap(tracing.WrapperFor(tp))
				}

				if egressSelector != nil {
					networkContext := egressselector.Cluster.AsNetworkContext()
					var egressDialer utilnet.DialFunc
					egressDialer, err = egressSelector.Lookup(networkContext)
					if err != nil {
						return nil, err
					}

					ret.Dial = egressDialer
				} else if proxyTransport != nil && proxyTransport.DialContext != nil {
					ret.Dial = proxyTransport.DialContext
				}
				return ret, nil
			},
		}
	}
	return webhookAuthResolverWrapper
}

// AuthenticationInfoResolver builds rest.Config base on the server or service
// name and service namespace.
type AuthenticationInfoResolver interface {
	// ClientConfigFor builds rest.Config based on the hostPort.
	ClientConfigFor(hostPort string) (*rest.Config, error)
	// ClientConfigForService builds rest.Config based on the serviceName and
	// serviceNamespace.
	ClientConfigForService(serviceName, serviceNamespace string, servicePort int) (*rest.Config, error)
}

// AuthenticationInfoResolverDelegator implements AuthenticationInfoResolver.
type AuthenticationInfoResolverDelegator struct {
	ClientConfigForFunc        func(hostPort string) (*rest.Config, error)
	ClientConfigForServiceFunc func(serviceName, serviceNamespace string, servicePort int) (*rest.Config, error)
}

// ClientConfigFor returns client config for given hostPort.
func (a *AuthenticationInfoResolverDelegator) ClientConfigFor(hostPort string) (*rest.Config, error) {
	return a.ClientConfigForFunc(hostPort)
}

// ClientConfigForService returns client config for given service.
func (a *AuthenticationInfoResolverDelegator) ClientConfigForService(serviceName, serviceNamespace string, servicePort int) (*rest.Config, error) {
	return a.ClientConfigForServiceFunc(serviceName, serviceNamespace, servicePort)
}

type defaultAuthenticationInfoResolver struct {
	kubeconfig clientcmdapi.Config
}

// NewDefaultAuthenticationInfoResolver generates an AuthenticationInfoResolver
// that builds rest.Config based on the kubeconfig file. kubeconfigFile is the
// path to the kubeconfig.
func NewDefaultAuthenticationInfoResolver(kubeconfigFile string) (AuthenticationInfoResolver, error) {
	if len(kubeconfigFile) == 0 {
		return &defaultAuthenticationInfoResolver{}, nil
	}

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = kubeconfigFile
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{})
	clientConfig, err := loader.RawConfig()
	if err != nil {
		return nil, err
	}

	return &defaultAuthenticationInfoResolver{kubeconfig: clientConfig}, nil
}

func (c *defaultAuthenticationInfoResolver) ClientConfigFor(hostPort string) (*rest.Config, error) {
	return c.clientConfig(hostPort)
}

func (c *defaultAuthenticationInfoResolver) ClientConfigForService(serviceName, serviceNamespace string, servicePort int) (*rest.Config, error) {
	return c.clientConfig(net.JoinHostPort(serviceName+"."+serviceNamespace+".svc", strconv.Itoa(servicePort)))
}

func (c *defaultAuthenticationInfoResolver) clientConfig(target string) (*rest.Config, error) {
	// exact match
	if authConfig, ok := c.kubeconfig.AuthInfos[target]; ok {
		return restConfigFromKubeconfig(authConfig)
	}

	// star prefixed match
	serverSteps := strings.Split(target, ".")
	for i := 1; i < len(serverSteps); i++ {
		nickName := "*." + strings.Join(serverSteps[i:], ".")
		if authConfig, ok := c.kubeconfig.AuthInfos[nickName]; ok {
			return restConfigFromKubeconfig(authConfig)
		}
	}

	// If target included the default https port (443), search again without the port
	if target, port, err := net.SplitHostPort(target); err == nil && port == "443" {
		// exact match without port
		if authConfig, ok := c.kubeconfig.AuthInfos[target]; ok {
			return restConfigFromKubeconfig(authConfig)
		}

		// star prefixed match without port
		serverSteps := strings.Split(target, ".")
		for i := 1; i < len(serverSteps); i++ {
			nickName := "*." + strings.Join(serverSteps[i:], ".")
			if authConfig, ok := c.kubeconfig.AuthInfos[nickName]; ok {
				return restConfigFromKubeconfig(authConfig)
			}
		}
	}

	// if we're trying to hit the kube-apiserver and there wasn't an explicit config, use the in-cluster config
	if target == "kubernetes.default.svc:443" {
		// if we can find an in-cluster-config use that.  If we can't, fall through.
		inClusterConfig, err := rest.InClusterConfig()
		if err == nil {
			return setGlobalDefaults(inClusterConfig), nil
		}
	}

	// star (default) match
	if authConfig, ok := c.kubeconfig.AuthInfos["*"]; ok {
		return restConfigFromKubeconfig(authConfig)
	}

	// use the current context from the kubeconfig if possible
	if len(c.kubeconfig.CurrentContext) > 0 {
		if currContext, ok := c.kubeconfig.Contexts[c.kubeconfig.CurrentContext]; ok {
			if len(currContext.AuthInfo) > 0 {
				if currAuth, ok := c.kubeconfig.AuthInfos[currContext.AuthInfo]; ok {
					return restConfigFromKubeconfig(currAuth)
				}
			}
		}
	}

	// anonymous
	return setGlobalDefaults(&rest.Config{}), nil
}

func restConfigFromKubeconfig(configAuthInfo *clientcmdapi.AuthInfo) (*rest.Config, error) {
	config := &rest.Config{}

	// blindly overwrite existing values based on precedence
	if len(configAuthInfo.Token) > 0 {
		config.BearerToken = configAuthInfo.Token
		config.BearerTokenFile = configAuthInfo.TokenFile
	} else if len(configAuthInfo.TokenFile) > 0 {
		tokenBytes, err := ioutil.ReadFile(configAuthInfo.TokenFile)
		if err != nil {
			return nil, err
		}
		config.BearerToken = string(tokenBytes)
		config.BearerTokenFile = configAuthInfo.TokenFile
	}
	if len(configAuthInfo.Impersonate) > 0 {
		config.Impersonate = rest.ImpersonationConfig{
			UserName: configAuthInfo.Impersonate,
			Groups:   configAuthInfo.ImpersonateGroups,
			Extra:    configAuthInfo.ImpersonateUserExtra,
		}
	}
	if len(configAuthInfo.ClientCertificate) > 0 || len(configAuthInfo.ClientCertificateData) > 0 {
		config.CertFile = configAuthInfo.ClientCertificate
		config.CertData = configAuthInfo.ClientCertificateData
		config.KeyFile = configAuthInfo.ClientKey
		config.KeyData = configAuthInfo.ClientKeyData
	}
	if len(configAuthInfo.Username) > 0 || len(configAuthInfo.Password) > 0 {
		config.Username = configAuthInfo.Username
		config.Password = configAuthInfo.Password
	}
	if configAuthInfo.Exec != nil {
		config.ExecProvider = configAuthInfo.Exec.DeepCopy()
	}
	if configAuthInfo.AuthProvider != nil {
		return nil, fmt.Errorf("auth provider not supported")
	}

	return setGlobalDefaults(config), nil
}

func setGlobalDefaults(config *rest.Config) *rest.Config {
	config.UserAgent = "kube-apiserver-admission"
	config.Timeout = 30 * time.Second

	return config
}
