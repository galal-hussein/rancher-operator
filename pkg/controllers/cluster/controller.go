package cluster

import (
	"context"

	"github.com/rancher/norman/types/convert"
	v1 "github.com/rancher/rancher-operator/pkg/apis/rancher.cattle.io/v1"
	"github.com/rancher/rancher-operator/pkg/clients"
	mgmtcontrollers "github.com/rancher/rancher-operator/pkg/generated/controllers/management.cattle.io/v3"
	rocontrollers "github.com/rancher/rancher-operator/pkg/generated/controllers/rancher.cattle.io/v1"
	"github.com/rancher/rancher-operator/pkg/kubeconfig"
	v3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	"github.com/rancher/wrangler/pkg/condition"
	corecontrollers "github.com/rancher/wrangler/pkg/generated/controllers/core/v1"
	"github.com/rancher/wrangler/pkg/generic"
	"github.com/rancher/wrangler/pkg/kstatus"
	"github.com/rancher/wrangler/pkg/name"
	"github.com/rancher/wrangler/pkg/relatedresource"
	apierror "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	byCluster = "by-cluster"
)

type handler struct {
	rclusterCache     mgmtcontrollers.ClusterCache
	rclusters         mgmtcontrollers.ClusterClient
	clusterTokenCache mgmtcontrollers.ClusterRegistrationTokenCache
	clusterTokens     mgmtcontrollers.ClusterRegistrationTokenClient
	clusters          rocontrollers.ClusterController
	secretCache       corecontrollers.SecretCache
	kubeconfigManager *kubeconfig.Manager
}

func Register(
	ctx context.Context,
	clients *clients.Clients) {
	h := handler{
		rclusterCache:     clients.Management.Cluster().Cache(),
		rclusters:         clients.Management.Cluster(),
		clusterTokenCache: clients.Management.ClusterRegistrationToken().Cache(),
		clusterTokens:     clients.Management.ClusterRegistrationToken(),
		clusters:          clients.Cluster(),
		secretCache:       clients.Core.Secret().Cache(),
		kubeconfigManager: kubeconfig.New(clients),
	}

	clients.Cluster().OnChange(ctx, "cluster-update", h.onChange)
	rocontrollers.RegisterClusterGeneratingHandler(ctx,
		clients.Cluster(),
		clients.Apply.WithCacheTypes(clients.Management.Cluster(),
			clients.Core.Secret()),
		"Created",
		"cluster-create",
		h.generateCluster,
		&generic.GeneratingHandlerOptions{
			AllowClusterScoped: true,
		},
	)

	clusterCache := clients.Cluster().Cache()
	relatedresource.Watch(ctx, "cluster-watch", func(namespace, name string, obj runtime.Object) ([]relatedresource.Key, error) {
		cluster, ok := obj.(*v3.Cluster)
		if !ok {
			return nil, nil
		}
		operatorClusters, err := clusterCache.GetByIndex(byCluster, cluster.Name)
		if err != nil || len(operatorClusters) == 0 {
			// ignore
			return nil, nil
		}
		return []relatedresource.Key{
			{
				Namespace: operatorClusters[0].Namespace,
				Name:      operatorClusters[0].Name,
			},
		}, nil
	}, clients.Cluster(), clients.Management.Cluster())

	clusterCache.AddIndexer(byCluster, func(obj *v1.Cluster) ([]string, error) {
		if obj.Status.ClusterName == "" {
			return nil, nil
		}
		return []string{obj.Status.ClusterName}, nil
	})
}

func (h *handler) onChange(key string, cluster *v1.Cluster) (*v1.Cluster, error) {
	if cluster == nil {
		return cluster, nil
	}

	if cluster.Spec.ControlPlaneEndpoint == nil {
		// just set to something, this doesn't really make sense to me
		cluster = cluster.DeepCopy()
		cluster.Spec.ControlPlaneEndpoint = &v1.Endpoint{
			Host: "localhost",
			Port: 6443,
		}
		return h.clusters.Update(cluster)
	}
	return cluster, nil
}

func (h *handler) generateCluster(cluster *v1.Cluster, status v1.ClusterStatus) ([]runtime.Object, v1.ClusterStatus, error) {
	switch {
	case cluster.Spec.ImportedConfig != nil:
		return h.importCluster(cluster, status, v3.ClusterSpec{
			ImportedConfig: &v3.ImportedConfig{},
		})
	case cluster.Spec.ReferencedConfig != nil:
		return h.referenceCluster(cluster, status)
	case cluster.Spec.RancherKubernetesEngineConfig != nil:
		return h.createCluster(cluster, status, v3.ClusterSpec{
			ClusterSpecBase: v3.ClusterSpecBase{
				RancherKubernetesEngineConfig: cluster.Spec.RancherKubernetesEngineConfig,
				LocalClusterAuthEndpoint:      cluster.Spec.LocalClusterAuthEndpoint,
			},
		})
	case cluster.Spec.EKSConfig != nil:
		return h.createCluster(cluster, status, v3.ClusterSpec{
			EKSConfig: cluster.Spec.EKSConfig,
		})
	case cluster.Spec.K3SConfig != nil:
		return h.createCluster(cluster, status, v3.ClusterSpec{
			K3sConfig: cluster.Spec.K3SConfig,
		})
	case cluster.Spec.RKE2Config != nil:
		return h.createCluster(cluster, status, v3.ClusterSpec{
			Rke2Config: cluster.Spec.RKE2Config,
		})
	default:
		return nil, status, nil
	}
}

func (h *handler) createCluster(cluster *v1.Cluster, status v1.ClusterStatus, spec v3.ClusterSpec) ([]runtime.Object, v1.ClusterStatus, error) {
	spec.DisplayName = cluster.Name
	spec.Description = cluster.Annotations["field.cattle.io/description"]
	spec.FleetWorkspaceName = cluster.Namespace
	newCluster := &v3.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name.SafeConcatName("c", cluster.Namespace, cluster.Name),
			Labels:      cluster.Labels,
			Annotations: cluster.Annotations,
		},
		Spec: spec,
	}

	// We do this so that we don't clobber status because the rancher object is pretty dirty and doesn't have a status subresource
	data, err := convert.EncodeToMap(newCluster)
	if err != nil {
		return nil, status, err
	}
	data = map[string]interface{}{
		"metadata": data["metadata"],
		"spec":     data["spec"],
	}
	data["kind"] = "Cluster"
	data["apiVersion"] = "management.cattle.io/v3"

	return h.updateStatus([]runtime.Object{&unstructured.Unstructured{Object: data}}, cluster, status, newCluster)
}

func (h *handler) updateStatus(objs []runtime.Object, cluster *v1.Cluster, status v1.ClusterStatus, rCluster *v3.Cluster) ([]runtime.Object, v1.ClusterStatus, error) {
	ready := false
	existing, err := h.rclusterCache.Get(rCluster.Name)
	if err != nil && !apierror.IsNotFound(err) {
		return nil, status, err
	} else if err == nil {
		if condition.Cond("Ready").IsTrue(existing) {
			ready = true
		}
	}

	// Never set ready back to false because we will end up deleting the secret
	status.Ready = status.Ready || ready
	status.ObservedGeneration = cluster.Generation
	status.ClusterName = rCluster.Name
	if ready {
		kstatus.SetActive(&status)
	} else {
		kstatus.SetTransitioning(&status, "")
	}

	if status.Ready {
		secret, err := h.kubeconfigManager.GetKubeConfig(cluster, status)
		if err != nil {
			return nil, status, err
		}
		if secret != nil {
			objs = append(objs, secret)
		}
		status.ClientSecretName = secret.Name
	}

	return objs, status, nil
}
