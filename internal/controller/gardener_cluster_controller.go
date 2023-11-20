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

package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	imv1 "github.com/kyma-project/infrastructure-manager/api/v1"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	lastKubeconfigSyncAnnotation      = "operator.kyma-project.io/last-sync"
	forceKubeconfigRotationAnnotation = "operator.kyma-project.io/force-kubeconfig-rotation"
	clusterCRNameLabel                = "operator.kyma-project.io/cluster-name"
)

// GardenerClusterController reconciles a GardenerCluster object
type GardenerClusterController struct {
	client.Client
	Scheme             *runtime.Scheme
	KubeconfigProvider KubeconfigProvider
	log                logr.Logger
	rotationPeriod     time.Duration
}

func NewGardenerClusterController(mgr ctrl.Manager, kubeconfigProvider KubeconfigProvider, logger logr.Logger, rotationPeriod time.Duration) *GardenerClusterController {
	return &GardenerClusterController{
		Client:             mgr.GetClient(),
		Scheme:             mgr.GetScheme(),
		KubeconfigProvider: kubeconfigProvider,
		log:                logger,
		rotationPeriod:     rotationPeriod,
	}
}

//go:generate mockery --name=KubeconfigProvider
type KubeconfigProvider interface {
	Fetch(ctx context.Context, shootName string) (string, error)
}

//+kubebuilder:rbac:groups=infrastructuremanager.kyma-project.io,resources=gardenerclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;delete
//+kubebuilder:rbac:groups=infrastructuremanager.kyma-project.io,resources=gardenerclusters/finalizers,verbs=update
//+kubebuilder:rbac:groups=infrastructuremanager.kyma-project.io,resources=gardenerclusters/status,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// the GardenerCluster object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.15.0/pkg/reconcile
func (controller *GardenerClusterController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) { //nolint:revive
	controller.log.Info("Starting reconciliation.", loggingContext(req)...)

	var cluster imv1.GardenerCluster

	err := controller.Get(ctx, req.NamespacedName, &cluster)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			err = controller.deleteKubeconfigSecret(req.Name)
		}

		if err == nil {
			controller.log.Info("Secret has been deleted.", loggingContext(req)...)
		}

		return controller.resultWithoutRequeue(), err
	}

	lastSyncTime := time.Now()
	kubeconfigStatus, err := controller.handleKubeconfig(ctx, &cluster, lastSyncTime)
	if err != nil {
		_ = controller.persistStatusChange(ctx, &cluster)
		// if a claster was not found in gardener,
		// CRD should not be rereconciled
		if k8serrors.IsNotFound(err) {
			err = nil
		}
		return controller.resultWithoutRequeue(), err
	}

	// there was a request to rotate the kubeconfig
	if kubeconfigStatus == ksRotated {
		err = controller.removeForceRotationAnnotation(ctx, &cluster)
		if err != nil {
			return controller.resultWithoutRequeue(), err
		}
	}

	if kubeconfigStatus == ksCreated || kubeconfigStatus == ksModified {
		_ = controller.persistStatusChange(ctx, &cluster)
		return controller.resultWithoutRequeue(), nil
	}

	return controller.resultWithRequeue(), nil
}

func loggingContextFromCluster(cluster *imv1.GardenerCluster) []any {
	return []any{"GardenerCluster", cluster.Name, "Namespace", cluster.Namespace}
}

func loggingContext(req ctrl.Request) []any {
	return []any{"GardenerCluster", req.Name, "Namespace", req.Namespace}
}

func (controller *GardenerClusterController) resultWithRequeue() ctrl.Result {
	return ctrl.Result{
		Requeue:      true,
		RequeueAfter: controller.rotationPeriod,
	}
}

func (controller *GardenerClusterController) resultWithoutRequeue() ctrl.Result {
	return ctrl.Result{}
}

func (controller *GardenerClusterController) persistStatusChange(ctx context.Context, cluster *imv1.GardenerCluster) error {
	err := controller.Client.Status().Update(ctx, cluster)
	if err != nil {
		controller.log.Error(err, "status update failed")
	}
	return err
}

func (controller *GardenerClusterController) deleteKubeconfigSecret(clusterCRName string) error {
	selector := client.MatchingLabels(map[string]string{
		clusterCRNameLabel: clusterCRName,
	})

	var secretList corev1.SecretList
	err := controller.Client.List(context.TODO(), &secretList, selector)
	if err != nil {
		return err
	}

	if len(secretList.Items) != 1 {
		return errors.Errorf("unexpected numer of secrets found for cluster CR `%s`", clusterCRName)
	}

	return controller.Client.Delete(context.TODO(), &secretList.Items[0])
}

func (controller *GardenerClusterController) getSecret(shootName string) (*corev1.Secret, error) {
	var secretList corev1.SecretList

	shootNameSelector := client.MatchingLabels(map[string]string{
		"kyma-project.io/shoot-name": shootName,
	})

	err := controller.List(context.Background(), &secretList, shootNameSelector)
	if err != nil {
		return nil, err
	}

	size := len(secretList.Items)

	if size == 0 {
		return nil, k8serrors.NewNotFound(schema.GroupResource{}, "") //nolint:all TODO: update with valid GroupResource
	}

	if size > 1 {
		return nil, fmt.Errorf("unexpected numer of secrets found for shoot `%s`", shootName)
	}

	return &secretList.Items[0], nil
}

type kubeconfigStatus int

const (
	ksZero kubeconfigStatus = iota
	ksCreated
	ksModified
	ksRotated
)

func (controller *GardenerClusterController) handleKubeconfig(ctx context.Context, cluster *imv1.GardenerCluster, lastSyncTime time.Time) (kubeconfigStatus, error) {
	existingSecret, err := controller.getSecret(cluster.Spec.Shoot.Name)
	if err != nil && !k8serrors.IsNotFound(err) {
		cluster.UpdateConditionForErrorState(imv1.ConditionTypeKubeconfigManagement, imv1.ConditionReasonFailedToGetSecret, err)
		return ksZero, err
	}

	kubeconfig, err := controller.KubeconfigProvider.Fetch(ctx, cluster.Spec.Shoot.Name)
	if err != nil {
		cluster.UpdateConditionForErrorState(imv1.ConditionTypeKubeconfigManagement, imv1.ConditionReasonFailedToGetKubeconfig, err)
		return ksZero, err
	}

	if secretRotationForced(cluster) {
		message := fmt.Sprintf("Rotation of secret %s in namespace %s forced.", cluster.Spec.Kubeconfig.Secret.Name, cluster.Spec.Kubeconfig.Secret.Namespace)
		controller.log.Info(message, loggingContextFromCluster(cluster)...)

		// delete secret containing kubeconfig to be rotated
		if err := controller.removeKubeconfig(ctx, cluster, existingSecret); err != nil {
			cluster.UpdateConditionForErrorState(imv1.ConditionTypeKubeconfigManagement, imv1.ConditionReasonFailedToDeleteSecret, err)
			return ksZero, err
		}

		return ksRotated, nil
	}

	if !secretNeedsToBeRotated(cluster, existingSecret, controller.rotationPeriod) {
		message := fmt.Sprintf("Secret %s in namespace %s does not need to be rotated yet.", cluster.Spec.Kubeconfig.Secret.Name, cluster.Spec.Kubeconfig.Secret.Namespace)
		controller.log.Info(message, loggingContextFromCluster(cluster)...)
		return ksZero, nil
	}

	if existingSecret != nil {
		return ksModified, controller.updateExistingSecret(ctx, kubeconfig, cluster, existingSecret, lastSyncTime)
	}

	return ksCreated, controller.createNewSecret(ctx, kubeconfig, cluster, lastSyncTime)
}

func secretNeedsToBeRotated(cluster *imv1.GardenerCluster, secret *corev1.Secret, rotationPeriod time.Duration) bool {
	return secretRotationTimePassed(secret, rotationPeriod) || secretRotationForced(cluster)
}

func secretRotationTimePassed(secret *corev1.Secret, rotationPeriod time.Duration) bool {
	const rotationPeriodRatio = 0.95

	if secret == nil {
		return true
	}

	annotations := secret.GetAnnotations()

	_, found := annotations[lastKubeconfigSyncAnnotation]

	if !found {
		return true
	}

	lastSyncTimeString := annotations[lastKubeconfigSyncAnnotation]
	lastSyncTime, err := time.Parse(time.RFC3339, lastSyncTimeString)
	if err != nil {
		return true
	}
	now := time.Now()
	alreadyValidFor := now.Sub(lastSyncTime)

	return alreadyValidFor.Minutes() >= rotationPeriodRatio*rotationPeriod.Minutes()
}

func secretRotationForced(cluster *imv1.GardenerCluster) bool {
	annotations := cluster.GetAnnotations()
	if annotations == nil {
		return false
	}

	_, found := annotations[forceKubeconfigRotationAnnotation]
	return found
}

func (controller *GardenerClusterController) createNewSecret(ctx context.Context, kubeconfig string, cluster *imv1.GardenerCluster, lastSyncTime time.Time) error {
	newSecret := controller.newSecret(*cluster, kubeconfig, lastSyncTime)
	err := controller.Create(ctx, &newSecret)
	if err != nil {
		cluster.UpdateConditionForErrorState(imv1.ConditionTypeKubeconfigManagement, imv1.ConditionReasonFailedToCreateSecret, err)
		return err
	}

	cluster.UpdateConditionForReadyState(imv1.ConditionTypeKubeconfigManagement, imv1.ConditionReasonKubeconfigSecretCreated, metav1.ConditionTrue)

	message := fmt.Sprintf("Secret %s has been created in %s namespace.", newSecret.Name, newSecret.Namespace)
	controller.log.Info(message, loggingContextFromCluster(cluster)...)

	return nil
}

func (controller *GardenerClusterController) removeKubeconfig(ctx context.Context, cluster *imv1.GardenerCluster, existingSecret *corev1.Secret) error {
	if existingSecret == nil {
		return nil
	}

	delete(existingSecret.Data, cluster.Spec.Kubeconfig.Secret.Key)

	if annotations := existingSecret.GetAnnotations(); annotations != nil {
		delete(annotations, lastKubeconfigSyncAnnotation)
	}

	return controller.Update(ctx, existingSecret)
}

func (controller *GardenerClusterController) updateExistingSecret(ctx context.Context, kubeconfig string, cluster *imv1.GardenerCluster, existingSecret *corev1.Secret, lastSyncTime time.Time) error {
	if existingSecret.Data == nil {
		existingSecret.Data = map[string][]byte{}
	}
	existingSecret.Data[cluster.Spec.Kubeconfig.Secret.Key] = []byte(kubeconfig)
	annotations := existingSecret.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}

	annotations[lastKubeconfigSyncAnnotation] = lastSyncTime.UTC().Format(time.RFC3339)
	existingSecret.SetAnnotations(annotations)

	err := controller.Update(ctx, existingSecret)
	if err != nil {
		cluster.UpdateConditionForErrorState(imv1.ConditionTypeKubeconfigManagement, imv1.ConditionReasonFailedToUpdateSecret, err)

		return err
	}

	cluster.UpdateConditionForReadyState(imv1.ConditionTypeKubeconfigManagement, imv1.ConditionReasonKubeconfigSecretRotated, metav1.ConditionTrue)

	message := fmt.Sprintf("Secret %s has been updated in %s namespace.", existingSecret.Name, existingSecret.Namespace)
	controller.log.Info(message, loggingContextFromCluster(cluster)...)

	return nil
}

func (controller *GardenerClusterController) removeForceRotationAnnotation(ctx context.Context, cluster *imv1.GardenerCluster) error {
	secretRotationForced := secretRotationForced(cluster)

	if secretRotationForced {
		key := types.NamespacedName{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
		}
		var clusterToUpdate imv1.GardenerCluster

		err := controller.Client.Get(ctx, key, &clusterToUpdate)
		if err != nil {
			return err
		}

		annotations := clusterToUpdate.GetAnnotations()
		delete(annotations, forceKubeconfigRotationAnnotation)
		clusterToUpdate.SetAnnotations(annotations)

		return controller.Client.Update(ctx, &clusterToUpdate)
	}

	return nil
}

func (controller *GardenerClusterController) newSecret(cluster imv1.GardenerCluster, kubeconfig string, lastSyncTime time.Time) corev1.Secret {
	labels := map[string]string{}

	for key, val := range cluster.Labels {
		labels[key] = val
	}
	labels["operator.kyma-project.io/managed-by"] = "infrastructure-manager"
	labels[clusterCRNameLabel] = cluster.Name

	return corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        cluster.Spec.Kubeconfig.Secret.Name,
			Namespace:   cluster.Spec.Kubeconfig.Secret.Namespace,
			Labels:      labels,
			Annotations: map[string]string{lastKubeconfigSyncAnnotation: lastSyncTime.UTC().Format(time.RFC3339)},
		},
		StringData: map[string]string{cluster.Spec.Kubeconfig.Secret.Key: kubeconfig},
	}
}

// SetupWithManager sets up the controller with the Manager.
func (controller *GardenerClusterController) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&imv1.GardenerCluster{}, builder.WithPredicates()).
		Complete(controller)
}
