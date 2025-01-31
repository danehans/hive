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

package hive

import (
	"bytes"
	"context"
	"crypto/md5"
	"fmt"
	"os"

	log "github.com/sirupsen/logrus"

	hivev1 "github.com/openshift/hive/pkg/apis/hive/v1alpha1"
	"github.com/openshift/hive/pkg/controller/images"
	"github.com/openshift/hive/pkg/operator/assets"
	"github.com/openshift/hive/pkg/operator/util"
	"github.com/openshift/hive/pkg/resource"

	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
)

const (
	dnsServersEnvVar = "ZONE_CHECK_DNS_SERVERS"

	// hiveAdditionalCASecret is the name of the secret in the hive namespace
	// that will contain the aggregate of all AdditionalCertificateAuthorities
	// secrets specified in HiveConfig
	hiveAdditionalCASecret = "hive-additional-ca"
)

func (r *ReconcileHiveConfig) deployHive(hLog log.FieldLogger, h *resource.Helper, instance *hivev1.HiveConfig, recorder events.Recorder) error {

	asset := assets.MustAsset("config/manager/deployment.yaml")
	hLog.Debug("reading deployment")
	hiveDeployment := resourceread.ReadDeploymentV1OrDie(asset)

	if r.hiveImage != "" {
		hiveDeployment.Spec.Template.Spec.Containers[0].Image = r.hiveImage
		// NOTE: overwriting all environment vars here, there are no others at the time of
		// writing:
		hiveImageEnvVar := corev1.EnvVar{
			Name:  images.HiveImageEnvVar,
			Value: r.hiveImage,
		}

		hiveDeployment.Spec.Template.Spec.Containers[0].Env = append(hiveDeployment.Spec.Template.Spec.Containers[0].Env, hiveImageEnvVar)
	}

	if zoneCheckDNSServers := os.Getenv(dnsServersEnvVar); len(zoneCheckDNSServers) > 0 {
		dnsServersEnvVar := corev1.EnvVar{
			Name:  dnsServersEnvVar,
			Value: zoneCheckDNSServers,
		}
		hiveDeployment.Spec.Template.Spec.Containers[0].Env = append(hiveDeployment.Spec.Template.Spec.Containers[0].Env, dnsServersEnvVar)
	}

	if err := r.includeAdditionalCAs(hLog, h, instance, hiveDeployment); err != nil {
		return err
	}

	result, err := h.ApplyRuntimeObject(hiveDeployment, scheme.Scheme)
	if err != nil {
		hLog.WithError(err).Error("error applying deployment")
		return err
	}
	hLog.Infof("deployment applied (%s)", result)

	applyAssets := []string{
		"config/manager/service.yaml",

		// Deploy the desired ClusterImageSets representing installable releases of OpenShift.
		// TODO: in future this should be pipelined somehow.
		"config/clusterimagesets/openshift-4.0-latest.yaml",
		"config/rbac/hive_admin_role.yaml",
		"config/rbac/hive_admin_role_binding.yaml",
		"config/rbac/hive_reader_role.yaml",
		"config/rbac/hive_reader_role_binding.yaml",

		// Due to bug with OLM not updating CRDs on upgrades, we are re-applying
		// the latest in the operator to ensure updates roll out.
		"config/crds/hive_v1alpha1_clusterdeployment.yaml",
		"config/crds/hive_v1alpha1_clusterdeprovisionrequest.yaml",
		"config/crds/hive_v1alpha1_clusterimageset.yaml",
		"config/crds/hive_v1alpha1_dnsendpoint.yaml",
		"config/crds/hive_v1alpha1_dnszone.yaml",
		"config/crds/hive_v1alpha1_hiveconfig.yaml",
		"config/crds/hive_v1alpha1_selectorsyncidentityprovider.yaml",
		"config/crds/hive_v1alpha1_selectorsyncset.yaml",
		"config/crds/hive_v1alpha1_syncidentityprovider.yaml",
		"config/crds/hive_v1alpha1_syncset.yaml",

		"config/configmaps/install-log-regexes-configmap.yaml",
	}
	for _, a := range applyAssets {
		err = util.ApplyAsset(h, a, hLog)
		if err != nil {
			return err
		}
	}

	// Remove legacy ClusterImageSets we do not want installable anymore.
	removeImageSets := []string{
		"openshift-v4.0-beta3",
		"openshift-v4.0-beta4",
	}
	for _, isName := range removeImageSets {
		clusterImageSet := &hivev1.ClusterImageSet{}
		err := r.Get(context.Background(), types.NamespacedName{Name: isName}, clusterImageSet)
		if err != nil && !errors.IsNotFound(err) {
			hLog.WithError(err).Error("error looking for obsolete ClusterImageSet")
			return err
		} else if err != nil {
			hLog.WithField("clusterImageSet", isName).Debug("legacy ClusterImageSet does not exist")
		} else {
			err = r.Delete(context.Background(), clusterImageSet)
			if err != nil {
				hLog.WithError(err).WithField("clusterImageSet", clusterImageSet).Error(
					"error deleting outdated ClusterImageSet")
				return err
			}
			hLog.WithField("clusterImageSet", isName).Info("deleted outdated ClusterImageSet")
		}

	}

	hLog.Info("all hive components successfully reconciled")
	return nil
}

func (r *ReconcileHiveConfig) includeAdditionalCAs(hLog log.FieldLogger, h *resource.Helper, instance *hivev1.HiveConfig, hiveDeployment *appsv1.Deployment) error {
	additionalCA := &bytes.Buffer{}
	for _, clientCARef := range instance.Spec.AdditionalCertificateAuthorities {
		caSecret := &corev1.Secret{}
		err := r.Get(context.TODO(), types.NamespacedName{Namespace: hiveNamespace, Name: clientCARef.Name}, caSecret)
		if err != nil {
			hLog.WithError(err).WithField("secret", clientCARef.Name).Errorf("Cannot read client CA secret")
			continue
		}
		crt, ok := caSecret.Data["ca.crt"]
		if !ok {
			hLog.WithField("secret", clientCARef.Name).Warning("Secret does not contain expected key (ca.crt)")
		}
		fmt.Fprintf(additionalCA, "%s\n", crt)
	}

	if additionalCA.Len() == 0 {
		caSecret := &corev1.Secret{}
		err := r.Get(context.TODO(), types.NamespacedName{Namespace: hiveNamespace, Name: hiveAdditionalCASecret}, caSecret)
		if err == nil {
			err = r.Delete(context.TODO(), caSecret)
			if err != nil {
				hLog.WithError(err).WithField("secret", fmt.Sprintf("%s/%s", hiveNamespace, hiveAdditionalCASecret)).
					Error("cannot delete hive additional ca secret")
				return err
			}
		}
		return nil
	}

	caSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: hiveNamespace,
			Name:      hiveAdditionalCASecret,
		},
		Data: map[string][]byte{
			"ca.crt": additionalCA.Bytes(),
		},
	}
	result, err := h.ApplyRuntimeObject(caSecret, scheme.Scheme)
	if err != nil {
		hLog.WithError(err).Error("error applying additional cert secret")
		return err
	}
	hLog.Infof("additional cert secret applied (%s)", result)

	// Generating a volume name with a hash based on the contents of the additional CA
	// secret will ensure that when there are changes to the secret, the hive controller
	// will be re-deployed.
	hash := fmt.Sprintf("%x", md5.Sum(additionalCA.Bytes()))
	volumeName := fmt.Sprintf("additionalca-%s", hash[:20])

	hiveDeployment.Spec.Template.Spec.Volumes = append(hiveDeployment.Spec.Template.Spec.Volumes, corev1.Volume{
		Name: volumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: hiveAdditionalCASecret,
			},
		},
	})

	hiveDeployment.Spec.Template.Spec.Containers[0].VolumeMounts = append(hiveDeployment.Spec.Template.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{
		Name:      volumeName,
		MountPath: "/additional/ca",
		ReadOnly:  true,
	})

	hiveDeployment.Spec.Template.Spec.Containers[0].Env = append(hiveDeployment.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{
		Name:  "ADDITIONAL_CA",
		Value: "/additional/ca/ca.crt",
	})

	return nil
}
