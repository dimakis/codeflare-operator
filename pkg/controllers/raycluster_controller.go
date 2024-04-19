/*
Copyright 2023.

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

package controllers

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"fmt"

	dsciv1 "github.com/opendatahub-io/opendatahub-operator/v2/apis/dscinitialization/v1"
	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	coreapply "k8s.io/client-go/applyconfigurations/core/v1"
	metav1apply "k8s.io/client-go/applyconfigurations/meta/v1"
	v1 "k8s.io/client-go/applyconfigurations/meta/v1"
	networkingapply "k8s.io/client-go/applyconfigurations/networking/v1"
	rbacapply "k8s.io/client-go/applyconfigurations/rbac/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	routev1 "github.com/openshift/api/route/v1"
	routeapply "github.com/openshift/client-go/route/applyconfigurations/route/v1"
	routev1client "github.com/openshift/client-go/route/clientset/versioned/typed/route/v1"

	"github.com/project-codeflare/codeflare-operator/pkg/config"
)

// RayClusterReconciler reconciles a RayCluster object
type RayClusterReconciler struct {
	client.Client
	kubeClient  *kubernetes.Clientset
	routeClient *routev1client.RouteV1Client
	Scheme      *runtime.Scheme
	CookieSalt  string
	Config      *config.KubeRayConfiguration
	IsOpenShift bool
}

const (
	requeueTime            = 10
	controllerName         = "codeflare-raycluster-controller"
	oAuthFinalizer         = "ray.openshift.ai/oauth-finalizer"
	oAuthServicePort       = 443
	oAuthServicePortName   = "oauth-proxy"
	ingressServicePortName = "dashboard"
	logRequeueing          = "requeueing"
)

var (
	deletePolicy  = metav1.DeletePropagationForeground
	deleteOptions = client.DeleteOptions{PropagationPolicy: &deletePolicy}
)

// +kubebuilder:rbac:groups=ray.io,resources=rayclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ray.io,resources=rayclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ray.io,resources=rayclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes;routes/custom-host,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;create;patch;delete;get
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create;
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create;
// +kubebuilder:rbac:groups=dscinitialization.opendatahub.io,resources=dscinitializations,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the RayCluster object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.15.3/pkg/reconcile

func (r *RayClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	cluster := &rayv1.RayCluster{}

	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		if !errors.IsNotFound(err) {
			logger.Error(err, "Error getting RayCluster resource")
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if cluster.ObjectMeta.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(cluster, oAuthFinalizer) {
			logger.Info("Add a finalizer", "finalizer", oAuthFinalizer)
			controllerutil.AddFinalizer(cluster, oAuthFinalizer)
			if err := r.Update(ctx, cluster); err != nil {
				// this log is info level since errors are not fatal and are expected
				logger.Info("WARN: Failed to update RayCluster with finalizer", "error", err.Error(), logRequeueing, true)
				return ctrl.Result{RequeueAfter: requeueTime}, err
			}
		}
	} else if controllerutil.ContainsFinalizer(cluster, oAuthFinalizer) {
		err := client.IgnoreNotFound(r.Client.Delete(
			ctx,
			&rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name: crbNameFromCluster(cluster),
				},
			},
			&deleteOptions,
		))
		if err != nil {
			logger.Error(err, "Failed to remove OAuth ClusterRoleBinding.", logRequeueing, true)
			return ctrl.Result{RequeueAfter: requeueTime}, err
		}
		controllerutil.RemoveFinalizer(cluster, oAuthFinalizer)
		if err := r.Update(ctx, cluster); err != nil {
			logger.Error(err, "Failed to remove finalizer from RayCluster", logRequeueing, true)
			return ctrl.Result{RequeueAfter: requeueTime}, err
		}
		logger.Info("Successfully removed finalizer.", logRequeueing, false)
		return ctrl.Result{}, nil
	}

	if cluster.Status.State != "suspended" && isRayDashboardOAuthEnabled(r.Config) && r.IsOpenShift {
		logger.Info("Creating OAuth Objects")
		_, err := r.routeClient.Routes(cluster.Namespace).Apply(ctx, desiredClusterRoute(cluster), metav1.ApplyOptions{FieldManager: controllerName, Force: true})
		if err != nil {
			logger.Error(err, "Failed to update OAuth Route")
			return ctrl.Result{RequeueAfter: requeueTime}, err
		}

		_, err = r.kubeClient.CoreV1().Secrets(cluster.Namespace).Apply(ctx, desiredOAuthSecret(cluster, r), metav1.ApplyOptions{FieldManager: controllerName, Force: true})
		if err != nil {
			logger.Error(err, "Failed to create OAuth Secret")
			return ctrl.Result{RequeueAfter: requeueTime}, err
		}

		_, err = r.kubeClient.CoreV1().Services(cluster.Namespace).Apply(ctx, desiredOAuthService(cluster), metav1.ApplyOptions{FieldManager: controllerName, Force: true})
		if err != nil {
			logger.Error(err, "Failed to update OAuth Service")
			return ctrl.Result{RequeueAfter: requeueTime}, err
		}

		_, err = r.kubeClient.CoreV1().ServiceAccounts(cluster.Namespace).Apply(ctx, desiredServiceAccount(cluster), metav1.ApplyOptions{FieldManager: controllerName, Force: true})
		if err != nil {
			logger.Error(err, "Failed to update OAuth ServiceAccount")
			return ctrl.Result{RequeueAfter: requeueTime}, err
		}

		_, err = r.kubeClient.RbacV1().ClusterRoleBindings().Apply(ctx, desiredOAuthClusterRoleBinding(cluster), metav1.ApplyOptions{FieldManager: controllerName, Force: true})
		if err != nil {
			logger.Error(err, "Failed to update OAuth ClusterRoleBinding")
			return ctrl.Result{RequeueAfter: requeueTime}, err
		}

		logger.Info("Creating RayClient Route")
		_, err = r.routeClient.Routes(cluster.Namespace).Apply(ctx, desiredRayClientRoute(cluster), metav1.ApplyOptions{FieldManager: controllerName, Force: true})
		if err != nil {
			logger.Error(err, "Failed to update RayClient Route")
			return ctrl.Result{RequeueAfter: requeueTime}, err
		}

	} else if cluster.Status.State != "suspended" && !isRayDashboardOAuthEnabled(r.Config) && !r.IsOpenShift {
		logger.Info("We detected being on Vanilla Kubernetes!")
		logger.Info("Creating Dashboard Ingress")
		dashboardName := dashboardNameFromCluster(cluster)
		dashboardIngressHost, err := getIngressHost(r.Config, cluster, dashboardName)
		if err != nil {
			return ctrl.Result{RequeueAfter: requeueTime}, err
		}
		_, err = r.kubeClient.NetworkingV1().Ingresses(cluster.Namespace).Apply(ctx, desiredClusterIngress(cluster, dashboardIngressHost), metav1.ApplyOptions{FieldManager: controllerName, Force: true})
		if err != nil {
			// This log is info level since errors are not fatal and are expected
			logger.Info("WARN: Failed to update Dashboard Ingress", "error", err.Error(), logRequeueing, true)
			return ctrl.Result{RequeueAfter: requeueTime}, err
		}
		logger.Info("Creating RayClient Ingress")
		rayClientName := rayClientNameFromCluster(cluster)
		rayClientIngressHost, err := getIngressHost(r.Config, cluster, rayClientName)
		if err != nil {
			return ctrl.Result{RequeueAfter: requeueTime}, err
		}
		_, err = r.kubeClient.NetworkingV1().Ingresses(cluster.Namespace).Apply(ctx, desiredRayClientIngress(cluster, rayClientIngressHost), metav1.ApplyOptions{FieldManager: controllerName, Force: true})
		if err != nil {
			logger.Error(err, "Failed to update RayClient Ingress")
			return ctrl.Result{RequeueAfter: requeueTime}, err
		}
	}

	// Locate the KubeRay operator deployment:
	// - First try to get the ODH / RHOAI application namespace from the DSCInitialization
	// - Or fallback to the well-known defaults
	var kubeRayNamespaces []string
	dsci := &dsciv1.DSCInitialization{}
	err := r.Client.Get(ctx, client.ObjectKey{Name: "default-dsci"}, dsci)
	if errors.IsNotFound(err) {
		kubeRayNamespaces = []string{"opendatahub", "redhat-ods-applications"}
	} else if err != nil {
		return ctrl.Result{}, err
	} else {
		kubeRayNamespaces = []string{dsci.Spec.ApplicationsNamespace}
	}

	_, err = r.kubeClient.NetworkingV1().NetworkPolicies(cluster.Namespace).Apply(ctx, desiredNetworkPolicy(cluster, kubeRayNamespaces), metav1.ApplyOptions{FieldManager: controllerName, Force: true})
	if err != nil {
		logger.Error(err, "Failed to update NetworkPolicy")
	}

	return ctrl.Result{}, nil
}

// getIngressHost generates the cluster URL string based on the cluster type, RayCluster, and ingress domain.
func getIngressHost(cfg *config.KubeRayConfiguration, cluster *rayv1.RayCluster, ingressNameFromCluster string) (string, error) {
	ingressDomain := ""
	if cfg != nil && cfg.IngressDomain != "" {
		ingressDomain = cfg.IngressDomain
	} else {
		return "", fmt.Errorf("missing IngressDomain configuration in ConfigMap 'codeflare-operator-config'")
	}
	return fmt.Sprintf("%s-%s.%s", ingressNameFromCluster, cluster.Namespace, ingressDomain), nil
}

func isRayDashboardOAuthEnabled(cfg *config.KubeRayConfiguration) bool {
	if cfg != nil && cfg.RayDashboardOAuthEnabled != nil {
		return *cfg.RayDashboardOAuthEnabled
	}
	return true
}

func crbNameFromCluster(cluster *rayv1.RayCluster) string {
	return cluster.Name + "-" + cluster.Namespace + "-auth" // NOTE: potential naming conflicts ie {name: foo, ns: bar-baz} and {name: foo-bar, ns: baz}
}

func desiredOAuthClusterRoleBinding(cluster *rayv1.RayCluster) *rbacapply.ClusterRoleBindingApplyConfiguration {
	return rbacapply.ClusterRoleBinding(
		crbNameFromCluster(cluster)).
		WithLabels(map[string]string{"ray.io/cluster-name": cluster.Name}).
		WithSubjects(
			rbacapply.Subject().
				WithKind("ServiceAccount").
				WithName(oauthServiceAccountNameFromCluster(cluster)).
				WithNamespace(cluster.Namespace),
		).
		WithRoleRef(
			rbacapply.RoleRef().
				WithAPIGroup("rbac.authorization.k8s.io").
				WithKind("ClusterRole").
				WithName("system:auth-delegator"),
		)
}

func oauthServiceAccountNameFromCluster(cluster *rayv1.RayCluster) string {
	return cluster.Name + "-oauth-proxy"
}

func desiredServiceAccount(cluster *rayv1.RayCluster) *coreapply.ServiceAccountApplyConfiguration {
	return coreapply.ServiceAccount(oauthServiceAccountNameFromCluster(cluster), cluster.Namespace).
		WithLabels(map[string]string{"ray.io/cluster-name": cluster.Name}).
		WithAnnotations(map[string]string{
			"serviceaccounts.openshift.io/oauth-redirectreference.first": "" +
				`{"kind":"OAuthRedirectReference","apiVersion":"v1",` +
				`"reference":{"kind":"Route","name":"` + dashboardNameFromCluster(cluster) + `"}}`,
		}).
		WithOwnerReferences(
			v1.OwnerReference().WithUID(cluster.UID).WithName(cluster.Name).WithKind(cluster.Kind).WithAPIVersion(cluster.APIVersion),
		)
}

func dashboardNameFromCluster(cluster *rayv1.RayCluster) string {
	return "ray-dashboard-" + cluster.Name
}

func rayClientNameFromCluster(cluster *rayv1.RayCluster) string {
	return "rayclient-" + cluster.Name
}

func desiredClusterRoute(cluster *rayv1.RayCluster) *routeapply.RouteApplyConfiguration {
	return routeapply.Route(dashboardNameFromCluster(cluster), cluster.Namespace).
		WithLabels(map[string]string{"ray.io/cluster-name": cluster.Name}).
		WithSpec(routeapply.RouteSpec().
			WithTo(routeapply.RouteTargetReference().WithKind("Service").WithName(oauthServiceNameFromCluster(cluster))).
			WithPort(routeapply.RoutePort().WithTargetPort(intstr.FromString((oAuthServicePortName)))).
			WithTLS(routeapply.TLSConfig().
				WithInsecureEdgeTerminationPolicy(routev1.InsecureEdgeTerminationPolicyRedirect).
				WithTermination(routev1.TLSTerminationReencrypt),
			),
		).
		WithOwnerReferences(
			v1.OwnerReference().WithUID(cluster.UID).WithName(cluster.Name).WithKind(cluster.Kind).WithAPIVersion(cluster.APIVersion),
		)
}

func oauthServiceNameFromCluster(cluster *rayv1.RayCluster) string {
	return cluster.Name + "-oauth"
}

func oauthServiceTLSSecretName(cluster *rayv1.RayCluster) string {
	return cluster.Name + "-proxy-tls-secret"
}

func desiredOAuthService(cluster *rayv1.RayCluster) *coreapply.ServiceApplyConfiguration {
	return coreapply.Service(oauthServiceNameFromCluster(cluster), cluster.Namespace).
		WithLabels(map[string]string{"ray.io/cluster-name": cluster.Name}).
		WithAnnotations(map[string]string{"service.beta.openshift.io/serving-cert-secret-name": oauthServiceTLSSecretName(cluster)}).
		WithSpec(
			coreapply.ServiceSpec().
				WithPorts(
					coreapply.ServicePort().
						WithName(oAuthServicePortName).
						WithPort(oAuthServicePort).
						WithTargetPort(intstr.FromString(oAuthServicePortName)).
						WithProtocol(corev1.ProtocolTCP),
				).
				WithSelector(map[string]string{"ray.io/cluster": cluster.Name, "ray.io/node-type": "head"}),
		).
		WithOwnerReferences(
			v1.OwnerReference().WithUID(cluster.UID).WithName(cluster.Name).WithKind(cluster.Kind).WithAPIVersion(cluster.APIVersion),
		)
}

func oauthSecretNameFromCluster(cluster *rayv1.RayCluster) string {
	return cluster.Name + "-oauth-config"
}

// desiredOAuthSecret defines the desired OAuth secret object
func desiredOAuthSecret(cluster *rayv1.RayCluster, r *RayClusterReconciler) *coreapply.SecretApplyConfiguration {
	// Generate the cookie secret for the OAuth proxy
	hasher := sha1.New() // REVIEW is SHA1 okay here?
	hasher.Write([]byte(cluster.Name + r.CookieSalt))
	cookieSecret := base64.StdEncoding.EncodeToString(hasher.Sum(nil))

	return coreapply.Secret(oauthSecretNameFromCluster(cluster), cluster.Namespace).
		WithLabels(map[string]string{"ray.io/cluster-name": cluster.Name}).
		WithStringData(map[string]string{"cookie_secret": cookieSecret}).
		WithOwnerReferences(
			v1.OwnerReference().WithUID(cluster.UID).WithName(cluster.Name).WithKind(cluster.Kind).WithAPIVersion(cluster.APIVersion),
		)
	// Create a Kubernetes secret to store the cookie secret
}

func desiredNetworkPolicy(cluster *rayv1.RayCluster, kubeRayNamespaces []string) *networkingapply.NetworkPolicyApplyConfiguration {
	return networkingapply.NetworkPolicy(cluster.Name, cluster.Namespace).
		WithLabels(map[string]string{"ray.io/cluster-name": cluster.Name}).
		WithSpec(networkingapply.NetworkPolicySpec().
			WithPodSelector(metav1apply.LabelSelector().WithMatchLabels(map[string]string{"ray.io/cluster": cluster.Name, "ray.io/node-type": "head"})).
			WithIngress(
				networkingapply.NetworkPolicyIngressRule().
					WithPorts(
						networkingapply.NetworkPolicyPort().WithProtocol(corev1.ProtocolTCP).WithPort(intstr.FromInt(6379)),
						networkingapply.NetworkPolicyPort().WithProtocol(corev1.ProtocolTCP).WithPort(intstr.FromInt(10001)),
						networkingapply.NetworkPolicyPort().WithProtocol(corev1.ProtocolTCP).WithPort(intstr.FromInt(8080)),
						networkingapply.NetworkPolicyPort().WithProtocol(corev1.ProtocolTCP).WithPort(intstr.FromInt(8265)),
					).WithFrom(
					networkingapply.NetworkPolicyPeer().WithPodSelector(metav1apply.LabelSelector()),
				),
				networkingapply.NetworkPolicyIngressRule().
					WithFrom(
						networkingapply.NetworkPolicyPeer().WithPodSelector(metav1apply.LabelSelector().
							WithMatchLabels(map[string]string{"app.kubernetes.io/component": "kuberay-operator"})).
							WithNamespaceSelector(metav1apply.LabelSelector().
								WithMatchExpressions(metav1apply.LabelSelectorRequirement().
									WithKey(corev1.LabelMetadataName).
									WithOperator(metav1.LabelSelectorOpIn).
									WithValues(kubeRayNamespaces...)))).
					WithPorts(
						networkingapply.NetworkPolicyPort().WithProtocol(corev1.ProtocolTCP).WithPort(intstr.FromInt(8265)),
						networkingapply.NetworkPolicyPort().WithProtocol(corev1.ProtocolTCP).WithPort(intstr.FromInt(10001)),
					),
				networkingapply.NetworkPolicyIngressRule().
					WithPorts(
						networkingapply.NetworkPolicyPort().WithProtocol(corev1.ProtocolTCP).WithPort(intstr.FromInt(8080)),
					).
					WithFrom(
						networkingapply.NetworkPolicyPeer().WithNamespaceSelector(metav1apply.LabelSelector().
							WithMatchExpressions(metav1apply.LabelSelectorRequirement().
								WithKey(corev1.LabelMetadataName).
								WithOperator(metav1.LabelSelectorOpIn).
								WithValues("openshift-monitoring"))),
					),
				networkingapply.NetworkPolicyIngressRule().
					WithPorts(
						networkingapply.NetworkPolicyPort().WithProtocol(corev1.ProtocolTCP).WithPort(intstr.FromInt(8443)),
					),
			),
		).
		WithOwnerReferences(
			v1.OwnerReference().WithUID(cluster.UID).WithName(cluster.Name).WithKind(cluster.Kind).WithAPIVersion(cluster.APIVersion),
		)
}

// SetupWithManager sets up the controller with the Manager.
func (r *RayClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.kubeClient = kubernetes.NewForConfigOrDie(mgr.GetConfig())
	r.routeClient = routev1client.NewForConfigOrDie(mgr.GetConfig())
	b := make([]byte, 16)
	_, err := rand.Read(b)
	if err != nil {
		return err
	}
	r.CookieSalt = string(b)
	return ctrl.NewControllerManagedBy(mgr).
		Named(controllerName).
		For(&rayv1.RayCluster{}).
		Complete(r)
}
