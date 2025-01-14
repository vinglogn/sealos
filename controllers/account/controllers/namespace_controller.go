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
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"sigs.k8s.io/controller-runtime/pkg/builder"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/watch"

	v1 "github.com/labring/sealos/controllers/account/api/v1"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const DebtLimit0Name = "debt-limit0"

// NamespaceReconciler reconciles a Namespace object
type NamespaceReconciler struct {
	Client client.WithWatch
	Log    logr.Logger
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=namespaces/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=core,resources=namespaces/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Namespace object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.11.2/pkg/reconcile
func (r *NamespaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := r.Log.WithValues("Namespace", req.Namespace, "Name", req.NamespacedName)

	ns := corev1.Namespace{}

	if err := r.Client.Get(ctx, req.NamespacedName, &ns); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	debtStatus, ok := ns.Annotations[v1.DebtNamespaceAnnoStatusKey]
	if !ok {
		logger.Error(fmt.Errorf("no debt status"), "no debt status")
		return ctrl.Result{}, nil
	}
	logger.Info("debt status", "status", debtStatus)
	switch debtStatus {
	case v1.SuspendDebtNamespaceAnnoStatus:
		logger.Info("suspend namespace resources")
		if err := r.SuspendUserResource(ctx, req.NamespacedName.Name); err != nil {
			logger.Error(err, "suspend namespace resources failed")
			return ctrl.Result{}, err
		}
	case v1.ResumeDebtNamespaceAnnoStatus:
		logger.Info("resume namespace resources")
		if err := r.ResumeUserResource(ctx, req.NamespacedName.Name); err != nil {
			logger.Error(err, "resume namespace resources failed")
			return ctrl.Result{}, err
		}
		ns.Annotations[v1.DebtNamespaceAnnoStatusKey] = v1.NormalDebtNamespaceAnnoStatus
		if err := r.Client.Update(ctx, &ns); err != nil {
			logger.Error(err, "update namespace status failed")
			return ctrl.Result{}, err
		}
	case v1.NormalDebtNamespaceAnnoStatus:
	default:
		logger.Error(fmt.Errorf("unknown namespace debt status, change to normal"), "", "debt status", ns.Annotations[v1.DebtNamespaceAnnoStatusKey])
		ns.Annotations[v1.DebtNamespaceAnnoStatusKey] = v1.NormalDebtNamespaceAnnoStatus
		if err := r.Client.Update(ctx, &ns); err != nil {
			logger.Error(err, "update namespace status failed")
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *NamespaceReconciler) SuspendUserResource(ctx context.Context, namespace string) error {
	// limit0 resource quota
	// suspend pod: deploy pod && clone unmanaged pod
	// delete infra cr
	pipelines := []func(context.Context, string) error{
		r.suspendOrphanPod,
		r.limitResourceQuotaCreate,
		r.deleteControlledPod,
		//TODO how to suspend infra cr or delete infra cr
		//r.suspendInfraResources,
	}
	for _, fn := range pipelines {
		if err := fn(ctx, namespace); err != nil {
			return err
		}
	}
	return nil
}

func (r *NamespaceReconciler) ResumeUserResource(ctx context.Context, namespace string) error {
	// delete limit0 resource quota
	// resume pod
	pipelines := []func(context.Context, string) error{
		r.limitResourceQuotaDelete,
		r.resumePod,
	}
	for _, fn := range pipelines {
		if err := fn(ctx, namespace); err != nil {
			return err
		}
	}
	return nil
}

func (r *NamespaceReconciler) limitResourceQuotaCreate(ctx context.Context, namespace string) error {
	limitQuota := GetLimit0ResourceQuota(namespace)
	_, err := ctrl.CreateOrUpdate(ctx, r.Client, limitQuota, func() error {
		return nil
	})
	return err
}

func (r *NamespaceReconciler) limitResourceQuotaDelete(ctx context.Context, namespace string) error {
	limitQuota := GetLimit0ResourceQuota(namespace)
	err := r.Client.Delete(ctx, limitQuota)
	return client.IgnoreNotFound(err)
}

func GetLimit0ResourceQuota(namespace string) *corev1.ResourceQuota {
	quota := corev1.ResourceQuota{}
	quota.Name = "debt-limit0"
	quota.Namespace = namespace
	quota.Spec.Hard = corev1.ResourceList{
		corev1.ResourceLimitsCPU:       resource.MustParse("0"),
		corev1.ResourceLimitsMemory:    resource.MustParse("0"),
		corev1.ResourceRequestsStorage: resource.MustParse("0"),
	}
	return &quota
}

func (r *NamespaceReconciler) suspendOrphanPod(ctx context.Context, namespace string) error {
	podList := corev1.PodList{}
	if err := r.Client.List(ctx, &podList, client.InNamespace(namespace)); err != nil {
		return err
	}
	for _, pod := range podList.Items {
		if pod.Spec.SchedulerName == v1.DebtSchedulerName || len(pod.ObjectMeta.OwnerReferences) > 0 {
			continue
		}
		/*Orphan pod is backed up separately*/
		clone := pod.DeepCopy()
		// We won't be able to create the object with the same resource version
		clone.ObjectMeta.ResourceVersion = ""

		// Remove assigned node to avoid scheduling
		clone.Spec.NodeName = ""

		// Reset status, not needed as its ignored but nice
		clone.Status = corev1.PodStatus{}

		// Assign our own scheduler to avoid the default scheduler interfer with the workload
		clone.Spec.SchedulerName = v1.DebtSchedulerName

		if clone.Annotations == nil {
			clone.Annotations = make(map[string]string)
		}

		clone.Annotations[v1.PreviousSchedulerName] = pod.Spec.SchedulerName
		err := r.recreatePod(ctx, pod, clone)
		if err != nil {
			return fmt.Errorf("recrete unowned pod `%s` failed: %w", pod.Name, err)
		}
	}
	return nil
}

func (r *NamespaceReconciler) deleteControlledPod(ctx context.Context, namespace string) error {
	podList := corev1.PodList{}
	if err := r.Client.List(ctx, &podList, client.InNamespace(namespace)); err != nil {
		return err
	}
	for _, pod := range podList.Items {
		if pod.Spec.SchedulerName == v1.DebtSchedulerName || len(pod.ObjectMeta.OwnerReferences) == 0 {
			r.Log.Info("skip pod", "pod", pod.Name)
			continue
		}
		r.Log.Info("delete pod", "pod", pod.Name)
		err := r.Client.Delete(ctx, &pod)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *NamespaceReconciler) resumePod(ctx context.Context, namespace string) error {
	var list corev1.PodList
	if err := r.Client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return err
	}
	deleteCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for _, pod := range list.Items {
		if pod.Status.Phase != v1.PodPhaseSuspended || pod.Spec.SchedulerName != v1.DebtSchedulerName {
			continue
		}
		if len(pod.ObjectMeta.OwnerReferences) > 0 {
			err := r.Client.Delete(deleteCtx, &pod)
			if err != nil {
				return fmt.Errorf("delete pod %s failed: %v", pod.Name, err)
			}
		} else {
			clone := pod.DeepCopy()

			// We won't be able to create the object with the same resource version
			clone.ObjectMeta.ResourceVersion = ""

			// Remove assigned node to avoid scheduling
			clone.Spec.NodeName = ""

			// Reset status, not needed as its ignored but nice
			clone.Status = corev1.PodStatus{}

			if scheduler, ok := clone.Annotations[v1.PreviousSchedulerName]; ok {
				clone.Spec.SchedulerName = scheduler
				delete(clone.Annotations, v1.PreviousSchedulerName)
			} else {
				clone.Spec.SchedulerName = ""
			}

			err := r.recreatePod(deleteCtx, pod, clone)
			if err != nil {
				return fmt.Errorf("recrete unowned pod %s failed: %v", pod.Name, err)
			}
		}
	}

	return nil
}

func (r *NamespaceReconciler) recreatePod(ctx context.Context, oldPod corev1.Pod, newPod *corev1.Pod) error {
	list := corev1.PodList{}
	watcher, err := r.Client.Watch(ctx, &list)
	if err != nil {
		return fmt.Errorf("failed to start watch stream for pod %s: %w", oldPod.Name, err)
	}

	ch := watcher.ResultChan()

	err = r.Client.Delete(ctx, &oldPod)
	if err != nil {
		return fmt.Errorf("failed to delete pod %s: %w", oldPod.Name, err)
	}

	// Wait for delete event before we can attempt create the clone
	for event := range ch {
		if event.Type == watch.Deleted {
			if val, ok := event.Object.(*corev1.Pod); ok && val.Name == oldPod.Name {
				err = r.Client.Create(ctx, newPod)
				if err != nil {
					return fmt.Errorf("failed to recreate pod %s: %w", newPod.Name, err)
				}

				watcher.Stop()
				break
			}
		}
	}

	return nil
}

//func (r *NamespaceReconciler) deleteInfraResources(ctx context.Context, namespace string) error {
//
//	u := unstructured.UnstructuredList{}
//	u.SetGroupVersionKind(schema.GroupVersionKind{
//		Group:   "infra.sealos.io",
//		Version: "v1",
//		Kind:    "infra",
//	})
//	if err := r.Client.List(ctx, &u, client.InNamespace(namespace)); err != nil {
//		return client.IgnoreNotFound(err)
//	}
//	for _, item := range u.Items {
//		r.Log.Info("delete resource", "resource name:", item.GetName(), "get GVK", item.GroupVersionKind())
//		if err := r.Client.Delete(ctx, &item); err != nil {
//			return client.IgnoreNotFound(err)
//		}
//	}
//	return nil
//}

// SetupWithManager sets up the controller with the Manager.
func (r *NamespaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Log = ctrl.Log.WithName("controllers").WithName("Namespace")
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Namespace{}, builder.WithPredicates(AnnotationChangedPredicate{})).
		Complete(r)
}

type AnnotationChangedPredicate struct {
	predicate.Funcs
}

func (AnnotationChangedPredicate) Update(e event.UpdateEvent) bool {
	oldObj, _ok1 := e.ObjectOld.(*corev1.Namespace)
	newObj, _ok2 := e.ObjectNew.(*corev1.Namespace)

	if !_ok1 || !_ok2 || newObj.Annotations == nil {
		return false
	}
	oldStatus := ""
	if oldAno := oldObj.Annotations; oldAno != nil {
		oldStatus = oldAno[v1.DebtNamespaceAnnoStatusKey]
	}
	newStatus, ok := newObj.Annotations[v1.DebtNamespaceAnnoStatusKey]

	return ok && oldStatus != newStatus
}

func (AnnotationChangedPredicate) Create(e event.CreateEvent) bool {
	_, ok := e.Object.GetAnnotations()[v1.DebtNamespaceAnnoStatusKey]
	return ok
}
