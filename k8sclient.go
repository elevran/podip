package podip

import (
	"flag"
	"os"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

// newClientSet returns a new k8s client interface.
// It resolves whether it is running inside a k8s cluster or not.
// When running out of cluster, it'll attempt to load the default kubeconfig file (or an explicit
// config path if provided)
func newClientSet() (*kubernetes.Clientset, error) {
	config, err := config()

	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(config)
}

// config returns a k8s REST configuration (in cluster or external)
func config() (*rest.Config, error) {
	var config *rest.Config
	var err error
	if _, inCluster := os.LookupEnv("KUBERNETES_SERVICE_HOST"); inCluster {
		config, err = rest.InClusterConfig()
		if err != nil {
			return nil, err
		}
	} else {
		defaultKubeConfigPath := ""

		if defaultKubeConfigPath = os.Getenv(clientcmd.RecommendedConfigPathEnvVar); defaultKubeConfigPath == "" {
			if homedir.HomeDir() != "" {
				defaultKubeConfigPath = clientcmd.RecommendedHomeFile
			}
		}
		kubeconfig := flag.String(clientcmd.RecommendedConfigPathFlag, defaultKubeConfigPath, "path to the kubeconfig file")
		flag.Parse()
		config, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
		if err != nil {
			return nil, err
		}
	}
	return config, err
}
