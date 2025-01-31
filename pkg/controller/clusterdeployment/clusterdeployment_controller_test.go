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
	"fmt"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	openshiftapiv1 "github.com/openshift/api/config/v1"
	routev1 "github.com/openshift/api/route/v1"
	"github.com/openshift/hive/pkg/apis"
	hivev1 "github.com/openshift/hive/pkg/apis/hive/v1alpha1"
	"github.com/openshift/hive/pkg/controller/images"
	controllerutils "github.com/openshift/hive/pkg/controller/utils"
	"github.com/openshift/hive/pkg/install"
)

const (
	testName              = "foo-lqmsh"
	testClusterName       = "bar"
	testClusterID         = "testFooClusterUUID"
	testInfraID           = "testFooInfraID"
	installJobName        = "foo-lqmsh-install"
	uninstallJobName      = "foo-lqmsh-uninstall"
	imageSetJobName       = "foo-lqmsh-imageset"
	testNamespace         = "default"
	metadataName          = "foo-lqmsh-metadata"
	sshKeySecret          = "ssh-key"
	pullSecretSecret      = "pull-secret"
	testUUID              = "fakeUUID"
	testAMI               = "ami-totallyfake"
	adminKubeconfigSecret = "foo-lqmsh-admin-kubeconfig"
	adminKubeconfig       = `clusters:
- cluster:
    certificate-authority-data: JUNK
    server: https://bar-api.clusters.example.com:6443
  name: bar
`
	testRemoteClusterCurrentVersion = "4.0.0"
	remoteClusterVersionObjectName  = "version"

	remoteClusterRouteObjectName      = "console"
	remoteClusterRouteObjectNamespace = "openshift-console"
	testClusterImageSetName           = "test-image-set"
)

func init() {
	log.SetLevel(log.DebugLevel)
}

func TestClusterDeploymentReconcile(t *testing.T) {
	apis.AddToScheme(scheme.Scheme)
	openshiftapiv1.Install(scheme.Scheme)
	routev1.Install(scheme.Scheme)

	// Utility function to get the test CD from the fake client
	getCD := func(c client.Client) *hivev1.ClusterDeployment {
		cd := &hivev1.ClusterDeployment{}
		err := c.Get(context.TODO(), client.ObjectKey{Name: testName, Namespace: testNamespace}, cd)
		if err == nil {
			return cd
		}
		return nil
	}

	getDNSZone := func(c client.Client) *hivev1.DNSZone {
		zone := &hivev1.DNSZone{}
		err := c.Get(context.TODO(), client.ObjectKey{Name: testName + "-zone", Namespace: testNamespace}, zone)
		if err == nil {
			return zone
		}
		return nil
	}

	getDeprovisionRequest := func(c client.Client) *hivev1.ClusterDeprovisionRequest {
		req := &hivev1.ClusterDeprovisionRequest{}
		err := c.Get(context.TODO(), client.ObjectKey{Name: testName, Namespace: testNamespace}, req)
		if err == nil {
			return req
		}
		return nil
	}
	getImageSetJob := func(c client.Client) *batchv1.Job {
		return getJob(c, imageSetJobName)
	}

	tests := []struct {
		name      string
		existing  []runtime.Object
		expectErr bool
		validate  func(client.Client, *testing.T)
	}{
		{
			name: "Add finalizer",
			existing: []runtime.Object{
				testClusterDeploymentWithoutFinalizer(),
				testSecret(corev1.SecretTypeDockerConfigJson, pullSecretSecret, corev1.DockerConfigJsonKey, "{}"),
				testSecret(corev1.SecretTypeOpaque, sshKeySecret, adminSSHKeySecretKey, "fakesshkey"),
			},
			validate: func(c client.Client, t *testing.T) {
				cd := getCD(c)
				if cd == nil || !controllerutils.HasFinalizer(cd, hivev1.FinalizerDeprovision) {
					t.Errorf("did not get expected clusterdeployment finalizer")
				}
			},
		},
		{
			name: "Create install job",
			existing: []runtime.Object{
				testClusterDeployment(),
				testSecret(corev1.SecretTypeDockerConfigJson, pullSecretSecret, corev1.DockerConfigJsonKey, "{}"),
				testSecret(corev1.SecretTypeOpaque, sshKeySecret, adminSSHKeySecretKey, "fakesshkey"),
			},
			validate: func(c client.Client, t *testing.T) {
				job := getInstallJob(c)
				if job == nil {
					t.Errorf("did not find expected install job")
				}
			},
		},
		{
			name: "No-op Running install job",
			existing: []runtime.Object{
				testClusterDeployment(),
				testInstallJob(),
				testSecret(corev1.SecretTypeDockerConfigJson, pullSecretSecret, corev1.DockerConfigJsonKey, "{}"),
				testSecret(corev1.SecretTypeOpaque, sshKeySecret, adminSSHKeySecretKey, "fakesshkey"),
			},
			validate: func(c client.Client, t *testing.T) {
				cd := getCD(c)
				if cd == nil || !apiequality.Semantic.DeepEqual(cd, testClusterDeployment()) {
					t.Errorf("got unexpected change in clusterdeployment")
				}
				job := getInstallJob(c)
				if job == nil || !apiequality.Semantic.DeepEqual(job, testInstallJob()) {
					t.Errorf("got unexpected change in install job")
				}
			},
		},
		{
			name: "Parse server URL from admin kubeconfig",
			existing: []runtime.Object{
				func() *hivev1.ClusterDeployment {
					cd := testClusterDeployment()
					cd.Status.Installed = true
					cd.Status.AdminKubeconfigSecret = corev1.LocalObjectReference{Name: adminKubeconfigSecret}
					return cd
				}(),
				testInstallJob(),
				testSecret(corev1.SecretTypeOpaque, adminKubeconfigSecret, "kubeconfig", adminKubeconfig),
				testSecret(corev1.SecretTypeDockerConfigJson, pullSecretSecret, corev1.DockerConfigJsonKey, "{}"),
				testSecret(corev1.SecretTypeOpaque, sshKeySecret, adminSSHKeySecretKey, "fakesshkey"),
				testMetadataConfigMap(),
			},
			validate: func(c client.Client, t *testing.T) {
				cd := getCD(c)
				assert.Equal(t, "https://bar-api.clusters.example.com:6443", cd.Status.APIURL)
				assert.Equal(t, "https://bar-api.clusters.example.com:6443/console", cd.Status.WebConsoleURL)
			},
		},
		{
			name: "Completed install job",
			existing: []runtime.Object{
				testClusterDeployment(),
				testCompletedInstallJob(),
				testMetadataConfigMap(),
				testSecret(corev1.SecretTypeOpaque, adminKubeconfigSecret, "kubeconfig", adminKubeconfig),
				testSecret(corev1.SecretTypeDockerConfigJson, pullSecretSecret, corev1.DockerConfigJsonKey, "{}"),
				testSecret(corev1.SecretTypeOpaque, sshKeySecret, adminSSHKeySecretKey, "fakesshkey"),
			},
			validate: func(c client.Client, t *testing.T) {
				cd := getCD(c)
				if cd == nil || !cd.Status.Installed {
					t.Errorf("did not get a clusterdeployment with a status of Installed")
					return
				}
			},
		},
		{
			name: "Legacy dockercfg pull secret causes no errors once installed",
			existing: []runtime.Object{
				func() *hivev1.ClusterDeployment {
					cd := testClusterDeployment()
					cd.Status.Installed = true
					cd.Status.AdminKubeconfigSecret = corev1.LocalObjectReference{Name: adminKubeconfigSecret}
					return cd
				}(),
				testCompletedInstallJob(),
				testMetadataConfigMap(),
				testSecret(corev1.SecretTypeOpaque, adminKubeconfigSecret, "kubeconfig", adminKubeconfig),
				testSecret(corev1.SecretTypeDockercfg, pullSecretSecret, corev1.DockerConfigKey, "{}"),
				testSecret(corev1.SecretTypeOpaque, sshKeySecret, adminSSHKeySecretKey, "fakesshkey"),
			},
		},
		{
			name: "Completed with install job manually deleted",
			existing: []runtime.Object{
				func() *hivev1.ClusterDeployment {
					cd := testClusterDeployment()
					cd.Status.Installed = true
					cd.Status.AdminKubeconfigSecret = corev1.LocalObjectReference{Name: adminKubeconfigSecret}
					return cd
				}(),
				testMetadataConfigMap(),
				testSecret(corev1.SecretTypeOpaque, adminKubeconfigSecret, "kubeconfig", adminKubeconfig),
				testSecret(corev1.SecretTypeDockerConfigJson, pullSecretSecret, corev1.DockerConfigJsonKey, "{}"),
				testSecret(corev1.SecretTypeOpaque, sshKeySecret, adminSSHKeySecretKey, "fakesshkey"),
			},
			validate: func(c client.Client, t *testing.T) {
				cd := getCD(c)
				assert.True(t, cd.Status.Installed)
				job := getInstallJob(c)
				assert.Nil(t, job)
			},
		},
		{
			name: "Delete cluster deployment",
			existing: []runtime.Object{
				testDeletedClusterDeployment(),
				testSecret(corev1.SecretTypeDockerConfigJson, pullSecretSecret, corev1.DockerConfigJsonKey, "{}"),
				testSecret(corev1.SecretTypeOpaque, sshKeySecret, adminSSHKeySecretKey, "fakesshkey"),
				func() *batchv1.Job {
					job, _, _ := install.GenerateInstallerJob(
						testExpiredClusterDeployment(),
						"example.com/fake:latest",
						"",
						"fakeserviceaccount",
						"sshkey",
						"pullsecret")
					return job
				}(),
			},
			validate: func(c client.Client, t *testing.T) {
				instJob := getInstallJob(c)
				if instJob != nil {
					t.Errorf("got unexpected install job (expected delete)")
				}
			},
		},
		{
			name: "No-op deleted cluster without finalizer",
			existing: []runtime.Object{
				testDeletedClusterDeploymentWithoutFinalizer(),
				testSecret(corev1.SecretTypeDockerConfigJson, pullSecretSecret, corev1.DockerConfigJsonKey, "{}"),
				testSecret(corev1.SecretTypeOpaque, sshKeySecret, adminSSHKeySecretKey, "fakesshkey"),
			},
			validate: func(c client.Client, t *testing.T) {
				deprovision := getDeprovisionRequest(c)
				if deprovision != nil {
					t.Errorf("got unexpected deprovision request")
				}
			},
		},
		{
			name: "Delete expired cluster deployment",
			existing: []runtime.Object{
				testExpiredClusterDeployment(),
				testSecret(corev1.SecretTypeDockerConfigJson, pullSecretSecret, corev1.DockerConfigJsonKey, "{}"),
				testSecret(corev1.SecretTypeOpaque, sshKeySecret, adminSSHKeySecretKey, "fakesshkey"),
			},
			validate: func(c client.Client, t *testing.T) {
				cd := getCD(c)
				if cd != nil {
					t.Errorf("got unexpected cluster deployment (expected deleted)")
				}
			},
		},
		{
			name: "Test PreserveOnDelete",
			existing: []runtime.Object{
				func() *hivev1.ClusterDeployment {
					cd := testDeletedClusterDeployment()
					cd.Status.Installed = true
					cd.Spec.PreserveOnDelete = true
					return cd
				}(),
				testSecret(corev1.SecretTypeDockerConfigJson, pullSecretSecret, corev1.DockerConfigJsonKey, "{}"),
				testSecret(corev1.SecretTypeOpaque, sshKeySecret, adminSSHKeySecretKey, "fakesshkey"),
				func() *batchv1.Job {
					job, _, _ := install.GenerateInstallerJob(
						testExpiredClusterDeployment(),
						"example.com/fake:latest",
						"",
						"fakeserviceaccount",
						"sshkey",
						"pullsecret")
					return job
				}(),
			},
			validate: func(c client.Client, t *testing.T) {
				deprovision := getDeprovisionRequest(c)
				assert.Nil(t, deprovision)
			},
		},
		{
			name: "Test deletion of expired jobs",
			existing: []runtime.Object{
				func() *hivev1.ClusterDeployment {
					cd := testClusterDeployment()
					cd.Status.Installed = false
					return cd
				}(),
				testSecret(corev1.SecretTypeOpaque, adminKubeconfigSecret, "kubeconfig", adminKubeconfig),
				testSecret(corev1.SecretTypeDockerConfigJson, pullSecretSecret, corev1.DockerConfigJsonKey, "{}"),
				testSecret(corev1.SecretTypeOpaque, sshKeySecret, adminSSHKeySecretKey, "fakesshkey"),
				func() *batchv1.Job {
					job, _, _ := install.GenerateInstallerJob(
						testClusterDeployment(),
						"fakeserviceaccount",
						"",
						"fakeserviceaccount",
						"sshkey",
						"pullsecret")
					wrongGeneration := "-1"
					job.Annotations[clusterDeploymentGenerationAnnotation] = wrongGeneration
					return job
				}(),
			},
			validate: func(c client.Client, t *testing.T) {
				job := getInstallJob(c)
				assert.Nil(t, job)
			},
		},
		{
			name: "Test creation of uninstall job when PreserveOnDelete is true but cluster deployment is not installed",
			existing: []runtime.Object{
				func() *hivev1.ClusterDeployment {
					cd := testDeletedClusterDeployment()
					cd.Spec.PreserveOnDelete = true
					cd.Status.Installed = false
					return cd
				}(),
				testSecret(corev1.SecretTypeDockerConfigJson, pullSecretSecret, corev1.DockerConfigJsonKey, "{}"),
				testSecret(corev1.SecretTypeOpaque, sshKeySecret, adminSSHKeySecretKey, "fakesshkey"),
			},
			validate: func(c client.Client, t *testing.T) {
				deprovision := getDeprovisionRequest(c)
				assert.NotNil(t, deprovision)
			},
		},
		{
			name: "Resolve installer image from spec.images.installerimage",
			existing: []runtime.Object{
				func() *hivev1.ClusterDeployment {
					cd := testClusterDeployment()
					cd.Status.InstallerImage = nil
					cd.Spec.Images.InstallerImage = "test-installer-image:latest"
					return cd
				}(),
				testSecret(corev1.SecretTypeDockerConfigJson, pullSecretSecret, corev1.DockerConfigJsonKey, "{}"),
				testSecret(corev1.SecretTypeOpaque, sshKeySecret, adminSSHKeySecretKey, "fakesshkey"),
			},
			validate: func(c client.Client, t *testing.T) {
				cd := getCD(c)
				if cd.Status.InstallerImage == nil || *cd.Status.InstallerImage != "test-installer-image:latest" {
					t.Errorf("unexpected status.installerImage")
				}
			},
		},
		{
			name: "Resolve installer image from imageSet.spec.installerimage",
			existing: []runtime.Object{
				func() *hivev1.ClusterDeployment {
					cd := testClusterDeployment()
					cd.Status.InstallerImage = nil
					cd.Spec.Images.InstallerImage = ""
					cd.Spec.ImageSet = &hivev1.ClusterImageSetReference{Name: testClusterImageSetName}
					return cd
				}(),
				func() *hivev1.ClusterImageSet {
					cis := testClusterImageSet()
					cis.Spec.InstallerImage = strPtr("test-cis-installer-image:latest")
					return cis
				}(),
				testSecret(corev1.SecretTypeDockerConfigJson, pullSecretSecret, corev1.DockerConfigJsonKey, "{}"),
				testSecret(corev1.SecretTypeOpaque, sshKeySecret, adminSSHKeySecretKey, "fakesshkey"),
			},
			validate: func(c client.Client, t *testing.T) {
				cd := getCD(c)
				if cd.Status.InstallerImage == nil || *cd.Status.InstallerImage != "test-cis-installer-image:latest" {
					t.Errorf("unexpected status.installerImage")
				}
			},
		},
		{
			name: "Create job to resolve installer image",
			existing: []runtime.Object{
				func() *hivev1.ClusterDeployment {
					cd := testClusterDeployment()
					cd.Status.InstallerImage = nil
					cd.Spec.Images.InstallerImage = ""
					cd.Spec.ImageSet = &hivev1.ClusterImageSetReference{Name: testClusterImageSetName}
					return cd
				}(),
				testClusterImageSet(),
				testSecret(corev1.SecretTypeDockerConfigJson, pullSecretSecret, corev1.DockerConfigJsonKey, "{}"),
				testSecret(corev1.SecretTypeOpaque, sshKeySecret, adminSSHKeySecretKey, "fakesshkey"),
			},
			validate: func(c client.Client, t *testing.T) {
				job := getImageSetJob(c)
				if job == nil {
					t.Errorf("did not find expected imageset job")
				}
				// Ensure that the release image from the imageset is used in the job
				envVars := job.Spec.Template.Spec.Containers[0].Env
				for _, e := range envVars {
					if e.Name == "RELEASE_IMAGE" {
						if e.Value != *testClusterImageSet().Spec.ReleaseImage {
							t.Errorf("unexpected release image used in job: %s", e.Value)
						}
						break
					}
				}
			},
		},
		{
			name: "Ensure release image from clusterdeployment (when present) is used to generate imageset job",
			existing: []runtime.Object{
				func() *hivev1.ClusterDeployment {
					cd := testClusterDeployment()
					cd.Status.InstallerImage = nil
					cd.Spec.Images.InstallerImage = ""
					cd.Spec.Images.ReleaseImage = "embedded-release-image:latest"
					cd.Spec.ImageSet = &hivev1.ClusterImageSetReference{Name: testClusterImageSetName}
					return cd
				}(),
				testClusterImageSet(),
				testSecret(corev1.SecretTypeDockerConfigJson, pullSecretSecret, corev1.DockerConfigJsonKey, "{}"),
				testSecret(corev1.SecretTypeOpaque, sshKeySecret, adminSSHKeySecretKey, "fakesshkey"),
			},
			validate: func(c client.Client, t *testing.T) {
				job := getImageSetJob(c)
				if job == nil {
					t.Errorf("did not find expected imageset job")
				}
				envVars := job.Spec.Template.Spec.Containers[0].Env
				for _, e := range envVars {
					if e.Name == "RELEASE_IMAGE" {
						if e.Value != "embedded-release-image:latest" {
							t.Errorf("unexpected release image used in job: %s", e.Value)
						}
						break
					}
				}
			},
		},
		{
			name: "Ensure release image from clusterimageset is used as override image in install job",
			existing: []runtime.Object{
				func() *hivev1.ClusterDeployment {
					cd := testClusterDeployment()
					cd.Status.InstallerImage = strPtr("test-installer-image:latest")
					cd.Spec.Images.InstallerImage = ""
					cd.Spec.ImageSet = &hivev1.ClusterImageSetReference{Name: testClusterImageSetName}
					return cd
				}(),
				func() *hivev1.ClusterImageSet {
					cis := testClusterImageSet()
					cis.Spec.ReleaseImage = strPtr("test-release-image:latest")
					return cis
				}(),
				testSecret(corev1.SecretTypeDockerConfigJson, pullSecretSecret, corev1.DockerConfigJsonKey, "{}"),
				testSecret(corev1.SecretTypeOpaque, sshKeySecret, adminSSHKeySecretKey, "fakesshkey"),
			},
			validate: func(c client.Client, t *testing.T) {
				job := getInstallJob(c)
				if job == nil {
					t.Errorf("did not get expected job")
					return
				}
				env := job.Spec.Template.Spec.Containers[0].Env
				variable := corev1.EnvVar{}
				found := false
				for _, e := range env {
					if e.Name == "OPENSHIFT_INSTALL_RELEASE_IMAGE_OVERRIDE" {
						variable = e
						found = true
						break
					}
				}
				if !found {
					t.Errorf("did not find expected override environment variable in job")
					return
				}
				if variable.Value != "test-release-image:latest" {
					t.Errorf("environment variable did not have the expected value. actual: %s", variable.Value)
				}
			},
		},
		{
			name: "Create DNSZone when manageDNS is true",
			existing: []runtime.Object{
				func() *hivev1.ClusterDeployment {
					cd := testClusterDeployment()
					cd.Spec.ManageDNS = true
					return cd
				}(),
				testSecret(corev1.SecretTypeDockerConfigJson, pullSecretSecret, corev1.DockerConfigJsonKey, "{}"),
				testSecret(corev1.SecretTypeOpaque, sshKeySecret, adminSSHKeySecretKey, "fakesshkey"),
			},
			validate: func(c client.Client, t *testing.T) {
				zone := getDNSZone(c)
				assert.NotNil(t, zone, "dns zone should exist")
			},
		},
		{
			name: "Wait when DNSZone is not available yet",
			existing: []runtime.Object{
				func() *hivev1.ClusterDeployment {
					cd := testClusterDeployment()
					cd.Spec.ManageDNS = true
					return cd
				}(),
				testSecret(corev1.SecretTypeDockerConfigJson, pullSecretSecret, corev1.DockerConfigJsonKey, "{}"),
				testSecret(corev1.SecretTypeOpaque, sshKeySecret, adminSSHKeySecretKey, "fakesshkey"),
				testDNSZone(),
			},
			validate: func(c client.Client, t *testing.T) {
				installJob := getInstallJob(c)
				assert.Nil(t, installJob, "install job should not exist")
			},
		},
		{
			name: "Create install job when DNSZone is ready",
			existing: []runtime.Object{
				func() *hivev1.ClusterDeployment {
					cd := testClusterDeployment()
					cd.Spec.ManageDNS = true
					return cd
				}(),
				testSecret(corev1.SecretTypeDockerConfigJson, pullSecretSecret, corev1.DockerConfigJsonKey, "{}"),
				testSecret(corev1.SecretTypeOpaque, sshKeySecret, adminSSHKeySecretKey, "fakesshkey"),
				testAvailableDNSZone(),
			},
			validate: func(c client.Client, t *testing.T) {
				installJob := getInstallJob(c)
				assert.NotNil(t, installJob, "install job should exist")
			},
		},
		{
			name: "Ensure managed DNSZone is deleted with cluster deployment",
			existing: []runtime.Object{
				func() *hivev1.ClusterDeployment {
					cd := testDeletedClusterDeployment()
					cd.Spec.ManageDNS = true
					return cd
				}(),
				testSecret(corev1.SecretTypeDockerConfigJson, pullSecretSecret, corev1.DockerConfigJsonKey, "{}"),
				testSecret(corev1.SecretTypeOpaque, sshKeySecret, adminSSHKeySecretKey, "fakesshkey"),
				testDNSZone(),
			},
			validate: func(c client.Client, t *testing.T) {
				dnsZone := getDNSZone(c)
				assert.Nil(t, dnsZone, "dnsZone should not exist")
			},
		},
		{
			name: "Delete cluster deployment with image from clusterimageset",
			existing: []runtime.Object{
				func() *hivev1.ClusterDeployment {
					cd := testDeletedClusterDeployment()
					cd.Spec.Images.HiveImage = ""
					cd.Spec.ImageSet = &hivev1.ClusterImageSetReference{Name: testClusterImageSetName}
					return cd
				}(),
				func() *hivev1.ClusterImageSet {
					cis := testClusterImageSet()
					testHiveImage := "hive-image-from-image-set:latest"
					cis.Spec.HiveImage = &testHiveImage
					return cis
				}(),
				testSecret(corev1.SecretTypeDockerConfigJson, pullSecretSecret, corev1.DockerConfigJsonKey, "{}"),
				testSecret(corev1.SecretTypeOpaque, sshKeySecret, adminSSHKeySecretKey, "fakesshkey"),
			},
			validate: func(c client.Client, t *testing.T) {
				deprovision := getDeprovisionRequest(c)
				if deprovision == nil {
					t.Errorf("did not find expected deprovision request")
				}
			},
		},
		{
			name: "Delete cluster deployment with missing clusterimageset",
			existing: []runtime.Object{
				func() *hivev1.ClusterDeployment {
					cd := testDeletedClusterDeployment()
					cd.Spec.Images.HiveImage = ""
					cd.Spec.ImageSet = &hivev1.ClusterImageSetReference{Name: testClusterImageSetName}
					return cd
				}(),
				testSecret(corev1.SecretTypeDockerConfigJson, pullSecretSecret, corev1.DockerConfigJsonKey, "{}"),
				testSecret(corev1.SecretTypeOpaque, sshKeySecret, adminSSHKeySecretKey, "fakesshkey"),
			},
			validate: func(c client.Client, t *testing.T) {
				deprovision := getDeprovisionRequest(c)
				if deprovision == nil {
					t.Errorf("did not find expected deprovision request")
				}
			},
		},
		{
			name: "Migrate wildcard ingress domains",
			existing: []runtime.Object{
				func() *hivev1.ClusterDeployment {
					cd := testClusterDeploymentWithIngress()
					cd.Spec.Ingress[0].Domain = fmt.Sprintf("*.apps.%s.example.com", cd.Spec.ClusterName)
					cd.Spec.Ingress = append(cd.Spec.Ingress, hivev1.ClusterIngress{
						Name:   "extraingress",
						Domain: fmt.Sprintf("*.moreingress.%s.example.com", cd.Spec.ClusterName),
					})
					cd.Spec.Ingress = append(cd.Spec.Ingress, hivev1.ClusterIngress{
						Name:   "notwildcardingress",
						Domain: fmt.Sprintf("notwild.%s.example.com", cd.Spec.ClusterName),
					})
					return cd
				}(),
			},
			validate: func(c client.Client, t *testing.T) {
				cd := getCD(c)
				for _, ingress := range cd.Spec.Ingress {
					assert.NotRegexp(t, `^\*.*`, ingress.Domain, "Ingress domain %s wasn't migrated from wildcards", ingress.Domain)
				}
			},
		},
		{
			name: "Delete old install job when job hash missing",
			existing: []runtime.Object{
				testClusterDeployment(),
				testSecret(corev1.SecretTypeDockerConfigJson, pullSecretSecret, corev1.DockerConfigJsonKey, "{}"),
				testSecret(corev1.SecretTypeOpaque, sshKeySecret, adminSSHKeySecretKey, "fakesshkey"),
				func() *batchv1.Job {
					job := testInstallJob()
					delete(job.Annotations, jobHashAnnotation)
					return job
				}(),
			},
			validate: func(c client.Client, t *testing.T) {
				installJob := getInstallJob(c)
				assert.Nil(t, installJob, "install job should not exist")
			},
		},
		{
			name: "Delete old install job when job hash changes",
			existing: []runtime.Object{
				testClusterDeployment(),
				testSecret(corev1.SecretTypeDockerConfigJson, pullSecretSecret, corev1.DockerConfigJsonKey, "{}"),
				testSecret(corev1.SecretTypeOpaque, sshKeySecret, adminSSHKeySecretKey, "fakesshkey"),
				func() *batchv1.Job {
					job := testInstallJob()
					job.Annotations[jobHashAnnotation] = "DIFFERENTHASH"
					return job
				}(),
			},
			validate: func(c client.Client, t *testing.T) {
				installJob := getInstallJob(c)
				assert.Nil(t, installJob, "install job should not exist")
			},
		},
		{
			name: "Ignore old install job hash difference if cluster already installed",
			existing: []runtime.Object{
				func() *hivev1.ClusterDeployment {
					cd := testClusterDeployment()
					cd.Status.Installed = true
					cd.Status.AdminKubeconfigSecret = corev1.LocalObjectReference{Name: adminKubeconfigSecret}
					return cd
				}(),
				testSecret(corev1.SecretTypeOpaque, adminKubeconfigSecret, "kubeconfig", adminKubeconfig),
				testSecret(corev1.SecretTypeDockerConfigJson, pullSecretSecret, corev1.DockerConfigJsonKey, "{}"),
				testSecret(corev1.SecretTypeOpaque, sshKeySecret, adminSSHKeySecretKey, "fakesshkey"),
				func() *batchv1.Job {
					job := testInstallJob()
					job.Annotations[jobHashAnnotation] = "DIFFERENTHASH"
					return job
				}(),
			},
			validate: func(c client.Client, t *testing.T) {
				installJob := getInstallJob(c)
				assert.NotNil(t, installJob, "install job should not be touched after the clusterdeployment is installed")
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fakeClient := fake.NewFakeClient(test.existing...)
			rcd := &ReconcileClusterDeployment{
				Client:                        fakeClient,
				scheme:                        scheme.Scheme,
				remoteClusterAPIClientBuilder: testRemoteClusterAPIClientBuilder,
			}

			_, err := rcd.Reconcile(reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      testName,
					Namespace: testNamespace,
				},
			})

			if test.validate != nil {
				test.validate(fakeClient, t)
			}

			if err != nil && !test.expectErr {
				t.Errorf("Unexpected error: %v", err)
			}
			if err == nil && test.expectErr {
				t.Errorf("Expected error but got none")
			}
		})
	}
}

func TestClusterDeploymentReconcileResults(t *testing.T) {
	apis.AddToScheme(scheme.Scheme)

	tests := []struct {
		name                     string
		existing                 []runtime.Object
		exptectedReconcileResult reconcile.Result
	}{
		{
			name: "Requeue after adding finalizer",
			existing: []runtime.Object{
				testClusterDeploymentWithoutFinalizer(),
			},
			exptectedReconcileResult: reconcile.Result{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fakeClient := fake.NewFakeClient(test.existing...)
			rcd := &ReconcileClusterDeployment{
				Client:                        fakeClient,
				scheme:                        scheme.Scheme,
				remoteClusterAPIClientBuilder: testRemoteClusterAPIClientBuilder,
			}

			reconcileResult, err := rcd.Reconcile(reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      testName,
					Namespace: testNamespace,
				},
			})

			assert.NoError(t, err, "unexpected error")

			assert.Equal(t, test.exptectedReconcileResult, reconcileResult, "unexpected reconcile result")
		})
	}
}

func testEmptyClusterDeployment() *hivev1.ClusterDeployment {
	cd := &hivev1.ClusterDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        testName,
			Namespace:   testNamespace,
			Finalizers:  []string{hivev1.FinalizerDeprovision},
			UID:         types.UID("1234"),
			Annotations: map[string]string{},
		},
	}
	return cd
}

func testClusterDeployment() *hivev1.ClusterDeployment {
	cd := testEmptyClusterDeployment()

	cd.Spec = hivev1.ClusterDeploymentSpec{
		ClusterName: testClusterName,
		SSHKey: &corev1.LocalObjectReference{
			Name: sshKeySecret,
		},
		ControlPlane: hivev1.MachinePool{},
		Compute:      []hivev1.MachinePool{},
		PullSecret: corev1.LocalObjectReference{
			Name: pullSecretSecret,
		},
		Platform: hivev1.Platform{
			AWS: &hivev1.AWSPlatform{
				Region: "us-east-1",
			},
		},
		Networking: hivev1.Networking{
			Type: hivev1.NetworkTypeOpenshiftSDN,
		},
		PlatformSecrets: hivev1.PlatformSecrets{
			AWS: &hivev1.AWSPlatformSecrets{
				Credentials: corev1.LocalObjectReference{
					Name: "aws-credentials",
				},
			},
		},
	}

	cd.Status = hivev1.ClusterDeploymentStatus{
		ClusterID:      testClusterID,
		InfraID:        testInfraID,
		InstallerImage: strPtr("installer-image:latest"),
	}

	controllerutils.FixupEmptyClusterVersionFields(&cd.Status.ClusterVersionStatus)
	return cd
}

func testClusterDeploymentWithoutFinalizer() *hivev1.ClusterDeployment {
	cd := testClusterDeployment()
	cd.Finalizers = []string{}
	return cd
}

func testDeletedClusterDeployment() *hivev1.ClusterDeployment {
	cd := testClusterDeployment()
	now := metav1.Now()
	cd.DeletionTimestamp = &now
	return cd
}

func testDeletedClusterDeploymentWithoutFinalizer() *hivev1.ClusterDeployment {
	cd := testClusterDeployment()
	now := metav1.Now()
	cd.DeletionTimestamp = &now
	cd.Finalizers = []string{}
	return cd
}

func testExpiredClusterDeployment() *hivev1.ClusterDeployment {
	cd := testClusterDeployment()
	cd.CreationTimestamp = metav1.Time{Time: metav1.Now().Add(-60 * time.Minute)}
	cd.Annotations[deleteAfterAnnotation] = "5m"
	return cd
}

func testInstallJob() *batchv1.Job {
	cd := testClusterDeployment()
	job, _, err := install.GenerateInstallerJob(cd,
		images.DefaultHiveImage,
		"",
		serviceAccountName, "testSSHKey", "testPullSecret")
	if err != nil {
		panic("should not error while generating test install job")
	}

	controllerutil.SetControllerReference(cd, job, scheme.Scheme)

	hash, err := calculateJobSpecHash(job)
	if err != nil {
		panic("should never get error calculating job spec hash")
	}

	job.Annotations[jobHashAnnotation] = hash
	return job
}

func testCompletedInstallJob() *batchv1.Job {
	job := testInstallJob()
	job.Status.Conditions = []batchv1.JobCondition{
		{
			Type:   batchv1.JobComplete,
			Status: corev1.ConditionTrue,
		},
	}
	return job
}

func testMetadataConfigMap() *corev1.ConfigMap {
	cm := &corev1.ConfigMap{}
	cm.Name = metadataName
	cm.Namespace = testNamespace
	metadataJSON := `{
		"aws": {
			"identifier": [{"openshiftClusterID": "testFooClusterUUID"}]
		}
	}`
	cm.Data = map[string]string{"metadata.json": metadataJSON}
	return cm
}

func testSecret(secretType corev1.SecretType, name, key, value string) *corev1.Secret {
	s := &corev1.Secret{
		Type: secretType,
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Data: map[string][]byte{
			key: []byte(value),
		},
	}
	return s
}

func testRemoteClusterAPIClientBuilder(secretData string) (client.Client, error) {
	remoteClusterVersion := &openshiftapiv1.ClusterVersion{
		ObjectMeta: metav1.ObjectMeta{
			Name: remoteClusterVersionObjectName,
		},
	}
	remoteClusterVersion.Status = testRemoteClusterVersionStatus()

	remoteClusterRouteObject := &routev1.Route{
		ObjectMeta: metav1.ObjectMeta{
			Name:      remoteClusterRouteObjectName,
			Namespace: remoteClusterRouteObjectNamespace,
		},
	}
	remoteClusterRouteObject.Spec.Host = "bar-api.clusters.example.com:6443/console"

	remoteClient := fake.NewFakeClient(remoteClusterVersion, remoteClusterRouteObject)
	return remoteClient, nil
}

func testRemoteClusterVersionStatus() openshiftapiv1.ClusterVersionStatus {
	status := openshiftapiv1.ClusterVersionStatus{
		History: []openshiftapiv1.UpdateHistory{
			{
				State:   openshiftapiv1.CompletedUpdate,
				Version: testRemoteClusterCurrentVersion,
				Image:   "TESTIMAGE",
			},
		},
		ObservedGeneration: 123456789,
		VersionHash:        "TESTVERSIONHASH",
	}
	return status
}

func testClusterImageSet() *hivev1.ClusterImageSet {
	cis := &hivev1.ClusterImageSet{}
	cis.Name = testClusterImageSetName
	cis.Spec.ReleaseImage = strPtr("test-release-image:latest")
	return cis
}

func testDNSZone() *hivev1.DNSZone {
	zone := &hivev1.DNSZone{}
	zone.Name = testName + "-zone"
	zone.Namespace = testNamespace
	return zone
}

func testAvailableDNSZone() *hivev1.DNSZone {
	zone := testDNSZone()
	zone.Status.Conditions = []hivev1.DNSZoneCondition{
		{
			Type:   hivev1.ZoneAvailableDNSZoneCondition,
			Status: corev1.ConditionTrue,
		},
	}
	return zone
}

func testClusterDeploymentWithIngress() *hivev1.ClusterDeployment {
	cd := testClusterDeployment()
	cd.Spec.Ingress = []hivev1.ClusterIngress{
		{
			Name:   "default",
			Domain: fmt.Sprintf("apps.%s.example.com", cd.Spec.ClusterName),
		},
	}

	return cd
}

func TestClusterDeploymentWildcardDomainMigration(t *testing.T) {
	apis.AddToScheme(scheme.Scheme)

	tests := []struct {
		name              string
		existing          *hivev1.ClusterDeployment
		migrationExpected bool
		expectedDomains   []string // must be same length as the number of ingress entries for the test clusterDeployment
	}{
		{
			name:              "No ingress, no migration",
			existing:          testClusterDeployment(),
			migrationExpected: false,
		},
		{
			name:              "No wildcards, no migration",
			existing:          testClusterDeploymentWithIngress(),
			migrationExpected: false,
			expectedDomains:   []string{fmt.Sprintf("apps.%s.example.com", testClusterName)},
		},
		{
			name: "Migrate wildcard domain",
			existing: func() *hivev1.ClusterDeployment {
				cd := testClusterDeploymentWithIngress()
				cd.Spec.Ingress[0].Domain = fmt.Sprintf("*.apps.%s.example.com", cd.Spec.ClusterName)

				return cd
			}(),
			migrationExpected: true,
			expectedDomains:   []string{fmt.Sprintf("apps.%s.example.com", testClusterName)},
		},
		{
			name: "Migrate when only one of two is wildcard",
			existing: func() *hivev1.ClusterDeployment {
				cd := testClusterDeploymentWithIngress()
				cd.Spec.Ingress = append(cd.Spec.Ingress, hivev1.ClusterIngress{
					Name:   "extraingress",
					Domain: fmt.Sprintf("*.moreingress.%s.example.com", cd.Spec.ClusterName),
				})

				return cd
			}(),
			migrationExpected: true,
			expectedDomains: []string{
				fmt.Sprintf("apps.%s.example.com", testClusterName),
				fmt.Sprintf("moreingress.%s.example.com", testClusterName),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {

			result := migrateWildcardIngress(test.existing)

			assert.Equal(t, test.migrationExpected, result)

			for i, domain := range test.expectedDomains {
				assert.Equal(t, domain, test.existing.Spec.Ingress[i].Domain)
			}
		})
	}
}

func TestClusterDeploymentJobHashing(t *testing.T) {
	apis.AddToScheme(scheme.Scheme)

	tests := []struct {
		name           string
		existingJob    *batchv1.Job
		generatedJob   *batchv1.Job
		expectedResult bool
		expectedError  bool
	}{
		{
			name: "No existing annotation",
			existingJob: func() *batchv1.Job {
				job := testInstallJob()
				delete(job.Annotations, jobHashAnnotation)
				return job
			}(),
			generatedJob:   testInstallJob(),
			expectedResult: true,
		},
		{
			name: "Different hash",
			existingJob: func() *batchv1.Job {
				job := testInstallJob()
				job.Annotations[jobHashAnnotation] = "DIFFERENTHASH"
				return job
			}(),
			generatedJob:   testInstallJob(),
			expectedResult: true,
		},
		{
			name:           "Same hash",
			existingJob:    testInstallJob(),
			generatedJob:   testInstallJob(),
			expectedResult: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fakeClient := fake.NewFakeClient(test.existingJob)
			rcd := &ReconcileClusterDeployment{
				Client: fakeClient,
			}
			tLogger := log.New()

			result, err := rcd.deleteJobOnHashChange(test.existingJob, test.generatedJob, tLogger)

			if test.expectedError {
				assert.Error(t, err, "expected error during test case")
			} else {
				assert.Equal(t, test.expectedResult, result)

				if test.expectedResult { //if job was deleted
					job := getInstallJob(fakeClient)
					assert.Nil(t, job, "previous install job should have been deleted")
				}
			}

		})
	}
}

func getJob(c client.Client, name string) *batchv1.Job {
	job := &batchv1.Job{}
	err := c.Get(context.TODO(), client.ObjectKey{Name: name, Namespace: testNamespace}, job)
	if err == nil {
		return job
	}
	return nil
}

func getInstallJob(c client.Client) *batchv1.Job {
	return getJob(c, installJobName)
}
