package controllers

import (
	"context"
	"fmt"
	"github.com/infinispan/infinispan-operator/pkg/reconcile/pipeline/infinispan/handler/manage"
	"k8s.io/apimachinery/pkg/util/validation"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	"github.com/iancoleman/strcase"
	v1 "github.com/infinispan/infinispan-operator/api/v1"
	"github.com/infinispan/infinispan-operator/api/v2alpha1"
	"github.com/infinispan/infinispan-operator/controllers/constants"
	"github.com/infinispan/infinispan-operator/pkg/infinispan/client/api"
	kube "github.com/infinispan/infinispan-operator/pkg/kubernetes"
	"github.com/infinispan/infinispan-operator/pkg/mime"
	"go.uber.org/zap"
	"gopkg.in/yaml.v2"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// CacheReconciler reconciles a Cache object
type CacheReconciler struct {
	client.Client
	log        logr.Logger
	scheme     *runtime.Scheme
	kubernetes *kube.Kubernetes
	eventRec   record.EventRecorder
}

type CacheListener struct {
	// The Infinispan cluster to listen to in the configured namespace
	Infinispan *v1.Infinispan
	Ctx        context.Context
	Kubernetes *kube.Kubernetes
	Log        *zap.SugaredLogger
}

type cacheRequest struct {
	*CacheReconciler
	ctx        context.Context
	cache      *v2alpha1.Cache
	infinispan *v1.Infinispan
	ispnClient api.Infinispan
	reqLogger  logr.Logger
}

// SetupWithManager sets up the controller with the Manager.
func (r *CacheReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	r.Client = mgr.GetClient()
	r.log = ctrl.Log.WithName("controllers").WithName("Cache")
	r.scheme = mgr.GetScheme()
	r.kubernetes = kube.NewKubernetesFromController(mgr)
	r.eventRec = mgr.GetEventRecorderFor("cache-controller")

	if err := mgr.GetFieldIndexer().IndexField(ctx, &v2alpha1.Cache{}, "spec.clusterName", func(obj client.Object) []string {
		return []string{obj.(*v2alpha1.Cache).Spec.ClusterName}
	}); err != nil {
		return err
	}

	builder := ctrl.NewControllerManagedBy(mgr).For(&v2alpha1.Cache{})
	builder.Watches(
		&source.Kind{Type: &v1.Infinispan{}},
		handler.EnqueueRequestsFromMapFunc(
			func(a client.Object) []reconcile.Request {
				i := a.(*v1.Infinispan)
				// Only enqueue requests once a Infinispan CR has the WellFormed condition or it has been deleted
				if !i.HasCondition(v1.ConditionWellFormed) || !a.GetDeletionTimestamp().IsZero() {
					return nil
				}

				var requests []reconcile.Request
				cacheList := &v2alpha1.CacheList{}
				if err := r.kubernetes.ResourcesListByField(a.GetNamespace(), "spec.clusterName", a.GetName(), cacheList, ctx); err != nil {
					r.log.Error(err, "watches failed to list Cache CRs")
				}

				for _, item := range cacheList.Items {
					requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: item.GetNamespace(), Name: item.GetName()}})
				}
				return requests
			}),
	)
	return builder.Complete(r)
}

// +kubebuilder:rbac:groups=infinispan.org,namespace=infinispan-operator-system,resources=caches;caches/status;caches/finalizers,verbs=get;list;watch;create;update;patch;delete

func (r *CacheReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	reqLogger := r.log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("+++++ Reconciling Cache.")
	defer reqLogger.Info("----- End Reconciling Cache.")

	// Fetch the Cache instance
	instance := &v2alpha1.Cache{}
	if err := r.Client.Get(ctx, request.NamespacedName, instance); err != nil {
		if errors.IsNotFound(err) {
			reqLogger.Info("Cache resource not found. Ignoring it since cache deletion is not supported")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	infinispan := &v1.Infinispan{}
	cache := &cacheRequest{
		CacheReconciler: r,
		ctx:             ctx,
		cache:           instance,
		infinispan:      infinispan,
		reqLogger:       reqLogger,
	}

	if cache.markedForDeletion() {
		reqLogger.Info("Cache CR marked for deletion. Attempting to remove.")
		// The ConfigListener has marked this resource for deletion
		// Remove finalizer and delete CR. No need to update the server as the cache has already been removed
		if err := cache.removeFinalizer(); err != nil {
			if errors.IsNotFound(err) {
				reqLogger.Info("Cache CR not found, nothing todo.")
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, err
		}
		if err := cache.kubernetes.Client.Delete(ctx, instance); err != nil {
			if errors.IsNotFound(err) {
				reqLogger.Info("Cache CR not found, nothing todo.")
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, err
		}
		reqLogger.Info("Cache CR Removed.")
		return ctrl.Result{}, nil
	}

	crDeleted := instance.GetDeletionTimestamp() != nil

	// Fetch the Infinispan cluster
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: instance.Namespace, Name: instance.Spec.ClusterName}, infinispan); err != nil {
		if errors.IsNotFound(err) {
			reqLogger.Error(err, fmt.Sprintf("Infinispan cluster %s not found", instance.Spec.ClusterName))
			if crDeleted {
				return ctrl.Result{}, cache.removeFinalizer()
			}
			// No need to requeue request here as the Infinispan watch ensures that a request is queued when the cluster is updated
			return ctrl.Result{}, cache.update(func() error {
				// Set CacheConditionReady to false in case the cluster was previously WellFormed
				instance.SetCondition(v2alpha1.CacheConditionReady, metav1.ConditionFalse, "")
				return nil
			})
		}
		return ctrl.Result{}, err
	}

	// Cluster must be well formed
	if !infinispan.IsWellFormed() {
		reqLogger.Info(fmt.Sprintf("Infinispan cluster %s not well formed", infinispan.Name))
		// No need to requeue request here as the Infinispan watch ensures that a request is queued when the cluster is updated
		return ctrl.Result{}, nil
	}

	ispnClient, err := NewInfinispan(ctx, infinispan, r.kubernetes)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("unable to create Infinispan client: %w", err)
	}
	cache.ispnClient = ispnClient

	if crDeleted {
		if controllerutil.ContainsFinalizer(instance, constants.InfinispanFinalizer) {
			// Remove Deleted caches from the server before removing the Finalizer
			cacheName := instance.GetCacheName()
			if err := ispnClient.Cache(cacheName).Delete(); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, cache.removeFinalizer()
		}
		return ctrl.Result{}, nil
	}

	// Don't contact the Infinispan server for resources created by the ConfigListener
	if cache.reconcileOnServer() {
		if result, err := cache.ispnCreateOrUpdate(); result != nil {
			if err != nil {
				return *result, cache.update(func() error {
					instance.SetCondition(v2alpha1.CacheConditionReady, metav1.ConditionFalse, err.Error())
					return nil
				})
			}
			return *result, err
		}
	}

	err = cache.update(func() error {
		instance.SetCondition(v2alpha1.CacheConditionReady, metav1.ConditionTrue, "")
		// Add finalizer so that the Cache is removed on the server when the Cache CR is deleted
		if !controllerutil.ContainsFinalizer(instance, constants.InfinispanFinalizer) {
			controllerutil.AddFinalizer(instance, constants.InfinispanFinalizer)
		}
		return nil
	})
	return ctrl.Result{}, err
}

func (r *cacheRequest) update(mutate func() error) error {
	cache := r.cache
	_, err := kube.CreateOrPatch(r.ctx, r.Client, cache, func() error {
		if cache.CreationTimestamp.IsZero() {
			return errors.NewNotFound(schema.ParseGroupResource("cache.infinispan.org"), cache.Name)
		}
		return mutate()
	})
	if err != nil {
		return fmt.Errorf("unable to update cache %s: %w", cache.Name, err)
	}
	return nil
}

// Determine if reconciliation was triggered by the ConfigListener
func (r *cacheRequest) reconcileOnServer() bool {
	if val, exists := r.cache.ObjectMeta.Annotations[constants.ListenerAnnotationGeneration]; exists {
		generation, _ := strconv.ParseInt(val, 10, 64)
		return generation != r.cache.GetGeneration()
	}
	return true
}

func (r *cacheRequest) markedForDeletion() bool {
	_, exists := r.cache.ObjectMeta.Annotations[constants.ListenerAnnotationDelete]
	return exists
}

func (r *cacheRequest) removeFinalizer() error {
	return r.update(func() error {
		controllerutil.RemoveFinalizer(r.cache, constants.InfinispanFinalizer)
		return nil
	})
}

func (r *cacheRequest) ispnCreateOrUpdate() (*ctrl.Result, error) {
	cacheName := r.cache.GetCacheName()
	cacheClient := r.ispnClient.Cache(cacheName)

	cacheExists, err := cacheClient.Exists()
	if err != nil {
		err := fmt.Errorf("unable to determine if cache exists: %w", err)
		r.reqLogger.Error(err, "")
		return &ctrl.Result{}, err
	}

	if r.infinispan.IsDataGrid() {
		err = r.reconcileDataGrid(cacheExists, cacheClient)
	} else {
		err = r.reconcileCacheService(cacheExists, cacheClient)
	}
	if err != nil {
		return &ctrl.Result{Requeue: true}, err
	}
	return nil, nil
}

func (r *cacheRequest) reconcileCacheService(cacheExists bool, cache api.Cache) error {
	spec := r.cache.Spec
	if cacheExists {
		err := fmt.Errorf("cannot update an existing cache in a CacheService cluster")
		r.reqLogger.Error(err, "Error updating cache")
		return err
	}

	if spec.TemplateName != "" || spec.Template != "" {
		err := fmt.Errorf("cannot create a cache with a template in a CacheService cluster")
		r.reqLogger.Error(err, "Error creating cache")
		return err
	}

	podList, err := PodList(r.infinispan, r.kubernetes, r.ctx)
	if err != nil {
		r.reqLogger.Error(err, "failed to list pods")
		return err
	}

	template, err := manage.DefaultCacheTemplateXML(podList.Items[0].Name, r.infinispan, r.kubernetes, r.reqLogger)
	if err != nil {
		err = fmt.Errorf("unable to obtain default cache template: %w", err)
		r.reqLogger.Error(err, "Error getting default XML")
		return err
	}
	if err = cache.Create(template, mime.ApplicationXml); err != nil {
		err = fmt.Errorf("unable to create cache using default template: %w", err)
		r.reqLogger.Error(err, "Error in creating cache")
		return err
	}
	return nil
}

func (r *cacheRequest) reconcileDataGrid(cacheExists bool, cache api.Cache) error {
	spec := r.cache.Spec
	if cacheExists {
		if spec.Template != "" {
			err := cache.UpdateConfig(spec.Template, mime.GuessMarkup(spec.Template))
			if err != nil {
				return fmt.Errorf("unable to update cache template: %w", err)
			}
		}
		return nil
	}

	var err error
	if spec.TemplateName != "" {
		if err = cache.CreateWithTemplate(spec.TemplateName); err != nil {
			err = fmt.Errorf("unable to create cache with template name '%s': %w", spec.TemplateName, err)
		}
	} else {
		if err = cache.Create(spec.Template, mime.GuessMarkup(spec.Template)); err != nil {
			err = fmt.Errorf("unable to create cache with template: %w", err)
		}
	}

	if err != nil {
		r.reqLogger.Error(err, "Unable to create Cache")
	}
	return err
}

func (cl *CacheListener) RemoveStaleResources(podName string) error {
	cl.Log.Info("Checking for stale cache resources")
	k8s := cl.Kubernetes
	ispn, err := NewInfinispanForPod(cl.Ctx, podName, cl.Infinispan, k8s)
	if err != nil {
		return err
	}

	// Retrieve names of caches defined on the server
	cacheNames, err := ispn.Caches().Names()
	if err != nil {
		return err
	}
	cl.Log.Debugf("Caches defined on the server: '%v'", cacheNames)

	// Create Set of CR names for 0(1) lookup
	serverCaches := make(map[string]struct{}, len(cacheNames))
	for _, name := range cacheNames {
		cacheCrName := strcase.ToKebab(name)
		serverCaches[cacheCrName] = struct{}{}
	}

	// Retrieve list of all Cache CRs in namespace
	cacheList := &v2alpha1.CacheList{}
	if err := k8s.Client.List(cl.Ctx, cacheList, &client.ListOptions{Namespace: cl.Infinispan.Namespace}); err != nil {
		return fmt.Errorf("unable to rerieve existing Cache resources: %w", err)
	}

	// Iterate over all existing CRs, marking for deletion any that do not have a cache definition on the server
	for _, cache := range cacheList.Items {
		listenerCreated := kube.IsOwnedBy(&cache, cl.Infinispan)
		_, cacheExists := serverCaches[cache.Name]
		cl.Log.Debugf("Checking if Cache CR '%s' is stale. ListenerCreated=%t. CacheExists=%t", cache.Name, listenerCreated, cacheExists)
		if listenerCreated && !cacheExists {
			cache.ObjectMeta.Annotations[constants.ListenerAnnotationDelete] = "true"
			cl.Log.Infof("Marking stale Cache resource '%s' for deletion", cache.Name)
			if err := k8s.Client.Update(cl.Ctx, &cache); err != nil {
				if !errors.IsNotFound(err) {
					return fmt.Errorf("unable to mark Cache '%s' for deletion: %w", cache.Name, err)
				}
			}
		}
	}
	return nil
}

var cacheNameRegexp = regexp.MustCompile("[^-a-z0-9]")

func (cl *CacheListener) CreateOrUpdate(data []byte) error {
	namespace := cl.Infinispan.Namespace
	clusterName := cl.Infinispan.Name
	cacheName, configYaml, err := unmarshallEventConfig(data)
	if err != nil {
		return err
	}

	if strings.HasPrefix(cacheName, "___") {
		cl.Log.Debugf("Ignoring internal cache %s", cacheName)
		return nil
	}

	cache, err := cl.findExistingCacheCR(cacheName, clusterName)
	if err != nil {
		return err
	}

	k8sClient := cl.Kubernetes.Client
	if cache == nil {
		// There's no Existing Cache CR, so we must create one
		sanitizedCacheName := cacheNameRegexp.ReplaceAllString(strcase.ToKebab(cacheName), "-")
		errs := validation.IsDNS1123Subdomain(sanitizedCacheName)
		if len(errs) > 0 {
			return fmt.Errorf("unable to create Cache Resource Name for cache=%s, cluster=%s: %s", cacheName, clusterName, strings.Join(errs, "."))
		}

		cache = &v2alpha1.Cache{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: sanitizedCacheName + "-",
				Namespace:    namespace,
				Annotations: map[string]string{
					constants.ListenerAnnotationGeneration: "1",
				},
			},
			Spec: v2alpha1.CacheSpec{
				ClusterName: cl.Infinispan.Name,
				Name:        cacheName,
				Template:    configYaml,
			},
		}
		controllerutil.AddFinalizer(cache, constants.InfinispanFinalizer)
		if err := controllerutil.SetOwnerReference(cl.Infinispan, cache, k8sClient.Scheme()); err != nil {
			return err
		}

		cl.Log.Infof("Creating Cache CR for '%s'\n%s", cacheName, configYaml)
		if err := k8sClient.Create(cl.Ctx, cache); err != nil {
			return fmt.Errorf("unable to create Cache CR for cache '%s': %w", cacheName, err)
		}
		cl.Log.Infof("Cache CR '%s' created", cache.Name)
	} else {
		// Update existing Cache
		maxRetries := 5
		for i := 1; i <= maxRetries; i++ {
			_, err = controllerutil.CreateOrPatch(cl.Ctx, k8sClient, cache, func() error {
				if cache.CreationTimestamp.IsZero() {
					return errors.NewNotFound(schema.ParseGroupResource("caches.infinispan.org"), cache.Name)
				}
				var template, templateName string
				if cache.Spec.Template != "" {
					cl.Log.Infof("Update Cache CR for '%s'\n%s", cache.Name, configYaml)
					// Determinate the original user markup format and convert stream configuration to that format if required
					mediaType := mime.GuessMarkup(cache.Spec.Template)
					if mediaType == mime.ApplicationYaml {
						template = configYaml
					} else {
						ispnClient, err := NewInfinispan(cl.Ctx, cl.Infinispan, cl.Kubernetes)
						if err != nil {
							return fmt.Errorf("unable to create Infinispan client: %w", err)
						}
						if template, err = ispnClient.Caches().ConvertConfiguration(configYaml, mime.ApplicationYaml, mediaType); err != nil {
							return fmt.Errorf("unable to convert cache configuration from '%s' to '%s': %w", mime.ApplicationYaml, mediaType, err)
						}
					}
				} else {
					templateName = cache.Spec.TemplateName
				}

				if cache.ObjectMeta.Annotations == nil {
					cache.ObjectMeta.Annotations = make(map[string]string, 1)
				}
				controllerutil.AddFinalizer(cache, constants.InfinispanFinalizer)
				cache.ObjectMeta.Annotations[constants.ListenerAnnotationGeneration] = strconv.FormatInt(cache.GetGeneration()+1, 10)
				cache.Spec = v2alpha1.CacheSpec{
					Name:         cacheName,
					ClusterName:  cl.Infinispan.Name,
					Template:     template,
					TemplateName: templateName,
				}
				return nil
			})
			if err == nil {
				break
			}

			if !errors.IsConflict(err) {
				return fmt.Errorf("unable to Update Cache CR '%s': %w", cache.Name, err)
			}
			cl.Log.Errorf("Conflict encountered on Cache CR '%s' update. Retry %d..%d", cache.Name, i, maxRetries)
		}
		if err != nil {
			return fmt.Errorf("unable to Update Cache CR %s after %d attempts", cache.Name, maxRetries)
		}
	}
	return nil
}

func (cl *CacheListener) findExistingCacheCR(cacheName, clusterName string) (*v2alpha1.Cache, error) {
	cacheList := &v2alpha1.CacheList{}
	listOpts := &client.ListOptions{
		Namespace: cl.Infinispan.Namespace,
	}
	if err := cl.Kubernetes.Client.List(cl.Ctx, cacheList, listOpts); err != nil {
		return nil, fmt.Errorf("unable to list existing Cache CRs: %w", err)
	}

	var caches []v2alpha1.Cache
	for _, c := range cacheList.Items {
		if c.Spec.Name == cacheName && c.Spec.ClusterName == clusterName {
			caches = append(caches, c)
		}
	}
	switch len(caches) {
	case 0:
		cl.Log.Debugf("No existing Cache CR found for Cache=%s, Cluster=%s", cacheName, clusterName)
		return nil, nil
	case 1:
		cl.Log.Debugf("An existing Cache CR '%s' was found for Cache=%s, Cluster=%s", caches[0].Name, cacheName, clusterName)
		return &caches[0], nil
	default:
		// Multiple existing Cache CRs found. Should never happen
		y, _ := yaml.Marshal(caches)
		return nil, fmt.Errorf("More than one Cache CR found for Cache=%s, Cluster=%s:\n%s", cacheName, clusterName, y)
	}
}

func (cl *CacheListener) Delete(data []byte) error {
	cacheName := string(data)
	cl.Log.Infof("Attempting to remove CR associated with cache '%s'", cacheName)

	existingCacheCr, err := cl.findExistingCacheCR(cacheName, cl.Infinispan.Name)
	if existingCacheCr == nil || err != nil {
		return err
	}

	cache := &v2alpha1.Cache{}
	existingCacheCr.DeepCopyInto(cache)

	cl.Log.Infof("Marking Cache CR '%s' for removal", cache.Name)
	_, err = controllerutil.CreateOrPatch(cl.Ctx, cl.Kubernetes.Client, cache, func() error {
		if cache.CreationTimestamp.IsZero() || !cache.DeletionTimestamp.IsZero() {
			return errors.NewNotFound(schema.ParseGroupResource("caches.infinispan.org"), cache.Name)
		}
		if cache.ObjectMeta.Annotations == nil {
			cache.ObjectMeta.Annotations = make(map[string]string, 1)
		}
		cache.ObjectMeta.Annotations[constants.ListenerAnnotationDelete] = "true"
		return nil
	})
	// If the CR can't be found, do nothing
	if !errors.IsNotFound(err) {
		cl.Log.Debugf("Cache CR '%s' not found, nothing todo.", cache.Name)
		return err
	}
	return nil
}

func unmarshallEventConfig(data []byte) (string, string, error) {
	type Config struct {
		Infinispan struct {
			CacheContainer struct {
				Caches map[string]interface{}
			} `yaml:"cacheContainer"`
		}
	}

	config := &Config{}
	if err := yaml.Unmarshal(data, config); err != nil {
		return "", "", fmt.Errorf("unable to unmarshal event data: %w", err)
	}

	if len(config.Infinispan.CacheContainer.Caches) != 1 {
		return "", "", fmt.Errorf("unexpected yaml format: %s", data)
	}
	var cacheName string
	var cacheConfig interface{}
	// Retrieve the first (and only) entry in the map
	for cacheName, cacheConfig = range config.Infinispan.CacheContainer.Caches {
		break
	}

	configYaml, err := yaml.Marshal(cacheConfig)
	if err != nil {
		return "", "", fmt.Errorf("unable to marshall cache configuration: %w", err)
	}
	return cacheName, string(configYaml), nil
}
