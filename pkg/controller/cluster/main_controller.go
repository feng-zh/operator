/*
 * Copyright (C) 2020, MinIO, Inc.
 *
 * This code is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License, version 3,
 * as published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License, version 3,
 * along with this program.  If not, see <http://www.gnu.org/licenses/>
 *
 */

package cluster

import (
	"context"
	"fmt"
	"time"

	"k8s.io/klog"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	appsinformers "k8s.io/client-go/informers/apps/v1"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	certapi "k8s.io/client-go/kubernetes/typed/certificates/v1beta1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	queue "k8s.io/client-go/util/workqueue"

	miniov1beta1 "github.com/minio/minio-operator/pkg/apis/operator.min.io/v1"
	clientset "github.com/minio/minio-operator/pkg/client/clientset/versioned"
	minioscheme "github.com/minio/minio-operator/pkg/client/clientset/versioned/scheme"
	informers "github.com/minio/minio-operator/pkg/client/informers/externalversions/operator.min.io/v1"
	listers "github.com/minio/minio-operator/pkg/client/listers/operator.min.io/v1"
	"github.com/minio/minio-operator/pkg/resources/deployments"
	"github.com/minio/minio-operator/pkg/resources/services"
	"github.com/minio/minio-operator/pkg/resources/statefulsets"
)

const controllerAgentName = "minio-operator"

const (
	// SuccessSynced is used as part of the Event 'reason' when a MinIOInstance is synced
	SuccessSynced = "Synced"
	// ErrResourceExists is used as part of the Event 'reason' when a MinIOInstance fails
	// to sync due to a StatefulSet of the same name already existing.
	ErrResourceExists = "ErrResourceExists"
	// MessageResourceExists is the message used for Events when a MinIOInstance
	// fails to sync due to a StatefulSet already existing
	MessageResourceExists = "Resource %q already exists and is not managed by MinIO Operator"
	// MessageResourceSynced is the message used for an Event fired when a MinIOInstance
	// is synced successfully
	MessageResourceSynced = "MinIOInstance synced successfully"
)

// Controller struct watches the Kubernetes API for changes to MinIOInstance resources
type Controller struct {
	// kubeClientSet is a standard kubernetes clientset
	kubeClientSet kubernetes.Interface
	// minioClientSet is a clientset for our own API group
	minioClientSet clientset.Interface
	// certClient is a clientset for our certficate management
	certClient certapi.CertificatesV1beta1Client
	// statefulSetLister is able to list/get StatefulSets from a shared
	// informer's store.
	statefulSetLister appslisters.StatefulSetLister
	// statefulSetListerSynced returns true if the StatefulSet shared informer
	// has synced at least once.
	statefulSetListerSynced cache.InformerSynced

	// deploymentLister is able to list/get Deployments from a shared
	// informer's store.
	deploymentLister appslisters.DeploymentLister
	// deploymentListerSynced returns true if the Deployment shared informer
	// has synced at least once.
	deploymentListerSynced cache.InformerSynced

	// minioInstancesLister lists MinIOInstance from a shared informer's
	// store.
	minioInstancesLister listers.MinIOInstanceLister
	// minioInstancesSynced returns true if the StatefulSet shared informer
	// has synced at least once.
	minioInstancesSynced cache.InformerSynced

	// serviceLister is able to list/get Services from a shared informer's
	// store.
	serviceLister corelisters.ServiceLister
	// serviceListerSynced returns true if the Service shared informer
	// has synced at least once.
	serviceListerSynced cache.InformerSynced

	// queue is a rate limited work queue. This is used to queue work to be
	// processed instead of performing it as soon as a change happens. This
	// means we can ensure we only process a fixed amount of resources at a
	// time, and makes it easy to ensure we are never processing the same item
	// simultaneously in two different workers.
	workqueue queue.RateLimitingInterface
	// recorder is an event recorder for recording Event resources to the
	// Kubernetes API.
	recorder record.EventRecorder
}

// NewController returns a new sample controller
func NewController(
	kubeClientSet kubernetes.Interface,
	minioClientSet clientset.Interface,
	certClient certapi.CertificatesV1beta1Client,
	statefulSetInformer appsinformers.StatefulSetInformer,
	deploymentInformer appsinformers.DeploymentInformer,
	minioInstanceInformer informers.MinIOInstanceInformer,
	serviceInformer coreinformers.ServiceInformer) *Controller {

	// Create event broadcaster
	// Add minio-controller types to the default Kubernetes Scheme so Events can be
	// logged for minio-controller types.
	minioscheme.AddToScheme(scheme.Scheme)
	klog.V(4).Info("Creating event broadcaster")
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeClientSet.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName})

	controller := &Controller{
		kubeClientSet:           kubeClientSet,
		minioClientSet:          minioClientSet,
		certClient:              certClient,
		statefulSetLister:       statefulSetInformer.Lister(),
		statefulSetListerSynced: statefulSetInformer.Informer().HasSynced,
		deploymentLister:        deploymentInformer.Lister(),
		deploymentListerSynced:  statefulSetInformer.Informer().HasSynced,
		minioInstancesLister:    minioInstanceInformer.Lister(),
		minioInstancesSynced:    minioInstanceInformer.Informer().HasSynced,
		serviceLister:           serviceInformer.Lister(),
		serviceListerSynced:     serviceInformer.Informer().HasSynced,
		workqueue:               queue.NewNamedRateLimitingQueue(queue.DefaultControllerRateLimiter(), "MinIOInstances"),
		recorder:                recorder,
	}

	klog.Info("Setting up event handlers")
	// Set up an event handler for when MinIOInstance resources change
	minioInstanceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueMinIOInstance,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueMinIOInstance(new)
		},
	})
	// Set up an event handler for when StatefulSet resources change. This
	// handler will lookup the owner of the given StatefulSet, and if it is
	// owned by a MinIOInstance resource will enqueue that MinIOInstance resource for
	// processing. This way, we don't need to implement custom logic for
	// handling StatefulSet resources. More info on this pattern:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-api-machinery/controllers.md
	statefulSetInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.handleObject,
		UpdateFunc: func(old, new interface{}) {
			newDepl := new.(*appsv1.StatefulSet)
			oldDepl := old.(*appsv1.StatefulSet)
			if newDepl.ResourceVersion == oldDepl.ResourceVersion {
				// Periodic resync will send update events for all known StatefulSet.
				// Two different versions of the same StatefulSet will always have different RVs.
				return
			}
			controller.handleObject(new)
		},
		DeleteFunc: controller.handleObject,
	})

	deploymentInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.handleObject,
		UpdateFunc: func(old, new interface{}) {
			newDepl := new.(*appsv1.Deployment)
			oldDepl := old.(*appsv1.Deployment)
			if newDepl.ResourceVersion == oldDepl.ResourceVersion {
				// Periodic resync will send update events for all known Deployments.
				// Two different versions of the same Deployments will always have different RVs.
				return
			}
			controller.handleObject(new)
		},
		DeleteFunc: controller.handleObject,
	})
	return controller
}

// Start will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting workers. It will block until stopCh
// is closed, at which point it w	ill shutdown the workqueue and wait for
// workers to finish processing their current work items.
func (c *Controller) Start(threadiness int, stopCh <-chan struct{}) error {

	// Start the informer factories to begin populating the informer caches
	klog.Info("Starting MinIOInstance controller")

	// Wait for the caches to be synced before starting workers
	klog.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, c.statefulSetListerSynced, c.deploymentListerSynced, c.minioInstancesSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	klog.Info("Starting workers")
	// Launch two workers to process MinIOInstance resources
	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	return nil
}

// Stop is called to shutdown the controller
func (c *Controller) Stop() {
	klog.Info("Stopping the minio controller")
	c.workqueue.ShutDown()
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func (c *Controller) runWorker() {
	defer runtime.HandleCrash()
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()

	if shutdown {
		return false
	}
	// We wrap this block in a func so we can defer c.workqueue.Done.
	err := func(obj interface{}) error {
		// We call Done here so the workqueue knows we have finished
		// processing this item. We also must remember to call Forget if we
		// do not want this work item being re-queued. For example, we do
		// not call Forget if a transient error occurs, instead the item is
		// put back on the workqueue and attempted again after a back-off
		// period.
		defer c.workqueue.Done(obj)
		var key string
		var ok bool
		// We expect strings to come off the workqueue. These are of the
		// form namespace/name. We do this as the delayed nature of the
		// workqueue means the items in the informer cache may actually be
		// more up to date that when the item was initially put onto the
		// workqueue.
		if key, ok = obj.(string); !ok {
			// As the item in the workqueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			c.workqueue.Forget(obj)
			runtime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		klog.V(2).Infof("Key from workqueue: %s", key)
		// Run the syncHandler, passing it the namespace/name string of the
		// MinIOInstance resource to be synced.
		if err := c.syncHandler(key); err != nil {
			return fmt.Errorf("error syncing '%s': %s", key, err.Error())
		}
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		c.workqueue.Forget(obj)
		klog.Infof("Successfully synced '%s'", key)
		return nil
	}(obj)

	if err != nil {
		runtime.HandleError(err)
		return true
	}
	return true
}

// syncHandler compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the MinIOInstance resource
// with the current status of the resource.
func (c *Controller) syncHandler(key string) error {
	ctx := context.Background()
	cOpts := metav1.CreateOptions{}
	uOpts := metav1.UpdateOptions{}
	gOpts := metav1.GetOptions{}

	var d *appsv1.Deployment

	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	klog.V(2).Infof("Key after splitting, namespace: %s, name: %s", namespace, name)
	if err != nil {
		runtime.HandleError(fmt.Errorf("Invalid resource key: %s", key))
		return nil
	}

	nsName := types.NamespacedName{Namespace: namespace, Name: name}

	// Get the MinIOInstance resource with this namespace/name
	mi, err := c.minioInstancesLister.MinIOInstances(namespace).Get(name)
	if err != nil {
		// The MinIOInstance resource may no longer exist, in which case we stop processing.
		if errors.IsNotFound(err) {
			runtime.HandleError(fmt.Errorf("MinIOInstance '%s' in work queue no longer exists", key))
			return nil
		}
		return err
	}

	mi.EnsureDefaults()

	svc, err := c.serviceLister.Services(mi.Namespace).Get(mi.GetServiceName())
	// If the service doesn't exist, we'll create it
	if apierrors.IsNotFound(err) {
		klog.V(2).Infof("Creating a new Service for cluster %q", nsName)
		svc = services.NewServiceForCluster(mi)
		_, err = c.kubeClientSet.CoreV1().Services(svc.Namespace).Create(ctx, svc, cOpts)
	}

	hlSvc, err := c.serviceLister.Services(mi.Namespace).Get(mi.GetHeadlessServiceName())
	// If the headless service doesn't exist, we'll create it
	if apierrors.IsNotFound(err) {
		klog.V(2).Infof("Creating a new Headless Service for cluster %q", nsName)
		hlSvc = services.NewHeadlessServiceForCluster(mi)
		_, err = c.kubeClientSet.CoreV1().Services(hlSvc.Namespace).Create(ctx, hlSvc, cOpts)
	}

	// If an error occurs during Get/Create, we'll requeue the item so we can
	// attempt processing again later. This could have been caused by a
	// temporary network failure, or any other transient reason.
	if err != nil {
		return err
	}

	// check if both auto certificate creation and external secret with certificate is passed,
	// this is an error as only one of this is allowed in one MinIOInstance
	if mi.RequiresAutoCertSetup() && mi.RequiresExternalCertSetup() {
		msg := "Please set either externalCertSecret or requestAutoCert in MinIOInstance config"
		klog.V(2).Infof(msg)
		return fmt.Errorf(msg)
	}

	if mi.RequiresAutoCertSetup() {
		_, err := c.certClient.CertificateSigningRequests().Get(ctx, mi.GetCSRName(), metav1.GetOptions{})
		if err != nil {
			// If the CSR doesn't exist, we'll create it
			if apierrors.IsNotFound(err) {
				klog.V(2).Infof("Creating a new Certificate Signing Request for cluster %q", nsName)
				// create CSR here
				c.createCSR(ctx, mi)
			} else {
				return err
			}
		}
	}

	// Get the StatefulSet with the name specified in MinIOInstance.spec
	ss, err := c.statefulSetLister.StatefulSets(mi.Namespace).Get(mi.Name)
	// If the resource doesn't exist, we'll create it
	if apierrors.IsNotFound(err) {
		ss = statefulsets.NewForCluster(mi, hlSvc.Name)
		ss, err = c.kubeClientSet.AppsV1().StatefulSets(mi.Namespace).Create(ctx, ss, cOpts)
	}

	// If an error occurs during Get/Create, we'll requeue the item so we can
	// attempt processing again later. This could have been caused by a
	// temporary network failure, or any other transient reason.
	if err != nil {
		return err
	}

	if mi.HasMcsEnabled() {
		// Get the Deployment with the name specified in MinIOInstance.spec
		d, err := c.deploymentLister.Deployments(mi.Namespace).Get(mi.Name)
		// If the resource doesn't exist, we'll create it
		if apierrors.IsNotFound(err) {
			if !mi.HasCredsSecret() || !mi.HasMcsSecret() {
				msg := "Please set the credentials"
				klog.V(2).Infof(msg)
				return fmt.Errorf(msg)
			}

			minioSecretName := mi.Spec.CredsSecret.Name
			minioSecret, sErr := c.kubeClientSet.CoreV1().Secrets(mi.Namespace).Get(ctx, minioSecretName, gOpts)
			if sErr != nil {
				return sErr
			}

			mcsSecretName := mi.Spec.Mcs.McsSecret.Name
			mcsSecret, sErr := c.kubeClientSet.CoreV1().Secrets(mi.Namespace).Get(ctx, mcsSecretName, gOpts)
			if sErr != nil {
				return sErr
			}
			// Check if any one replica is READY
			if ss.Status.ReadyReplicas > 0 {
				if pErr := mi.CreateMcsUser(minioSecret.Data, mcsSecret.Data); pErr != nil {
					klog.V(2).Infof(pErr.Error())
					return pErr
				}
				// Create MCS Deployment
				d = deployments.NewMcsDeployment(mi)
				d, err = c.kubeClientSet.AppsV1().Deployments(mi.Namespace).Create(ctx, d, cOpts)
				if err != nil {
					klog.V(2).Infof(err.Error())
					return err
				}
				// Create MCS service
				mcsSvc := services.NewMcsServiceForCluster(mi)
				_, err = c.kubeClientSet.CoreV1().Services(svc.Namespace).Create(ctx, mcsSvc, cOpts)
				if err != nil {
					klog.V(2).Infof(err.Error())
					return err
				}
			}
		} else {
			return err
		}
	}

	// If the StatefulSet is not controlled by this MinIOInstance resource, we should log
	// a warning to the event recorder and ret
	if !metav1.IsControlledBy(ss, mi) {
		msg := fmt.Sprintf(MessageResourceExists, ss.Name)
		c.recorder.Event(mi, corev1.EventTypeWarning, ErrResourceExists, msg)
		return fmt.Errorf(msg)
	}

	// If this number of the replicas on the MinIOInstance resource is specified, and the
	// number does not equal the current desired replicas on the StatefulSet, we
	// should update the StatefulSet resource.
	if mi.GetReplicas() != *ss.Spec.Replicas {
		klog.V(4).Infof("MinIOInstance %s replicas: %d, StatefulSet replicas: %d", name, mi.GetReplicas(), *ss.Spec.Replicas)
		ss = statefulsets.NewForCluster(mi, hlSvc.Name)
		err = c.kubeClientSet.AppsV1().StatefulSets(mi.Namespace).Delete(ctx, ss.Name, metav1.DeleteOptions{})
		if err != nil {
			return err
		}
		_, err = c.kubeClientSet.AppsV1().StatefulSets(mi.Namespace).Create(ctx, ss, cOpts)
		if err != nil {
			return err
		}
	}

	// If this container version on the MinIOInstance resource is specified, and the
	// version does not equal the current desired version in the StatefulSet, we
	// should update the StatefulSet resource.
	if mi.Spec.Image != ss.Spec.Template.Spec.Containers[0].Image {
		klog.V(4).Infof("Updating MinIOInstance %s MinIO server version %s, to: %s", name, mi.Spec.Image, ss.Spec.Template.Spec.Containers[0].Image)
		ss = statefulsets.NewForCluster(mi, hlSvc.Name)
		_, err = c.kubeClientSet.AppsV1().StatefulSets(mi.Namespace).Update(ctx, ss, uOpts)
	}

	// If an error occurs during Update, we'll requeue the item so we can
	// attempt processing again later. This could have been caused by a
	// temporary network failure, or any other transient reason.
	if err != nil {
		return err
	}

	if mi.HasMcsEnabled() && d != nil && mi.Spec.Mcs.Image != d.Spec.Template.Spec.Containers[0].Image {
		klog.V(4).Infof("Updating MinIOInstance %s mcs version %s, to: %s", name, mi.Spec.Mcs.Image, d.Spec.Template.Spec.Containers[0].Image)
		d = deployments.NewMcsDeployment(mi)
		_, err = c.kubeClientSet.AppsV1().Deployments(mi.Namespace).Update(ctx, d, uOpts)
		// If an error occurs during Update, we'll requeue the item so we can
		// attempt processing again later. This could have been caused by a
		// temporary network failure, or any other transient reason.
		if err != nil {
			return err
		}
	}

	// Finally, we update the status block of the MinIOInstance resource to reflect the
	// current state of the world
	err = c.updateMinIOInstanceStatus(ctx, mi, ss)
	if err != nil {
		return err
	}
	return nil
}

func (c *Controller) updateMinIOInstanceStatus(ctx context.Context, minioInstance *miniov1beta1.MinIOInstance, statefulSet *appsv1.StatefulSet) error {
	opts := metav1.UpdateOptions{}
	// NEVER modify objects from the store. It's a read-only, local cache.
	// You can use DeepCopy() to make a deep copy of original object and modify this copy
	// Or create a copy manually for better performance
	minioInstanceCopy := minioInstance.DeepCopy()
	minioInstanceCopy.Status.AvailableReplicas = statefulSet.Status.Replicas
	// If the CustomResourceSubresources feature gate is not enabled,
	// we must use Update instead of UpdateStatus to update the Status block of the MinIOInstance resource.
	// UpdateStatus will not allow changes to the Spec of the resource,
	// which is ideal for ensuring nothing other than resource status has been updated.
	_, err := c.minioClientSet.OperatorV1().MinIOInstances(minioInstance.Namespace).Update(ctx, minioInstanceCopy, opts)
	return err
}

// enqueueMinIOInstance takes a MinIOInstance resource and converts it into a namespace/name
// string which is then put onto the work queue. This method should *not* be
// passed resources of any type other than MinIOInstance.
func (c *Controller) enqueueMinIOInstance(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		runtime.HandleError(err)
		return
	}
	c.workqueue.AddRateLimited(key)
}

// handleObject will take any resource implementing metav1.Object and attempt
// to find the MinIOInstance resource that 'owns' it. It does this by looking at the
// objects metadata.ownerReferences field for an appropriate OwnerReference.
// It then enqueues that MinIOInstance resource to be processed. If the object does not
// have an appropriate OwnerReference, it will simply be skipped.
func (c *Controller) handleObject(obj interface{}) {
	var object metav1.Object
	var ok bool
	if object, ok = obj.(metav1.Object); !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			runtime.HandleError(fmt.Errorf("error decoding object, invalid type"))
			return
		}
		object, ok = tombstone.Obj.(metav1.Object)
		if !ok {
			runtime.HandleError(fmt.Errorf("error decoding object tombstone, invalid type"))
			return
		}
		klog.V(4).Infof("Recovered deleted object '%s' from tombstone", object.GetName())
	}
	klog.V(4).Infof("Processing object: %s", object.GetName())
	if ownerRef := metav1.GetControllerOf(object); ownerRef != nil {
		// If this object is not owned by a MinIOInstance, we should not do anything more
		// with it.
		if ownerRef.Kind != "MinIOInstance" {
			return
		}

		minioInstance, err := c.minioInstancesLister.MinIOInstances(object.GetNamespace()).Get(ownerRef.Name)
		if err != nil {
			klog.V(4).Infof("ignoring orphaned object '%s' of minioInstance '%s'", object.GetSelfLink(), ownerRef.Name)
			return
		}

		c.enqueueMinIOInstance(minioInstance)
		return
	}
}