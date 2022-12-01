// Package podip provides a utility, mapping between pod IP addresses and
// a Pod's namespace/name
package podip

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// Controller maintains a mapping between IP addresses and Pods
type Controller interface {
	Run(stopChannel <-chan struct{}) error
	LookupIP(ip net.IP) (string, string)
	LookupAddr(addr netip.Addr) (string, string)
}

// controller provides a Controller implementation
type controller struct {
	inf     informers.SharedInformerFactory
	pods    map[netip.Addr]string
	skipNS  map[string]struct{} // ignored namespaces
	ignored map[string]struct{} // ignored Pods (to avoid overloading the log...)
	log     logr.Logger
	lock    sync.RWMutex
}

const (
	separator = "/"
)

// NewController returns a new controller using the default kubeconfig (in or out of cluster)
func NewController(l logr.Logger, ignoreNamespaces []string) (Controller, error) {
	cs, err := newClientSet()
	if err != nil {
		return nil, err
	}
	return newControllerFromClientSet(l, ignoreNamespaces, cs)
}

// newControllerFromClientSet returns a new controller using the provided k8s REST API configuration
// This is a convenience function and internally calls NewControllerFromFactory after creating
// a kubernetes.ClientSet and a SharedInformerFactory from it.
func newControllerFromClientSet(l logr.Logger, ignoreNamespaces []string, clientset *kubernetes.Clientset) (Controller, error) {
	factory := informers.NewSharedInformerFactory(clientset, time.Minute*5)
	return newControllerFromFactory(l, ignoreNamespaces, factory)
}

// newControllerFromFactory creates a new Controller using the provided informer factory
func newControllerFromFactory(l logr.Logger, ignoreNamespaces []string, inf informers.SharedInformerFactory) (Controller, error) {
	c := &controller{
		inf:     inf,
		pods:    make(map[netip.Addr]string),
		skipNS:  make(map[string]struct{}),
		ignored: make(map[string]struct{}),
		log:     l,
	}

	for _, ns := range ignoreNamespaces {
		c.skipNS[ns] = struct{}{}
	}
	return c, nil
}

// Run sets up the controller to continuously watch for Pod events, until
// stop channel is signalled/closed.
func (c *controller) Run(stopChannel <-chan struct{}) error {
	c.log = c.log.WithName("podip-controller")
	c.log.Info("running...")
	if len(c.skipNS) > 0 {
		c.log.Info("ignoring namespaces", "namespaces", c.skipNS)
	}

	podInformer := c.inf.Core().V1().Pods()

	podInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    c.podAdd,
			UpdateFunc: c.podUpdate,
			DeleteFunc: c.podDelete,
		},
	)
	c.inf.Start(stopChannel)

	// hold lock while waiting for the initial synchronization of the local cache
	c.lock.Lock()
	defer c.lock.Unlock()
	if !cache.WaitForCacheSync(stopChannel, podInformer.Informer().HasSynced) {
		return fmt.Errorf("failed to synchronize Pod informer cache")
	}
	c.log.Info("cache synched")
	return nil
}

// LookupIP returns the Pod name and namespace corresponding to the provided IP.
// Empty strings are returned if no matching Pod is found.
func (c *controller) LookupIP(ip net.IP) (string, string) {
	if addr, ok := netip.AddrFromSlice(ip); ok {
		return c.LookupAddr(addr)
	}
	return "", ""
}

// LookupAddr returns the Pod name and namespace corresponding to the provided address.
// Empty strings are returned if no matching Pod is found.
func (c *controller) LookupAddr(addr netip.Addr) (string, string) {
	c.lock.RLock()
	defer c.lock.RUnlock()

	pod, found := c.pods[addr]
	if !found {
		return "", ""
	}
	names := strings.Split(pod, separator)
	return names[0], names[1]
}

// a pod is added
func (c *controller) podAdd(obj interface{}) {
	pod := obj.(*corev1.Pod)
	if len(pod.Status.PodIPs) == 0 { // IP addresses not assigned yet - ignore
		c.log.Info("no IP assigned", "name", namespacedName(pod))
		return
	}

	value := namespacedName(pod)
	if _, ignoreNS := c.skipNS[pod.Namespace]; ignoreNS { // skipping namespace of Pod
		if _, found := c.ignored[value]; !found { // only report once
			c.ignored[value] = struct{}{}
			c.log.Info("namespace filter", "name", value)
		}
		return
	}

	c.lock.Lock()
	defer c.lock.Unlock()

	for _, ip := range pod.Status.PodIPs {
		key, err := netip.ParseAddr(ip.IP)
		if err != nil {
			c.log.Info("unable to parse Pod IP", "IP", ip.String(), "key", key, "pod", value)
		}
		current, found := c.pods[key]

		if !found || current != value {
			c.log.Info("adding Pod", "IP", ip.String(), "key", key, "pod", value)
			c.pods[key] = value
		}
	}
}

// a pod is updated
func (c *controller) podUpdate(oldObj, newObj interface{}) {
	oldPod := oldObj.(*corev1.Pod)
	newPod := newObj.(*corev1.Pod)

	if oldPod.Status.PodIP != "" &&
		((newPod.Status.PodIP != oldPod.Status.PodIP) || (newPod.Name != oldPod.Name)) {
		c.log.Info("updating Pod", "name", namespacedName(newPod))
		c.podDelete(oldObj)
	}
	c.podAdd(newObj)
}

// a pod is deleted
func (c *controller) podDelete(obj interface{}) {
	pod := obj.(*corev1.Pod)

	if len(pod.Status.PodIPs) == 0 { // nothing to clear
		return
	}

	c.lock.Lock()
	defer c.lock.Unlock()

	c.log.Info("deleting Pod", "name", namespacedName(pod))
	for _, ip := range pod.Status.PodIPs {
		key, _ := netip.ParseAddr(ip.IP)
		name, found := c.pods[key]
		if !found {
			continue
		}
		if name != namespacedName(pod) { // duplicate IP assigned?
			// @todo - if this is possible - handle it more gracefully ;-)
			continue
		}
		delete(c.pods, key)
	}
}

// canonical Pod name
func namespacedName(pod *corev1.Pod) string {
	return strings.Join([]string{pod.Namespace, pod.Name}, separator)
}
