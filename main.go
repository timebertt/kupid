// Copyright (c) 2020 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	extensionswebhook "github.com/gardener/gardener/extensions/pkg/webhook"
	"github.com/gardener/gardener/extensions/pkg/webhook/certificates"
	uberzap "go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	batchv1beta1 "k8s.io/api/batch/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/client-go/rest"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	kupidv1alpha1 "github.com/gardener/kupid/api/v1alpha1"
	"github.com/gardener/kupid/pkg/webhook"
)

var (
	scheme                 = runtime.NewScheme()
	setupLog               = ctrl.Log.WithName("setup")
	mutateOnCreateOrUpdate = []admissionregistrationv1.OperationType{
		admissionregistrationv1.Create,
		admissionregistrationv1.Update,
	}
	mutateOnCreate = []admissionregistrationv1.OperationType{
		admissionregistrationv1.Create,
	}
)

const (
	webhookName     = "kupid"
	webhookFullName = "gardener-extension-" + webhookName

	flagWebhookPort           = "webhook-port"
	flagMetricsAddr           = "metrics-addr"
	flagHealthzAddr           = "healthz-addr"
	flagCertDir               = "cert-dir"
	flagRegisterWebhooks      = "register-webhooks"
	flagWebhookTimeoutSeconds = "webhook-timeout-seconds"
	flagSyncPeriod            = "sync-period"
	flagQPS                   = "qps"
	flagBurst                 = "burst"
	envNamespace              = "WEBHOOK_CONFIG_NAMESPACE"

	defaultWebhookPort           = 9443
	defaultWebhookTimeoutSeconds = 30
	defaultMetricsAddr           = ":8081"
	defaultHealthzAddr           = ":8080"
	defaultSyncPeriod            = 1 * time.Hour
)

func init() {
	_ = clientgoscheme.AddToScheme(scheme)

	_ = kupidv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)
	_ = batchv1beta1.AddToScheme(scheme)
}

func main() {
	var (
		ctx = ctrl.SetupSignalHandler()

		webhookPort           int
		metricsAddr           string
		healthzAddr           string
		certDir               string
		registerWebhooks      bool
		webhookTimeoutSeconds int
		syncPeriod            time.Duration
		qps                   float64
		burst                 int
		namespace             string
		logLevel              = uberzap.LevelFlag("v", zapcore.InfoLevel, "Logging level")
	)

	flag.IntVar(&webhookPort, flagWebhookPort, defaultWebhookPort, "The port for the webhook server to listen on.")
	flag.StringVar(&metricsAddr, flagMetricsAddr, defaultMetricsAddr, "The address the metric endpoint binds to.")
	flag.StringVar(&healthzAddr, flagHealthzAddr, defaultHealthzAddr, "The address the healthz endpoint binds to.")
	flag.StringVar(&certDir, flagCertDir, "./certs", "The directory where the serving certs are kept.")
	flag.BoolVar(&registerWebhooks, flagRegisterWebhooks, false, "If enabled registers the webhook configurations automatically. The webhook is assumed to be reachable by a service with the name 'gardener-extension-kupid' within the same namespace. If necessary this will also generate TLS certificates for the webhook as well as the webhook configurations. The generated certificates will be published by creating a secret with the name 'gardener-extension-webhook-cert'. If the secret already exists then it is reused and no new certificates are generated.")
	flag.IntVar(&webhookTimeoutSeconds, flagWebhookTimeoutSeconds, defaultWebhookTimeoutSeconds, "If webhooks are registered automatically then they are configured with this timeout.")
	flag.DurationVar(&syncPeriod, flagSyncPeriod, defaultSyncPeriod, "SyncPeriod determines the minimum frequency at which watched resources are reconciled. A lower period will correct entropy more quickly, but reduce responsiveness to change if there are many watched resources. Change this value only if you know what you are doing.")
	flag.Float64Var(&qps, flagQPS, float64(rest.DefaultQPS), "Throttling QPS configuration for the client to host apiserver.")
	flag.IntVar(&burst, flagBurst, rest.DefaultBurst, "Throttling burst configuration for the client to host apiserver.")

	flag.Parse()

	level := uberzap.NewAtomicLevelAt(*logLevel)
	ctrl.SetLogger(zap.New(zap.Level(&level)))

	namespace = os.Getenv(envNamespace)

	setupLog.Info("Running with",
		flagWebhookPort, webhookPort,
		flagMetricsAddr, metricsAddr,
		flagCertDir, certDir,
		flagRegisterWebhooks, registerWebhooks,
		flagWebhookTimeoutSeconds, webhookTimeoutSeconds,
		flagSyncPeriod, syncPeriod,
		flagQPS, qps,
		flagBurst, burst,
		envNamespace, namespace)

	config := ctrl.GetConfigOrDie()

	config.QPS = float32(qps)
	config.Burst = burst

	mgr, err := ctrl.NewManager(config, ctrl.Options{
		Scheme:                 scheme,
		MetricsBindAddress:     metricsAddr,
		Port:                   webhookPort,
		HealthProbeBindAddress: healthzAddr,
		SyncPeriod:             &syncPeriod,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Initialize webhook certificates
	func() {
		ws := mgr.GetWebhookServer()
		ws.CertDir = certDir
	}()

	if registerWebhooks {
		if err := doRegisterWebhooks(ctx, mgr, certDir, namespace, int32(webhookTimeoutSeconds)); err != nil {
			setupLog.Error(err, "Error registering webhooks. Aborting startup...")
			os.Exit(1)
		}
	}

	if w, err := webhook.NewDefaultWebhook(); err != nil {
		setupLog.Error(err, "unable to create default webhook", "webhook", webhookName)
	} else if err := w.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to setup", "webhook", webhookName)
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func doRegisterWebhooks(ctx context.Context, mgr manager.Manager, certDir, namespace string, timeoutSeconds int32) error {
	setupLog.Info("Registering webhooks in seed cluster")

	var (
		c                       = mgr.GetClient()
		clientConfig            = buildWebhookClientConfig(namespace)
		validatingWebhookConfig = newValidatingWebhookConfig(clientConfig, timeoutSeconds)
		mutatingWebhookConfig   = newMutatingWebhookConfig(clientConfig, timeoutSeconds)
	)
	setupLog.Info("Registering TLS certificates if necessary.")

	if namespace == "" {
		// If the namespace is not set (e.g. when running locally), then we can't use the secrets manager for managing
		// the webhook certificates. We simply generate a new certificate and write it to CertDir in this case.
		setupLog.Info("Running webhooks with unmanaged certificates (i.e., the webhook CA will not be rotated automatically). " +
			"This mode is supposed to be used for development purposes only. Make sure to set WEBHOOK_CONFIG_NAMESPACE in production.")

		caBundle, err := certificates.GenerateUnmanagedCertificates(webhookName, certDir, extensionswebhook.ModeService, "")
		if err != nil {
			return fmt.Errorf("error generating new certificates for webhook server: %w", err)
		}

		// register seed webhook config once we become leader – with the CA bundle we just generated
		if err := mgr.Add(runOnceWithLeaderElection(func(ctx context.Context) error {
			if err := extensionswebhook.ReconcileSeedWebhookConfig(ctx, c, validatingWebhookConfig, namespace, caBundle); err != nil {
				return nil
			}
			return extensionswebhook.ReconcileSeedWebhookConfig(ctx, c, mutatingWebhookConfig, namespace, caBundle)
		})); err != nil {
			return fmt.Errorf("error reconciling seed webhook config: %w", err)
		}

		return nil
	}

	// register seed webhook config once we become leader – without CA bundle
	// We only care about registering the desired webhooks here, but not the CA bundle, it will be managed by the
	// reconciler.
	if err := mgr.Add(runOnceWithLeaderElection(func(ctx context.Context) error {
		if err := extensionswebhook.ReconcileSeedWebhookConfig(ctx, c, validatingWebhookConfig, namespace, nil); err != nil {
			return nil
		}
		return extensionswebhook.ReconcileSeedWebhookConfig(ctx, c, mutatingWebhookConfig, namespace, nil)
	})); err != nil {
		return fmt.Errorf("error reconciling seed webhook config: %w", err)
	}

	if err := certificates.AddCertificateManagementToManager(
		ctx,
		mgr,
		clock.RealClock{},
		seedWebhookConfig,
		shootWebhookConfig,
		atomicShootWebhookConfig,
		webhookName,
		c.shootWebhookManagedResourceName,
		c.shootNamespaceSelector,
		c.Server.Namespace,
		c.Server.Mode,
		c.Server.URL,
	); err != nil {
		return err
	}

	caBundle, err := extensionswebhook.GenerateCertificates(
		ctx,
		mgr,
		certDir,
		namespace,
		webhookName,
		extensionswebhook.ModeService,
		"",
	)
	if err != nil {
		return err
	}

	return nil
}

func buildWebhookClientConfig(namespace string) admissionregistrationv1.WebhookClientConfig {
	var path = webhook.WebhookPath
	return admissionregistrationv1.WebhookClientConfig{
		Service: &admissionregistrationv1.ServiceReference{
			Namespace: namespace,
			Name:      webhookFullName,
			Path:      &path,
		},
	}
}

func buildRuleWithOperations(gv schema.GroupVersion, resources []string, operations []admissionregistrationv1.OperationType) admissionregistrationv1.RuleWithOperations {
	return admissionregistrationv1.RuleWithOperations{
		Operations: operations,
		Rule: admissionregistrationv1.Rule{
			APIGroups:   []string{gv.Group},
			APIVersions: []string{gv.Version},
			Resources:   resources,
		},
	}
}

func newValidatingWebhookConfig(clientConfig admissionregistrationv1.WebhookClientConfig, timeoutSeconds int32) *admissionregistrationv1.ValidatingWebhookConfiguration {
	var (
		ignore = admissionregistrationv1.Ignore
		exact  = admissionregistrationv1.Exact
		none   = admissionregistrationv1.SideEffectClassNone
	)

	return &admissionregistrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: webhookFullName,
		},
		Webhooks: []admissionregistrationv1.ValidatingWebhook{
			{
				Name:         "validate." + kupidv1alpha1.GroupVersion.Group,
				ClientConfig: clientConfig,
				Rules: []admissionregistrationv1.RuleWithOperations{
					buildRuleWithOperations(
						kupidv1alpha1.GroupVersion,
						[]string{
							"clusterpodschedulingpolicies",
							"podschedulingpolicies",
						},
						mutateOnCreateOrUpdate,
					),
				},
				FailurePolicy:  &ignore,
				MatchPolicy:    &exact,
				SideEffects:    &none,
				TimeoutSeconds: &timeoutSeconds,
				AdmissionReviewVersions: []string{
					"v1",
				},
			},
		},
	}
}

func newMutatingWebhookConfig(clientConfig admissionregistrationv1.WebhookClientConfig, timeoutSeconds int32) *admissionregistrationv1.MutatingWebhookConfiguration {
	var (
		ignore     = admissionregistrationv1.Ignore
		equivalent = admissionregistrationv1.Equivalent
		none       = admissionregistrationv1.SideEffectClassNone
		ifNeeded   = admissionregistrationv1.IfNeededReinvocationPolicy
	)

	return &admissionregistrationv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: webhookFullName,
		},
		Webhooks: []admissionregistrationv1.MutatingWebhook{
			{
				Name: "mutate." + kupidv1alpha1.GroupVersion.Group,
				NamespaceSelector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{
							Key:      "role",
							Operator: metav1.LabelSelectorOpNotIn,
							Values:   []string{"kube-system"},
						},
					},
				},
				ClientConfig: clientConfig,
				Rules: []admissionregistrationv1.RuleWithOperations{
					buildRuleWithOperations(
						appsv1.SchemeGroupVersion,
						[]string{
							"daemonsets",
							"deployments",
							"statefulsets",
						},
						mutateOnCreateOrUpdate,
					),
					buildRuleWithOperations(
						batchv1.SchemeGroupVersion,
						[]string{
							"jobs",
						},
						mutateOnCreate,
					),
					buildRuleWithOperations(
						batchv1beta1.SchemeGroupVersion,
						[]string{
							"cronjobs",
						},
						mutateOnCreateOrUpdate,
					),
				},
				FailurePolicy:      &ignore,
				MatchPolicy:        &equivalent,
				SideEffects:        &none,
				TimeoutSeconds:     &timeoutSeconds,
				ReinvocationPolicy: &ifNeeded,
				AdmissionReviewVersions: []string{
					"v1",
				},
			},
		},
	}
}

// runOnceWithLeaderElection is a function that is run exactly once when the manager, it is added to, becomes leader.
type runOnceWithLeaderElection func(ctx context.Context) error

func (r runOnceWithLeaderElection) NeedLeaderElection() bool {
	return true
}

func (r runOnceWithLeaderElection) Start(ctx context.Context) error {
	return r(ctx)
}
