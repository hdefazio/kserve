/*
Copyright 2023 The KServe Authors.

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

package main

import (
	"context"
	"flag"
	"net/http"
	"os"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	otelv1beta1 "github.com/open-telemetry/opentelemetry-operator/apis/v1beta1"
	istio_networking "istio.io/api/networking/v1alpha3"
	istioclientv1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/client-go/tools/record"
	knservingv1 "knative.dev/serving/pkg/apis/serving/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/kserve/kserve/pkg/apis/serving/v1alpha1"
	"github.com/kserve/kserve/pkg/apis/serving/v1beta1"
	"github.com/kserve/kserve/pkg/constants"
	graphcontroller "github.com/kserve/kserve/pkg/controller/v1alpha1/inferencegraph"
	trainedmodelcontroller "github.com/kserve/kserve/pkg/controller/v1alpha1/trainedmodel"
	"github.com/kserve/kserve/pkg/controller/v1alpha1/trainedmodel/reconcilers/modelconfig"
	v1beta1controller "github.com/kserve/kserve/pkg/controller/v1beta1/inferenceservice"
	"github.com/kserve/kserve/pkg/utils"
	"github.com/kserve/kserve/pkg/webhook/admission/localmodelcache"
	"github.com/kserve/kserve/pkg/webhook/admission/pod"
	"github.com/kserve/kserve/pkg/webhook/admission/servingruntime"
)

var setupLog = ctrl.Log.WithName("setup")

const (
	LeaderLockName = "kserve-controller-manager-leader-lock"
)

// Options defines the program configurable options that may be passed on the command line.
type Options struct {
	metricsAddr          string
	webhookPort          int
	enableLeaderElection bool
	probeAddr            string
	zapOpts              zap.Options
}

// DefaultOptions returns the default values for the program options.
func DefaultOptions() Options {
	return Options{
		metricsAddr:          ":8080",
		webhookPort:          9443,
		enableLeaderElection: false,
		probeAddr:            ":8081",
		zapOpts:              zap.Options{},
	}
}

// GetOptions parses the program flags and returns them as Options.
func GetOptions() Options {
	opts := DefaultOptions()
	flag.StringVar(&opts.metricsAddr, "metrics-addr", opts.metricsAddr, "The address the metric endpoint binds to.")
	flag.IntVar(&opts.webhookPort, "webhook-port", opts.webhookPort, "The port that the webhook server binds to.")
	flag.BoolVar(&opts.enableLeaderElection, "leader-elect", opts.enableLeaderElection,
		"Enable leader election for kserve controller manager. "+
			"Enabling this will ensure there is only one active kserve controller manager.")
	flag.StringVar(&opts.probeAddr, "health-probe-addr", opts.probeAddr, "The address the probe endpoint binds to.")
	opts.zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()
	return opts
}

func init() {
	// Allow unknown fields in Istio API client for backwards compatibility if cluster has existing vs with deprecated fields.
	istio_networking.VirtualServiceUnmarshaler.AllowUnknownFields = true
	istio_networking.GatewayUnmarshaler.AllowUnknownFields = true
}

func main() {
	options := GetOptions()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&options.zapOpts)))

	// Get a config to talk to the apiserver
	setupLog.Info("Setting up client for manager")
	cfg, err := config.GetConfig()
	if err != nil {
		setupLog.Error(err, "unable to set up client config")
		os.Exit(1)
	}

	// Setup clientset to directly talk to the api server
	clientSet, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		setupLog.Error(err, "unable to create clientSet")
		os.Exit(1)
	}

	// Create a new Cmd to provide shared dependencies and start components
	setupLog.Info("Setting up manager")
	mgr, err := manager.New(cfg, manager.Options{
		Metrics: metricsserver.Options{
			BindAddress: options.metricsAddr,
		},
		WebhookServer: webhook.NewServer(webhook.Options{
			Port: options.webhookPort,
		}),
		LeaderElection:         options.enableLeaderElection,
		LeaderElectionID:       LeaderLockName,
		HealthProbeBindAddress: options.probeAddr,
	})
	if err != nil {
		setupLog.Error(err, "unable to set up overall controller manager")
		os.Exit(1)
	}

	setupLog.Info("Registering Components.")

	setupLog.Info("Setting up KServe v1alpha1 scheme")
	if err := v1alpha1.AddToScheme(mgr.GetScheme()); err != nil {
		setupLog.Error(err, "unable to add KServe v1alpha1 to scheme")
		os.Exit(1)
	}

	setupLog.Info("Setting up KServe v1beta1 scheme")
	if err := v1beta1.AddToScheme(mgr.GetScheme()); err != nil {
		setupLog.Error(err, "unable to add KServe v1beta1 to scheme")
		os.Exit(1)
	}

	isvcConfigMap, err := v1beta1.GetInferenceServiceConfigMap(context.Background(), clientSet)
	if err != nil {
		setupLog.Error(err, "unable to get configmap", "name", constants.InferenceServiceConfigMapName, "namespace", constants.KServeNamespace)
		os.Exit(1)
	}
	deployConfig, err := v1beta1.NewDeployConfig(isvcConfigMap)
	if err != nil {
		setupLog.Error(err, "unable to get deploy config.")
		os.Exit(1)
	}
	ingressConfig, err := v1beta1.NewIngressConfig(isvcConfigMap)
	if err != nil {
		setupLog.Error(err, "unable to get ingress config.")
		os.Exit(1)
	}

	// Update Global GPU Resource Type List when custom GPU resource types are provided
	_, err = v1beta1.NewMultiNodeConfig(isvcConfigMap)
	if err != nil {
		setupLog.Error(err, "unable to get multiNode config.")
		os.Exit(1)
	}

	ksvcFound, ksvcCheckErr := utils.IsCrdAvailable(cfg, knservingv1.SchemeGroupVersion.String(), constants.KnativeServiceKind)
	if ksvcCheckErr != nil {
		setupLog.Error(ksvcCheckErr, "error when checking if Knative Service kind is available")
		os.Exit(1)
	}
	if ksvcFound {
		setupLog.Info("Setting up Knative scheme")
		if err := knservingv1.AddToScheme(mgr.GetScheme()); err != nil {
			setupLog.Error(err, "unable to add Knative APIs to scheme")
			os.Exit(1)
		}
	}
	if !ingressConfig.DisableIstioVirtualHost {
		vsFound, vsCheckErr := utils.IsCrdAvailable(cfg, istioclientv1beta1.SchemeGroupVersion.String(), constants.IstioVirtualServiceKind)
		if vsCheckErr != nil {
			setupLog.Error(vsCheckErr, "error when checking if Istio VirtualServices are available")
			os.Exit(1)
		}
		if vsFound {
			setupLog.Info("Setting up Istio schemes")
			if err := istioclientv1beta1.AddToScheme(mgr.GetScheme()); err != nil {
				setupLog.Error(err, "unable to add Istio v1beta1 APIs to scheme")
				os.Exit(1)
			}
		}
	}

	kedaFound, kedaCheckErr := utils.IsCrdAvailable(cfg, kedav1alpha1.SchemeGroupVersion.String(), constants.KedaScaledObjectKind)
	if kedaCheckErr != nil {
		setupLog.Error(ksvcCheckErr, "error when checking if KEDA ScaledObject kind is available")
		os.Exit(1)
	}
	if kedaFound {
		setupLog.Info("Setting up KEDA scheme")
		if err := kedav1alpha1.AddToScheme(mgr.GetScheme()); err != nil {
			setupLog.Error(err, "unable to add KEDA APIs to scheme")
			os.Exit(1)
		}
	}

	otelFound, otelCheckErr := utils.IsCrdAvailable(cfg, otelv1beta1.GroupVersion.String(), constants.OpenTelemetryCollector)
	if otelCheckErr != nil {
		setupLog.Error(ksvcCheckErr, "error when checking if OpentelemetryCollector kind is available")
		os.Exit(1)
	}
	if otelFound {
		setupLog.Info("Setting up OTEL scheme")
		if err := otelv1beta1.AddToScheme(mgr.GetScheme()); err != nil {
			setupLog.Error(err, "unable to add OTEL APIs to scheme")
			os.Exit(1)
		}
	}

	setupLog.Info("Setting up gateway api scheme")
	if err := gwapiv1.Install(mgr.GetScheme()); err != nil {
		setupLog.Error(err, "unable to add Gateway APIs to scheme")
		os.Exit(1)
	}

	setupLog.Info("Setting up core scheme")
	if err := corev1.AddToScheme(mgr.GetScheme()); err != nil {
		setupLog.Error(err, "unable to add Core APIs to scheme")
		os.Exit(1)
	}

	// Setup all Controllers
	setupLog.Info("Setting up v1beta1 controller")
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: clientSet.CoreV1().Events("")})
	if err = (&v1beta1controller.InferenceServiceReconciler{
		Client:    mgr.GetClient(),
		Clientset: clientSet,
		Log:       ctrl.Log.WithName("v1beta1Controllers").WithName("InferenceService"),
		Scheme:    mgr.GetScheme(),
		Recorder: eventBroadcaster.NewRecorder(
			mgr.GetScheme(), corev1.EventSource{Component: "v1beta1Controllers"}),
	}).SetupWithManager(mgr, deployConfig, ingressConfig); err != nil {
		setupLog.Error(err, "unable to create controller", "v1beta1Controller", "InferenceService")
		os.Exit(1)
	}

	// Setup TrainedModel controller
	trainedModelEventBroadcaster := record.NewBroadcaster()
	setupLog.Info("Setting up v1beta1 TrainedModel controller")
	trainedModelEventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: clientSet.CoreV1().Events("")})
	if err = (&trainedmodelcontroller.TrainedModelReconciler{
		Client:                mgr.GetClient(),
		Log:                   ctrl.Log.WithName("v1beta1Controllers").WithName("TrainedModel"),
		Scheme:                mgr.GetScheme(),
		Recorder:              eventBroadcaster.NewRecorder(mgr.GetScheme(), corev1.EventSource{Component: "v1beta1Controllers"}),
		ModelConfigReconciler: modelconfig.NewModelConfigReconciler(mgr.GetClient(), clientSet, mgr.GetScheme()),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "v1beta1Controllers", "TrainedModel")
		os.Exit(1)
	}

	// Setup Inference graph controller
	inferenceGraphEventBroadcaster := record.NewBroadcaster()
	setupLog.Info("Setting up InferenceGraph controller")
	inferenceGraphEventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: clientSet.CoreV1().Events("")})
	if err = (&graphcontroller.InferenceGraphReconciler{
		Client:    mgr.GetClient(),
		Clientset: clientSet,
		Log:       ctrl.Log.WithName("v1alpha1Controllers").WithName("InferenceGraph"),
		Scheme:    mgr.GetScheme(),
		Recorder:  eventBroadcaster.NewRecorder(mgr.GetScheme(), corev1.EventSource{Component: "InferenceGraphController"}),
	}).SetupWithManager(mgr, deployConfig); err != nil {
		setupLog.Error(err, "unable to create controller", "v1alpha1Controllers", "InferenceGraph")
		os.Exit(1)
	}

	setupLog.Info("setting up webhook server")
	hookServer := mgr.GetWebhookServer()

	setupLog.Info("registering webhooks to the webhook server")
	hookServer.Register("/mutate-pods", &webhook.Admission{
		Handler: &pod.Mutator{Client: mgr.GetClient(), Clientset: clientSet, Decoder: admission.NewDecoder(mgr.GetScheme())},
	})

	setupLog.Info("registering cluster serving runtime validator webhook to the webhook server")
	hookServer.Register("/validate-serving-kserve-io-v1alpha1-clusterservingruntime", &webhook.Admission{
		Handler: &servingruntime.ClusterServingRuntimeValidator{Client: mgr.GetClient(), Decoder: admission.NewDecoder(mgr.GetScheme())},
	})

	setupLog.Info("registering serving runtime validator webhook to the webhook server")
	hookServer.Register("/validate-serving-kserve-io-v1alpha1-servingruntime", &webhook.Admission{
		Handler: &servingruntime.ServingRuntimeValidator{Client: mgr.GetClient(), Decoder: admission.NewDecoder(mgr.GetScheme())},
	})

	if err = ctrl.NewWebhookManagedBy(mgr).
		For(&v1alpha1.TrainedModel{}).
		WithValidator(&v1alpha1.TrainedModelValidator{}).
		Complete(); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "v1alpha1")
		os.Exit(1)
	}

	if err = ctrl.NewWebhookManagedBy(mgr).
		For(&v1alpha1.InferenceGraph{}).
		WithValidator(&v1alpha1.InferenceGraphValidator{}).
		Complete(); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "v1alpha1")
		os.Exit(1)
	}

	if err = ctrl.NewWebhookManagedBy(mgr).
		For(&v1beta1.InferenceService{}).
		WithDefaulter(&v1beta1.InferenceServiceDefaulter{}).
		WithValidator(&v1beta1.InferenceServiceValidator{}).
		Complete(); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "v1beta1")
		os.Exit(1)
	}

	if err = ctrl.NewWebhookManagedBy(mgr).
		For(&v1alpha1.LocalModelCache{}).
		WithValidator(&localmodelcache.LocalModelCacheValidator{Client: mgr.GetClient()}).
		Complete(); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "localmodelcache")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", func(req *http.Request) error {
		return mgr.GetWebhookServer().StartedChecker()(req)
	}); err != nil {
		setupLog.Error(err, "Unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", func(req *http.Request) error {
		return mgr.GetWebhookServer().StartedChecker()(req)
	}); err != nil {
		setupLog.Error(err, "Unable to set up ready check")
		os.Exit(1)
	}

	// Start the Cmd
	setupLog.Info("Starting the Cmd.")
	if err := mgr.Start(signals.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "unable to run the manager")
		os.Exit(1)
	}
}
