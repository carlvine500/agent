/*
Copyright 2015 The Kubernetes Authors All rights reserved.

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

package k8s

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	flag "github.com/spf13/pflag"
	"golang.org/x/net/context"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/pkg/apis/extensions/v1beta1"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	resyncPeriod = 5 * time.Minute
	metricsPath  = "/metrics"
	healthzPath  = "/healthz"
)

var (
	flags = flag.NewFlagSet("", flag.ExitOnError)

	inCluster = flags.Bool("in-cluster", false, `If true, use the built in kubernetes cluster for creating the client`)

	apiserver = flags.String("apiserver", "", `The URL of the apiserver to use as a master`)

	kubeconfig = flags.String("kubeconfig", "./config", "absolute path to the kubeconfig file")

	help = flags.BoolP("help", "h", false, "Print help text")

	port = flags.Int("port", 80, `Port to expose metrics on.`)
)

func main() {
	flags.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		flags.PrintDefaults()
	}

	err := flags.Parse(os.Args)
	if err != nil {
		glog.Fatalf("Error: %s", err)
	}

	if *help {
		flags.Usage()
		os.Exit(0)
	}

	if *apiserver == "" && !(*inCluster) {
		glog.Fatalf("--apiserver not set and --in-cluster is false; apiserver must be set to a valid URL")
	}
	glog.Infof("apiServer set to: %v", *apiserver)

	kubeClient, err := CreateKubeClient(*apiserver)
	if err != nil {
		glog.Fatalf("Failed to create client: %v", err)
	}

	InitializeMetricCollection(kubeClient)
	metricsServer()
}

func CreateKubeClient(strApiServer string) (kubeClient clientset.Interface, err error) {
	glog.Infof("Creating client")
	if *inCluster {
		config, err := restclient.InClusterConfig()
		if err != nil {
			return nil, err
		}
		// Allow overriding of apiserver even if using inClusterConfig
		// (necessary if kube-proxy isn't properly set up).
		if *apiserver != "" {
			config.Host = *apiserver
		}
		tokenPresent := false
		if len(config.BearerToken) > 0 {
			tokenPresent = true
		}
		glog.Infof("service account token present: %v", tokenPresent)
		glog.Infof("service host: %s", config.Host)
		if kubeClient, err = clientset.NewForConfig(config); err != nil {
			return nil, err
		}
	} else {
		// loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
		// // if you want to change the loading rules (which files in which order), you can do so here
		// configOverrides := &clientcmd.ConfigOverrides{}
		// // if you want to change override values or bind them to flags, there are methods to help you
		// kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
		// config, err := kubeConfig.ClientConfig()
		//config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
		config, err := clientcmd.DefaultClientConfig.ClientConfig()
		if err != nil {
			return nil, err
		}
		config.Host = strApiServer
		kubeClient, err = clientset.NewForConfig(config)
		if err != nil {
			return nil, err
		}
	}

	// Informers don't seem to do a good job logging error messages when it
	// can't reach the server, making debugging hard. This makes it easier to
	// figure out if apiserver is configured incorrectly.
	glog.Infof("testing communication with server")
	_, err = kubeClient.Discovery().ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("ERROR communicating with apiserver: %v", err)
	}

	return kubeClient, nil
}

func Gather() ([]*dto.MetricFamily, error) {
	mfs, err := prometheus.DefaultGatherer.Gather()
	return mfs, err
}

func metricsServer() {
	// Address to listen on for web interface and telemetry
	listenAddress := fmt.Sprintf(":%d", *port)

	glog.Infof("Starting metrics server: %s", listenAddress)
	// Add metricsPath
	http.Handle(metricsPath, prometheus.UninstrumentedHandler())
	// Add healthzPath
	http.HandleFunc(healthzPath, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	})
	// Add index
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
             <head><title>Kube Metrics Server</title></head>
             <body>
             <h1>Kube Metrics</h1>
			 <ul>
             <li><a href='` + metricsPath + `'>metrics</a></li>
             <li><a href='` + healthzPath + `'>healthz</a></li>
			 </ul>
             </body>
             </html>`))
	})
	log.Fatal(http.ListenAndServe(listenAddress, nil))
}

type DeploymentLister func() ([]v1beta1.Deployment, error)

func (l DeploymentLister) List() ([]v1beta1.Deployment, error) {
	return l()
}

type PodLister func() ([]v1.Pod, error)

func (l PodLister) List() ([]v1.Pod, error) {
	return l()
}

type NodeLister func() (v1.NodeList, error)

func (l NodeLister) List() (v1.NodeList, error) {
	return l()
}

type RCLister func() ([]v1.ReplicationController, error)

func (l RCLister) List() ([]v1.ReplicationController, error) {
	return l()
}

// initializeMetricCollection creates and starts informers and initializes and
// registers metrics for collection.
func InitializeMetricCollection(kubeClient clientset.Interface) {
	cclient := kubeClient.Core().RESTClient()
	eclient := kubeClient.Extensions().RESTClient()

	dlw := cache.NewListWatchFromClient(eclient, "deployments", api.NamespaceAll, nil)
	plw := cache.NewListWatchFromClient(cclient, "pods", api.NamespaceAll, nil)
	nlw := cache.NewListWatchFromClient(cclient, "nodes", api.NamespaceAll, nil)
	rlw := cache.NewListWatchFromClient(cclient, "replicationcontrollers", api.NamespaceAll, nil)

	dinf := cache.NewSharedInformer(dlw, &v1beta1.Deployment{}, resyncPeriod)
	pinf := cache.NewSharedInformer(plw, &v1.Pod{}, resyncPeriod)
	ninf := cache.NewSharedInformer(nlw, &v1.Node{}, resyncPeriod)
	rinf := cache.NewSharedInformer(rlw, &v1.ReplicationController{}, resyncPeriod)

	dplLister := DeploymentLister(func() (deployments []v1beta1.Deployment, err error) {
		for _, c := range dinf.GetStore().List() {
			deployments = append(deployments, *(c.(*v1beta1.Deployment)))
		}
		return deployments, nil
	})

	podLister := PodLister(func() (pods []v1.Pod, err error) {
		for _, m := range pinf.GetStore().List() {
			pods = append(pods, *m.(*v1.Pod))
		}
		return pods, nil
	})

	nodeLister := NodeLister(func() (machines v1.NodeList, err error) {
		for _, m := range ninf.GetStore().List() {
			machines.Items = append(machines.Items, *(m.(*v1.Node)))
		}
		return machines, nil
	})

	rcLister := RCLister(func() (rcs []v1.ReplicationController, err error) {
		for _, m := range rinf.GetStore().List() {
			rcs = append(rcs, *m.(*v1.ReplicationController))
		}
		return rcs, nil
	})

	prometheus.MustRegister(&deploymentCollector{store: dplLister})
	prometheus.MustRegister(&podCollector{store: podLister})
	prometheus.MustRegister(&nodeCollector{store: nodeLister})
	prometheus.MustRegister(&replicationcontrollerCollector{store: rcLister})

	go dinf.Run(context.Background().Done())
	go pinf.Run(context.Background().Done())
	go ninf.Run(context.Background().Done())
	go rinf.Run(context.Background().Done())
}

func SetApiServer(apiservertmp string) {
	*apiserver = apiservertmp
}
