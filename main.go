package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	kubevirtv1 "kubevirt.io/api/core/v1"

	"github.com/swisscom/kubevirt-online-resize-helper/pkg/controller"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(kubevirtv1.AddToScheme(scheme))
}

func main() {
	var (
		namespace       string
		infraKubeconfig string
	)
	flag.StringVar(&namespace, "namespace", "", "Namespace to watch on the infra cluster (required)")
	flag.StringVar(&infraKubeconfig, "infra-kubeconfig", "", "Path to kubeconfig for the KubeVirt infra cluster (required)")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	if namespace == "" {
		namespace = os.Getenv("WATCH_NAMESPACE")
	}
	if infraKubeconfig == "" {
		infraKubeconfig = os.Getenv("INFRA_KUBECONFIG")
	}
	if namespace == "" || infraKubeconfig == "" {
		ctrl.Log.WithName("setup").Error(nil, "--namespace and --infra-kubeconfig are required")
		os.Exit(1)
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("setup")

	// Build rest.Config from the infra kubeconfig.
	infraConfig, err := clientcmd.BuildConfigFromFlags("", infraKubeconfig)
	if err != nil {
		log.Error(err, "unable to build infra cluster config")
		os.Exit(1)
	}

	// The manager connects to the infra cluster (watches PVCs, VMIs, execs into pods there).
	mgr, err := ctrl.NewManager(infraConfig, ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				namespace: {},
			},
		},
		Metrics: metricsserver.Options{
			BindAddress: ":8080",
		},
		HealthProbeBindAddress: ":8081",
	})
	if err != nil {
		log.Error(err, "unable to create manager")
		os.Exit(1)
	}

	if err := controller.SetupPVCReconciler(mgr); err != nil {
		log.Error(err, "unable to setup PVC reconciler")
		os.Exit(1)
	}

	log.Info("starting manager", "namespace", namespace, "infraKubeconfig", infraKubeconfig)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited with error")
		os.Exit(1)
	}
}
