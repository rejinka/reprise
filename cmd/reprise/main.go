package main

import (
	"flag"
	"log"
	"os"

	"github.com/rejinka/reprise/internal/controller"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

func main() {
	var metrics, probes string
	flag.StringVar(&metrics, "metrics-bind-address", ":8080", "Metrics listen address")
	flag.StringVar(&probes, "health-probe-bind-address", ":8081", "Probe listen address")
	flag.Parse()
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Metrics:                 controller.MetricsOptions(metrics),
		HealthProbeBindAddress:  probes,
		LeaderElection:          true,
		LeaderElectionID:        "reprise.naji-dev.de",
		LeaderElectionNamespace: os.Getenv("POD_NAMESPACE"),
	})
	if err != nil {
		log.Fatal(err)
	}
	request := &unstructured.Unstructured{}
	request.SetGroupVersionKind(schema.GroupVersionKind{Group: "reprise.naji-dev.de", Version: "v1alpha1", Kind: "RepriseRequest"})
	if err := ctrl.NewControllerManagedBy(mgr).For(request).Complete(&controller.Reconciler{Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Mapper: mgr.GetRESTMapper()}); err != nil {
		log.Fatal(err)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Fatal(err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Fatal(err)
	}
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
