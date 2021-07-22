package operator

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/deploymentcontroller"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/assets"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/dynamic"
	corev1informers "k8s.io/client-go/informers/core/v1"
	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	opv1 "github.com/openshift/api/operator/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	configinformers "github.com/openshift/client-go/config/informers/externalversions"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/csi/csicontrollerset"
	"github.com/openshift/library-go/pkg/operator/csi/csidrivercontrollerservicecontroller"
	"github.com/openshift/library-go/pkg/operator/csi/csidrivernodeservicecontroller"
	goc "github.com/openshift/library-go/pkg/operator/genericoperatorclient"
	"github.com/openshift/library-go/pkg/operator/loglevel"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/storageclasscontroller"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/targetconfigcontroller"
)

const (
	// Operand and operator run in the same namespace
	defaultNamespace                  = "openshift-cluster-csi-drivers"
	cloudConfigNamespace              = "openshift-config"
	operatorName                      = "vmware-vsphere-csi-driver-operator"
	operandName                       = "vmware-vsphere-csi-driver"
	secretName                        = "vmware-vsphere-cloud-credentials"
	envVMWareVsphereDriverSyncerImage = "VMWARE_VSPHERE_SYNCER_IMAGE"
)

func RunOperator(ctx context.Context, controllerConfig *controllercmd.ControllerContext) error {
	// Create core clientset and informers
	kubeClient := kubeclient.NewForConfigOrDie(rest.AddUserAgent(controllerConfig.KubeConfig, operatorName))
	kubeInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(kubeClient, defaultNamespace, cloudConfigNamespace, "")
	secretInformer := kubeInformersForNamespaces.InformersFor(defaultNamespace).Core().V1().Secrets()

	// Create config clientset and informer. This is used to get the cluster ID
	configClient := configclient.NewForConfigOrDie(rest.AddUserAgent(controllerConfig.KubeConfig, operatorName))
	configInformers := configinformers.NewSharedInformerFactory(configClient, 20*time.Minute)

	// Create GenericOperatorclient. This is used by the library-go controllers created down below
	gvr := opv1.SchemeGroupVersion.WithResource("clustercsidrivers")
	operatorClient, dynamicInformers, err := goc.NewClusterScopedOperatorClientWithConfigName(
		controllerConfig.KubeConfig,
		gvr,
		string(opv1.VSphereCSIDriver),
	)
	if err != nil {
		return err
	}

	dynamicClient, err := dynamic.NewForConfig(controllerConfig.KubeConfig)
	if err != nil {
		return err
	}

	csiControllerSet := csicontrollerset.NewCSIControllerSet(
		operatorClient,
		controllerConfig.EventRecorder,
	).WithLogLevelController().WithManagementStateController(
		operandName,
		false,
	).WithStaticResourcesController(
		"VMwareVSphereDriverStaticResourcesController",
		kubeClient,
		dynamicClient,
		kubeInformersForNamespaces,
		assets.ReadFile,
		[]string{
			"vsphere_features_config.yaml",
			"controller_sa.yaml",
			"node_sa.yaml",
			"csidriver.yaml",
			"service.yaml",
			"ca_configmap.yaml",
			"rbac/csi_driver_role.yaml",
			"rbac/csi_driver_binding.yaml",
			"rbac/attacher_role.yaml",
			"rbac/attacher_binding.yaml",
			"rbac/privileged_role.yaml",
			"rbac/controller_privileged_binding.yaml",
			"rbac/node_privileged_binding.yaml",
			"rbac/provisioner_role.yaml",
			"rbac/provisioner_binding.yaml",
			"rbac/resizer_role.yaml",
			"rbac/resizer_binding.yaml",
			"rbac/kube_rbac_proxy_role.yaml",
			"rbac/kube_rbac_proxy_binding.yaml",
			"rbac/prometheus_role.yaml",
			"rbac/prometheus_rolebinding.yaml",
		},
	).WithCSIConfigObserverController(
		"VMwareVSphereDriverCSIConfigObserverController",
		configInformers,
	).WithCSIDriverControllerService(
		"VMwareVSphereDriverControllerServiceController",
		assets.ReadFile,
		"controller.yaml",
		kubeClient,
		kubeInformersForNamespaces.InformersFor(defaultNamespace),
		configInformers,
		[]factory.Informer{
			secretInformer.Informer(),
		},
		WithVSphereCredentials(defaultNamespace, secretName, secretInformer),
		WithSyncerImageHook(),
		WithLogLevelDeploymentHook(),
		csidrivercontrollerservicecontroller.WithObservedProxyDeploymentHook(),
		csidrivercontrollerservicecontroller.WithSecretHashAnnotationHook(
			defaultNamespace,
			secretName,
			secretInformer,
		),
	).WithCSIDriverNodeService(
		"VMwareVSphereDriverNodeServiceController",
		assets.ReadFile,
		"node.yaml",
		kubeClient,
		kubeInformersForNamespaces.InformersFor(defaultNamespace),
		nil,
		WithLogLevelDaemonSetHook(),
		csidrivernodeservicecontroller.WithObservedProxyDaemonSetHook(),
	).WithServiceMonitorController(
		"VMWareVSphereDriverServiceMonitorController",
		dynamicClient,
		assets.ReadFile,
		"servicemonitor.yaml",
	)

	if err != nil {
		return err
	}

	cloudConfigBytes, err := assets.ReadFile("vsphere_cloud_config.yaml")
	if err != nil {
		return err
	}
	targetConfigController := targetconfigcontroller.NewTargetConfigController(
		"VMwareVSphereDriverTargetConfigController",
		defaultNamespace,
		cloudConfigBytes,
		kubeClient,
		kubeInformersForNamespaces,
		operatorClient,
		configInformers,
		controllerConfig.EventRecorder,
	)

	scBytes, err := assets.ReadFile("storageclass.yaml")
	if err != nil {
		return err
	}
	storageClassController := storageclasscontroller.NewStorageClassController(
		"VMwareVSphereDriverStorageClassController",
		defaultNamespace,
		scBytes,
		kubeClient,
		kubeInformersForNamespaces,
		operatorClient,
		configInformers,
		controllerConfig.EventRecorder,
	)

	klog.Info("Starting the informers")
	go kubeInformersForNamespaces.Start(ctx.Done())
	go dynamicInformers.Start(ctx.Done())
	go configInformers.Start(ctx.Done())

	klog.Info("Starting targetconfigcontroller")
	go targetConfigController.Run(ctx, 1)

	klog.Info("Starting storageclasscontroller")
	go storageClassController.Run(ctx, 1)

	klog.Info("Starting controllerset")
	go csiControllerSet.Run(ctx, 1)

	<-ctx.Done()

	return fmt.Errorf("stopped")
}

func WithVSphereCredentials(
	namespace string,
	secretName string,
	secretInformer corev1informers.SecretInformer,
) deploymentcontroller.DeploymentHookFunc {
	return func(opSpec *opv1.OperatorSpec, deployment *appsv1.Deployment) error {
		secret, err := secretInformer.Lister().Secrets(namespace).Get(secretName)
		if err != nil {
			return err
		}

		// CCO generates a secret that contains dynamic keys, for example:
		// oc get secret/vmware-vsphere-cloud-credentials -o json | jq .data
		// {
		//   "vcenter.xyz.vmwarevmc.com.password": "***",
		//   "vcenter.xyz.vmwarevmc.com.username": "***"
		// }
		// So we need to figure those keys out
		var usernameKey, passwordKey string
		for k := range secret.Data {
			if strings.HasSuffix(k, ".username") {
				usernameKey = k
			} else if strings.HasSuffix(k, ".password") {
				passwordKey = k
			}
		}
		if usernameKey == "" || passwordKey == "" {
			return fmt.Errorf("could not find vSphere credentials in secret %s/%s", secret.Namespace, secret.Name)
		}

		// Add to csi-driver and vsphere-syncer containers the vSphere credentials, as env vars.
		containers := deployment.Spec.Template.Spec.Containers
		for i := range containers {
			if containers[i].Name != "csi-driver" && containers[i].Name != "vsphere-syncer" {
				continue
			}
			containers[i].Env = append(
				containers[i].Env,
				newSecretEnvVar(secretName, "VSPHERE_USER", usernameKey),
				newSecretEnvVar(secretName, "VSPHERE_PASSWORD", passwordKey),
			)
		}
		deployment.Spec.Template.Spec.Containers = containers
		return nil
	}
}

func WithSyncerImageHook() deploymentcontroller.DeploymentHookFunc {
	return func(opSpec *opv1.OperatorSpec, deployment *appsv1.Deployment) error {
		containers := deployment.Spec.Template.Spec.Containers
		for i := range containers {
			if containers[i].Name == "vsphere-syncer" {
				containers[i].Image = os.Getenv(envVMWareVsphereDriverSyncerImage)
			}
		}
		deployment.Spec.Template.Spec.Containers = containers
		return nil
	}
}

// WithLogLevelDeploymentHook sets the X_CSI_DEBUG environment variable to a positive
// value when CR.LogLevel is Debug or higher.
func WithLogLevelDeploymentHook() deploymentcontroller.DeploymentHookFunc {
	return func(opSpec *opv1.OperatorSpec, deployment *appsv1.Deployment) error {
		deployment.Spec.Template.Spec.Containers = maybeAppendDebug(deployment.Spec.Template.Spec.Containers, opSpec)
		return nil
	}
}

// WithLogLevelDaemonSetHook sets the X_CSI_DEBUG environment variable to a positive
// value when CR.LogLevel is Debug or higher.
func WithLogLevelDaemonSetHook() csidrivernodeservicecontroller.DaemonSetHookFunc {
	return func(opSpec *opv1.OperatorSpec, ds *appsv1.DaemonSet) error {
		ds.Spec.Template.Spec.Containers = maybeAppendDebug(ds.Spec.Template.Spec.Containers, opSpec)
		return nil
	}
}

// maybeAppendDebug works like the append() builtin; it returns a new slice of containers
// with the debug env var properly set (or not).
func maybeAppendDebug(containers []v1.Container, opSpec *opv1.OperatorSpec) []v1.Container {
	// Don't set the debug option when the current level is lower than debug
	if loglevel.LogLevelToVerbosity(opSpec.LogLevel) < loglevel.LogLevelToVerbosity(opv1.Debug) {
		return containers
	}
	for i := range containers {
		if containers[i].Name != "csi-driver" && containers[i].Name != "vsphere-syncer" {
			continue
		}
		containers[i].Env = append(
			containers[i].Env,
			v1.EnvVar{Name: "X_CSI_DEBUG", Value: "true"},
		)
	}
	return containers
}

func newSecretEnvVar(secretName, envVarName, key string) v1.EnvVar {
	return v1.EnvVar{
		Name: envVarName,
		ValueFrom: &v1.EnvVarSource{
			SecretKeyRef: &v1.SecretKeySelector{
				LocalObjectReference: v1.LocalObjectReference{
					Name: secretName,
				},
				Key: key,
			},
		},
	}
}
