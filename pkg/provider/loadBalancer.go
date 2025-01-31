package provider

import (
	"context"
	"fmt"

	"github.com/kube-vip/kube-vip-cloud-provider/pkg/ipam"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	cloudprovider "k8s.io/cloud-provider"

	"k8s.io/klog"
)

//kubevipLoadBalancerManager -
type kubevipLoadBalancerManager struct {
	kubeClient     *kubernetes.Clientset
	nameSpace      string
	cloudConfigMap string
}

func newLoadBalancer(kubeClient *kubernetes.Clientset, ns, cm, serviceCidr string) cloudprovider.LoadBalancer {
	k := &kubevipLoadBalancerManager{
		kubeClient:     kubeClient,
		nameSpace:      ns,
		cloudConfigMap: cm,
	}
	return k
}

func (k *kubevipLoadBalancerManager) EnsureLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) (lbs *v1.LoadBalancerStatus, err error) {
	return k.syncLoadBalancer(ctx, service)
}
func (k *kubevipLoadBalancerManager) UpdateLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) (err error) {
	_, err = k.syncLoadBalancer(ctx, service)
	return err
}

func (k *kubevipLoadBalancerManager) EnsureLoadBalancerDeleted(ctx context.Context, clusterName string, service *v1.Service) error {
	return k.deleteLoadBalancer(ctx, service)
}

func (k *kubevipLoadBalancerManager) GetLoadBalancer(ctx context.Context, clusterName string, service *v1.Service) (status *v1.LoadBalancerStatus, exists bool, err error) {
	if service.Labels["implementation"] == "kube=vip" {
		return &service.Status.LoadBalancer, true, nil
	} else {
		return nil, false, nil
	}

}

// GetLoadBalancerName returns the name of the load balancer. Implementations must treat the
// *v1.Service parameter as read-only and not modify it.
func (k *kubevipLoadBalancerManager) GetLoadBalancerName(_ context.Context, clusterName string, service *v1.Service) string {
	return getDefaultLoadBalancerName(service)
}

func getDefaultLoadBalancerName(service *v1.Service) string {
	return cloudprovider.DefaultLoadBalancerName(service)
}

func (k *kubevipLoadBalancerManager) deleteLoadBalancer(ctx context.Context, service *v1.Service) error {
	klog.Infof("deleting service '%s' (%s)", service.Name, service.UID)

	return nil
}

// syncLoadBalancer
// 1. Is this loadBalancer already created, and does it have an address? return status
// 2. Is this a new loadBalancer (with no IP address)
// 2a. Get all existing kube-vip services
// 2b. Get the network configuration for this service (namespace) / (CIDR/Range)
// 2c. Between the two find a free address

func (k *kubevipLoadBalancerManager) syncLoadBalancer(ctx context.Context, service *v1.Service) (*v1.LoadBalancerStatus, error) {
	// This function reconciles the load balancer state
	klog.Infof("syncing service '%s' (%s)", service.Name, service.UID)

	// The loadBalancer address has already been populated
	if service.Spec.LoadBalancerIP != "" {
		return &service.Status.LoadBalancer, nil
	}

	// Get all services in this namespace, that have the correct label
	svcs, err := k.kubeClient.CoreV1().Services(service.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "implementation=kube-vip"})
	if err != nil {
		return &service.Status.LoadBalancer, err
	}

	// Get the clound controller configuration map
	controllerCM, err := k.GetConfigMap(ctx, KubeVipClientConfig, "kube-system")
	if err != nil {
		klog.Errorf("Unable to retrieve kube-vip ipam config from configMap [%s] in kube-system", KubeVipClientConfig)
		// TODO - determine best course of action, create one if it doesn't exist
		controllerCM, err = k.CreateConfigMap(ctx, KubeVipClientConfig, "kube-system")
		if err != nil {
			return nil, err
		}
	}

	var existingServiceIPS []string
	for x := range svcs.Items {
		existingServiceIPS = append(existingServiceIPS, svcs.Items[x].Labels["ipam-address"])
	}

  // If the LoadBalancer address is empty, then do a local IPAM lookup
	loadBalancerIP, err := discoverAddress(controllerCM, service.Namespace, k.cloudConfigMap, existingServiceIPS)

	if err != nil {
		return nil, err
	}

	// Update the services with this new address
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		recentService, getErr := k.kubeClient.CoreV1().Services(service.Namespace).Get(ctx, service.Name, metav1.GetOptions{})
		if getErr != nil {
			return getErr
		}

		klog.Infof("Updating service [%s], with load balancer IPAM address [%s]", service.Name, loadBalancerIP)

		if recentService.Labels == nil {
			// Just because ..
			recentService.Labels = make(map[string]string)
		}
		// Set Label for service lookups
		recentService.Labels["implementation"] = "kube-vip"
		recentService.Labels["ipam-address"] = loadBalancerIP

		// Set IPAM address to Load Balancer Service
		recentService.Spec.LoadBalancerIP = loadBalancerIP

		// Update the actual service with teh address and the labels
		_, updateErr := k.kubeClient.CoreV1().Services(recentService.Namespace).Update(ctx, recentService, metav1.UpdateOptions{})
		return updateErr
	})
	if retryErr != nil {
		return nil, fmt.Errorf("error updating Service Spec [%s] : %v", service.Name, err)
	}

	return &service.Status.LoadBalancer, nil
}

func discoverAddress(cm *v1.ConfigMap, namespace, configMapName string, existingServiceIPS []string) (vip string, err error) {
	var cidr, ipRange string
	var ok bool

	// Find Cidr
	cidrKey := fmt.Sprintf("cidr-%s", namespace)
	// Lookup current namespace
	if cidr, ok = cm.Data[cidrKey]; !ok {
		klog.Info(fmt.Errorf("no cidr config for namespace [%s] exists in key [%s] configmap [%s]", namespace, cidrKey, configMapName))
		// Lookup global cidr configmap data
		if cidr, ok = cm.Data["cidr-global"]; !ok {
			klog.Info(fmt.Errorf("no global cidr config exists [cidr-global]"))
		} else {
			klog.Infof("Taking address from [cidr-global] pool")
		}
	} else {
		klog.Infof("Taking address from [%s] pool", cidrKey)
	}
	if ok {
		vip, err = ipam.FindAvailableHostFromCidr(namespace, cidr, existingServiceIPS)
		if err != nil {
			return "", err
		}
		return
	}

	// Find Range
	rangeKey := fmt.Sprintf("range-%s", namespace)
	// Lookup current namespace
	if ipRange, ok = cm.Data[rangeKey]; !ok {
		klog.Info(fmt.Errorf("no range config for namespace [%s] exists in key [%s] configmap [%s]", namespace, rangeKey, configMapName))
		// Lookup global range configmap data
		if ipRange, ok = cm.Data["range-global"]; !ok {
			klog.Info(fmt.Errorf("no global range config exists [range-global]"))
		} else {
			klog.Infof("Taking address from [range-global] pool")
		}
	} else {
		klog.Infof("Taking address from [%s] pool", rangeKey)
	}
	if ok {
		vip, err = ipam.FindAvailableHostFromRange(namespace, ipRange, existingServiceIPS)
		if err != nil {
			return vip, err
		}
		return
	}
	return "", fmt.Errorf("no IP address ranges could be found either range-global or range-<namespace>")
}
