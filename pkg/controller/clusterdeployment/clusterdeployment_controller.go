/*
Copyright 2018 The Kubernetes Authors.

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

package clusterdeployment

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	kapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	routev1 "github.com/openshift/api/route/v1"

	apihelpers "github.com/openshift/hive/pkg/apis/helpers"
	hivev1 "github.com/openshift/hive/pkg/apis/hive/v1alpha1"
	"github.com/openshift/hive/pkg/controller/images"
	hivemetrics "github.com/openshift/hive/pkg/controller/metrics"
	controllerutils "github.com/openshift/hive/pkg/controller/utils"
	"github.com/openshift/hive/pkg/imageset"
	"github.com/openshift/hive/pkg/install"
)

const (
	controllerName = "clusterDeployment"

	// serviceAccountName will be a service account that can run the installer and then
	// upload artifacts to the cluster's namespace.
	serviceAccountName = "cluster-installer"

	// deleteAfterAnnotation is the annotation that contains a duration after which the cluster should be cleaned up.
	deleteAfterAnnotation       = "hive.openshift.io/delete-after"
	adminCredsSecretPasswordKey = "password"
	adminSSHKeySecretKey        = "ssh-publickey"
	adminKubeconfigKey          = "kubeconfig"
	rawAdminKubeconfigKey       = "raw-kubeconfig"
	clusterVersionObjectName    = "version"
	clusterVersionUnknown       = "undef"

	clusterDeploymentGenerationAnnotation = "hive.openshift.io/cluster-deployment-generation"
	clusterImageSetNotFoundReason         = "ClusterImageSetNotFound"
	clusterImageSetFoundReason            = "ClusterImageSetFound"

	dnsZoneCheckInterval = 30 * time.Second

	defaultRequeueTime = 10 * time.Second

	jobHashAnnotation = "hive.openshift.io/jobhash"
)

var (
	metricCompletedInstallJobRestarts = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "hive_cluster_deployment_completed_install_restart",
			Help:    "Distribution of the number of restarts for all completed cluster installations.",
			Buckets: []float64{0, 2, 10, 20, 50},
		},
		[]string{"cluster_type"},
	)
	metricInstallJobDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "hive_cluster_deployment_install_job_duration_seconds",
			Help:    "Distribution of the runtime of completed install jobs.",
			Buckets: []float64{60, 300, 600, 1200, 1800, 2400, 3000, 3600},
		},
	)
	metricInstallDelaySeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "hive_cluster_deployment_install_job_delay_seconds",
			Help:    "Time between cluster deployment creation and creation of the job to install/provision the cluster.",
			Buckets: []float64{30, 60, 120, 300, 600, 1200, 1800},
		},
	)
	metricImageSetDelaySeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "hive_cluster_deployment_imageset_job_delay_seconds",
			Help:    "Time between cluster deployment creation and creation of the job which resolves the installer image to use for a ClusterImageSet.",
			Buckets: []float64{10, 30, 60, 300, 600, 1200, 1800},
		},
	)
	metricClustersCreated = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hive_cluster_deployments_created_total",
		Help: "Counter incremented every time we observe a new cluster.",
	},
		[]string{"cluster_type"},
	)
	metricClustersInstalled = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hive_cluster_deployments_installed_total",
		Help: "Counter incremented every time we observe a successful installation.",
	},
		[]string{"cluster_type"},
	)
	metricClustersDeleted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hive_cluster_deployments_deleted_total",
		Help: "Counter incremented every time we observe a deleted cluster.",
	},
		[]string{"cluster_type"},
	)

	// regex to find/replace wildcard ingress entries
	// case-insensitive leading literal '*' followed by a literal '.'
	wildcardDomain = regexp.MustCompile(`(?i)^\*\.`)
)

func init() {
	metrics.Registry.MustRegister(metricInstallJobDuration)
	metrics.Registry.MustRegister(metricCompletedInstallJobRestarts)
	metrics.Registry.MustRegister(metricInstallDelaySeconds)
	metrics.Registry.MustRegister(metricImageSetDelaySeconds)
	metrics.Registry.MustRegister(metricClustersCreated)
	metrics.Registry.MustRegister(metricClustersInstalled)
	metrics.Registry.MustRegister(metricClustersDeleted)
}

// Add creates a new ClusterDeployment Controller and adds it to the Manager with default RBAC. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return AddToManager(mgr, NewReconciler(mgr))
}

// NewReconciler returns a new reconcile.Reconciler
func NewReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileClusterDeployment{
		Client:                        hivemetrics.NewClientWithMetricsOrDie(mgr, controllerName),
		scheme:                        mgr.GetScheme(),
		remoteClusterAPIClientBuilder: controllerutils.BuildClusterAPIClientFromKubeconfig,
	}
}

// AddToManager adds a new Controller to mgr with r as the reconcile.Reconciler
func AddToManager(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("clusterdeployment-controller", mgr, controller.Options{Reconciler: r, MaxConcurrentReconciles: controllerutils.GetConcurrentReconciles()})
	if err != nil {
		return err
	}

	// Watch for changes to ClusterDeployment
	err = c.Watch(&source.Kind{Type: &hivev1.ClusterDeployment{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// Watch for jobs created by a ClusterDeployment:
	err = c.Watch(&source.Kind{Type: &batchv1.Job{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &hivev1.ClusterDeployment{},
	})

	// Watch for pods created by an install job:
	err = c.Watch(&source.Kind{Type: &corev1.Pod{}}, &handler.EnqueueRequestsFromMapFunc{
		ToRequests: handler.ToRequestsFunc(selectorPodWatchHandler),
	})
	if err != nil {
		return err
	}

	// Watch for deprovision requests created by a ClusterDeployment:
	err = c.Watch(&source.Kind{Type: &hivev1.ClusterDeprovisionRequest{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &hivev1.ClusterDeployment{},
	})

	// Watch for dnszones created by a ClusterDeployment:
	err = c.Watch(&source.Kind{Type: &hivev1.DNSZone{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &hivev1.ClusterDeployment{},
	})
	if err != nil {
		return err
	}

	return nil
}

var _ reconcile.Reconciler = &ReconcileClusterDeployment{}

// ReconcileClusterDeployment reconciles a ClusterDeployment object
type ReconcileClusterDeployment struct {
	client.Client
	scheme *runtime.Scheme

	// remoteClusterAPIClientBuilder is a function pointer to the function that builds a client for the
	// remote cluster's cluster-api
	remoteClusterAPIClientBuilder func(string) (client.Client, error)
}

// Reconcile reads that state of the cluster for a ClusterDeployment object and makes changes based on the state read
// and what is in the ClusterDeployment.Spec
//
// Automatically generate RBAC rules to allow the Controller to read and write Deployments
//
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=serviceaccounts;secrets;configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods;namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=hive.openshift.io,resources=clusterdeployments;clusterdeployments/status;clusterdeployments/finalizers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=hive.openshift.io,resources=clusterimagesets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=hive.openshift.io,resources=clusterimagesets/status,verbs=get;update;patch
func (r *ReconcileClusterDeployment) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	start := time.Now()
	cdLog := log.WithFields(log.Fields{
		"clusterDeployment": request.Name,
		"namespace":         request.Namespace,
		"controller":        controllerName,
	})

	// For logging, we need to see when the reconciliation loop starts and ends.
	cdLog.Info("reconciling cluster deployment")
	defer func() {
		dur := time.Since(start)
		cdLog.WithField("elapsed", dur).Info("reconcile complete")
	}()

	// Fetch the ClusterDeployment instance
	cd := &hivev1.ClusterDeployment{}
	err := r.Get(context.TODO(), request.NamespacedName, cd)
	if err != nil {
		if errors.IsNotFound(err) {
			// Object not found, return.  Created objects are automatically garbage collected.
			// For additional cleanup logic use finalizers.
			cdLog.Info("cluster deployment Not Found")
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		cdLog.WithError(err).Error("Error getting cluster deployment")
		return reconcile.Result{}, err
	}

	return r.reconcile(request, cd, cdLog)
}

func (r *ReconcileClusterDeployment) reconcile(request reconcile.Request, cd *hivev1.ClusterDeployment, cdLog log.FieldLogger) (reconcile.Result, error) {
	origCD := cd
	cd = cd.DeepCopy()

	// We previously allowed clusterdeployment.spec.ingress[] entries to have ingress domains with a leading '*'.
	// Migrate the clusterdeployment to the new format if we find a wildcard ingress domain.
	// TODO: we can one day remove this once all clusterdeployment are known to have non-wildcard data
	if migrateWildcardIngress(cd) {
		cdLog.Info("migrating wildcard ingress entries")
		err := r.Update(context.TODO(), cd)
		if err != nil {
			cdLog.WithError(err).Error("failed to update cluster deployment")
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, nil
	}

	imageSet, modified, err := r.getClusterImageSet(cd, cdLog)
	if modified || err != nil {
		return reconcile.Result{}, err
	}

	hiveImage := r.getHiveImage(cd, imageSet, cdLog)
	releaseImage := r.getReleaseImage(cd, imageSet, cdLog)

	if cd.DeletionTimestamp != nil {
		if !controllerutils.HasFinalizer(cd, hivev1.FinalizerDeprovision) {
			clearUnderwaySecondsMetrics(cd)
			return reconcile.Result{}, nil
		}

		// Deprovision still underway, report metric for this cluster.
		hivemetrics.MetricClusterDeploymentDeprovisioningUnderwaySeconds.WithLabelValues(
			cd.Name,
			cd.Namespace,
			hivemetrics.GetClusterDeploymentType(cd)).Set(
			time.Since(cd.DeletionTimestamp.Time).Seconds())

		// If the cluster never made it to installed, make sure we clear the provisioning
		// underway metric.
		if !cd.Status.Installed {
			hivemetrics.MetricClusterDeploymentProvisionUnderwaySeconds.WithLabelValues(
				cd.Name,
				cd.Namespace,
				hivemetrics.GetClusterDeploymentType(cd)).Set(0.0)
		}

		return r.syncDeletedClusterDeployment(cd, hiveImage, cdLog)
	}

	// requeueAfter will be used to determine if cluster should be requeued after
	// reconcile has completed
	var requeueAfter time.Duration
	// Check for the delete-after annotation, and if the cluster has expired, delete it
	deleteAfter, ok := cd.Annotations[deleteAfterAnnotation]
	if ok {
		cdLog.Debugf("found delete after annotation: %s", deleteAfter)
		dur, err := time.ParseDuration(deleteAfter)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("error parsing %s as a duration: %v", deleteAfterAnnotation, err)
		}
		if !cd.CreationTimestamp.IsZero() {
			expiry := cd.CreationTimestamp.Add(dur)
			cdLog.Debugf("cluster expires at: %s", expiry)
			if time.Now().After(expiry) {
				cdLog.WithField("expiry", expiry).Info("cluster has expired, issuing delete")
				err := r.Delete(context.TODO(), cd)
				if err != nil {
					cdLog.WithError(err).Error("error deleting expired cluster")
				}
				return reconcile.Result{}, err
			}

			// We have an expiry time but we're not expired yet. Set requeueAfter for just after expiry time
			// so that we requeue cluster for deletion once reconcile has completed
			requeueAfter = expiry.Sub(time.Now()) + 60*time.Second
		}
	}

	if !controllerutils.HasFinalizer(cd, hivev1.FinalizerDeprovision) {
		cdLog.Debugf("adding clusterdeployment finalizer")
		if err := r.addClusterDeploymentFinalizer(cd); err != nil {
			cdLog.WithError(err).Error("error adding finalizer")
			return reconcile.Result{}, err
		}
		metricClustersCreated.WithLabelValues(hivemetrics.GetClusterDeploymentType(cd)).Inc()
		return reconcile.Result{}, nil
	}

	cdLog.Debug("loading SSH key secret")
	if cd.Spec.SSHKey == nil {
		cdLog.Error("cluster has no ssh key set, unable to launch install")
		return reconcile.Result{}, fmt.Errorf("cluster has no ssh key set, unable to launch install")
	}
	sshKey, err := controllerutils.LoadSecretData(r.Client, cd.Spec.SSHKey.Name,
		cd.Namespace, adminSSHKeySecretKey)
	if err != nil {
		cdLog.WithError(err).Error("unable to load ssh key from secret")
		return reconcile.Result{}, err
	}

	if cd.Status.InstallerImage == nil {
		return r.resolveInstallerImage(cd, imageSet, releaseImage, hiveImage, cdLog)
	}

	if cd.Spec.ManageDNS {
		managedDNSZoneAvailable, err := r.ensureManagedDNSZone(cd, cdLog)
		if err != nil {
			return reconcile.Result{}, err
		}
		if !managedDNSZoneAvailable {
			// The clusterdeployment will be queued when the owned DNSZone's status
			// is updated to available.
			cdLog.Debug("DNSZone is not yet available. Waiting for zone to become available.")
			return reconcile.Result{}, nil
		}
	}

	// firstInstalledObserve is the flag that is used for reporting the provision job duration metric
	firstInstalledObserve := false
	containerRestarts := 0
	// Check if an install job already exists:
	existingJob := &batchv1.Job{}
	installJobName := install.GetInstallJobName(cd)
	err = r.Get(context.TODO(), types.NamespacedName{Name: installJobName, Namespace: cd.Namespace}, existingJob)
	if err != nil && errors.IsNotFound(err) {
		cdLog.Debug("no install job exists")
		existingJob = nil
	} else if err != nil {
		cdLog.WithError(err).Error("error looking for install job")
		return reconcile.Result{}, err
	} else if err == nil && !existingJob.DeletionTimestamp.IsZero() {
		cdLog.WithError(err).Error("install job is being deleted, requeueing to wait for deletion")
		return reconcile.Result{RequeueAfter: defaultRequeueTime}, nil
	} else {
		// setting the flag so that we can report the metric after cd is installed
		if existingJob.Status.Succeeded > 0 && !cd.Status.Installed {
			firstInstalledObserve = true
		}
	}

	if cd.Status.Installed {
		cdLog.Debug("cluster is already installed, no processing of install job needed")
	} else {
		// Indicate that the cluster is still installing:
		hivemetrics.MetricClusterDeploymentProvisionUnderwaySeconds.WithLabelValues(
			cd.Name,
			cd.Namespace,
			hivemetrics.GetClusterDeploymentType(cd)).Set(
			time.Since(cd.CreationTimestamp.Time).Seconds())

		cdLog.Debug("loading pull secret secret")
		pullSecret, err := controllerutils.LoadSecretData(r.Client, cd.Spec.PullSecret.Name, cd.Namespace, corev1.DockerConfigJsonKey)
		if err != nil {
			cdLog.WithError(err).Error("unable to load pull secret from secret")
			return reconcile.Result{}, err
		}

		job, cfgMap, err := install.GenerateInstallerJob(
			cd,
			hiveImage,
			releaseImage,
			serviceAccountName,
			sshKey,
			pullSecret)
		if err != nil {
			cdLog.WithError(err).Error("error generating install job")
			return reconcile.Result{}, err
		}

		jobHash, err := calculateJobSpecHash(job)
		if err != nil {
			cdLog.WithError(err).Error("failed to calulcate hash for generated install job")
			return reconcile.Result{}, err
		}
		if job.Annotations == nil {
			job.Annotations = map[string]string{}
		}
		job.Annotations[jobHashAnnotation] = jobHash

		if err = controllerutil.SetControllerReference(cd, job, r.scheme); err != nil {
			cdLog.WithError(err).Error("error setting controller reference on job")
			return reconcile.Result{}, err
		}
		if err = controllerutil.SetControllerReference(cd, cfgMap, r.scheme); err != nil {
			cdLog.WithError(err).Error("error setting controller reference on config map")
			return reconcile.Result{}, err
		}

		cdLog = cdLog.WithField("job", job.Name)

		// Check if the ConfigMap already exists for this ClusterDeployment:
		cdLog.Debug("checking if install-config.yaml config map exists")
		existingCfgMap := &kapi.ConfigMap{}
		err = r.Get(context.TODO(), types.NamespacedName{Name: cfgMap.Name, Namespace: cfgMap.Namespace}, existingCfgMap)
		if err != nil && errors.IsNotFound(err) {
			cdLog.WithField("configMap", cfgMap.Name).Infof("creating config map")
			err = r.Create(context.TODO(), cfgMap)
			if err != nil {
				cdLog.Errorf("error creating config map: %v", err)
				return reconcile.Result{}, err
			}
		} else if err != nil {
			cdLog.Errorf("error getting config map: %v", err)
			return reconcile.Result{}, err
		}

		if existingJob == nil {
			cdLog.Infof("creating install job")
			_, err = controllerutils.SetupClusterInstallServiceAccount(r, cd.Namespace, cdLog)
			if err != nil {
				cdLog.WithError(err).Error("error setting up service account and role")
				return reconcile.Result{}, err
			}

			err = r.Create(context.TODO(), job)
			if err != nil {
				cdLog.Errorf("error creating job: %v", err)
				return reconcile.Result{}, err
			}
			kickstartDuration := time.Since(cd.CreationTimestamp.Time)
			cdLog.WithField("elapsed", kickstartDuration.Seconds()).Info("calculated time to install job seconds")
			metricInstallDelaySeconds.Observe(float64(kickstartDuration.Seconds()))
		} else {
			cdLog.Debug("provision job exists")
			containerRestarts, err = r.calcInstallPodRestarts(cd, cdLog)
			if err != nil {
				// Metrics calculation should not shut down reconciliation, logging and moving on.
				log.WithError(err).Warn("error listing pods, unable to calculate pod restarts but continuing")
			} else {
				if containerRestarts > 0 {
					cdLog.WithFields(log.Fields{
						"restarts": containerRestarts,
					}).Warn("install pod has restarted")
				}

				// Store the restart count on the cluster deployment status.
				cd.Status.InstallRestarts = containerRestarts
			}

			if existingJob.Annotations != nil && cfgMap.Annotations != nil {
				didGenerationChange, err := r.updateOutdatedConfigurations(cd.Generation, existingJob, cfgMap, cdLog)
				if didGenerationChange || err != nil {
					return reconcile.Result{}, err
				}
			}

			jobDeleted, err := r.deleteJobOnHashChange(existingJob, job, cdLog)
			if jobDeleted || err != nil {
				return reconcile.Result{}, err
			}
		}
	}

	err = r.updateClusterDeploymentStatus(cd, origCD, existingJob, cdLog)
	if err != nil {
		cdLog.WithError(err).Errorf("error updating cluster deployment status")
		return reconcile.Result{}, err
	}

	// firstInstalledObserve will be true if this is the first time we've noticed the install job completed.
	// If true, we know we can report the metrics associated with a completed job.
	if firstInstalledObserve {
		// jobDuration calculates the time elapsed since the install job started
		jobDuration := existingJob.Status.CompletionTime.Time.Sub(existingJob.Status.StartTime.Time)
		cdLog.WithField("duration", jobDuration.Seconds()).Debug("install job completed")
		metricInstallJobDuration.Observe(float64(jobDuration.Seconds()))

		// Report a metric for the total number of container restarts:
		metricCompletedInstallJobRestarts.WithLabelValues(hivemetrics.GetClusterDeploymentType(cd)).
			Observe(float64(containerRestarts))

		// Clear the install underway seconds metric. After this no-one should be reporting
		// this metric for this cluster.
		hivemetrics.MetricClusterDeploymentProvisionUnderwaySeconds.WithLabelValues(
			cd.Name,
			cd.Namespace,
			hivemetrics.GetClusterDeploymentType(cd)).Set(0.0)

		metricClustersInstalled.WithLabelValues(hivemetrics.GetClusterDeploymentType(cd)).Inc()
	}

	// Check for requeueAfter duration
	if requeueAfter != 0 {
		cdLog.Debugf("cluster will re-sync due to expiry time in: %v", requeueAfter)
		return reconcile.Result{RequeueAfter: requeueAfter}, nil
	}
	return reconcile.Result{}, nil
}

// getHiveImage looks for a Hive image to use in clusterdeployment jobs in the following order:
// 1 - specified in the cluster deployment spec.images.hiveImage
// 2 - referenced in the cluster deployment spec.imageSet
// 3 - specified via environment variable to the hive controller
// 4 - fallback default hardcoded image reference
func (r *ReconcileClusterDeployment) getHiveImage(cd *hivev1.ClusterDeployment, imageSet *hivev1.ClusterImageSet, cdLog log.FieldLogger) string {
	if cd.Spec.Images.HiveImage != "" {
		return cd.Spec.Images.HiveImage
	}
	if imageSet != nil && imageSet.Spec.HiveImage != nil {
		return *imageSet.Spec.HiveImage
	}
	return images.GetHiveImage(cdLog)
}

// getReleaseImage looks for a a release image in clusterdeployment or its corresponding imageset in the following order:
// 1 - specified in the cluster deployment spec.images.releaseImage
// 2 - referenced in the cluster deployment spec.imageSet
func (r *ReconcileClusterDeployment) getReleaseImage(cd *hivev1.ClusterDeployment, imageSet *hivev1.ClusterImageSet, cdLog log.FieldLogger) string {
	if cd.Spec.Images.ReleaseImage != "" {
		return cd.Spec.Images.ReleaseImage
	}
	if imageSet != nil && imageSet.Spec.ReleaseImage != nil {
		return *imageSet.Spec.ReleaseImage
	}
	return ""
}

func (r *ReconcileClusterDeployment) getClusterImageSet(cd *hivev1.ClusterDeployment, cdLog log.FieldLogger) (*hivev1.ClusterImageSet, bool, error) {
	if cd.Spec.ImageSet == nil || len(cd.Spec.ImageSet.Name) == 0 {
		return nil, false, nil
	}
	imageSet := &hivev1.ClusterImageSet{}
	err := r.Get(context.TODO(), types.NamespacedName{Name: cd.Spec.ImageSet.Name}, imageSet)
	switch {
	case errors.IsNotFound(err):
		cdLog.WithField("clusterimageset", cd.Spec.ImageSet.Name).Warning("clusterdeployment references non-existent clusterimageset")
		modified, err := r.setImageSetNotFoundCondition(cd, false, cdLog)
		return nil, modified, err
	case err != nil:
		cdLog.WithError(err).WithField("clusterimageset", cd.Spec.ImageSet.Name).Error("unexpected error retrieving clusterimageset")
		return nil, false, err
	default:
		return imageSet, false, nil
	}
}

func (r *ReconcileClusterDeployment) statusUpdate(cd *hivev1.ClusterDeployment, cdLog log.FieldLogger) error {
	err := r.Status().Update(context.TODO(), cd)
	if err != nil {
		cdLog.WithError(err).Error("cannot update clusterdeployment status")
	}
	return err
}

func (r *ReconcileClusterDeployment) resolveInstallerImage(cd *hivev1.ClusterDeployment, imageSet *hivev1.ClusterImageSet, releaseImage, hiveImage string, cdLog log.FieldLogger) (reconcile.Result, error) {
	if len(cd.Spec.Images.InstallerImage) > 0 {
		cdLog.WithField("image", cd.Spec.Images.InstallerImage).
			Debug("setting status.InstallerImage to the value in spec.images.installerImage")
		cd.Status.InstallerImage = &cd.Spec.Images.InstallerImage
		return reconcile.Result{}, r.statusUpdate(cd, cdLog)
	}
	if imageSet != nil && imageSet.Spec.InstallerImage != nil {
		cd.Status.InstallerImage = imageSet.Spec.InstallerImage
		cdLog.WithField("imageset", imageSet.Name).Debug("setting status.InstallerImage using imageSet.Spec.InstallerImage")
		return reconcile.Result{}, r.statusUpdate(cd, cdLog)
	}
	cliImage := images.GetCLIImage(cdLog)
	job := imageset.GenerateImageSetJob(cd, releaseImage, serviceAccountName, imageset.AlwaysPullImage(cliImage), imageset.AlwaysPullImage(hiveImage))
	if err := controllerutil.SetControllerReference(cd, job, r.scheme); err != nil {
		cdLog.WithError(err).Error("error setting controller reference on job")
		return reconcile.Result{}, err
	}

	jobName := types.NamespacedName{Name: job.Name, Namespace: job.Namespace}
	jobLog := cdLog.WithField("job", jobName)

	existingJob := &batchv1.Job{}
	err := r.Get(context.TODO(), jobName, existingJob)
	switch {
	// If the job exists but is in the process of getting deleted, requeue and wait for the delete
	// to complete.
	case err == nil && !job.DeletionTimestamp.IsZero():
		jobLog.Debug("imageset job is being deleted. Will recreate once deleted")
		return reconcile.Result{RequeueAfter: defaultRequeueTime}, err
	// If job exists and is finished, delete so we can recreate it
	case err == nil && controllerutils.IsFinished(existingJob):
		jobLog.WithField("successful", controllerutils.IsSuccessful(existingJob)).
			Warning("Finished job found, but installer image is not yet resolved. Deleting.")
		err := r.Delete(context.Background(), existingJob,
			client.PropagationPolicy(metav1.DeletePropagationForeground))
		if err != nil {
			jobLog.WithError(err).Error("cannot delete imageset job")
		}
		return reconcile.Result{}, err
	case errors.IsNotFound(err):
		jobLog.WithField("releaseImage", releaseImage).Info("creating imageset job")
		_, err = controllerutils.SetupClusterInstallServiceAccount(r, cd.Namespace, cdLog)
		if err != nil {
			cdLog.WithError(err).Error("error setting up service account and role")
			return reconcile.Result{}, err
		}

		err = r.Create(context.TODO(), job)
		if err != nil {
			jobLog.WithError(err).Error("error creating job")
		} else {
			// kickstartDuration calculates the delay between creation of cd and start of imageset job
			kickstartDuration := time.Since(cd.CreationTimestamp.Time)
			cdLog.WithField("elapsed", kickstartDuration.Seconds()).Info("calculated time to imageset job seconds")
			metricImageSetDelaySeconds.Observe(float64(kickstartDuration.Seconds()))
		}
		return reconcile.Result{}, err
	case err != nil:
		jobLog.WithError(err).Error("cannot get job")
		return reconcile.Result{}, err
	default:
		jobLog.Debug("job exists and is in progress")
	}
	return reconcile.Result{}, nil
}

func (r *ReconcileClusterDeployment) setImageSetNotFoundCondition(cd *hivev1.ClusterDeployment, isNotFound bool, cdLog log.FieldLogger) (modified bool, err error) {
	original := cd.DeepCopy()
	status := corev1.ConditionFalse
	reason := clusterImageSetFoundReason
	message := fmt.Sprintf("ClusterImageSet %s is available", cd.Spec.ImageSet.Name)
	if isNotFound {
		status = corev1.ConditionTrue
		reason = clusterImageSetNotFoundReason
		message = fmt.Sprintf("ClusterImageSet %s is not available", cd.Spec.ImageSet.Name)
	}
	cd.Status.Conditions = controllerutils.SetClusterDeploymentCondition(
		cd.Status.Conditions,
		hivev1.ClusterImageSetNotFoundCondition,
		status,
		reason,
		message,
		controllerutils.UpdateConditionNever)
	if !reflect.DeepEqual(original.Status.Conditions, cd.Status.Conditions) {
		cdLog.Info("setting ClusterImageSetNotFoundCondition to %v", status)
		err := r.Status().Update(context.TODO(), cd)
		if err != nil {
			cdLog.WithError(err).Error("cannot update status conditions")
		}
		return true, err
	}
	return false, nil
}

// Deletes the job if it exists and its generation does not match the cluster deployment's
// genetation. Updates the config map if it is outdated too
func (r *ReconcileClusterDeployment) updateOutdatedConfigurations(cdGeneration int64, existingJob *batchv1.Job, cfgMap *corev1.ConfigMap, cdLog log.FieldLogger) (bool, error) {
	var err error
	var didGenerationChange bool
	if jobGeneration, ok := existingJob.Annotations[clusterDeploymentGenerationAnnotation]; ok {
		convertedJobGeneration, _ := strconv.ParseInt(jobGeneration, 10, 64)
		if convertedJobGeneration < cdGeneration {
			didGenerationChange = true
			cdLog.Info("deleting outdated install job due to cluster deployment generation change")
			err = r.Delete(context.TODO(), existingJob, client.PropagationPolicy(metav1.DeletePropagationForeground))
			if err != nil {
				cdLog.WithError(err).Errorf("error deleting outdated install job")
				return didGenerationChange, err
			}
		}
	}
	if cfgMapGeneration, ok := cfgMap.Annotations[clusterDeploymentGenerationAnnotation]; ok {
		convertedMapGeneration, _ := strconv.ParseInt(cfgMapGeneration, 10, 64)
		if convertedMapGeneration < cdGeneration {
			didGenerationChange = true
			cdLog.Info("deleting outdated installconfig configmap due to cluster deployment generation change")
			err = r.Update(context.TODO(), cfgMap)
			if err != nil {
				cdLog.WithError(err).Errorf("error deleting outdated config map")
				return didGenerationChange, err
			}
		}
	}
	return didGenerationChange, err
}

func (r *ReconcileClusterDeployment) updateClusterDeploymentStatus(cd *hivev1.ClusterDeployment, origCD *hivev1.ClusterDeployment, job *batchv1.Job, cdLog log.FieldLogger) error {
	cdLog.Debug("updating cluster deployment status")
	if job != nil && job.Name != "" && job.Namespace != "" {
		// Job exists, check it's status:
		cd.Status.Installed = controllerutils.IsSuccessful(job)
	}

	// The install manager sets this secret name, but we don't consider it a critical failure and
	// will attempt to heal it here, as the value is predictable.
	if cd.Status.Installed && cd.Status.AdminKubeconfigSecret.Name == "" {
		cd.Status.AdminKubeconfigSecret = corev1.LocalObjectReference{Name: apihelpers.GetResourceName(cd.Name, "admin-kubeconfig")}
	}

	if cd.Status.AdminKubeconfigSecret.Name != "" {
		adminKubeconfigSecret := &corev1.Secret{}
		err := r.Get(context.Background(), types.NamespacedName{Namespace: cd.Namespace, Name: cd.Status.AdminKubeconfigSecret.Name}, adminKubeconfigSecret)
		if err != nil {
			if errors.IsNotFound(err) {
				log.Warn("admin kubeconfig does not yet exist")
			} else {
				return err
			}
		} else {
			err = r.fixupAdminKubeconfigSecret(adminKubeconfigSecret, cdLog)
			if err != nil {
				return err
			}
			err = r.setAdminKubeconfigStatus(cd, adminKubeconfigSecret, cdLog)
			if err != nil {
				return err
			}
		}
	}

	// Update cluster deployment status if changed:
	if !reflect.DeepEqual(cd.Status, origCD.Status) {
		cdLog.Infof("status has changed, updating cluster deployment")
		cdLog.Debugf("orig: %v", origCD)
		cdLog.Debugf("new : %v", cd.Status)
		err := r.Status().Update(context.TODO(), cd)
		if err != nil {
			cdLog.Errorf("error updating cluster deployment: %v", err)
			return err
		}
	} else {
		cdLog.Debug("cluster deployment status unchanged")
	}
	return nil
}

func (r *ReconcileClusterDeployment) fixupAdminKubeconfigSecret(secret *corev1.Secret, cdLog log.FieldLogger) error {
	originalSecret := secret.DeepCopy()

	rawData, hasRawData := secret.Data[rawAdminKubeconfigKey]
	if !hasRawData {
		secret.Data[rawAdminKubeconfigKey] = secret.Data[adminKubeconfigKey]
		rawData = secret.Data[adminKubeconfigKey]
	}

	var err error
	secret.Data[adminKubeconfigKey], err = controllerutils.FixupKubeconfig(rawData)
	if err != nil {
		cdLog.WithError(err).Errorf("cannot fixup kubeconfig to generate new one")
		return err
	}

	if reflect.DeepEqual(originalSecret.Data, secret.Data) {
		cdLog.Debug("secret data has not changed, no need to update")
		return nil
	}

	err = r.Update(context.TODO(), secret)
	if err != nil {
		cdLog.WithError(err).Error("error updated admin kubeconfig secret")
		return err
	}

	return nil
}

// setAdminKubeconfigStatus sets all cluster status fields that depend on the admin kubeconfig.
func (r *ReconcileClusterDeployment) setAdminKubeconfigStatus(cd *hivev1.ClusterDeployment, adminKubeconfigSecret *corev1.Secret, cdLog log.FieldLogger) error {
	if cd.Status.WebConsoleURL == "" || cd.Status.APIURL == "" {
		remoteClusterAPIClient, err := r.remoteClusterAPIClientBuilder(string(adminKubeconfigSecret.Data[adminKubeconfigKey]))
		if err != nil {
			cdLog.WithError(err).Error("error building remote cluster-api client connection")
			return err
		}

		// Parse the admin kubeconfig for the server URL:
		config, err := clientcmd.Load(adminKubeconfigSecret.Data["kubeconfig"])
		if err != nil {
			return err
		}
		cluster, ok := config.Clusters[cd.Spec.ClusterName]
		if !ok {
			return fmt.Errorf("error parsing admin kubeconfig secret data")
		}

		// We should be able to assume only one cluster in here:
		server := cluster.Server
		cdLog.Debugf("found cluster API URL in kubeconfig: %s", server)
		cd.Status.APIURL = server
		routeObject := &routev1.Route{}
		err = remoteClusterAPIClient.Get(context.Background(),
			types.NamespacedName{Namespace: "openshift-console", Name: "console"}, routeObject)
		if err != nil {
			cdLog.WithError(err).Error("error fetching remote route object")
			return err
		}
		cdLog.Debugf("read remote route object: %s", routeObject)
		cd.Status.WebConsoleURL = "https://" + routeObject.Spec.Host
	}
	return nil
}

// ensureManagedDNSZoneDeleted is a safety check to ensure that the child managed DNSZone
// linked to the parent cluster deployment gets a deletionTimestamp when the parent is deleted.
// Normally we expect Kube garbage collection to do this for us, but in rare cases we've seen it
// not working as intended.
func (r *ReconcileClusterDeployment) ensureManagedDNSZoneDeleted(cd *hivev1.ClusterDeployment, cdLog log.FieldLogger) (*reconcile.Result, error) {
	if !cd.Spec.ManageDNS {
		return nil, nil
	}
	dnsZone := &hivev1.DNSZone{}
	dnsZoneNamespacedName := types.NamespacedName{Namespace: cd.Namespace, Name: dnsZoneName(cd.Name)}
	err := r.Get(context.TODO(), dnsZoneNamespacedName, dnsZone)
	if err != nil && !errors.IsNotFound(err) {
		cdLog.WithError(err).Error("error looking up managed dnszone")
		return &reconcile.Result{}, err
	}
	if err != nil {
		cdLog.Debug("managed zone does not exist, nothing to cleanup")
		return nil, nil
	}
	if err == nil && !dnsZone.DeletionTimestamp.IsZero() {
		cdLog.Debug("managed zone is being deleted, will wait for its deletion to complete")
		return &reconcile.Result{RequeueAfter: defaultRequeueTime}, nil
	}
	cdLog.Warn("managed dnszone did not get a deletionTimestamp when parent cluster deployment was deleted, deleting manually")
	err = r.Delete(context.TODO(), dnsZone,
		client.PropagationPolicy(metav1.DeletePropagationForeground))
	if err != nil {
		cdLog.WithError(err).Error("error deleting managed dnszone")
	}
	return &reconcile.Result{}, err
}

func (r *ReconcileClusterDeployment) syncDeletedClusterDeployment(cd *hivev1.ClusterDeployment, hiveImage string, cdLog log.FieldLogger) (reconcile.Result, error) {

	result, err := r.ensureManagedDNSZoneDeleted(cd, cdLog)
	if result != nil {
		return *result, err
	}
	if err != nil {
		return reconcile.Result{}, err
	}

	// Delete the install job in case it's still running:
	installJob := &batchv1.Job{}
	err = r.Get(context.Background(),
		types.NamespacedName{
			Name:      install.GetInstallJobName(cd),
			Namespace: cd.Namespace,
		},
		installJob)
	if err != nil && errors.IsNotFound(err) {
		cdLog.Debug("install job no longer exists, nothing to cleanup")
	} else if err != nil {
		cdLog.WithError(err).Errorf("error getting existing install job for deleted cluster deployment")
		return reconcile.Result{}, err
	} else if err == nil && !installJob.DeletionTimestamp.IsZero() {
		cdLog.Debug("install job is being deleted, requeueing to wait for deletion")
		return reconcile.Result{RequeueAfter: defaultRequeueTime}, nil
	} else {
		err = r.Delete(context.Background(), installJob,
			client.PropagationPolicy(metav1.DeletePropagationForeground))
		if err != nil {
			cdLog.WithError(err).Errorf("error deleting existing install job for deleted cluster deployment")
			return reconcile.Result{}, err
		}
		cdLog.WithField("jobName", installJob.Name).Info("install job deleted")
		return reconcile.Result{}, nil
	}

	// Skips creation of deprovision request if PreserveOnDelete is true and cluster is installed
	if cd.Spec.PreserveOnDelete {
		if cd.Status.Installed {
			cdLog.Warn("skipping creation of deprovisioning request for installed cluster due to PreserveOnDelete=true")
			if controllerutils.HasFinalizer(cd, hivev1.FinalizerDeprovision) {
				err = r.removeClusterDeploymentFinalizer(cd)
				if err != nil {
					cdLog.WithError(err).Error("error removing finalizer")
				}
				return reconcile.Result{}, err
			}
			return reconcile.Result{}, nil
		}
		// Overriding PreserveOnDelete because we might have deleted the cluster deployment before it finished
		// installing, which can cause AWS resources to leak
		cdLog.Infof("PreserveOnDelete=true but creating deprovisioning request as cluster was never successfully provisioned")
	}

	if cd.Status.InfraID == "" {
		cdLog.Warn("skipping uninstall for cluster that never had clusterID set")
		err = r.removeClusterDeploymentFinalizer(cd)
		if err != nil {
			cdLog.WithError(err).Error("error removing finalizer")
		}
		return reconcile.Result{}, err
	}

	// Generate a deprovision request
	request := generateDeprovisionRequest(cd)
	err = controllerutil.SetControllerReference(cd, request, r.scheme)
	if err != nil {
		cdLog.Errorf("error setting controller reference on deprovision request: %v", err)
		return reconcile.Result{}, err
	}

	// Check if deprovision request already exists:
	existingRequest := &hivev1.ClusterDeprovisionRequest{}
	err = r.Get(context.TODO(), types.NamespacedName{Name: cd.Name, Namespace: cd.Namespace}, existingRequest)
	if err != nil && errors.IsNotFound(err) {
		cdLog.Infof("creating deprovision request for cluster deployment")
		err = r.Create(context.TODO(), request)
		if err != nil {
			cdLog.WithError(err).Errorf("error creating deprovision request")
			// Check if namespace is terminated, if so we can give up, remove the finalizer, and let
			// the cluster go away.
			ns := &corev1.Namespace{}
			err = r.Get(context.TODO(), types.NamespacedName{Name: cd.Namespace}, ns)
			if err != nil {
				cdLog.WithError(err).Error("error checking for deletionTimestamp on namespace")
				return reconcile.Result{}, err
			}
			if ns.DeletionTimestamp != nil {
				cdLog.Warn("detected a namespace deleted before deprovision request could be created, giving up on deprovision and removing finalizer")
				err = r.removeClusterDeploymentFinalizer(cd)
				if err != nil {
					cdLog.WithError(err).Error("error removing finalizer")
				}
			}
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, nil
	} else if err != nil {
		cdLog.WithError(err).Errorf("error getting deprovision request")
		return reconcile.Result{}, err
	}

	// Deprovision request exists, check whether it has completed
	if existingRequest.Status.Completed {
		cdLog.Infof("deprovision request completed, removing finalizer")
		err = r.removeClusterDeploymentFinalizer(cd)
		if err != nil {
			cdLog.WithError(err).Error("error removing finalizer")
		}
		return reconcile.Result{}, err
	}

	cdLog.Debug("deprovision request not yet completed")

	return reconcile.Result{}, nil
}

func (r *ReconcileClusterDeployment) addClusterDeploymentFinalizer(cd *hivev1.ClusterDeployment) error {
	cd = cd.DeepCopy()
	controllerutils.AddFinalizer(cd, hivev1.FinalizerDeprovision)
	return r.Update(context.TODO(), cd)
}

func (r *ReconcileClusterDeployment) removeClusterDeploymentFinalizer(cd *hivev1.ClusterDeployment) error {

	cd = cd.DeepCopy()
	controllerutils.DeleteFinalizer(cd, hivev1.FinalizerDeprovision)
	err := r.Update(context.TODO(), cd)

	if err == nil {
		clearUnderwaySecondsMetrics(cd)

		// Increment the clusters deleted counter:
		metricClustersDeleted.WithLabelValues(hivemetrics.GetClusterDeploymentType(cd)).Inc()
	}

	return err
}

func (r *ReconcileClusterDeployment) ensureManagedDNSZone(cd *hivev1.ClusterDeployment, cdLog log.FieldLogger) (bool, error) {
	// for now we only support AWS
	if cd.Spec.AWS == nil || cd.Spec.PlatformSecrets.AWS == nil {
		cdLog.Error("cluster deployment platform is not AWS, cannot manage DNS zone")
		return false, fmt.Errorf("only AWS managed DNS is supported")
	}
	dnsZone := &hivev1.DNSZone{}
	dnsZoneNamespacedName := types.NamespacedName{Namespace: cd.Namespace, Name: dnsZoneName(cd.Name)}
	logger := cdLog.WithField("zone", dnsZoneNamespacedName.String())

	err := r.Get(context.TODO(), dnsZoneNamespacedName, dnsZone)
	if err == nil {
		availableCondition := controllerutils.FindDNSZoneCondition(dnsZone.Status.Conditions, hivev1.ZoneAvailableDNSZoneCondition)
		return availableCondition != nil && availableCondition.Status == corev1.ConditionTrue, nil
	}
	if errors.IsNotFound(err) {
		logger.Info("creating new DNSZone for cluster deployment")
		return false, r.createManagedDNSZone(cd, logger)
	}
	logger.WithError(err).Error("failed to fetch DNS zone")
	return false, err
}

func (r *ReconcileClusterDeployment) createManagedDNSZone(cd *hivev1.ClusterDeployment, logger log.FieldLogger) error {
	dnsZone := &hivev1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dnsZoneName(cd.Name),
			Namespace: cd.Namespace,
		},
		Spec: hivev1.DNSZoneSpec{
			Zone:               cd.Spec.BaseDomain,
			LinkToParentDomain: true,
			AWS: &hivev1.AWSDNSZoneSpec{
				AccountSecret: cd.Spec.PlatformSecrets.AWS.Credentials,
				Region:        cd.Spec.AWS.Region,
			},
		},
	}

	for k, v := range cd.Spec.AWS.UserTags {
		dnsZone.Spec.AWS.AdditionalTags = append(dnsZone.Spec.AWS.AdditionalTags, hivev1.AWSResourceTag{Key: k, Value: v})
	}

	if err := controllerutil.SetControllerReference(cd, dnsZone, r.scheme); err != nil {
		logger.WithError(err).Error("error setting controller reference on dnszone")
		return err
	}

	err := r.Create(context.TODO(), dnsZone)
	if err != nil {
		logger.WithError(err).Error("cannot create DNS zone")
		return err
	}
	logger.Info("dns zone created")
	return nil
}

func dnsZoneName(cdName string) string {
	return apihelpers.GetResourceName(cdName, "zone")
}

func selectorPodWatchHandler(a handler.MapObject) []reconcile.Request {
	retval := []reconcile.Request{}

	pod := a.Object.(*corev1.Pod)
	if pod == nil {
		// Wasn't a Pod, bail out. This should not happen.
		log.Errorf("Error converting MapObject.Object to Pod. Value: %+v", a.Object)
		return retval
	}
	if pod.Labels == nil {
		return retval
	}
	cdName, ok := pod.Labels[install.ClusterDeploymentNameLabel]
	if !ok {
		return retval
	}
	retval = append(retval, reconcile.Request{NamespacedName: types.NamespacedName{
		Name:      cdName,
		Namespace: pod.Namespace,
	}})
	return retval
}

func (r *ReconcileClusterDeployment) calcInstallPodRestarts(cd *hivev1.ClusterDeployment, cdLog log.FieldLogger) (int, error) {
	installerPodLabels := map[string]string{install.ClusterDeploymentNameLabel: cd.Name, install.InstallJobLabel: "true"}
	parsedLabels := labels.SelectorFromSet(installerPodLabels)
	pods := &corev1.PodList{}
	err := r.Client.List(context.Background(), &client.ListOptions{Namespace: cd.Namespace, LabelSelector: parsedLabels}, pods)
	if err != nil {
		return 0, err
	}

	if len(pods.Items) > 1 {
		log.Warnf("found %d install pods for cluster", len(pods.Items))
	}

	// Calculate restarts across all containers in the pod:
	containerRestarts := 0
	for _, pod := range pods.Items {
		for _, cs := range pod.Status.ContainerStatuses {
			containerRestarts += int(cs.RestartCount)
		}
	}
	return containerRestarts, nil
}

func (r *ReconcileClusterDeployment) deleteJobOnHashChange(existingJob, generatedJob *batchv1.Job, cdLog log.FieldLogger) (bool, error) {
	newJobNeeded := false
	if _, ok := existingJob.Annotations[jobHashAnnotation]; !ok {
		// this job predates tracking the job hash, so assume we need a new job
		newJobNeeded = true
	}

	if existingJob.Annotations[jobHashAnnotation] != generatedJob.Annotations[jobHashAnnotation] {
		// delete the job so we get a fresh one with the new job spec
		newJobNeeded = true
	}

	if newJobNeeded {
		// delete the existing job
		cdLog.Info("deleting existing install job due to updated/missing hash detected")
		err := r.Delete(context.TODO(), existingJob, client.PropagationPolicy(metav1.DeletePropagationForeground))
		if err != nil {
			cdLog.WithError(err).Errorf("error deleting outdated install job")
			return newJobNeeded, err
		}
	}

	return newJobNeeded, nil
}

func generateDeprovisionRequest(cd *hivev1.ClusterDeployment) *hivev1.ClusterDeprovisionRequest {
	req := &hivev1.ClusterDeprovisionRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cd.Name,
			Namespace: cd.Namespace,
		},
		Spec: hivev1.ClusterDeprovisionRequestSpec{
			InfraID:   cd.Status.InfraID,
			ClusterID: cd.Status.ClusterID,
			Platform: hivev1.ClusterDeprovisionRequestPlatform{
				AWS: &hivev1.AWSClusterDeprovisionRequest{},
			},
		},
	}

	if cd.Spec.Platform.AWS != nil {
		req.Spec.Platform.AWS.Region = cd.Spec.Platform.AWS.Region
	}

	if cd.Spec.PlatformSecrets.AWS != nil {
		req.Spec.Platform.AWS.Credentials = &cd.Spec.PlatformSecrets.AWS.Credentials
	}

	return req
}

func migrateWildcardIngress(cd *hivev1.ClusterDeployment) bool {
	migrated := false
	for i, ingress := range cd.Spec.Ingress {
		newIngress := wildcardDomain.ReplaceAllString(ingress.Domain, "")
		if newIngress != ingress.Domain {
			cd.Spec.Ingress[i].Domain = newIngress
			migrated = true
		}
	}
	return migrated
}

func calculateJobSpecHash(job *batchv1.Job) (string, error) {

	hasher := md5.New()
	jobSpecBytes, err := job.Spec.Marshal()
	if err != nil {
		return "", err
	}

	_, err = hasher.Write(jobSpecBytes)
	if err != nil {
		return "", err
	}

	sum := hex.EncodeToString(hasher.Sum(nil))

	return sum, nil
}

func strPtr(s string) *string {
	return &s
}

func clearUnderwaySecondsMetrics(cd *hivev1.ClusterDeployment) {
	// If we've successfully cleared the deprovision finalizer we know this is a good time to
	// reset the underway metric to 0, after which it will no longer be reported.
	hivemetrics.MetricClusterDeploymentDeprovisioningUnderwaySeconds.WithLabelValues(
		cd.Name,
		cd.Namespace,
		hivemetrics.GetClusterDeploymentType(cd)).Set(0.0)

	// Clear the install underway seconds metric if this cluster was still installing.
	if !cd.Status.Installed {
		hivemetrics.MetricClusterDeploymentProvisionUnderwaySeconds.WithLabelValues(
			cd.Name,
			cd.Namespace,
			hivemetrics.GetClusterDeploymentType(cd)).Set(0.0)
	}
}
