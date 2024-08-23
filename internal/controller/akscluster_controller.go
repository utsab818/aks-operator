/*
Copyright 2024.

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

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	azurev1 "github.com/utsab818/aks-operator/api/v1"
)

// AKSClusterReconciler reconciles a AKSCluster object
type AKSClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=azure.utsabazure.com,resources=aksclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=azure.utsabazure.com,resources=aksclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=azure.utsabazure.com,resources=aksclusters/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the AKSCluster object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.18.2/pkg/reconcile
func (r *AKSClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the AKSCluster instance
	var aksCluster azurev1.AKSCluster
	if err := r.Get(ctx, req.NamespacedName, &aksCluster); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		// Erro reading the object - requeue the request.
		return ctrl.Result{}, err
	}

	// Check if the AKS cluster is already created by checking the status
	if aksCluster.Status.ClusterID == "" {
		log.Info("Creating AKS Cluster in Azure", "ClusterName", aksCluster.Spec.Name)

		clusterID, provisioningState, err := r.createAKSClusterInAzure(ctx, &aksCluster)
		if err != nil {
			log.Error(err, "Failed to create AKS Cluster in Azure")
			return ctrl.Result{}, err
		}

		// Update the status with the new cluster information
		aksCluster.Status.ClusterID = clusterID
		aksCluster.Status.ProvisioningState = provisioningState
		if err := r.Status().Update(ctx, &aksCluster); err != nil {
			log.Error(err, "Failed to update AKSCluster status")
			return ctrl.Result{}, err
		}

		// Requeue the request to check the provisioning state later
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Check the provisioning state and update status if necessary
	log.Info("Checking AKS Cluster provisioning state", "ClusterName", aksCluster.Spec.Name)
	provisioningState, err := r.getAKSClusterProvisioningState(ctx, &aksCluster)
	if err != nil {
		log.Error(err, "Failed to get AKS Cluster provisioning state")
		return ctrl.Result{}, err
	}

	aksCluster.Status.ProvisioningState = provisioningState
	if err = r.Status().Update(ctx, &aksCluster); err != nil {
		log.Error(err, "Failed to update AKSCluster status")
		return ctrl.Result{}, err
	}

	// Requeue the request to check the provisioning state again if it's still in progress.
	if provisioningState != "Succeeded" {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	return ctrl.Result{}, nil
}

func (r *AKSClusterReconciler) createAKSClusterInAzure(ctx context.Context, aksCluster *azurev1.AKSCluster) (string, string, error) {
	// Authenticate with Azure
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return "", "", fmt.Errorf("failed to obtain a credential: %v", err)
	}

	// Create a new managed clusters client
	client, err := armcontainerservice.NewManagedClustersClient("<subscription-id>", cred, nil)
	if err != nil {
		return "", "", fmt.Errorf("failed to create managed clusters client: %v", err)
	}

	// Create the managed cluster
	poller, err := client.BeginCreateOrUpdate(ctx, aksCluster.Spec.ResourceGroup, aksCluster.Spec.Name, armcontainerservice.ManagedCluster{
		Location: to.Ptr(aksCluster.Spec.Location),
		Properties: &armcontainerservice.ManagedClusterProperties{
			KubernetesVersion: to.Ptr(aksCluster.Spec.KubernetesVersion),
			DNSPrefix:         to.Ptr(aksCluster.Spec.DNSPrefix),
			AgentPoolProfiles: []*armcontainerservice.ManagedClusterAgentPoolProfile{
				{
					Name:              to.Ptr("agentpool"),
					Count:             to.Ptr[int32](int32(aksCluster.Spec.NodeCount)),
					VMSize:            to.Ptr(aksCluster.Spec.NodeSize),
					Mode:              to.Ptr(armcontainerservice.AgentPoolModeSystem),
					EnableAutoScaling: to.Ptr(aksCluster.Spec.AutoScaling),
					MinCount:          to.Ptr[int32](int32(aksCluster.Spec.MinNodeCount)),
					MaxCount:          to.Ptr[int32](int32(aksCluster.Spec.MaxNodeCount)),
				},
			},
			ServicePrincipalProfile: &armcontainerservice.ManagedClusterServicePrincipalProfile{
				ClientID: to.Ptr("<your-client-id>"),
				Secret:   to.Ptr("<your-secret-password>"),
			},
		},
	}, nil)
	if err != nil {
		return "", "", fmt.Errorf("failed to begin creating or updating managed cluster: %v", err)
	}

	// Wait for the operation to complete
	resp, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return "", "", fmt.Errorf("failed to create or update managed cluster: %v", err)
	}

	return *resp.ManagedCluster.ID, *resp.ManagedCluster.Properties.ProvisioningState, nil
}

func (r *AKSClusterReconciler) getAKSClusterProvisioningState(ctx context.Context, aksCluster *azurev1.AKSCluster) (string, error) {
	// Kubernetes controllers, including those built with Kubebuilder, are generally designed to be stateless.
	// Each reconciliation loop (the process where the controller checks and updates the state of the resources
	// it manages) is isolated, meaning that no state is maintained between successive loops.
	// As a result, you need to re-authenticate and create any necessary clients (such as the ManagedClustersClient)
	// each time the Reconcile function is called. This ensures that the operator always has the latest credentials
	// and an active connection to Azure.

	// Authenticate with Azure
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return "", fmt.Errorf("failed to obtain a credential: %v", err)
	}

	// Create a new new managed clusters client
	client, err := armcontainerservice.NewManagedClustersClient("<subscription-id>", cred, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create managed clusters client: %v", err)
	}

	// Get the managed cluster
	resp, err := client.Get(ctx, aksCluster.Spec.ResourceGroup, aksCluster.Spec.Name, nil)
	if err != nil {
		return "", fmt.Errorf("failed to get managed cluster %v", err)
	}

	return *resp.ManagedCluster.Properties.ProvisioningState, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AKSClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&azurev1.AKSCluster{}).
		Complete(r)
}
