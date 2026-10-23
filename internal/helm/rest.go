package helm

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

type SimpleRESTClientGetter struct {
	ClientConfig    clientcmd.ClientConfig
	RESTConfig      *rest.Config
	CachedDiscovery discovery.CachedDiscoveryInterface
	RESTMapper      meta.RESTMapper
}

// NewRESTClientGetter returns a RESTClientGetter for the cluster of the rest.Config.
// If restMapper is nil, a RESTMapper backed by the discovery of the cluster is used.
func NewRESTClientGetter(restConfig *rest.Config, restMapper meta.RESTMapper) (*SimpleRESTClientGetter, error) {
	k8s, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to get k8s clientset: %w", err)
	}

	cache := memory.NewMemCacheClient(k8s.Discovery())

	if restMapper == nil {
		restMapper = restmapper.NewDeferredDiscoveryRESTMapper(cache)
	}
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{})

	return &SimpleRESTClientGetter{
		ClientConfig:    kubeConfig,
		RESTConfig:      restConfig,
		CachedDiscovery: cache,
		RESTMapper:      restMapper,
	}, nil
}

func (s *SimpleRESTClientGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	return s.ClientConfig
}

func (s *SimpleRESTClientGetter) ToRESTConfig() (*rest.Config, error) {
	return s.RESTConfig, nil
}

func (s *SimpleRESTClientGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	return s.CachedDiscovery, nil
}

func (s *SimpleRESTClientGetter) ToRESTMapper() (meta.RESTMapper, error) {
	return s.RESTMapper, nil
}
